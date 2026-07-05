package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// --- pure helpers -----------------------------------------------------------

// managedBranch recognises a lab/ or afk/ branch and nothing else — not a human's
// branch, not the reference repo's default branch.
func TestManagedBranch(t *testing.T) {
	for _, tc := range []struct {
		branch string
		want   bool
	}{
		{"lab/x-20260608-1530", true},
		{"afk/7", true},
		{"main", false},
		{"feature/x", false},
		{"laboratory/x", false}, // prefix is "lab/", not "lab"
		{"", false},
	} {
		if got := managedBranch(tc.branch); got != tc.want {
			t.Errorf("managedBranch(%q) = %v; want %v", tc.branch, got, tc.want)
		}
	}
}

// ownedBranches maps each live session of a project to the branch it occupies,
// derived exactly as Start does: a manual afk-<N> and an auto afk-auto-<N> both
// collapse to afk/<N>, a manual label to lab/<label>. Other projects (prefix-safe)
// and the login session are excluded. This is the set reconciliation must never
// tear down — the ownership guard, not the merged-check, protects a live run.
func TestOwnedBranches(t *testing.T) {
	live := []string{
		"proj~afk-7",                 // manual AFK → afk/7
		"proj~afk-auto-8",            // auto AFK → afk/8
		"proj~feature-20260608-1530", // manual labelled → lab/feature-20260608-1530
		"proj~20260608-1600",         // manual unlabelled → lab/20260608-1600
		"projfoo~afk-9",              // different project (prefix-safe) → excluded
		"other~afk-3",                // different project → excluded
		loginSession,                 // excluded
	}
	want := map[string]bool{
		"afk/7": true, "afk/8": true,
		"lab/feature-20260608-1530": true, "lab/20260608-1600": true,
	}
	if got := ownedBranches(live, "proj"); !reflect.DeepEqual(got, want) {
		t.Errorf("ownedBranches = %v; want %v", got, want)
	}
	if got := ownedBranches(nil, "proj"); len(got) != 0 {
		t.Errorf("ownedBranches(nil) = %v; want empty (no live session owns anything)", got)
	}
}

// --- startup reconciliation (fake git) --------------------------------------

// removedPaths is the sorted set of worktree paths RemoveWorktree was called with.
func removedPaths(gt *fakeGit) []string {
	var p []string
	for _, c := range gt.removed {
		p = append(p, c.Path)
	}
	sort.Strings(p)
	return p
}

// newReconcileServer is a logged-in single-project ("proj") server wired to a fake
// git and a private tmux with no live sessions, plus the project's reference dir —
// the substrate for the reconcile/sweep decision tests.
func newReconcileServer(t *testing.T) (*Server, *fakeGit, string) {
	t.Helper()
	srv := newAFKServer(t, &fakeTracker{}, &fakeGit{origin: cloonarOrigin})
	gt := srv.git.(*fakeGit)
	dir, err := srv.projectDir("proj")
	if err != nil {
		t.Fatal(err)
	}
	return srv, gt, dir
}

