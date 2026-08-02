package instance

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// addImportTarget registers a SECOND repo — a real origin, a real bare
// reference clone in the fixture's reposDir, and the given clone status — and
// declares it a read-only import of the fixture repo (issue #261), exactly as
// the repo-settings write path would. Returns the target row and its origin
// directory: the tests read the origin's HEAD to assert what the snapshot
// captured, and re-point the bare's remote to drive the refusal path.
func (f *fixture) addImportTarget(t *testing.T, name, cloneStatus string) (store.Repo, string) {
	t.Helper()
	origin := makeOrigin(t, f.home, "main", 2)
	repoID := ids.NewID("repo")
	bare := filepath.Join(f.reposDir, repoID+".git")
	if err := f.svc.git.CloneBare(t.Context(), "file://"+origin, bare, f.env, nil); err != nil {
		t.Fatalf("CloneBare(import target %s): %v", name, err)
	}
	target, err := f.st.CreateRepo(t.Context(), store.Repo{
		ID: repoID, Name: name, RemoteURL: "file://" + origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/", Runner: store.RunnerHost,
		CloneStatus: cloneStatus, CreatedAt: clockTime,
	})
	if err != nil {
		t.Fatalf("CreateRepo(import target %s): %v", name, err)
	}
	if err := f.st.AddRepoImport(t.Context(), f.repo.ID, target.ID); err != nil {
		t.Fatalf("AddRepoImport: %v", err)
	}
	// t.TempDir's own cleanup is a plain RemoveAll, which cannot remove a
	// write-protected snapshot (the host runner's `chmod a-w`) — production
	// removes these through instancehome.Wipe, which restores the bits first.
	// Give the state dir the same treatment so a test that deliberately leaves
	// a run live still tears down.
	t.Cleanup(func() {
		_ = filepath.WalkDir(f.instancesDir, func(path string, d fs.DirEntry, err error) error {
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
	})
	return target, origin
}

// assertNothingClaimed pins the refusal-before-claim contract (ADR-0063,
// extending ADR-0052/0053 to one more precondition): a refused spawn created
// no worktree, no branch, no run row, and no session — an AFK spec refused
// this way parks no issue — and the per-run tree it had already materialized
// is wiped, so nothing of the launch survives anywhere. Mirrors the assertion
// block of TestStart_containerRefusals, one line looser at the end: those
// refusals fire before Materialize, this one after, so the instances ROOT
// exists but must be empty.
func assertNothingClaimed(t *testing.T, f *fixture) {
	t.Helper()
	if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
		t.Error("refused spawn created a worktree")
	}
	if f.branchExists("lab/20260608-1530") {
		t.Error("refused spawn created a branch")
	}
	if _, err := f.st.RunBySession(t.Context(), "proj~20260608-1530"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RunBySession after refusal: %v, want ErrNotFound (no run row)", err)
	}
	if _, live := f.runner.Session("proj~20260608-1530"); live {
		t.Error("refused spawn left a session")
	}
	entries, err := os.ReadDir(f.instancesDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadDir(instances): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("per-run tree survived the refusal: %v", entries)
	}
}

// The host-runner happy path (issue #261 / ADR-0063): every spawn materializes
// each declared read-only import as a .git-less snapshot under the run's
// private imports dir, records its commit in a sidecar for /pull-base, names
// it in the generated context file, and — on the host runner, best-effort —
// strips the tree's write bits.
func TestStart_readOnlyImportSnapshotAndContextFile(t *testing.T) {
	f := newFixture(t)
	target, origin := f.addImportTarget(t, "libcore", store.CloneStatusReady)
	wantCommit := gitCmd(t, f.home, origin, "rev-parse", "HEAD")

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The snapshot lives at <instances>/<runID>/imports/<target name> and
	// carries the origin's content.
	dest := filepath.Join(f.instancesDir, run.ID, "imports", target.Name)
	if want := filepath.Join(f.homes.ImportsPath(run.ID), target.Name); dest != want {
		t.Fatalf("snapshot path %q disagrees with ImportsPath %q", dest, want)
	}
	if !dirExists(dest) {
		t.Fatalf("import snapshot %s not materialized", dest)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "f0.txt")); err != nil || string(b) != "c0\n" {
		t.Errorf("snapshot f0.txt = %q, %v; want the origin's content %q", b, err, "c0\n")
	}
	// A snapshot is a tree, not a repo: no .git, so no history rides along and
	// no agent can branch, commit, or push inside it.
	if _, err := os.Stat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Errorf("snapshot carries a .git (err %v) — it must be a plain tree", err)
	}

	// The sidecar (0600) records the snapshotted commit for /pull-base, and
	// sits OUTSIDE the snapshot as its sibling — never inside a tree that is
	// a byte-faithful export of the imported repo.
	sidecar := dest + ".commit"
	b, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("commit sidecar missing: %v", err)
	}
	if string(b) != wantCommit+"\n" {
		t.Errorf("sidecar = %q, want the origin HEAD %q", b, wantCommit+"\n")
	}
	if fi, err := os.Stat(sidecar); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("sidecar mode = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
	if got := filepath.Dir(sidecar); got != f.homes.ImportsPath(run.ID) {
		t.Errorf("sidecar dir = %q, want the imports dir %q (a sibling of the snapshot, never inside it)", got, f.homes.ImportsPath(run.ID))
	}

	// The generated context file names the import: an import the agent cannot
	// find is not an import.
	wt := filepath.Join(f.worktreeRoot, "proj-20260608-1530")
	local, err := os.ReadFile(filepath.Join(wt, "CLAUDE.local.md"))
	if err != nil {
		t.Fatalf("CLAUDE.local.md missing after Launch: %v", err)
	}
	for _, want := range []string{"## Read-only imports", target.Name, dest, wantCommit[:12]} {
		if !strings.Contains(string(local), want) {
			t.Errorf("CLAUDE.local.md missing %q", want)
		}
	}
	// Seeding a longer context file keeps the worktree clean, as ever.
	if out := gitCmd(t, f.home, wt, "status", "--porcelain"); out != "" {
		t.Errorf("git status after an import-bearing Launch = %q; want empty", out)
	}

	// Host runner: the snapshot is write-protected, files AND directories
	// (advisory — ADR-0063 — but the same path holds the same content under
	// both runners).
	for _, p := range []string{dest, filepath.Join(dest, "f0.txt")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if fi.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s is mode %04o, want the write bits stripped", p, fi.Mode().Perm())
		}
	}
}

