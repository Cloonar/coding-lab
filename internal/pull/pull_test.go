package pull

// Digest rendering is pinned with golden-string unit tests (the digest is
// pasted verbatim to an agent, so its exact shape IS the contract). The
// service tests mirror the crmerge/gitx pull-test harness: a real sqlite
// store via testutil, a real gitx.Engine over fixture repos under
// testutil.HermeticGitEnv, and the production topology — a bare origin, a
// work clone that advances origin's main (the "someone else pushed" actor),
// the lab bare reference clone, and the run's live worktree the pull merges
// into.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// --- digest rendering ------------------------------------------------------

const (
	digestOldSHA = "0123456789abcdef0123456789abcdef01234567"
	digestNewSHA = "fedcba9876543210fedcba9876543210fedcba98"
	digestRange  = "0123456789ab..fedcba987654"
)

func TestRenderDigest_fastForwardNoTruncation(t *testing.T) {
	r := Result{Base: "main", OldHead: digestOldSHA, NewHead: digestNewSHA, FastForward: true}
	sum := gitx.RangeSummary{
		Subjects:     []string{"feat: add thing (#12)", "fix: nil deref"},
		TotalCommits: 2,
		Files: []gitx.FileChange{
			{Status: "M", Path: "internal/a.go"},
			{Status: "A", Path: "docs/b.md"},
		},
		TotalFiles: 2,
	}
	want := "Lab pulled origin/main into this worktree: " + digestRange + " (fast-forward).\n" +
		"Incoming commits (2):\n" +
		"- feat: add thing (#12)\n" +
		"- fix: nil deref\n" +
		"Files changed:\n" +
		"M\tinternal/a.go\n" +
		"A\tdocs/b.md\n" +
		"Prior reads of these files may be stale — re-read anything you rely on before acting on it. " +
		"For detail: git log " + digestRange + " --oneline, git diff " + digestRange
	if got := renderDigest(r, sum); got != want {
		t.Errorf("digest mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderDigest_mergeCommitTruncated(t *testing.T) {
	r := Result{Base: "release", OldHead: digestOldSHA, NewHead: digestNewSHA}
	sum := gitx.RangeSummary{
		Subjects:     []string{"subject three", "subject two"},
		TotalCommits: 5,
		Files:        []gitx.FileChange{{Status: "R100", Path: "old.go -> new.go"}},
		TotalFiles:   4,
	}
	rng := "0123456789ab..fedcba987654"
	want := "Lab pulled origin/release into this worktree: " + rng + " (merge commit).\n" +
		"Incoming commits (5):\n" +
		"- subject three\n" +
		"- subject two\n" +
		"… and 3 more\n" +
		"Files changed:\n" +
		"R100\told.go -> new.go\n" +
		"… and 3 more\n" +
		"Prior reads of these files may be stale — re-read anything you rely on before acting on it. " +
		"For detail: git log " + rng + " --oneline, git diff " + rng
	if got := renderDigest(r, sum); got != want {
		t.Errorf("digest mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// The read-only import block (issue #261 / ADR-0063) hangs off the ordinary
// pull digest: one line per import in declaration order, all four outcomes
// distinguishable, and a single closing caution — pinned byte-for-byte,
// because this is what an agent reads to decide whether to re-read a sibling.
func TestRenderDigest_mergedBaseWithMixedImports(t *testing.T) {
	r := Result{
		Base: "main", OldHead: digestOldSHA, NewHead: digestNewSHA, FastForward: true,
		Imports: []ImportChange{
			{Name: "libcore", Old: digestOldSHA, New: digestNewSHA, Commits: 3},
			{Name: "libnew", New: digestNewSHA}, // no sidecar: previous commit unknown
			{Name: "libweb", Old: digestNewSHA, New: digestNewSHA},
			{Name: "libz", Failed: true, Reason: "fatal: 'origin' does not appear to be a git repository"},
		},
	}
	sum := gitx.RangeSummary{
		Subjects:     []string{"feat: add thing (#12)"},
		TotalCommits: 1,
		Files:        []gitx.FileChange{{Status: "M", Path: "internal/a.go"}},
		TotalFiles:   1,
	}
	want := "Lab pulled origin/main into this worktree: " + digestRange + " (fast-forward).\n" +
		"Incoming commits (1):\n" +
		"- feat: add thing (#12)\n" +
		"Files changed:\n" +
		"M\tinternal/a.go\n" +
		"Prior reads of these files may be stale — re-read anything you rely on before acting on it. " +
		"For detail: git log " + digestRange + " --oneline, git diff " + digestRange + "\n" +
		"Imports refreshed:\n" +
		"- libcore: 0123456789ab..fedcba987654 (3 commits)\n" +
		"- libnew: refreshed to fedcba987654\n" +
		"- libweb: unchanged\n" +
		"- libz: refresh failed — fatal: 'origin' does not appear to be a git repository\n" +
		"Prior reads of moved snapshots may be stale — re-read anything you rely on from them."
	if got := renderDigest(r, sum); got != want {
		t.Errorf("digest mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A pull that only refreshed imports has no range, no commits and no files to
// report, so the base block collapses to one line that says so — a digest can
// never claim to have pulled something it did not.
func TestRenderDigest_importsOnlyBaseUpToDate(t *testing.T) {
	r := Result{
		Base: "main", OldHead: digestOldSHA, NewHead: digestOldSHA,
		Imports: []ImportChange{
			{Name: "libcore", Old: digestOldSHA, New: digestNewSHA, Commits: 2},
			{Name: "libweb", Old: digestNewSHA, New: digestNewSHA},
		},
	}
	want := "Lab refreshed this run's read-only imports; origin/main was already up to date.\n" +
		"Imports refreshed:\n" +
		"- libcore: 0123456789ab..fedcba987654 (2 commits)\n" +
		"- libweb: unchanged\n" +
		"Prior reads of moved snapshots may be stale — re-read anything you rely on from them."
	if got := renderDigest(r, gitx.RangeSummary{}); got != want {
		t.Errorf("digest mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// Imports that all came back on the same commit still get their block — the
// agent asked, and "unchanged" is an answer — but NOT the caution line: no
// snapshot moved, so nothing an agent already read went stale.
func TestRenderDigest_unchangedImportsCarryNoCaution(t *testing.T) {
	r := Result{
		Base: "main", OldHead: digestOldSHA, NewHead: digestNewSHA, FastForward: true,
		Imports: []ImportChange{{Name: "libcore", Old: digestNewSHA, New: digestNewSHA}},
	}
	want := "Lab pulled origin/main into this worktree: " + digestRange + " (fast-forward).\n" +
		"Incoming commits (0):\n" +
		"Files changed:\n" +
		"Prior reads of these files may be stale — re-read anything you rely on before acting on it. " +
		"For detail: git log " + digestRange + " --oneline, git diff " + digestRange + "\n" +
		"Imports refreshed:\n" +
		"- libcore: unchanged"
	if got := renderDigest(r, gitx.RangeSummary{}); got != want {
		t.Errorf("digest mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// --- service fixture -------------------------------------------------------

const (
	pullAuthorName  = "Repo Author"
	pullAuthorEmail = "repo@example.invalid"
	pullAuthorID    = pullAuthorName + "|" + pullAuthorEmail + "|" + pullAuthorName + "|" + pullAuthorEmail
	pullBranch      = "afk/9"
)

type fixture struct {
	t        *testing.T
	svc      *Service
	st       *store.Store
	bus      *events.Bus
	eng      *gitx.Engine
	home     string
	work     string // working clone that advances origin's main
	wt       string // the run's live worktree on pullBranch
	reposDir string // the lab bare reference clones (this repo's and every import target's)
	homes    *instancehome.Manager
	env      []string
	repo     store.Repo
	run      store.Run
}

// newFixture builds the production-shaped topology plus the store rows the
// service reads: repo (identity configured via mod, like httpapi's
// newCRRepo) and an active manual run bound to the live worktree. It also
// wires an instancehome.Manager over a temp instances root, so the read-only
// import refresh (issue #261) has a real per-run imports directory to work in
// — a repo that declares no imports never touches it.
func newFixture(t *testing.T, mod func(*store.Repo)) *fixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	env := testutil.HermeticGitEnv(home)

	work := makeOrigin(t, home, "main", 2) // f0.txt, f1.txt
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitCmd(t, home, "", "init", "-q", "--bare", "-b", "main", origin)
	gitCmd(t, home, work, "remote", "add", "origin", origin)
	gitCmd(t, home, work, "push", "-q", "origin", "main")

	reposDir := t.TempDir()
	repoID := ids.NewID("repo")
	eng := gitx.New("git")
	if err := eng.CloneBare(t.Context(), origin, filepath.Join(reposDir, repoID+".git"), env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := eng.AddWorktree(t.Context(), filepath.Join(reposDir, repoID+".git"), wt, pullBranch, "main", env); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	st := testutil.TempStore(t)
	name, email := pullAuthorName, pullAuthorEmail
	repoRow := store.Repo{
		ID: repoID, Name: "proj", RemoteURL: origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		GitAuthorName: &name, GitAuthorEmail: &email,
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/", Runner: store.RunnerHost,
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	}
	if mod != nil {
		mod(&repoRow)
	}
	repo, err := st.CreateRepo(t.Context(), repoRow)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	run, err := st.CreateRun(t.Context(), store.Run{
		ID: ids.NewID("run"), RepoID: repoID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: pullBranch, WorktreePath: wt, SessionName: "proj~pull-20260712-1200",
		Model: "opus", Effort: "max", StartedAt: time.Now(), Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// The instances root the run's import snapshots live under. A refreshed
	// snapshot is write-protected on the host runner, and t.TempDir's own
	// cleanup is a plain RemoveAll that cannot descend into one — production
	// removes these through instancehome.Wipe, which restores the write bits
	// first — so give the root the same treatment before the temp dir goes.
	instancesDir := t.TempDir()
	homes := instancehome.New(instancesDir)
	t.Cleanup(func() { unprotectTree(instancesDir) })

	bus := events.NewBus()
	svc := New(Options{Store: st, Git: eng, Bus: bus, Homes: homes, ReposDir: reposDir, GitEnv: env})
	return &fixture{
		t: t, svc: svc, st: st, bus: bus, eng: eng, home: home, work: work, wt: wt,
		reposDir: reposDir, homes: homes, env: env, repo: repo, run: run,
	}
}

// unprotectTree restores owner write (and traverse) bits over root so a plain
// RemoveAll can take it — the test-side stand-in for instancehome.Wipe's
// hardening.
func unprotectTree(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		perm := info.Mode().Perm() | 0o200
		if d.IsDir() {
			perm |= 0o700
		}
		_ = os.Chmod(path, perm)
		return nil
	})
}

// addImportTarget registers a SECOND repo — its own origin and its own bare
// reference clone in the fixture's reposDir — and declares it a read-only
// import of the fixture's repo (issue #261), exactly as the repo-settings
// write path would. Returns the target row and its origin working directory,
// which advanceImport commits into to move the import's base.
func (f *fixture) addImportTarget(name string) (store.Repo, string) {
	f.t.Helper()
	origin := makeOrigin(f.t, f.home, "main", 2) // f0.txt, f1.txt
	repoID := ids.NewID("repo")
	if err := f.eng.CloneBare(f.t.Context(), origin, filepath.Join(f.reposDir, repoID+".git"), f.env, nil); err != nil {
		f.t.Fatalf("CloneBare(import target %s): %v", name, err)
	}
	target, err := f.st.CreateRepo(f.t.Context(), store.Repo{
		ID: repoID, Name: name, RemoteURL: origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/", Runner: store.RunnerHost,
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		f.t.Fatalf("CreateRepo(import target %s): %v", name, err)
	}
	if err := f.st.AddRepoImport(f.t.Context(), f.repo.ID, target.ID); err != nil {
		f.t.Fatalf("AddRepoImport: %v", err)
	}
	return target, origin
}

// spawnSnapshot materializes target's snapshot into this run's imports dir the
// way instance.Launch does at spawn — the real MaterializeSnapshot, the 0600
// sidecar beside the directory, and (on anything but the container runner) the
// advisory write protection — and returns the snapshot directory and the
// commit it captured. The protection uses this package's own copy of
// protectSnapshot, which is what makes a host-runner refresh start from the
// write-protected tree production hands it.
func (f *fixture) spawnSnapshot(target store.Repo) (dest, commit string) {
	f.t.Helper()
	dest = filepath.Join(f.homes.ImportsPath(f.run.ID), target.Name)
	commit, err := f.eng.MaterializeSnapshot(f.t.Context(),
		filepath.Join(f.reposDir, target.ID+".git"), dest, target.DefaultBranch, f.env)
	if err != nil {
		f.t.Fatalf("MaterializeSnapshot(%s): %v", target.Name, err)
	}
	if err := os.WriteFile(dest+".commit", []byte(commit+"\n"), 0o600); err != nil {
		f.t.Fatalf("write commit sidecar: %v", err)
	}
	if f.repo.Runner != store.RunnerContainer {
		if err := protectSnapshot(dest); err != nil {
			f.t.Fatalf("protectSnapshot: %v", err)
		}
	}
	return dest, commit
}

// advanceImport commits file=content in an import target's origin — movement
// the target's bare reference clone has NOT fetched, i.e. a sibling repo
// getting a push while this run is live.
func (f *fixture) advanceImport(origin, file, content string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(origin, file), []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", file, err)
	}
	gitCmd(f.t, f.home, origin, "add", "-A")
	gitCmd(f.t, f.home, origin, "commit", "-q", "-m", "import advance "+file)
	return gitCmd(f.t, f.home, origin, "rev-parse", "HEAD")
}

// sidecar reads back the commit recorded beside a snapshot directory.
func (f *fixture) sidecar(dest string) string {
	f.t.Helper()
	b, err := os.ReadFile(dest + ".commit")
	if err != nil {
		f.t.Fatalf("read commit sidecar: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func gitCmd(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(home)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func makeOrigin(t *testing.T, home, branch string, commits int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	gitCmd(t, home, "", "init", "-q", "-b", branch, dir)
	for i := range commits {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), fmt.Appendf(nil, "c%d\n", i), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		gitCmd(t, home, dir, "add", ".")
		gitCmd(t, home, dir, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}
	return dir
}

// advanceOrigin commits file=content in the work clone and pushes it to
// origin's main — base movement the bare reference clone has NOT fetched.
func (f *fixture) advanceOrigin(file, content string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, file), []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", file, err)
	}
	gitCmd(f.t, f.home, f.work, "add", "-A")
	gitCmd(f.t, f.home, f.work, "commit", "-q", "-m", "advance "+file)
	gitCmd(f.t, f.home, f.work, "push", "-q", "origin", "main")
	return gitCmd(f.t, f.home, f.work, "rev-parse", "HEAD")
}

// commitWorktree commits file=content on the run's branch in the live
// worktree and returns the new HEAD sha.
func (f *fixture) commitWorktree(file, content, msg string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.wt, file), []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", file, err)
	}
	gitCmd(f.t, f.home, f.wt, "add", "-A")
	gitCmd(f.t, f.home, f.wt, "commit", "-q", "-m", msg)
	return gitCmd(f.t, f.home, f.wt, "rev-parse", "HEAD")
}

func (f *fixture) worktreeHead() string {
	f.t.Helper()
	return gitCmd(f.t, f.home, f.wt, "rev-parse", "HEAD")
}

// wantRunChanged asserts exactly one run.changed for the fixture repo — and,
// since a pull always concerns exactly one run, carrying that run's identity
// (issue #175) — is on the subscription, then nothing else.
func (f *fixture) wantRunChanged(evts <-chan events.Event) {
	f.t.Helper()
	select {
	case e := <-evts:
		if e.Type != EventRunChanged {
			f.t.Fatalf("event type = %q, want %q", e.Type, EventRunChanged)
		}
		payload, ok := e.Payload.(repoScopedPayload)
		if !ok || payload.RepoID != f.repo.ID || payload.Type != EventRunChanged || payload.RunID != f.run.ID {
			f.t.Fatalf("event payload = %+v, want {%s %s %s}", e.Payload, EventRunChanged, f.repo.ID, f.run.ID)
		}
	case <-time.After(time.Second):
		f.t.Fatal("timed out waiting for run.changed")
	}
	f.wantNoEvent(evts)
}

// wantNoEvent asserts nothing (more) reached the subscription. Publish is
// synchronous, so by the time PullBase returned any event is buffered.
func (f *fixture) wantNoEvent(evts <-chan events.Event) {
	f.t.Helper()
	select {
	case e := <-evts:
		f.t.Fatalf("unexpected event: %+v", e)
	default:
	}
}

// --- service ---------------------------------------------------------------

func TestPullBase_fastForwardDigestAndEvent(t *testing.T) {
	f := newFixture(t, nil)
	f.advanceOrigin("base.txt", "base content\n")
	oldHead := f.worktreeHead()
	evts, cancel := f.bus.Subscribe(t.Context())
	defer cancel()

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if res.Base != "main" {
		t.Errorf("Base = %q, want main", res.Base)
	}
	if res.UpToDate || !res.FastForward {
		t.Errorf("UpToDate=%v FastForward=%v, want false/true", res.UpToDate, res.FastForward)
	}
	if res.OldHead != oldHead || res.NewHead == oldHead {
		t.Errorf("OldHead/NewHead = %s/%s, want %s and a moved head", res.OldHead, res.NewHead, oldHead)
	}
	if h := f.worktreeHead(); h != res.NewHead {
		t.Errorf("worktree HEAD = %s, want %s", h, res.NewHead)
	}
	// The digest carries the range, the wording, an incoming subject, and a
	// name-status file line.
	rng := res.OldHead[:12] + ".." + res.NewHead[:12]
	for _, want := range []string{
		"Lab pulled origin/main into this worktree: " + rng + " (fast-forward).",
		"- advance base.txt",
		"A\tbase.txt",
		"git log " + rng + " --oneline",
	} {
		if !strings.Contains(res.Digest, want) {
			t.Errorf("digest missing %q:\n%s", want, res.Digest)
		}
	}
	if strings.Contains(res.Digest, "… and") {
		t.Errorf("digest carries a truncation line for an untruncated summary:\n%s", res.Digest)
	}
	f.wantRunChanged(evts)
}

func TestPullBase_mergeCommitUsesRepoIdentity(t *testing.T) {
	f := newFixture(t, nil)
	// The global settings pair must lose to the repo override (D15 measure 5).
	if err := f.st.SetSetting(t.Context(), store.SettingGitAuthorName, "Settings Author"); err != nil {
		t.Fatalf("SetSetting name: %v", err)
	}
	if err := f.st.SetSetting(t.Context(), store.SettingGitAuthorEmail, "settings@example.invalid"); err != nil {
		t.Fatalf("SetSetting email: %v", err)
	}
	localSHA := f.commitWorktree("local.txt", "local\n", "local work")
	baseSHA := f.advanceOrigin("base.txt", "base\n") // disjoint files → clean merge

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if res.UpToDate || res.FastForward {
		t.Errorf("UpToDate=%v FastForward=%v for a diverged pull, want false/false", res.UpToDate, res.FastForward)
	}
	if res.NewHead == localSHA || res.NewHead == baseSHA {
		t.Fatalf("NewHead = %s, want a fresh merge commit (local %s, base %s)", res.NewHead, localSHA, baseSHA)
	}
	if !strings.Contains(res.Digest, "(merge commit).") {
		t.Errorf("digest wording for a merge-commit pull:\n%s", res.Digest)
	}
	// The merge commit is authored with the repo's configured real identity —
	// the same resolution crmerge uses, overriding both the settings pair and
	// the ambient GIT_AUTHOR_*/GIT_COMMITTER_* (HermeticGitEnv sets lab-test).
	if id := gitCmd(t, f.home, f.wt, "log", "-1", "--format=%an|%ae|%cn|%ce", res.NewHead); id != pullAuthorID {
		t.Errorf("merge commit identity = %q, want %q", id, pullAuthorID)
	}
}

func TestPullBase_upToDateEmptyDigestNoEvent(t *testing.T) {
	f := newFixture(t, nil)
	head := f.worktreeHead()
	evts, cancel := f.bus.Subscribe(t.Context())
	defer cancel()

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if !res.UpToDate || res.FastForward {
		t.Errorf("UpToDate=%v FastForward=%v, want true/false", res.UpToDate, res.FastForward)
	}
	if res.OldHead != head || res.NewHead != head {
		t.Errorf("OldHead/NewHead = %s/%s, want both %s", res.OldHead, res.NewHead, head)
	}
	if res.Digest != "" {
		t.Errorf("Digest = %q for an up-to-date pull, want empty", res.Digest)
	}
	f.wantNoEvent(evts)
}

func TestPullBase_conflictTypedErrorWorktreeUntouched(t *testing.T) {
	f := newFixture(t, nil)
	localSHA := f.commitWorktree("f0.txt", "local version\n", "local change to f0")
	f.advanceOrigin("f0.txt", "base version\n") // same file → conflict
	evts, cancel := f.bus.Subscribe(t.Context())
	defer cancel()

	_, err := f.svc.PullBase(t.Context(), f.run)
	if err == nil {
		t.Fatal("conflicting PullBase succeeded, want error")
	}
	// gitx's typed conflict passes through verbatim for the HTTP layer.
	if !errors.Is(err, gitx.ErrMergeConflict) {
		t.Fatalf("err = %v, want gitx.ErrMergeConflict", err)
	}
	var ce *gitx.ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *gitx.ConflictError", err)
	}
	if len(ce.Files) != 1 || ce.Files[0] != "f0.txt" {
		t.Errorf("ConflictError.Files = %v, want [f0.txt]", ce.Files)
	}
	// The worktree-untouched contract: HEAD unchanged, status clean, no event.
	if h := f.worktreeHead(); h != localSHA {
		t.Errorf("worktree HEAD = %s after conflict, want unchanged %s", h, localSHA)
	}
	if out := gitCmd(t, f.home, f.wt, "status", "--porcelain"); out != "" {
		t.Errorf("worktree not clean after conflict:\n%s", out)
	}
	f.wantNoEvent(evts)
}

func TestPullBase_noIdentityFastForwardSucceeds(t *testing.T) {
	f := newFixture(t, func(r *store.Repo) { r.GitAuthorName, r.GitAuthorEmail = nil, nil })
	// origin moved and the worktree has NOT diverged → the pull fast-forwards,
	// which authors no commit and so needs no identity anywhere (#151). The old
	// up-front refusal would have blocked this pull outright.
	f.advanceOrigin("base.txt", "base\n")
	oldHead := f.worktreeHead()

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("identity-free fast-forward PullBase: %v", err)
	}
	if res.UpToDate || !res.FastForward {
		t.Errorf("UpToDate=%v FastForward=%v, want false/true", res.UpToDate, res.FastForward)
	}
	if res.OldHead != oldHead || res.NewHead == oldHead {
		t.Errorf("OldHead/NewHead = %s/%s, want %s and a moved head", res.OldHead, res.NewHead, oldHead)
	}
	if res.Digest == "" {
		t.Error("Digest is empty for a fast-forward that moved HEAD")
	}
}

func TestPullBase_noIdentityDivergedRefused(t *testing.T) {
	f := newFixture(t, func(r *store.Repo) { r.GitAuthorName, r.GitAuthorEmail = nil, nil })
	// A diverged worktree forces a merge commit, which has nobody to author it
	// with no identity anywhere → ErrNoAuthorIdentity (mapped from gitx's
	// ErrAuthorIdentityRequired), worktree untouched (#151).
	localSHA := f.commitWorktree("local.txt", "local\n", "local work")
	f.advanceOrigin("base.txt", "base\n") // disjoint files → would merge cleanly, but for the missing identity

	_, err := f.svc.PullBase(t.Context(), f.run)
	if !errors.Is(err, ErrNoAuthorIdentity) {
		t.Fatalf("err = %v, want ErrNoAuthorIdentity", err)
	}
	if h := f.worktreeHead(); h != localSHA {
		t.Errorf("worktree HEAD = %s after refusal, want unchanged %s", h, localSHA)
	}
}

// --- read-only imports (issue #261) ----------------------------------------

// The full refresh (ADR-0063): a /pull-base that merges a moved base ALSO
// re-materializes the run's import snapshots in place, reports both in the one
// digest, and leaves the snapshot on disk holding the sibling's new content
// with its sidecar rewritten to match.
func TestPullBase_baseAndImportBothMoved(t *testing.T) {
	f := newFixture(t, nil)
	target, targetOrigin := f.addImportTarget("libcore")
	dest, spawnCommit := f.spawnSnapshot(target)
	newImportCommit := f.advanceImport(targetOrigin, "api.go", "package api // v2\n")
	f.advanceOrigin("base.txt", "base content\n")
	evts, cancel := f.bus.Subscribe(t.Context())
	defer cancel()

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if res.UpToDate || !res.FastForward {
		t.Errorf("UpToDate=%v FastForward=%v, want false/true", res.UpToDate, res.FastForward)
	}
	want := ImportChange{Name: "libcore", Old: spawnCommit, New: newImportCommit, Commits: 1}
	if len(res.Imports) != 1 || res.Imports[0] != want {
		t.Fatalf("Imports = %+v, want [%+v]", res.Imports, want)
	}
	// Both blocks, in one digest: the base's and the import's.
	for _, w := range []string{
		"Lab pulled origin/main into this worktree: ",
		"Imports refreshed:\n- libcore: " + spawnCommit[:12] + ".." + newImportCommit[:12] + " (1 commits)",
		"Prior reads of moved snapshots may be stale",
	} {
		if !strings.Contains(res.Digest, w) {
			t.Errorf("digest missing %q:\n%s", w, res.Digest)
		}
	}
	// The snapshot on disk really moved — new file present, in place (the
	// directory a container would have bind-mounted is the same one).
	if b, err := os.ReadFile(filepath.Join(dest, "api.go")); err != nil || string(b) != "package api // v2\n" {
		t.Errorf("snapshot api.go = %q, %v; want the sibling's new content", b, err)
	}
	if got := f.sidecar(dest); got != newImportCommit {
		t.Errorf("sidecar = %s, want the refreshed commit %s", got, newImportCommit)
	}
	f.wantRunChanged(evts)
}

// Base current, import current: the silent path is unchanged by the feature —
// UpToDate, no digest, no event. This is the 200-notice case the chat layer
// answers with "Already up to date".
func TestPullBase_upToDateWithUnchangedImport(t *testing.T) {
	f := newFixture(t, nil)
	target, _ := f.addImportTarget("libcore")
	dest, spawnCommit := f.spawnSnapshot(target)
	evts, cancel := f.bus.Subscribe(t.Context())
	defer cancel()

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if !res.UpToDate || res.Digest != "" {
		t.Errorf("UpToDate=%v Digest=%q, want true and empty", res.UpToDate, res.Digest)
	}
	want := ImportChange{Name: "libcore", Old: spawnCommit, New: spawnCommit}
	if len(res.Imports) != 1 || res.Imports[0] != want {
		t.Errorf("Imports = %+v, want [%+v] (refreshed, but on the same commit)", res.Imports, want)
	}
	if got := f.sidecar(dest); got != spawnCommit {
		t.Errorf("sidecar = %s, want the unchanged commit %s", got, spawnCommit)
	}
	f.wantNoEvent(evts)
}

// Base current but a SIBLING moved: no longer the silent path. UpToDate drops
// to false, a digest is injected under its own first line (nothing was
// pulled — HEAD did not move), and run.changed is published.
func TestPullBase_upToDateBaseButImportMoved(t *testing.T) {
	f := newFixture(t, nil)
	target, targetOrigin := f.addImportTarget("libcore")
	dest, spawnCommit := f.spawnSnapshot(target)
	newImportCommit := f.advanceImport(targetOrigin, "api.go", "package api // v2\n")
	head := f.worktreeHead()
	evts, cancel := f.bus.Subscribe(t.Context())
	defer cancel()

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if res.UpToDate {
		t.Error("UpToDate is true although an import moved — the agent would never be told")
	}
	if res.OldHead != head || res.NewHead != head {
		t.Errorf("OldHead/NewHead = %s/%s, want both the unmoved %s", res.OldHead, res.NewHead, head)
	}
	wantDigest := "Lab refreshed this run's read-only imports; origin/main was already up to date.\n" +
		"Imports refreshed:\n" +
		"- libcore: " + spawnCommit[:12] + ".." + newImportCommit[:12] + " (1 commits)\n" +
		"Prior reads of moved snapshots may be stale — re-read anything you rely on from them."
	if res.Digest != wantDigest {
		t.Errorf("digest mismatch:\ngot:\n%s\nwant:\n%s", res.Digest, wantDigest)
	}
	if strings.Contains(res.Digest, "Lab pulled") {
		t.Error("digest claims a pull that did not happen")
	}
	if b, err := os.ReadFile(filepath.Join(dest, "api.go")); err != nil || string(b) != "package api // v2\n" {
		t.Errorf("snapshot api.go = %q, %v; want the sibling's new content", b, err)
	}
	f.wantRunChanged(evts)
}

// An import DECLARED after this run spawned has no snapshot directory and so
// no mount to fill: skipped silently, no digest line, and — with nothing else
// moving — the pull stays the silent up-to-date one. The declaration takes
// effect at the next spawn.
func TestPullBase_declaredImportWithoutSnapshotSkipped(t *testing.T) {
	f := newFixture(t, nil)
	target, targetOrigin := f.addImportTarget("libcore")
	f.advanceImport(targetOrigin, "api.go", "package api // v2\n") // would refresh, if it were mounted
	evts, cancel := f.bus.Subscribe(t.Context())
	defer cancel()

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if !res.UpToDate || res.Digest != "" || len(res.Imports) != 0 {
		t.Errorf("UpToDate=%v Digest=%q Imports=%+v, want true, empty, none", res.UpToDate, res.Digest, res.Imports)
	}
	dest := filepath.Join(f.homes.ImportsPath(f.run.ID), target.Name)
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want it never created: a mid-run declaration has no mount to fill", dest, err)
	}
	f.wantNoEvent(evts)
}

// A dead import target is REPORTED, never fatal: the base pull keeps its own
// semantics and its own digest, the failing import gets one line naming the
// cause, and the snapshot the run was spawned with is still on disk — gitx
// destroys nothing until the new commit is known.
func TestPullBase_importRefreshFailureReportedNotFatal(t *testing.T) {
	f := newFixture(t, nil)
	target, _ := f.addImportTarget("libcore")
	dest, spawnCommit := f.spawnSnapshot(target)
	gitCmd(t, f.home, filepath.Join(f.reposDir, target.ID+".git"),
		"remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	f.advanceOrigin("base.txt", "base content\n")
	evts, cancel := f.bus.Subscribe(t.Context())
	defer cancel()

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v — an import failure must not abort the pull", err)
	}
	if !res.FastForward || res.UpToDate {
		t.Errorf("UpToDate=%v FastForward=%v, want false/true — the base merge is unaffected", res.UpToDate, res.FastForward)
	}
	if len(res.Imports) != 1 || !res.Imports[0].Failed || res.Imports[0].New != "" {
		t.Fatalf("Imports = %+v, want one failed entry with no new commit", res.Imports)
	}
	if !strings.Contains(res.Digest, "- libcore: refresh failed — ") ||
		!strings.Contains(res.Digest, "does not appear to be a git repository") {
		t.Errorf("digest does not report the failing import with its cause:\n%s", res.Digest)
	}
	if strings.Contains(res.Imports[0].Reason, "\n") {
		t.Errorf("failure reason spans lines, want the first only: %q", res.Imports[0].Reason)
	}
	// The old snapshot survives a failed refresh intact, sidecar included.
	if b, err := os.ReadFile(filepath.Join(dest, "f0.txt")); err != nil || string(b) != "c0\n" {
		t.Errorf("snapshot f0.txt = %q, %v; want the spawn snapshot intact", b, err)
	}
	if got := f.sidecar(dest); got != spawnCommit {
		t.Errorf("sidecar = %s after a failed refresh, want the spawn commit %s", got, spawnCommit)
	}
	f.wantRunChanged(evts)
}

// The advisory write protection is re-applied after a host-runner refresh: the
// fresh extraction writes git's own 0644/0755 modes back, so without this the
// same path would hold the same content under weaker modes than the spawn gave
// it. Under the container runner the tree stays writable host-side — the `:ro`
// bind is the enforcement there, and /pull-base has to be able to rewrite it.
func TestPullBase_importWriteProtectionFollowsRunner(t *testing.T) {
	for _, tc := range []struct {
		runner      string
		wantWritten bool // may the refreshed tree still be written host-side?
	}{
		{store.RunnerHost, false},
		{store.RunnerContainer, true},
	} {
		t.Run(tc.runner, func(t *testing.T) {
			f := newFixture(t, func(r *store.Repo) { r.Runner = tc.runner })
			target, targetOrigin := f.addImportTarget("libcore")
			dest, _ := f.spawnSnapshot(target)
			f.advanceImport(targetOrigin, "api.go", "package api // v2\n")

			res, err := f.svc.PullBase(t.Context(), f.run)
			if err != nil {
				t.Fatalf("PullBase: %v", err)
			}
			if len(res.Imports) != 1 || res.Imports[0].Failed {
				t.Fatalf("Imports = %+v, want one successful refresh", res.Imports)
			}
			for _, p := range []string{dest, filepath.Join(dest, "api.go")} {
				fi, err := os.Stat(p)
				if err != nil {
					t.Fatalf("stat %s: %v", p, err)
				}
				if writable := fi.Mode().Perm()&0o222 != 0; writable != tc.wantWritten {
					t.Errorf("%s is mode %04o after a %s-runner refresh, want writable=%v",
						p, fi.Mode().Perm(), tc.runner, tc.wantWritten)
				}
			}
		})
	}
}

// A refresh that cannot tell where it came from — no sidecar (a snapshot from
// before the feature recorded one, or a lost file) — still refreshes, and says
// only what it knows: the commit it landed on.
func TestPullBase_importWithoutSidecarReportsNewCommitOnly(t *testing.T) {
	f := newFixture(t, nil)
	target, targetOrigin := f.addImportTarget("libcore")
	dest, _ := f.spawnSnapshot(target)
	if err := os.Remove(dest + ".commit"); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	newImportCommit := f.advanceImport(targetOrigin, "api.go", "package api // v2\n")

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	want := ImportChange{Name: "libcore", New: newImportCommit}
	if len(res.Imports) != 1 || res.Imports[0] != want {
		t.Fatalf("Imports = %+v, want [%+v]", res.Imports, want)
	}
	if !strings.Contains(res.Digest, "- libcore: refreshed to "+newImportCommit[:12]) {
		t.Errorf("digest does not report the refreshed commit:\n%s", res.Digest)
	}
	// And the sidecar is written back, so the NEXT pull can report a range.
	if got := f.sidecar(dest); got != newImportCommit {
		t.Errorf("sidecar = %s, want it rewritten to %s", got, newImportCommit)
	}
}

// One import's failure must not undo another's refresh: the snapshots are
// rewritten as each succeeds, and the digest reports them independently, in
// declaration (name) order.
func TestPullBase_oneImportFailsOthersStillRefresh(t *testing.T) {
	f := newFixture(t, nil)
	broken, _ := f.addImportTarget("liba")
	good, goodOrigin := f.addImportTarget("libb")
	brokenDest, brokenCommit := f.spawnSnapshot(broken)
	goodDest, goodSpawn := f.spawnSnapshot(good)
	goodNew := f.advanceImport(goodOrigin, "api.go", "package api // v2\n")
	gitCmd(t, f.home, filepath.Join(f.reposDir, broken.ID+".git"),
		"remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	res, err := f.svc.PullBase(t.Context(), f.run)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if len(res.Imports) != 2 || res.Imports[0].Name != "liba" || res.Imports[1].Name != "libb" {
		t.Fatalf("Imports = %+v, want liba then libb (declaration order)", res.Imports)
	}
	if !res.Imports[0].Failed {
		t.Errorf("liba = %+v, want a failure", res.Imports[0])
	}
	if got, want := res.Imports[1], (ImportChange{Name: "libb", Old: goodSpawn, New: goodNew, Commits: 1}); got != want {
		t.Errorf("libb = %+v, want %+v — an earlier failure must not hold back a later refresh", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(goodDest, "api.go")); err != nil || string(b) != "package api // v2\n" {
		t.Errorf("libb snapshot api.go = %q, %v; want the refreshed content", b, err)
	}
	if got := f.sidecar(goodDest); got != goodNew {
		t.Errorf("libb sidecar = %s, want %s", got, goodNew)
	}
	if got := f.sidecar(brokenDest); got != brokenCommit {
		t.Errorf("liba sidecar = %s, want the untouched spawn commit %s", got, brokenCommit)
	}
}

// A conflicting merge returns its typed error with the worktree untouched —
// and the imports are untouched too: there is no digest for import lines to
// ride on, and a refused pull must not rewrite the run's world on its way out.
func TestPullBase_conflictLeavesImportsUntouched(t *testing.T) {
	f := newFixture(t, nil)
	target, targetOrigin := f.addImportTarget("libcore")
	dest, spawnCommit := f.spawnSnapshot(target)
	f.advanceImport(targetOrigin, "api.go", "package api // v2\n")
	f.commitWorktree("f0.txt", "local version\n", "local change to f0")
	f.advanceOrigin("f0.txt", "base version\n") // same file → conflict

	if _, err := f.svc.PullBase(t.Context(), f.run); !errors.Is(err, gitx.ErrMergeConflict) {
		t.Fatalf("err = %v, want gitx.ErrMergeConflict", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "api.go")); !os.IsNotExist(err) {
		t.Errorf("stat api.go = %v, want the snapshot untouched by a refused pull", err)
	}
	if got := f.sidecar(dest); got != spawnCommit {
		t.Errorf("sidecar = %s, want the untouched spawn commit %s", got, spawnCommit)
	}
}

// keyedMutex is the per-run pull serializer (a copy of crmerge's — each
// service owns its own). Pin its two properties: same key excludes,
// different keys do not.
func TestKeyedMutex(t *testing.T) {
	var km keyedMutex
	unlockA := km.lock("a")
	// Different key: never blocks.
	done := make(chan struct{})
	go func() {
		unlock := km.lock("b")
		unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lock(b) blocked behind lock(a)")
	}
	// Same key: blocks until release.
	acquired := make(chan struct{})
	go func() {
		unlock := km.lock("a")
		unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second lock(a) acquired while held")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock(a) never acquired after release")
	}
}