// reconcileProject applies the guarded teardown to orphan worktrees and GCs bare
// merged branches — the startup pass (ADR-0017). The decision table:
//   - clean orphan, unmerged → worktree removed, branch kept;
//   - clean orphan, merged → worktree removed AND branch deleted;
//   - dirty orphan → both kept (unsaved work survives);
//   - bare merged branch (no worktree) → deleted (the afk/<N> GC lab used to skip);
//   - bare unmerged branch → kept;
//   - a live instance's worktree/branch → never touched, even when clean+merged.
func TestReconcileProject(t *testing.T) {
	const lab = "lab/x-20260608-1530"
	const wt = "/wt/proj-x"

	t.Run("clean orphan unmerged: worktree removed, branch kept", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.worktrees = []Worktree{{Path: dir, Branch: "main"}, {Path: wt, Branch: lab}}
		gt.labBranches = []string{lab}

		srv.reconcileProject(dir, "proj")

		if want := []string{wt}; !reflect.DeepEqual(removedPaths(gt), want) {
			t.Errorf("removed = %v; want %v (clean orphan worktree)", removedPaths(gt), want)
		}
		if len(gt.deleted) != 0 {
			t.Errorf("deleted = %v; want the unmerged branch kept", gt.deleted)
		}
	})

	t.Run("clean orphan merged: worktree removed and branch deleted", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.worktrees = []Worktree{{Path: wt, Branch: lab}}
		gt.labBranches = []string{lab}
		gt.merged = map[string]bool{lab: true}

		srv.reconcileProject(dir, "proj")

		if want := []string{wt}; !reflect.DeepEqual(removedPaths(gt), want) {
			t.Errorf("removed = %v; want %v", removedPaths(gt), want)
		}
		if want := []string{lab}; !reflect.DeepEqual(gt.deleted, want) {
			t.Errorf("deleted = %v; want %v", gt.deleted, want)
		}
	})

	t.Run("dirty orphan: worktree and branch kept", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.worktrees = []Worktree{{Path: wt, Branch: lab}}
		gt.labBranches = []string{lab}
		gt.dirty = map[string]bool{wt: true}
		gt.merged = map[string]bool{lab: true} // merged, but dirty wins

		srv.reconcileProject(dir, "proj")

		if len(gt.removed) != 0 || len(gt.deleted) != 0 {
			t.Errorf("dirty orphan: removed=%v deleted=%v; want both kept", gt.removed, gt.deleted)
		}
	})

	t.Run("bare merged afk branch: deleted (the GC lab used to skip)", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.branches = map[int]bool{7: true} // afk/7 claim, no worktree
		gt.merged = map[string]bool{"afk/7": true}

		srv.reconcileProject(dir, "proj")

		if want := []string{"afk/7"}; !reflect.DeepEqual(gt.deleted, want) {
			t.Errorf("deleted = %v; want %v (bare merged afk branch GC'd)", gt.deleted, want)
		}
		if len(gt.removed) != 0 {
			t.Errorf("removed = %v; want none (no worktree)", gt.removed)
		}
	})

	t.Run("bare unmerged branch: kept", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.labBranches = []string{lab} // no worktree, not merged

		srv.reconcileProject(dir, "proj")

		if len(gt.deleted) != 0 {
			t.Errorf("deleted = %v; want the unmerged dangling branch kept", gt.deleted)
		}
	})

	t.Run("live instance untouched even if clean and merged", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		run := startAFKSession(t, srv, "proj", 7) // live → owns afk/7
		gt.worktrees = []Worktree{{Path: srv.worktreePath(afkID("proj", 7)), Branch: "afk/7"}}
		gt.branches = map[int]bool{7: true}
		gt.merged = map[string]bool{"afk/7": true} // clean+merged: only ownership protects it

		srv.reconcileProject(dir, "proj")

		if len(gt.removed) != 0 || len(gt.deleted) != 0 {
			t.Errorf("live %q: removed=%v deleted=%v; want both kept (owned)", run.Name, gt.removed, gt.deleted)
		}
	})
}

// --- runtime sweep (fake git) -----------------------------------------------

