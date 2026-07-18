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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
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

// --- service fixture -------------------------------------------------------

const (
	pullAuthorName  = "Repo Author"
	pullAuthorEmail = "repo@example.invalid"
	pullAuthorID    = pullAuthorName + "|" + pullAuthorEmail + "|" + pullAuthorName + "|" + pullAuthorEmail
	pullBranch      = "afk/9"
)

type fixture struct {
	t    *testing.T
	svc  *Service
	st   *store.Store
	bus  *events.Bus
	home string
	work string // working clone that advances origin's main
	wt   string // the run's live worktree on pullBranch
	env  []string
	repo store.Repo
	run  store.Run
}

// newFixture builds the production-shaped topology plus the store rows the
// service reads: repo (identity configured via mod, like httpapi's
// newCRRepo) and an active manual run bound to the live worktree.
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
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
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

	bus := events.NewBus()
	svc := New(Options{Store: st, Git: eng, Bus: bus, ReposDir: reposDir, GitEnv: env})
	return &fixture{t: t, svc: svc, st: st, bus: bus, home: home, work: work, wt: wt, env: env, repo: repo, run: run}
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