// A failing import fetch refuses the spawn with an actionable error NAMING the
// target — gitx only ever knew a directory — and lands BEFORE the claim, so an
// AFK spec refused this way leaves its issue selectable.
func TestStart_readOnlyImportFetchFailureRefusesBeforeClaim(t *testing.T) {
	f := newFixture(t)
	target, _ := f.addImportTarget(t, "libcore", store.CloneStatusReady)
	// Point the TARGET's bare repo at nowhere: its fetch fails, and gitx's
	// fail-before-clearing order means nothing of the snapshot exists either.
	gitCmd(t, f.home, filepath.Join(f.reposDir, target.ID+".git"),
		"remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var startFailed *StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("Start err = %v, want StartFailedError", err)
	}
	if !strings.Contains(err.Error(), `read-only import "libcore"`) {
		t.Errorf("refusal = %q, want it to name the failing target", err)
	}
	if !strings.Contains(err.Error(), "does not appear to be a git repository") {
		t.Errorf("refusal = %q, want git's own explanation carried through", err)
	}
	assertNothingClaimed(t, f)
}

// An import target whose own clone has not finished has no reference repo to
// export from: refused up front, naming the target and its status, with the
// same nothing-claimed guarantee — and without firing a single fetch.
func TestStart_readOnlyImportNotCloneReadyRefusesBeforeClaim(t *testing.T) {
	f := newFixture(t)
	f.addImportTarget(t, "libcore", store.CloneStatusCloning)

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var startFailed *StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("Start err = %v, want StartFailedError", err)
	}
	for _, want := range []string{`read-only import "libcore"`, "not ready", store.CloneStatusCloning} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to carry %q", err, want)
		}
	}
	assertNothingClaimed(t, f)
}

// The POST-claim rollback needs no import-specific step of its own: its last
// action is the per-run tree wipe, and the snapshots live inside that tree,
// write protection and all. Driven through a spawn failure — the deepest
// rollback there is — which must leave the exact pre-launch state.
func TestStart_rollbackWipesImportSnapshots(t *testing.T) {
	f := newFixture(t)
	f.addImportTarget(t, "libcore", store.CloneStatusReady)
	f.runner.FailStart("proj~20260608-1530", errors.New("tmux new-session failed"))

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var startFailed *StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("Start err = %v, want StartFailedError", err)
	}
	assertNothingClaimed(t, f)
}

// Snapshots die with the per-run tree (ADR-0063): Stop wipes it, write
// protection and all — the end-to-end exercise of instancehome.Wipe's
// hardening through the real teardown path.
func TestStop_wipesWriteProtectedImportSnapshots(t *testing.T) {
	f := newFixture(t)
	f.addImportTarget(t, "libcore", store.CloneStatusReady)

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	dest := filepath.Join(f.homes.ImportsPath(run.ID), "libcore")
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("snapshot dir is mode %04o; the teardown case needs a write-protected tree", fi.Mode().Perm())
	}

	if _, err := f.svc.Stop(t.Context(), run.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if dirExists(filepath.Join(f.instancesDir, run.ID)) {
		t.Error("per-run tree survived Stop — a write-protected import snapshot blocked the wipe")
	}
}