// sweepProject is merged-only and never touches dirty/unmerged (ADR-0017). The
// decision table differs from the startup reconcile in exactly one row: a clean
// UNMERGED orphan worktree is LEFT here (startup reclaims it), not removed.
func TestSweepProject(t *testing.T) {
	const lab = "lab/x-20260608-1530"
	const wt = "/wt/proj-x"

	t.Run("merged bare branch: deleted", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.labBranches = []string{lab}
		gt.merged = map[string]bool{lab: true}

		srv.sweepProject(dir, "proj")

		if want := []string{lab}; !reflect.DeepEqual(gt.deleted, want) {
			t.Errorf("deleted = %v; want %v", gt.deleted, want)
		}
	})

	t.Run("merged + clean worktree: both removed", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.worktrees = []Worktree{{Path: wt, Branch: lab}}
		gt.labBranches = []string{lab}
		gt.merged = map[string]bool{lab: true}

		srv.sweepProject(dir, "proj")

		if want := []string{wt}; !reflect.DeepEqual(removedPaths(gt), want) {
			t.Errorf("removed = %v; want %v", removedPaths(gt), want)
		}
		if want := []string{lab}; !reflect.DeepEqual(gt.deleted, want) {
			t.Errorf("deleted = %v; want %v", gt.deleted, want)
		}
	})

	t.Run("merged + dirty worktree: kept (never touch dirty)", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.worktrees = []Worktree{{Path: wt, Branch: lab}}
		gt.labBranches = []string{lab}
		gt.merged = map[string]bool{lab: true}
		gt.dirty = map[string]bool{wt: true}

		srv.sweepProject(dir, "proj")

		if len(gt.removed) != 0 || len(gt.deleted) != 0 {
			t.Errorf("merged+dirty: removed=%v deleted=%v; want both kept", gt.removed, gt.deleted)
		}
	})

	t.Run("unmerged clean worktree: untouched at runtime (merged-only)", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		gt.worktrees = []Worktree{{Path: wt, Branch: lab}}
		gt.labBranches = []string{lab} // not merged

		srv.sweepProject(dir, "proj")

		if len(gt.removed) != 0 || len(gt.deleted) != 0 {
			t.Errorf("unmerged clean at runtime: removed=%v deleted=%v; want untouched (only startup reclaims it)", gt.removed, gt.deleted)
		}
	})

	t.Run("live merged instance: untouched", func(t *testing.T) {
		srv, gt, dir := newReconcileServer(t)
		startAFKSession(t, srv, "proj", 7)
		gt.worktrees = []Worktree{{Path: srv.worktreePath(afkID("proj", 7)), Branch: "afk/7"}}
		gt.branches = map[int]bool{7: true}
		gt.merged = map[string]bool{"afk/7": true}

		srv.sweepProject(dir, "proj")

		if len(gt.removed) != 0 || len(gt.deleted) != 0 {
			t.Errorf("owned merged: removed=%v deleted=%v; want untouched (live)", gt.removed, gt.deleted)
		}
	})
}

// A merged, clean worktree whose Start is still in flight (its tmux session not yet
// live) must NOT be swept. A fresh branch forked from origin/<default> reads as
// "merged" and an un-edited worktree reads clean, so the starting-set guard — not
// the merged/dirty check — is the only thing keeping the sweep from removing a
// worktree out from under an in-flight Start. This is the race the runtime sweep
// would otherwise introduce (the reaper never scanned the filesystem).
func TestSweepProject_skipsStartingInstance(t *testing.T) {
	srv, gt, dir := newReconcileServer(t)
	name := composeSessionName(instanceID{Project: "proj", Label: "x-20260608-1530"})
	const branch = "lab/x-20260608-1530"
	const wt = "/wt/proj-x"
	srv.markStarting(name) // mid-Start: worktree+branch exist, session not live yet
	gt.worktrees = []Worktree{{Path: wt, Branch: branch}}
	gt.labBranches = []string{branch}
	gt.merged = map[string]bool{branch: true} // merged + clean: only the guard protects it

	srv.sweepProject(dir, "proj")

	if len(gt.removed) != 0 || len(gt.deleted) != 0 {
		t.Errorf("swept a mid-Start instance: removed=%v deleted=%v; want untouched (starting-set guard)", gt.removed, gt.deleted)
	}

	// Once the Start completes (clearStarting) with no live session, the same
	// worktree IS a genuine orphan and gets swept — proving the guard, not some
	// unrelated skip, was what protected it above.
	srv.clearStarting(name)
	srv.sweepProject(dir, "proj")
	if want := []string{wt}; !reflect.DeepEqual(removedPaths(gt), want) {
		t.Errorf("after clearStarting: removed = %v; want the now-orphan worktree GC'd", removedPaths(gt))
	}
	if want := []string{branch}; !reflect.DeepEqual(gt.deleted, want) {
		t.Errorf("after clearStarting: deleted = %v; want the merged branch GC'd", gt.deleted)
	}
}

