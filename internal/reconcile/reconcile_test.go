package reconcile

import (
	"context"
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
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

var recClock = time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)

type recFixture struct {
	t                                     *testing.T
	svc                                   *Service
	st                                    *store.Store
	runner                                *tmuxx.Fake
	guard                                 *startguard.Guard
	mat                                   *vault.Materializer
	git                                   *gitx.Engine
	home, reposDir, worktreeRoot, runtime string
	env                                   []string
	repo                                  store.Repo
	armed                                 *[]store.Run
	afkEnded                              *[]afkEndedCall
}

// afkEndedCall records one AFKRunEnded report (the lab_afk_runs_total seam).
type afkEndedCall struct {
	kind, outcome string
	duration      time.Duration
}

func newRecFixture(t *testing.T) *recFixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := recMakeOrigin(t, home, "main", 2)
	env := testutil.HermeticGitEnv(home)

	stateDir := t.TempDir()
	reposDir := filepath.Join(stateDir, "repos")
	worktreeRoot := filepath.Join(stateDir, "worktrees")
	runtime := filepath.Join(stateDir, "runtime")

	git := gitx.New("git")
	st := testutil.TempStore(t)
	repoID := ids.NewID("repo")
	bare := filepath.Join(reposDir, repoID+".git")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatalf("mkdir repos: %v", err)
	}
	if err := git.CloneBare(t.Context(), "file://"+origin, bare, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	repo, err := st.CreateRepo(t.Context(), store.Repo{
		ID: repoID, Name: "proj", RemoteURL: "file://" + origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		Provider: "claude-code", AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: recClock,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	mat, err := vault.NewMaterializer(runtime)
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	runner := tmuxx.NewFake()
	guard := startguard.New()
	var armed []store.Run
	var afkEnded []afkEndedCall
	svc, err := New(Options{
		Store: st, Git: git, Runner: runner, Guard: guard, Materializer: mat, Bus: events.NewBus(),
		ReposDir: reposDir, GitEnv: env, Now: func() time.Time { return recClock },
		ArmCapture: func(r store.Run) { armed = append(armed, r) },
		AFKRunEnded: func(kind, outcome string, d time.Duration) {
			afkEnded = append(afkEnded, afkEndedCall{kind, outcome, d})
		},
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	return &recFixture{
		t: t, svc: svc, st: st, runner: runner, guard: guard, mat: mat, git: git,
		home: home, reposDir: reposDir, worktreeRoot: worktreeRoot, runtime: runtime,
		env: env, repo: repo, armed: &armed, afkEnded: &afkEnded,
	}
}

func recGitCmd(t *testing.T, home, dir string, args ...string) string {
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

func recMakeOrigin(t *testing.T, home, branch string, commits int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	recGitCmd(t, home, "", "init", "-q", "-b", branch, dir)
	for i := range commits {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), fmt.Appendf(nil, "c%d\n", i), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		recGitCmd(t, home, dir, "add", ".")
		recGitCmd(t, home, dir, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}
	return dir
}

func (f *recFixture) bare() string { return filepath.Join(f.reposDir, f.repo.ID+".git") }

// addWorktree forks branch from origin/main (a clean, merged worktree) at a
// deterministic path and returns it.
func (f *recFixture) addWorktree(branch string) string {
	f.t.Helper()
	wt := filepath.Join(f.worktreeRoot, strings.ReplaceAll(branch, "/", "-"))
	if err := f.git.AddWorktree(f.t.Context(), f.bare(), wt, branch, "main", f.env); err != nil {
		f.t.Fatalf("AddWorktree %s: %v", branch, err)
	}
	return wt
}

// commitAhead adds one commit in the worktree, making its branch unmerged
// (ahead of origin/main) while keeping the tree clean.
func (f *recFixture) commitAhead(wt string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(wt, "ahead.txt"), []byte("ahead\n"), 0o644); err != nil {
		f.t.Fatalf("write: %v", err)
	}
	recGitCmd(f.t, f.home, wt, "add", ".")
	recGitCmd(f.t, f.home, wt, "commit", "-q", "-m", "ahead")
}

func (f *recFixture) dirty(wt string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		f.t.Fatalf("write: %v", err)
	}
}

// bareBranch creates branch at origin/main (merged, no worktree).
func (f *recFixture) bareBranch(branch string) {
	f.t.Helper()
	recGitCmd(f.t, f.home, f.bare(), "branch", branch, "refs/remotes/origin/main")
}

func (f *recFixture) branchExists(branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = f.bare()
	cmd.Env = append(os.Environ(), f.env...)
	return cmd.Run() == nil
}

func recDirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// --- startup reconcile decision table (port-spec §2.4) -------------------

func TestReconcileProject_cleanUnmergedOrphan_removesWorktreeKeepsBranch(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/x-20260608-1530")
	f.commitAhead(wt) // clean but unmerged
	f.svc.reconcileProject(t.Context(), f.repo)
	if recDirExists(wt) {
		t.Error("clean unmerged orphan worktree not removed")
	}
	if !f.branchExists("lab/x-20260608-1530") {
		t.Error("unmerged branch deleted; commits must survive")
	}
}

func TestReconcileProject_cleanMergedOrphan_removesBoth(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/x-20260608-1530") // fresh fork == merged
	f.svc.reconcileProject(t.Context(), f.repo)
	if recDirExists(wt) {
		t.Error("clean merged worktree not removed")
	}
	if f.branchExists("lab/x-20260608-1530") {
		t.Error("merged branch not deleted")
	}
}

func TestReconcileProject_dirtyOrphan_keepsBoth(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/x-20260608-1530") // merged, but…
	f.dirty(wt)                                // …dirty wins
	f.svc.reconcileProject(t.Context(), f.repo)
	if !recDirExists(wt) {
		t.Error("dirty worktree removed; unsaved work must survive")
	}
	if !f.branchExists("lab/x-20260608-1530") {
		t.Error("dirty worktree's branch deleted")
	}
}

func TestReconcileProject_bareMergedBranch_deleted(t *testing.T) {
	f := newRecFixture(t)
	f.bareBranch("afk/7") // merged, no worktree
	f.svc.reconcileProject(t.Context(), f.repo)
	if f.branchExists("afk/7") {
		t.Error("bare merged claim branch not GC'd")
	}
}

func TestReconcileProject_bareUnmergedBranch_kept(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/y-20260608-1530")
	f.commitAhead(wt)
	if err := f.git.RemoveWorktree(t.Context(), f.bare(), wt, f.env); err != nil {
		t.Fatalf("RemoveWorktree: %v", err) // now a bare unmerged branch
	}
	f.svc.reconcileProject(t.Context(), f.repo)
	if !f.branchExists("lab/y-20260608-1530") {
		t.Error("bare unmerged dangling branch deleted; commits must survive")
	}
}

func TestReconcileProject_liveOwned_notTouched(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/z") // clean + merged; only ownership protects it
	f.runner.AddLive("proj~z")
	f.svc.reconcileProject(t.Context(), f.repo)
	if !recDirExists(wt) || !f.branchExists("lab/z") {
		t.Error("live-owned worktree/branch was torn down")
	}
}

// --- runtime sweep: merged-only (port-spec §2.5) -------------------------

func TestSweepProject_unmergedCleanOrphan_left(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/x-20260608-1530")
	f.commitAhead(wt) // clean + unmerged
	f.svc.sweepProject(t.Context(), f.repo)
	if !recDirExists(wt) {
		t.Error("runtime sweep removed a clean UNMERGED orphan (only startup reclaims it)")
	}
	if !f.branchExists("lab/x-20260608-1530") {
		t.Error("runtime sweep deleted an unmerged branch")
	}
}

func TestSweepProject_mergedCleanWorktree_removesBoth(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/x-20260608-1530") // merged + clean
	f.svc.sweepProject(t.Context(), f.repo)
	if recDirExists(wt) || f.branchExists("lab/x-20260608-1530") {
		t.Error("sweep did not remove a merged clean worktree + branch")
	}
}

func TestSweepProject_mergedDirty_kept(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/x-20260608-1530")
	f.dirty(wt)
	f.svc.sweepProject(t.Context(), f.repo)
	if !recDirExists(wt) || !f.branchExists("lab/x-20260608-1530") {
		t.Error("sweep touched a dirty worktree")
	}
}

// TestSweepProject_skipsStartingInstance is the starting-set race guard
// (port-spec §2.6): a merged, clean, mid-Start worktree must not be swept.
func TestSweepProject_skipsStartingInstance(t *testing.T) {
	f := newRecFixture(t)
	name := "proj~x-20260608-1530"
	wt := f.addWorktree("lab/x-20260608-1530") // merged + clean → only the guard protects it
	f.guard.Mark(name)

	f.svc.sweepProject(t.Context(), f.repo)
	if !recDirExists(wt) || !f.branchExists("lab/x-20260608-1530") {
		t.Fatal("the mid-Start (marked) worktree was swept — the starting guard failed")
	}

	// Once cleared with no live session it becomes a genuine orphan.
	f.guard.Clear(name)
	f.svc.sweepProject(t.Context(), f.repo)
	if recDirExists(wt) || f.branchExists("lab/x-20260608-1530") {
		t.Error("after clearStarting the merged clean orphan was not swept")
	}
}

// --- re-adoption (§3b) ---------------------------------------------------

func (f *recFixture) activeRun(session, branch string, deepLink *string) store.Run {
	return f.activeRunKind(session, branch, store.RunKindManual, deepLink)
}

func (f *recFixture) activeRunKind(session, branch, kind string, deepLink *string) store.Run {
	f.t.Helper()
	r, err := f.st.CreateRun(f.t.Context(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: kind, Provider: "claude-code",
		Branch: branch, WorktreePath: "/wt/" + branch, SessionName: session, Model: "opus[1m]", Effort: "max",
		DeepLinkURL: deepLink, StartedAt: recClock, Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	return r
}

func TestReadopt_liveStaysActiveDeadMarkedDeath(t *testing.T) {
	f := newRecFixture(t)
	liveRun := f.activeRun("proj~live-20260608-1530", "lab/live-20260608-1530", nil)
	deadRun := f.activeRun("proj~dead-20260608-1530", "lab/dead-20260608-1530", nil)
	f.runner.AddLive("proj~live-20260608-1530") // only the live one survived the restart

	if _, err := f.svc.readopt(t.Context()); err != nil {
		t.Fatalf("readopt: %v", err)
	}

	// Live run stays active; its NULL deep link re-arms capture.
	if got, err := f.st.RunBySession(t.Context(), liveRun.SessionName); err != nil || got.Outcome != store.RunOutcomeActive {
		t.Errorf("live run not kept active: %+v err %v", got, err)
	}
	if len(*f.armed) != 1 || (*f.armed)[0].ID != liveRun.ID {
		t.Errorf("capture not re-armed for the live run: %v", *f.armed)
	}
	// Dead run → death, pinned reason, and it leaves the active set.
	hist, _ := f.st.RunsByRepo(t.Context(), f.repo.ID, 10)
	var dead *store.Run
	for i := range hist {
		if hist[i].ID == deadRun.ID {
			dead = &hist[i]
		}
	}
	if dead == nil || dead.Outcome != store.RunOutcomeDeath {
		t.Fatalf("dead run outcome = %v, want death", dead)
	}
	if dead.FailureReason == nil || *dead.FailureReason != deathReasonAtStartup {
		t.Errorf("death reason = %v, want %q", dead.FailureReason, deathReasonAtStartup)
	}
	// consecutive_failures is untouched (still 0 on the repo).
	repo, _ := f.st.RepoByID(t.Context(), f.repo.ID)
	if repo.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0 (downtime deaths invisible to the counter)", repo.ConsecutiveFailures)
	}
}

func TestReadopt_liveWithDeepLink_notReArmed(t *testing.T) {
	f := newRecFixture(t)
	url := "https://claude.ai/code/session_real"
	f.activeRun("proj~live-20260608-1530", "lab/live-20260608-1530", &url)
	f.runner.AddLive("proj~live-20260608-1530")
	if _, err := f.svc.readopt(t.Context()); err != nil {
		t.Fatalf("readopt: %v", err)
	}
	if len(*f.armed) != 0 {
		t.Errorf("capture re-armed for a run that already has a deep link: %v", *f.armed)
	}
}

// --- credential keep-set (design §6) -------------------------------------

func TestStartupReconcile_credentialKeepSet(t *testing.T) {
	f := newRecFixture(t)
	// A live run keeps its file; a dead run's and an orphan's are removed;
	// known_hosts is never touched.
	liveRun := f.activeRun("proj~live-20260608-1530", "lab/live-20260608-1530", nil)
	deadRun := f.activeRun("proj~dead-20260608-1530", "lab/dead-20260608-1530", nil)
	f.runner.AddLive("proj~live-20260608-1530")

	write := func(name string) string {
		p := filepath.Join(f.runtime, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	liveFile := write("cred_a." + liveRun.ID + ".key")
	deadFile := write("cred_a." + deadRun.ID + ".key")
	orphanFile := write("cred_a.run_orphan.key")
	knownHosts := write("known_hosts")
	// Orphans from before a restart are old; backdate them past the vault's
	// in-flight age guard (>5min) so the keep-set actually reaps them.
	old := time.Now().Add(-10 * time.Minute)
	for _, p := range []string{deadFile, orphanFile} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.svc.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}

	if _, err := os.Stat(liveFile); err != nil {
		t.Errorf("live run's credential file was removed: %v", err)
	}
	for _, p := range []string{deadFile, orphanFile} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan credential file survived: %s (err %v)", p, err)
		}
	}
	if _, err := os.Stat(knownHosts); err != nil {
		t.Errorf("known_hosts was removed: %v", err)
	}
}

// --- parked view ---------------------------------------------------------

func TestParked_matrix(t *testing.T) {
	f := newRecFixture(t)
	// afk/7: bare merged claim branch (worktree "", ahead 0, unpushed 0).
	f.bareBranch("afk/7")
	// lab/foo: worktree, dirty, one commit ahead + unpushed (never pushed).
	wtFoo := f.addWorktree("lab/foo-20260608-1530")
	f.commitAhead(wtFoo)
	f.dirty(wtFoo)
	// afk/9: owned by a live run → excluded.
	f.addWorktree("afk/9")
	f.runner.AddLive("proj~afk-9")

	entries, err := f.svc.Parked(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("Parked: %v", err)
	}
	byBranch := map[string]ParkedEntry{}
	for _, e := range entries {
		byBranch[e.Branch] = e
	}
	if _, ok := byBranch["afk/9"]; ok {
		t.Error("afk/9 (owned by a live run) appeared in the parked set")
	}
	afk7, ok := byBranch["afk/7"]
	if !ok || afk7.WorktreePath != "" || afk7.Dirty {
		t.Errorf("afk/7 entry = %+v, want bare + clean", afk7)
	}
	foo, ok := byBranch["lab/foo-20260608-1530"]
	if !ok {
		t.Fatal("lab/foo not in parked set")
	}
	if foo.WorktreePath != wtFoo || !foo.Dirty {
		t.Errorf("lab/foo entry = %+v, want worktree %s + dirty", foo, wtFoo)
	}
	if foo.CommitsAhead != 1 || foo.Unpushed != 1 {
		t.Errorf("lab/foo ahead=%d unpushed=%d, want 1/1 (never-pushed branch one commit ahead)", foo.CommitsAhead, foo.Unpushed)
	}
}

// --- unguarded discard ---------------------------------------------------

func TestDiscard_dirtyUnmergedRemovedAnyway(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/x-20260608-1530")
	f.commitAhead(wt) // unmerged
	f.dirty(wt)       // dirty — a guarded teardown would keep both
	if err := f.svc.Discard(t.Context(), f.repo.ID, "lab/x-20260608-1530"); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if recDirExists(wt) {
		t.Error("discard did not force-remove the dirty worktree")
	}
	if f.branchExists("lab/x-20260608-1530") {
		t.Error("discard did not force-delete the unmerged branch")
	}
}

func TestDiscard_bareBranchDeletesOnly(t *testing.T) {
	f := newRecFixture(t)
	f.bareBranch("afk/7")
	if err := f.svc.Discard(t.Context(), f.repo.ID, "afk/7"); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if f.branchExists("afk/7") {
		t.Error("bare branch not deleted")
	}
}

func TestDiscard_killsLiveSessionAndTerminatesRun(t *testing.T) {
	f := newRecFixture(t)
	wt := f.addWorktree("lab/x-20260608-1530")
	name := "proj~x-20260608-1530"
	f.runner.AddLive(name)
	run := f.activeRun(name, "lab/x-20260608-1530", nil)

	if err := f.svc.Discard(t.Context(), f.repo.ID, "lab/x-20260608-1530"); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, live := f.runner.Session(name); live {
		t.Error("discard did not kill the live session on the branch")
	}
	if _, err := f.st.RunBySession(t.Context(), name); err == nil {
		t.Error("discard did not terminate the run")
	}
	if recDirExists(wt) || f.branchExists("lab/x-20260608-1530") {
		t.Error("discard left the worktree/branch behind")
	}
	_ = run
}

// TestDiscard_reportsAFKRunEnded pins the discard kill as a terminal-outcome
// metrics writer (regression: lab_afk_runs_total under-counted 'stopped'
// because only the reaper/Stop chokepoints reported): an AFK run killed by
// Discard reports (kind, stopped, started_at→now) through AFKRunEnded, and a
// manual-kind run killed the same way reports nothing — manual runs are
// outside the metric's label vocabulary.
func TestDiscard_reportsAFKRunEnded(t *testing.T) {
	f := newRecFixture(t)

	f.addWorktree("afk/7")
	afkSession := "proj~afk-auto-7"
	f.runner.AddLive(afkSession)
	f.activeRunKind(afkSession, "afk/7", store.RunKindAFKAuto, nil)
	if err := f.svc.Discard(t.Context(), f.repo.ID, "afk/7"); err != nil {
		t.Fatalf("Discard(afk/7): %v", err)
	}
	if got := *f.afkEnded; len(got) != 1 ||
		got[0].kind != store.RunKindAFKAuto || got[0].outcome != store.RunOutcomeStopped {
		t.Fatalf("afkEnded after AFK discard = %+v, want one {afk_auto stopped}", got)
	}

	// Manual-kind run on a managed branch: killed and terminated, not counted.
	f.addWorktree("lab/x-20260608-1530")
	manSession := "proj~x-20260608-1530"
	f.runner.AddLive(manSession)
	f.activeRun(manSession, "lab/x-20260608-1530", nil)
	if err := f.svc.Discard(t.Context(), f.repo.ID, "lab/x-20260608-1530"); err != nil {
		t.Fatalf("Discard(lab/x): %v", err)
	}
	if got := *f.afkEnded; len(got) != 1 {
		t.Errorf("afkEnded after manual discard = %+v, want still exactly the AFK entry", got)
	}
	if _, err := f.st.RunBySession(t.Context(), manSession); err == nil {
		t.Error("manual run not terminated by discard")
	}
}

func TestDiscard_rejectsNonManagedBranch(t *testing.T) {
	f := newRecFixture(t)
	for _, branch := range []string{"main", "feature/x", ""} {
		err := f.svc.Discard(t.Context(), f.repo.ID, branch)
		var bad *BadRequestError
		if err == nil || !asBadRequest(err, &bad) {
			t.Errorf("Discard(%q) err = %v, want BadRequestError", branch, err)
		}
	}
}

func asBadRequest(err error, target **BadRequestError) bool {
	b, ok := err.(*BadRequestError)
	if ok {
		*target = b
	}
	return ok
}

var _ = context.Background

// Regression (M6 review): an OPEN change request's head branch is owned — the
// sweep and startup pass B must never GC it, even when it reads merged (the
// branch is the CR's reviewable substance, and the crash-recovery retry of an
// unrecorded merge needs it). Merging or closing the CR releases the head.
func TestSweep_sparesOpenCRHeadBranch(t *testing.T) {
	f := newRecFixture(t)
	// A bare branch at origin/main reads merged — normally instant GC fodder.
	f.bareBranch("afk/4")
	cr, err := f.st.CreateCR(t.Context(), f.repo.ID, "t", "Closes #4", "afk/4", "main", []int{4}, recClock)
	if err != nil {
		t.Fatalf("CreateCR: %v", err)
	}

	f.svc.RuntimeSweep(t.Context())
	if !f.branchExists("afk/4") {
		t.Fatal("open CR's head branch was swept")
	}
	if err := f.svc.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if !f.branchExists("afk/4") {
		t.Fatal("open CR's head branch was GC'd by startup pass B")
	}

	// A merged CR releases the head to the normal merged-branch GC.
	if _, err := f.st.MergeCR(t.Context(), f.repo.ID, cr.Number, "deadbeef", recClock); err != nil {
		t.Fatalf("MergeCR: %v", err)
	}
	f.svc.RuntimeSweep(t.Context())
	if f.branchExists("afk/4") {
		t.Error("merged CR's head branch survived the sweep")
	}
}