// The runtime entry point fetches each project before sweeping (so the merged-check
// is fresh) and a fetch failure is best-effort — it must not abort the GC.
func TestSweepMergedWorktrees_fetchesEachProjectBestEffort(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, p, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srv := newAFKServerAt(t, root, filepath.Join(t.TempDir(), "s.json"), &fakeTracker{}, &fakeGit{origin: cloonarOrigin})
	gt := srv.git.(*fakeGit)
	gt.fetchErr = errors.New("offline") // best-effort: must not abort the sweep
	gt.labBranches = []string{"lab/old-20260608-1530"}
	gt.merged = map[string]bool{"lab/old-20260608-1530": true}

	srv.sweepMergedWorktrees()

	dirA, _ := srv.projectDir("alpha")
	dirB, _ := srv.projectDir("beta")
	sort.Strings(gt.fetched)
	if want := []string{dirA, dirB}; !reflect.DeepEqual(gt.fetched, want) {
		t.Errorf("fetched = %v; want both projects %v (best-effort fetch each)", gt.fetched, want)
	}
	if len(gt.deleted) == 0 {
		t.Error("a fetch error aborted the sweep; want it best-effort (the merged branch still GC'd)")
	}
}

// --- end-to-end: crash recovery over real git -------------------------------

// Acceptance (#135): after a simulated crash/reboot — sessions gone, worktrees on
// disk — startup reconciliation removes the CLEAN orphan worktree (keeping its
// unmerged branch), keeps the DIRTY orphan whole, and GCs a bare merged branch.
// Driven over real git so RemoveWorktree / DeleteBranch / Worktrees / Branches /
// BranchMerged all interlock the way production wires them.
func TestReconcileWorktrees_realGitCrashRecovery(t *testing.T) {
	requireGit(t)
	requireTmux(t)
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	{
		origin := t.TempDir()
		mustGit(t, origin, "init", "-q", "--bare", "-b", "main")
		seed := t.TempDir()
		mustGit(t, seed, "init", "-q", "-b", "main")
		mustGit(t, seed, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root")
		mustGit(t, seed, "remote", "add", "origin", origin)
		mustGit(t, seed, "push", "-q", "origin", "main")
		mustGit(t, root, "clone", "-q", origin, "proj")
	}
	wtRoot := t.TempDir()

	// A clean, UNMERGED orphan worktree: a commit ahead of origin/main.
	cleanWT := filepath.Join(wtRoot, "proj-clean")
	mustGit(t, repo, "worktree", "add", "-q", "-b", "lab/clean-20260608-1530", cleanWT, "origin/main")
	mustGit(t, cleanWT, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "ahead")

	// A DIRTY orphan worktree (an uncommitted file).
	dirtyWT := filepath.Join(wtRoot, "proj-dirty")
	mustGit(t, repo, "worktree", "add", "-q", "-b", "lab/dirty-20260608-1531", dirtyWT, "origin/main")
	if err := os.WriteFile(filepath.Join(dirtyWT, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A bare MERGED branch with no worktree (the afk/<N> claim lab used to keep).
	mustGit(t, repo, "branch", "afk/7", "origin/main")

	// Server with real git and a private tmux that has NO live sessions (the crash).
	srv := newTestServer(t, root, NewSessions(privateTmux(t), []string{"sh", "-c", "sleep 600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	srv.git = NewGit("git")
	srv.worktreeRoot = wtRoot

	srv.reconcileWorktrees()

	// Clean orphan: worktree removed, its UNMERGED branch kept.
	if _, err := os.Stat(cleanWT); !os.IsNotExist(err) {
		t.Errorf("clean orphan worktree still present (stat err %v); want it removed", err)
	}
	if !branchExists(t, repo, "lab/clean-20260608-1530") {
		t.Error("clean orphan's unmerged branch was deleted; want it kept")
	}
	// Dirty orphan: worktree AND branch kept.
	if _, err := os.Stat(dirtyWT); err != nil {
		t.Errorf("dirty orphan worktree removed (stat err %v); want it kept", err)
	}
	if !branchExists(t, repo, "lab/dirty-20260608-1531") {
		t.Error("dirty orphan's branch was deleted; want it kept")
	}
	// Bare merged branch: GC'd.
	if branchExists(t, repo, "afk/7") {
		t.Error("bare merged afk/7 still present; want it GC'd")
	}
}

// branchExists reports whether refs/heads/<branch> resolves in repo.
func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repo
	return cmd.Run() == nil
}
