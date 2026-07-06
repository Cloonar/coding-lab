package afk

// Engine tests: fakes for tmux/tracker/provider, a REAL store on a temp
// sqlite file, and REAL git fixtures (hermetic env) — the design §11 bar.
// The v0 behavioral contract rows (afk-engine port spec §3) are exercised
// against the persisted-budget port: claim race, full success cycle,
// timeout, death, three-strikes pause + reset, neutral Stop (§4c), and the
// scheduler predicate wiring.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

var clockTime = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

// --- fakes -----------------------------------------------------------------

// fakeTracker is a scriptable tracker.Tracker: the ready queue and the pull
// listing the engine reads, plus injectable errors and a Pulls call counter.
type fakeTracker struct {
	mu         sync.Mutex
	ready      []tracker.Issue
	pulls      []tracker.PullRef
	readyErr   error
	pullsErr   error
	pullsCalls int
}

func (f *fakeTracker) ReadyIssues(context.Context) ([]tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readyErr != nil {
		return nil, f.readyErr
	}
	return append([]tracker.Issue(nil), f.ready...), nil
}

func (f *fakeTracker) Issues(context.Context, string) ([]tracker.Issue, error) { return nil, nil }
func (f *fakeTracker) Issue(context.Context, int) (tracker.Issue, error) {
	return tracker.Issue{}, tracker.ErrNotFound
}
func (f *fakeTracker) CreateComment(context.Context, int, string) error { return nil }
func (f *fakeTracker) Pulls(context.Context) ([]tracker.PullRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullsCalls++
	if f.pullsErr != nil {
		return nil, f.pullsErr
	}
	return append([]tracker.PullRef(nil), f.pulls...), nil
}
func (f *fakeTracker) Pull(context.Context, int) (tracker.PullDetail, error) {
	return tracker.PullDetail{}, tracker.ErrNotFound
}
func (f *fakeTracker) CreatePull(context.Context, string, string, string, string) (tracker.PullRef, error) {
	return tracker.PullRef{}, errors.New("not implemented")
}
func (f *fakeTracker) CloseIssue(context.Context, int) error { return nil }
func (f *fakeTracker) CreateIssue(context.Context, string, string, []string) (tracker.Issue, error) {
	return tracker.Issue{}, errors.New("not implemented")
}
func (f *fakeTracker) AddIssueLabels(context.Context, int, []string) error    { return nil }
func (f *fakeTracker) RemoveIssueLabels(context.Context, int, []string) error { return nil }
func (f *fakeTracker) Labels(context.Context) ([]tracker.Label, error)        { return nil, nil }
func (f *fakeTracker) EnsureLabel(context.Context, string, string, string) (tracker.Label, error) {
	return tracker.Label{}, errors.New("not implemented")
}

func (f *fakeTracker) setReady(ns ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready = f.ready[:0]
	for _, n := range ns {
		f.ready = append(f.ready, tracker.Issue{Number: n, Title: fmt.Sprintf("issue %d", n)})
	}
}

func (f *fakeTracker) addPull(head, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, tracker.PullRef{Number: len(f.pulls) + 1, HeadBranch: head, State: state})
}

func (f *fakeTracker) failPulls(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullsErr = err
}

func (f *fakeTracker) pullsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pullsCalls
}

// fakeResolver maps repo ids onto scripted trackers (the TrackerResolver
// seam).
type fakeResolver struct {
	mu  sync.Mutex
	byM map[string]tracker.Tracker
}

func newFakeResolver() *fakeResolver { return &fakeResolver{byM: map[string]tracker.Tracker{}} }

func (r *fakeResolver) set(repoID string, trk tracker.Tracker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byM[repoID] = trk
}

func (r *fakeResolver) TrackerFor(_ context.Context, repo store.Repo) (tracker.Tracker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	trk, ok := r.byM[repo.ID]
	if !ok {
		return nil, fmt.Errorf("no tracker bound for repo %q", repo.ID)
	}
	return trk, nil
}

// --- fixture ----------------------------------------------------------------

type fixture struct {
	t        *testing.T
	svc      *Service
	inst     *instance.Service
	st       *store.Store
	runner   *tmuxx.Fake
	prov     *providertest.Fake
	bus      *events.Bus
	clock    *testutil.FakeClock
	trackers *fakeResolver
	git      *gitx.Engine
	guard    *startguard.Guard

	home         string
	env          []string
	reposDir     string
	worktreeRoot string

	repo store.Repo
	trk  *fakeTracker
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

func makeOrigin(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	gitCmd(t, home, "", "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	gitCmd(t, home, dir, "add", ".")
	gitCmd(t, home, dir, "commit", "-q", "-m", "c0")
	return dir
}

func newFixture(t *testing.T) *fixture { return newFixtureWithPattern(t, "afk/<N>") }

func newFixtureWithPattern(t *testing.T, pattern string) *fixture {
	return newFixtureWrapped(t, pattern, nil)
}

// newFixtureWrapped builds the fixture with an optional runner decorator: wrap
// receives the underlying *tmuxx.Fake and returns the SessionRunner the
// services actually use, so a test can interpose behaviour (e.g. a session
// appearing mid-tick) while keeping the raw Fake for its own controls. wrap
// nil is the plain Fake.
func newFixtureWrapped(t *testing.T, pattern string, wrap func(*tmuxx.Fake) tmuxx.SessionRunner) *fixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	env := testutil.HermeticGitEnv(home)

	stateDir := t.TempDir()
	reposDir := filepath.Join(stateDir, "repos")
	worktreeRoot := filepath.Join(stateDir, "worktrees")

	git := gitx.New("git")
	st := testutil.TempStore(t)
	if err := st.SeedDefaultSettings(t.Context(), 6); err != nil {
		t.Fatalf("SeedDefaultSettings: %v", err)
	}

	vlt, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	mat, err := vault.NewMaterializer(filepath.Join(stateDir, "runtime"))
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	prov := providertest.New()
	reg, err := provider.NewRegistry(prov)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	runner := tmuxx.NewFake()
	var svcRunner tmuxx.SessionRunner = runner
	if wrap != nil {
		svcRunner = wrap(runner)
	}
	bus := events.NewBus()
	clock := testutil.NewFakeClock(clockTime)

	guard := startguard.New()
	inst, err := instance.New(instance.Options{
		Store: st, Git: git, Runner: svcRunner, Providers: reg, Vault: vlt, Materializer: mat,
		Guard: guard, Bus: bus, ReposDir: reposDir, WorktreeRoot: worktreeRoot,
		LabURL: "http://127.0.0.1:8080", GitEnv: env, CaptureCtx: context.Background(),
		Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("instance.New: %v", err)
	}

	trackers := newFakeResolver()
	svc, err := New(Options{
		Store: st, Git: git, Runner: svcRunner, Providers: reg, Trackers: trackers,
		Instances: inst, Materializer: mat, Bus: bus, Guard: guard,
		ReposDir: reposDir, WorktreeRoot: worktreeRoot, GitEnv: env, Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("afk.New: %v", err)
	}
	inst.SetAFKStopper(svc)

	f := &fixture{
		t: t, svc: svc, inst: inst, st: st, runner: runner, prov: prov, bus: bus,
		clock: clock, trackers: trackers, git: git, guard: guard,
		home: home, env: env, reposDir: reposDir, worktreeRoot: worktreeRoot,
	}
	f.repo, f.trk = f.addRepo("proj", pattern)
	return f
}

// addRepo creates another repo fixture: its own origin, bare reference
// clone, repos row, and scripted tracker.
func (f *fixture) addRepo(name, pattern string) (store.Repo, *fakeTracker) {
	f.t.Helper()
	origin := makeOrigin(f.t, f.home)
	repoID := ids.NewID("repo")
	bare := filepath.Join(f.reposDir, repoID+".git")
	if err := os.MkdirAll(f.reposDir, 0o755); err != nil {
		f.t.Fatalf("mkdir repos: %v", err)
	}
	if err := f.git.CloneBare(f.t.Context(), "file://"+origin, bare, f.env, nil); err != nil {
		f.t.Fatalf("CloneBare: %v", err)
	}
	repo, err := f.st.CreateRepo(f.t.Context(), store.Repo{
		ID: repoID, Name: name, RemoteURL: "file://" + origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		Provider: "claude-code", AFKBranchPattern: pattern, ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: clockTime,
	})
	if err != nil {
		f.t.Fatalf("CreateRepo: %v", err)
	}
	trk := &fakeTracker{}
	f.trackers.set(repo.ID, trk)
	return repo, trk
}

func (f *fixture) bare(repo store.Repo) string { return filepath.Join(f.reposDir, repo.ID+".git") }

func (f *fixture) branchExists(repo store.Repo, branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = f.bare(repo)
	cmd.Env = append(os.Environ(), f.env...)
	return cmd.Run() == nil
}

// createClaimBranch parks an issue by hand: the branch existing IS the claim.
func (f *fixture) createClaimBranch(repo store.Repo, branch string) {
	gitCmd(f.t, f.home, f.bare(repo), "branch", branch, "main")
}

// commitInWorktree gives the run's branch a commit of its own, so the branch
// is clean-but-unmerged (the parked shape) instead of a trivially-merged
// fresh fork.
func (f *fixture) commitInWorktree(wt string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(wt, "work.txt"), []byte("done\n"), 0o644); err != nil {
		f.t.Fatalf("write work file: %v", err)
	}
	gitCmd(f.t, f.home, wt, "add", ".")
	gitCmd(f.t, f.home, wt, "commit", "-q", "-m", "feat: work")
}

func (f *fixture) dirtyWorktree(wt string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		f.t.Fatalf("dirty worktree: %v", err)
	}
}

func (f *fixture) setFailures(repo store.Repo, n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		if _, err := f.st.IncrementRepoFailures(f.t.Context(), repo.ID); err != nil {
			f.t.Fatalf("IncrementRepoFailures: %v", err)
		}
	}
}

func (f *fixture) failures(repo store.Repo) int {
	f.t.Helper()
	got, err := f.st.RepoByID(f.t.Context(), repo.ID)
	if err != nil {
		f.t.Fatalf("RepoByID: %v", err)
	}
	return got.ConsecutiveFailures
}

func (f *fixture) runRow(id string) store.Run {
	f.t.Helper()
	runs, err := f.st.RunsByRepo(f.t.Context(), f.repo.ID, 0)
	if err != nil {
		f.t.Fatalf("RunsByRepo: %v", err)
	}
	for _, r := range runs {
		if r.ID == id {
			return r
		}
	}
	f.t.Fatalf("run %s not found", id)
	return store.Run{}
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// --- manual start + claim --------------------------------------------------

func TestStartManualAFK_claimsLowestAndLaunches(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(12, 7)

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if run.Kind != store.RunKindAFKManual || run.IssueNumber == nil || *run.IssueNumber != 7 {
		t.Fatalf("run = %+v, want kind afk_manual for issue 7 (lowest computed, not list order)", run)
	}
	if run.SessionName != "proj~afk-7" || run.Branch != "afk/7" {
		t.Errorf("identity = %s / %s, want proj~afk-7 / afk/7", run.SessionName, run.Branch)
	}
	wt := filepath.Join(f.worktreeRoot, "proj-7")
	if run.WorktreePath != wt {
		t.Errorf("worktree path = %q, want %q (<repo>-<N>, never '~')", run.WorktreePath, wt)
	}
	if !dirExists(wt) || !f.branchExists(f.repo, "afk/7") {
		t.Error("claim not created (worktree or branch missing)")
	}

	// D12b: the budget clock is PERSISTED on the run row (settings default
	// 120 minutes), re-readable — never in-memory-only.
	wantDeadline := clockTime.Add(120 * time.Minute)
	if run.BudgetDeadline == nil || !run.BudgetDeadline.Equal(wantDeadline) {
		t.Fatalf("budget deadline = %v, want %v", run.BudgetDeadline, wantDeadline)
	}
	persisted := f.runRow(run.ID)
	if persisted.BudgetDeadline == nil || !persisted.BudgetDeadline.Equal(wantDeadline) {
		t.Errorf("persisted budget deadline = %v, want %v", persisted.BudgetDeadline, wantDeadline)
	}

	// Session spawned in the worktree with the seed prompt carried as the
	// trailing spawn-argv positional (pinned v0 mechanism: present before the
	// process, never injected post-spawn where it could race the cold-start
	// TUI).
	sess, live := f.runner.Session(run.SessionName)
	if !live || sess.Dir != wt {
		t.Fatalf("session live=%v dir=%q, want live in %q", live, sess.Dir, wt)
	}
	if last := sess.Argv[len(sess.Argv)-1]; last != SeedPrompt(7, "afk/7", false) {
		t.Errorf("last spawn argv = %q, want the exact SeedPrompt as one trailing positional", last)
	}
	if sent := f.runner.Sent(run.SessionName); len(sent) != 0 {
		t.Errorf("seed delivered via SendKeys (%d batches), want the argv-only mechanism", len(sent))
	}

	// §3a: the run token expires at budget_deadline + 30min.
	token := envValue(sess.ExtraEnv, "LAB_TOKEN")
	if !strings.HasPrefix(token, "lab_run_") {
		t.Fatalf("LAB_TOKEN = %q, want a lab_run_ token", token)
	}
	info, err := f.st.RunTokenByHash(t.Context(), ids.HashToken(token))
	if err != nil {
		t.Fatalf("RunTokenByHash: %v", err)
	}
	wantExpiry := wantDeadline.Add(30 * time.Minute)
	if info.ExpiresAt == nil || !info.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("token expiry = %v, want %v", info.ExpiresAt, wantExpiry)
	}
}

func TestStartManualAFK_branchPatternFlowsEverywhere(t *testing.T) {
	f := newFixtureWithPattern(t, "issue-<N>")
	f.trk.setReady(7)

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if run.Branch != "issue-7" {
		t.Errorf("branch = %q, want issue-7 (rendered from the repo pattern, never literal afk/)", run.Branch)
	}
	if run.SessionName != "proj~afk-7" {
		t.Errorf("session = %q, want proj~afk-7 (label grammar is fixed regardless of pattern)", run.SessionName)
	}
	if !f.branchExists(f.repo, "issue-7") || f.branchExists(f.repo, "afk/7") {
		t.Error("claim branch not rendered from the repo pattern")
	}
	sess, _ := f.runner.Session(run.SessionName)
	seed := sess.Argv[len(sess.Argv)-1]
	if !strings.Contains(seed, "branch `issue-7`") {
		t.Errorf("seed prompt (spawn argv trailing positional) does not carry the rendered branch: %q", seed)
	}

	// The claim oracle drains around the pattern branch too.
	f.trk.setReady(7, 8)
	second, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("second StartManualAFK: %v", err)
	}
	if second.IssueNumber == nil || *second.IssueNumber != 8 {
		t.Errorf("second claim = %+v, want issue 8 (7 already claimed by issue-7)", second.IssueNumber)
	}
}

func TestStartManualAFK_noReadyAndParkedOnly(t *testing.T) {
	f := newFixture(t)

	// Empty queue → the pinned 409 sentinel, nothing claimed.
	_, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if !errors.Is(err, ErrNoReady) {
		t.Fatalf("err = %v, want ErrNoReady", err)
	}
	if err.Error() != "no ready-for-agent issues to start" {
		t.Errorf("ErrNoReady message = %q (pinned API body)", err.Error())
	}

	// Parked-only: the sole ready issue already has its claim branch —
	// no re-claim, EVER (the no-flapping guarantee).
	f.trk.setReady(7)
	f.createClaimBranch(f.repo, "afk/7")
	_, err = f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if !errors.Is(err, ErrNoReady) {
		t.Fatalf("parked-only err = %v, want ErrNoReady", err)
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("parked-only start left %d active runs", len(active))
	}
}

func TestStartManualAFK_drainsAroundClaims(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7, 8)
	f.createClaimBranch(f.repo, "afk/7")

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if run.IssueNumber == nil || *run.IssueNumber != 8 {
		t.Errorf("claimed issue = %v, want 8 (claimed lowest skipped)", run.IssueNumber)
	}
}

func TestStartManualAFK_pausedIs409(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	f.setFailures(f.repo, PauseThreshold)

	_, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if !errors.Is(err, ErrRepoPaused) {
		t.Fatalf("err = %v, want ErrRepoPaused (D12: manual start on a paused repo is refused)", err)
	}
	if f.branchExists(f.repo, "afk/7") {
		t.Error("paused start still claimed the issue")
	}

	// A human reset re-arms the manual start.
	if _, err := f.st.ResetRepoFailures(t.Context(), f.repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); err != nil {
		t.Fatalf("StartManualAFK after reset: %v", err)
	}
}

func TestStartManualAFK_overCapAndLoggedOut(t *testing.T) {
	t.Run("over cap", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
			t.Fatal(err)
		}
		f.runner.AddLive("other~existing")
		_, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if !errors.Is(err, instance.ErrOverCap) {
			t.Fatalf("err = %v, want ErrOverCap", err)
		}
		if f.branchExists(f.repo, "afk/7") {
			t.Error("at-cap start claimed anyway (cap must veto before the claim)")
		}
	})
	t.Run("logged out", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		f.prov.SetLoggedIn(false)
		_, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if !errors.Is(err, instance.ErrLoggedOut) {
			t.Fatalf("err = %v, want ErrLoggedOut", err)
		}
		if f.branchExists(f.repo, "afk/7") {
			t.Error("logged-out start claimed an issue into a doomed session")
		}
	})
}

func TestStartManualAFK_budgetRepoOverride(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	mins := 45
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		BudgetMinutes: store.Set(&mins),
	}); err != nil {
		t.Fatal(err)
	}

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	want := clockTime.Add(45 * time.Minute)
	if run.BudgetDeadline == nil || !run.BudgetDeadline.Equal(want) {
		t.Errorf("budget deadline = %v, want %v (repos.budget_minutes override)", run.BudgetDeadline, want)
	}
}

// The seed prompt now rides the spawn argv, so a failed spawn (not a separate
// post-spawn keystroke) is what a seeding failure collapses into. A spawn
// failure must still roll the whole claim back — worktree, branch, run row.
func TestStartManualAFK_spawnFailureReleasesClaim(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	f.runner.FailStart("proj~afk-7", errors.New("new-session: server exited"))

	_, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	var startFailed *instance.StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("err = %v, want StartFailedError", err)
	}
	if f.branchExists(f.repo, "afk/7") || dirExists(filepath.Join(f.worktreeRoot, "proj-7")) {
		t.Error("failed spawn stranded the claim (worktree/branch survived rollback)")
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("failed launch left %d active runs", len(active))
	}
	if _, live := f.runner.Session("proj~afk-7"); live {
		t.Error("failed launch left the session running")
	}
}

func TestClaimRace_concurrentStartsPickDifferentIssues(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7, 8)

	var wg sync.WaitGroup
	results := make([]store.Run, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.svc.StartManualAFK(context.Background(), f.repo.ID)
		}()
	}
	wg.Wait()

	claimed := map[int]bool{}
	for i := range 2 {
		if errs[i] != nil {
			// A racer may legitimately lose (e.g. no claimable left) — but
			// with two ready issues both must land.
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if results[i].IssueNumber == nil {
			t.Fatalf("racer %d: run has no issue", i)
		}
		claimed[*results[i].IssueNumber] = true
	}
	if !claimed[7] || !claimed[8] {
		t.Errorf("claimed issues = %v, want exactly {7, 8} (never the same issue twice)", claimed)
	}
	if !f.branchExists(f.repo, "afk/7") || !f.branchExists(f.repo, "afk/8") {
		t.Error("claim branches missing after the race")
	}
}

// --- reaper ------------------------------------------------------------------

func TestReap_successCycle(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	f.setFailures(f.repo, 2) // a success reap must reset the streak

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	f.commitInWorktree(run.WorktreePath) // clean-but-unmerged: the branch has real work

	// The agent opened its PR (open state) — the done-signal.
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.clock.Advance(10 * time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	got := f.runRow(run.ID)
	if got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success", got.Outcome)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(f.clock.Now()) {
		t.Errorf("ended_at = %v, want the reap time", got.EndedAt)
	}
	if _, live := f.runner.Session(run.SessionName); live {
		t.Error("session still live after a success reap")
	}
	// Guarded teardown: clean worktree removed; unmerged branch KEPT while
	// the PR is open (the runtime sweep GCs it after merge).
	if dirExists(run.WorktreePath) {
		t.Error("clean worktree not removed on success")
	}
	if !f.branchExists(f.repo, "afk/7") {
		t.Error("unmerged branch deleted on success (must be kept until merged)")
	}
	// Counter reset; tokens gone.
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("failures = %d, want 0 (success resets the streak)", n)
	}
	if runs, _ := f.st.ActiveRuns(t.Context()); len(runs) != 0 {
		t.Errorf("still %d active runs after reap", len(runs))
	}

	// A second sweep is a total no-op: the run is terminal, so no pulls are
	// even listed.
	before := f.trk.pullsCallCount()
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if f.trk.pullsCallCount() != before {
		t.Error("second sweep re-listed pulls for a reaped run")
	}
}

func TestReap_tokenDeadAfterReap(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := f.runner.Session(run.SessionName)
	token := envValue(sess.ExtraEnv, "LAB_TOKEN")

	f.trk.addPull("afk/7", tracker.PullMerged)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	if _, err := f.st.RunTokenByHash(t.Context(), ids.HashToken(token)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("run token survives the terminal outcome: %v", err)
	}
}

func TestReap_timeout(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.commitInWorktree(run.WorktreePath)

	// One minute under budget: in progress, untouched.
	f.svc.ReapOnce(t.Context(), clockTime.Add(119*time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Fatalf("under-budget reap classified the run: %q", got.Outcome)
	}

	// At the boundary (>= is inclusive): timeout.
	f.svc.ReapOnce(t.Context(), clockTime.Add(120*time.Minute))
	got := f.runRow(run.ID)
	if got.Outcome != store.RunOutcomeTimeout {
		t.Fatalf("outcome = %q, want timeout at the inclusive boundary", got.Outcome)
	}
	if got.FailureReason == nil || *got.FailureReason == "" {
		t.Error("timeout recorded no failure_reason")
	}
	if _, live := f.runner.Session(run.SessionName); live {
		t.Error("timed-out session not stopped")
	}
	if dirExists(run.WorktreePath) {
		t.Error("clean worktree not reclaimed on timeout")
	}
	if !f.branchExists(f.repo, "afk/7") {
		t.Error("unmerged claim branch deleted on timeout (issue must stay parked)")
	}
	if n := f.failures(f.repo); n != 1 {
		t.Errorf("failures = %d, want 1", n)
	}
}

func TestReap_death(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Kill(run.SessionName) // crashed outside lab's control

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	got := f.runRow(run.ID)
	if got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("outcome = %q, want death", got.Outcome)
	}
	if n := f.failures(f.repo); n != 1 {
		t.Errorf("failures = %d, want 1", n)
	}
}

func TestReap_deathBeatsTimeoutAndPRBeatsDeath(t *testing.T) {
	t.Run("dead over budget is death", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.runner.Kill(run.SessionName)
		f.svc.ReapOnce(t.Context(), clockTime.Add(240*time.Minute))
		if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
			t.Errorf("outcome = %q, want death (dead beats timeout)", got.Outcome)
		}
	})
	t.Run("dead with merged PR is success", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.runner.Kill(run.SessionName)
		f.trk.addPull("afk/7", tracker.PullMerged)
		f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
		if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
			t.Errorf("outcome = %q, want success (PR beats death)", got.Outcome)
		}
		if n := f.failures(f.repo); n != 0 {
			t.Errorf("failures = %d, want 0", n)
		}
	})
	t.Run("closed-unmerged PR is not a done-signal", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.runner.Kill(run.SessionName)
		f.trk.addPull("afk/7", tracker.PullClosed)
		f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
		if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
			t.Errorf("outcome = %q, want death (closed-unmerged PR must not reap as success)", got.Outcome)
		}
	})
}

func TestReap_dirtyWorktreeKeptOnFailure(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.dirtyWorktree(run.WorktreePath)
	f.runner.Kill(run.SessionName)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("outcome = %q, want death", got.Outcome)
	}
	if !dirExists(run.WorktreePath) || !f.branchExists(f.repo, "afk/7") {
		t.Error("dirty worktree/branch destroyed on failure (unsaved work must survive)")
	}
}

func TestReap_trackerErrorSkipsRunThisTick(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Kill(run.SessionName)
	f.trk.failPulls(errors.New("forge is down"))

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Fatalf("outcome = %q — classified on missing tracker data", got.Outcome)
	}
	// Next tick, tracker healthy again → real death.
	f.trk.failPulls(nil)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(2*time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Errorf("outcome = %q, want death once the tracker answers", got.Outcome)
	}
}

func TestReap_autoRunReapedLikeManual(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		AFKAutoEnabled: store.Set(true),
	}); err != nil {
		t.Fatal(err)
	}
	f.svc.ScheduleOnce(t.Context())
	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 1 || active[0].Kind != store.RunKindAFKAuto {
		t.Fatalf("scheduler launch = %+v, want one afk_auto run", active)
	}
	run := active[0]
	if run.SessionName != "proj~afk-auto-7" {
		t.Errorf("session = %q, want proj~afk-auto-7", run.SessionName)
	}

	f.trk.addPull("afk/7", tracker.PullOpen)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Errorf("auto run outcome = %q, want success (auto label must stay reapable)", got.Outcome)
	}
	if dirExists(run.WorktreePath) {
		t.Error("auto run worktree not removed on success")
	}
}

// Regression (§4b): the reaper must not classify a mid-Launch run as a death.
// instance.Launch writes the active runs row BEFORE its tmux session is live
// and marks the startguard across the whole window; a reaper tick landing
// inside it must leave the run — and its fresh-fork claim — completely alone.
// Without the guard check the not-yet-live session reads dead, the run is
// reaped as a death, and the fresh fork (clean, zero commits → clean+merged)
// is torn down: worktree AND claim branch gone, plus a spurious three-strikes
// strike. Once the guard clears and the session is genuinely gone, the same
// run is a real death and reaps normally.
func TestReap_midLaunchGuardedRunNotReaped(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	// Reproduce the claim→live window: row active, session not yet live,
	// startguard still marking it (a fresh fork with no commit — the §4b
	// clean+merged teardown hazard).
	f.runner.Kill(run.SessionName)
	f.guard.Mark(run.SessionName)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))

	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Fatalf("guarded mid-Launch run classified %q, want still active", got.Outcome)
	}
	if !dirExists(run.WorktreePath) {
		t.Error("fresh-fork worktree torn down for a guarded mid-Launch run")
	}
	if !f.branchExists(f.repo, "afk/7") {
		t.Error("claim branch deleted for a guarded mid-Launch run")
	}
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("failures = %d, want 0 (a guarded mid-Launch run is not a death)", n)
	}

	// Guard cleared, session still gone → a genuine death, reaped as normal.
	f.guard.Clear(run.SessionName)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(2*time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("unmarked dead run classified %q, want death", got.Outcome)
	}
	if n := f.failures(f.repo); n != 1 {
		t.Errorf("failures = %d, want 1 after the real death", n)
	}
}

// Regression: a tmux kill that fails after a terminal-outcome write leaves a
// LIVE session whose runs row is terminal — invisible to the active-run reaper
// forever, un-stoppable via the API, and counted against the cap. The reaper's
// zombie drain (v0's self-healing) must retry the kill next tick.
func TestReap_drainsZombieSessionAfterFailedKill(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}

	// Neutral Stop writes outcome 'stopped', then the kill fails: zombie.
	f.runner.FailStop(run.SessionName, errors.New("tmux: server busy"))
	if err := f.svc.StopAFK(t.Context(), run.SessionName); err != nil {
		t.Fatalf("StopAFK: %v", err)
	}
	if _, live := f.runner.Session(run.SessionName); !live {
		t.Fatal("precondition: a failed kill should have left the session live")
	}
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeStopped {
		t.Fatalf("run outcome = %q, want stopped", got.Outcome)
	}

	// The transient tmux failure clears; the next reaper tick drains it.
	f.runner.FailStop(run.SessionName, nil)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if _, live := f.runner.Session(run.SessionName); live {
		t.Error("zombie session not drained by the reaper (leaks a cap slot forever)")
	}
}

// Regression: the zombie drain must skip a startguard-marked (mid-Launch)
// session — killing it would race the launcher-owned rollback — while still
// draining the same row-less session once it is unmarked.
func TestReap_zombieDrainRespectsStartguard(t *testing.T) {
	f := newFixture(t)
	name := gitx.ComposeSessionName(f.repo.Name, "afk-9") // belongs to repo "proj"
	f.runner.AddLive(name)                                // live, but no active run row
	f.guard.Mark(name)

	// Marked: the drain leaves it alone.
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if _, live := f.runner.Session(name); !live {
		t.Fatal("drain killed a startguard-marked mid-Launch session")
	}

	// Unmarked: the same row-less live session is a genuine zombie and drains.
	f.guard.Clear(name)
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if _, live := f.runner.Session(name); live {
		t.Error("drain left a row-less, unmarked zombie session alive")
	}
}

// --- neutral Stop (§4c) -------------------------------------------------------

func TestNeutralStop_parksAndNeverReclassifies(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	f.setFailures(f.repo, 1) // prove "unchanged" isn't 0 == 0
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := f.runner.Session(run.SessionName)
	token := envValue(sess.ExtraEnv, "LAB_TOKEN")

	// Stop through the INSTANCE service — the §4c delegation seam.
	outcome, err := f.inst.Stop(t.Context(), run.SessionName)
	if err != nil {
		t.Fatalf("instance.Stop: %v", err)
	}
	if outcome != instance.OutcomeParked {
		t.Errorf("stop outcome = %q, want parked", outcome)
	}
	got := f.runRow(run.ID)
	if got.Outcome != store.RunOutcomeStopped {
		t.Fatalf("run outcome = %q, want stopped", got.Outcome)
	}
	if got.FailureReason != nil {
		t.Errorf("neutral stop wrote a failure reason: %v", *got.FailureReason)
	}
	if _, live := f.runner.Session(run.SessionName); live {
		t.Error("session still live after Stop")
	}
	// Worktree AND claim branch parked; counter bit-for-bit unchanged;
	// tokens dead.
	if !dirExists(run.WorktreePath) || !f.branchExists(f.repo, "afk/7") {
		t.Error("neutral stop tore down the parked worktree/branch")
	}
	if n := f.failures(f.repo); n != 1 {
		t.Errorf("failures = %d, want 1 (Stop never touches the counter)", n)
	}
	if _, err := f.st.RunTokenByHash(t.Context(), ids.HashToken(token)); !errors.Is(err, store.ErrNotFound) {
		t.Error("run token survives a Stop")
	}

	// A later over-budget sweep must NOT reclassify the stopped run — it
	// does not even list pulls for it (no candidates).
	before := f.trk.pullsCallCount()
	f.svc.ReapOnce(t.Context(), clockTime.Add(500*time.Minute))
	if f.trk.pullsCallCount() != before {
		t.Error("sweep listed pulls for a stopped run")
	}
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeStopped {
		t.Errorf("stopped run reclassified to %q", got.Outcome)
	}
	if n := f.failures(f.repo); n != 1 {
		t.Errorf("failures = %d after sweep, want 1", n)
	}
	if !dirExists(run.WorktreePath) || !f.branchExists(f.repo, "afk/7") {
		t.Error("sweep tore down a stopped run's parked work")
	}
}

func TestNeutralStop_racesReapWithoutDestroyingWork(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7, 8, 9)

	var runs []store.Run
	for range 3 {
		run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.dirtyWorktree(run.WorktreePath)
		runs = append(runs, run)
	}

	// Every run is over budget; user Stops race reaper sweeps (§4c: the one
	// lock decides). In EVERY interleaving: exactly one terminal outcome per
	// run (stopped or timeout, never both effects), and no dirty worktree is
	// ever removed.
	over := clockTime.Add(300 * time.Minute)
	var wg sync.WaitGroup
	for _, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.svc.StopAFK(context.Background(), run.SessionName)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				t.Errorf("StopAFK(%s): %v", run.SessionName, err)
			}
		}()
	}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.svc.ReapOnce(context.Background(), over)
		}()
	}
	wg.Wait()

	timeouts := 0
	for _, run := range runs {
		got := f.runRow(run.ID)
		switch got.Outcome {
		case store.RunOutcomeStopped:
		case store.RunOutcomeTimeout:
			timeouts++
		default:
			t.Errorf("run %s outcome = %q, want stopped or timeout", run.SessionName, got.Outcome)
		}
		if !dirExists(run.WorktreePath) {
			t.Errorf("dirty worktree of %s removed (neutral Stop keeps; dirty guard keeps)", run.SessionName)
		}
		if !f.branchExists(f.repo, run.Branch) {
			t.Errorf("claim branch %s deleted", run.Branch)
		}
	}
	if n := f.failures(f.repo); n != timeouts {
		t.Errorf("failures = %d, want exactly the %d timeout reaps (stops are neutral)", n, timeouts)
	}
}

// --- three strikes -------------------------------------------------------------

func TestThreeStrikes_pauseSchedulerAndManualUntilReset(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7, 8, 9, 10)
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		AFKAutoEnabled: store.Set(true),
	}); err != nil {
		t.Fatal(err)
	}
	f.setFailures(f.repo, 2) // two strikes down — the boundary is >= 3

	// Below the threshold the scheduler still launches (boundary check).
	f.svc.ScheduleOnce(t.Context())
	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 1 {
		t.Fatalf("below-threshold scheduler launched %d runs, want 1", len(active))
	}
	victim := active[0]

	// Third strike: the run dies → counter 3 → paused.
	f.runner.Kill(victim.SessionName)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if n := f.failures(f.repo); n != PauseThreshold {
		t.Fatalf("failures = %d, want %d", n, PauseThreshold)
	}

	// Paused: the scheduler skips the repo; a manual start 409s.
	f.svc.ScheduleOnce(t.Context())
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("paused repo still scheduled %d runs", len(active))
	}
	if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); !errors.Is(err, ErrRepoPaused) {
		t.Errorf("manual start on paused repo = %v, want ErrRepoPaused", err)
	}

	// Human reset re-arms both paths.
	if _, err := f.st.ResetRepoFailures(t.Context(), f.repo.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.ScheduleOnce(t.Context())
	active, _ = f.st.ActiveRuns(t.Context())
	if len(active) != 1 {
		t.Errorf("reset repo scheduled %d runs, want 1 (re-armed)", len(active))
	}
}

// --- scheduler -------------------------------------------------------------------

func autoOn(f *fixture, repo store.Repo) {
	f.t.Helper()
	if _, err := f.st.UpdateRepoSettings(f.t.Context(), repo.ID, store.RepoSettingsUpdate{
		AFKAutoEnabled: store.Set(true),
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestSchedule_launchesLowestAsAuto(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(12, 7)
	autoOn(f, f.repo)

	f.svc.ScheduleOnce(t.Context())
	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 1 {
		t.Fatalf("scheduled %d runs, want 1", len(active))
	}
	run := active[0]
	if run.Kind != store.RunKindAFKAuto || run.SessionName != "proj~afk-auto-7" || run.Branch != "afk/7" {
		t.Errorf("run = kind %q session %q branch %q, want afk_auto proj~afk-auto-7 afk/7", run.Kind, run.SessionName, run.Branch)
	}
	if run.BudgetDeadline == nil {
		t.Error("auto run has no persisted budget deadline")
	}
}

func TestSchedule_serialPerRepoButManualAdditive(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7, 8)
	autoOn(f, f.repo)

	// A live MANUAL AFK run does not block the auto loop…
	if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.ScheduleOnce(t.Context())
	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 2 {
		t.Fatalf("active = %d, want 2 (manual + auto)", len(active))
	}
	autos := 0
	for _, r := range active {
		if r.Kind == store.RunKindAFKAuto {
			autos++
		}
	}
	if autos != 1 {
		t.Fatalf("auto runs = %d, want exactly 1", autos)
	}

	// …but a live AUTO run keeps the loop serial: nothing new even with a
	// fresh ready issue.
	f.trk.setReady(7, 8, 9)
	f.svc.ScheduleOnce(t.Context())
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 2 {
		t.Errorf("second sweep launched a second auto run (%d active)", len(active))
	}
}

func TestSchedule_gates(t *testing.T) {
	t.Run("auto off does nothing", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		f.svc.ScheduleOnce(t.Context())
		if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
			t.Error("auto-off repo launched")
		}
	})
	t.Run("no ready idles", func(t *testing.T) {
		f := newFixture(t)
		autoOn(f, f.repo)
		f.svc.ScheduleOnce(t.Context())
		if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
			t.Error("empty queue launched")
		}
	})
	t.Run("parked issue reads zero and cannot loop", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		f.createClaimBranch(f.repo, "afk/7")
		autoOn(f, f.repo)
		f.svc.ScheduleOnce(t.Context())
		if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
			t.Error("parked issue re-claimed by the scheduler")
		}
	})
	t.Run("at cap launches nothing", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		autoOn(f, f.repo)
		if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
			t.Fatal(err)
		}
		f.runner.AddLive("other~unrelated")
		f.svc.ScheduleOnce(t.Context())
		if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
			t.Error("at-cap sweep claimed/launched")
		}
		if f.branchExists(f.repo, "afk/7") {
			t.Error("at-cap sweep still created a claim")
		}
	})
	t.Run("logged out does not claim", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		autoOn(f, f.repo)
		f.prov.SetLoggedIn(false)
		f.svc.ScheduleOnce(t.Context())
		if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
			t.Error("logged-out sweep launched")
		}
		if f.branchExists(f.repo, "afk/7") {
			t.Error("logged-out sweep claimed an issue")
		}
	})
}

func TestSchedule_pauseIsPerRepo(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	autoOn(f, f.repo)
	f.setFailures(f.repo, PauseThreshold)

	healthy, healthyTrk := f.addRepo("healthy", "afk/<N>")
	healthyTrk.setReady(7)
	autoOn(f, healthy)

	f.svc.ScheduleOnce(t.Context())
	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1 (only the healthy repo)", len(active))
	}
	if active[0].RepoID != healthy.ID {
		t.Errorf("launched repo = %s, want the healthy one (pause is strictly per repo)", active[0].RepoID)
	}
}

func TestSchedule_concurrentSweepsStaySerial(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	autoOn(f, f.repo)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.svc.ScheduleOnce(context.Background())
		}()
	}
	wg.Wait()

	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 1 {
		t.Errorf("concurrent sweeps launched %d runs, want exactly 1 (single-flighted claim)", len(active))
	}
	if !f.branchExists(f.repo, "afk/7") || f.branchExists(f.repo, "afk/8") {
		t.Error("claim set wrong after concurrent sweeps")
	}
}

// listHookRunner interposes a callback before every SessionRunner.List, so a
// test can make a session appear mid-tick — between the scheduler's initial
// live-count snapshot and launch()'s fresh locked cap re-check, the window that
// turns a per-repo cap hit into launchAtCap.
type listHookRunner struct {
	*tmuxx.Fake
	mu     sync.Mutex
	before func(*tmuxx.Fake)
}

func (r *listHookRunner) List(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	hook := r.before
	r.mu.Unlock()
	if hook != nil {
		hook(r.Fake)
	}
	return r.Fake.List(ctx)
}

// Regression (D12c): launchAtCap must skip only the capped repo, not abort the
// whole scheduler tick. Under per-repo cap overrides one repo hitting its cap
// no longer implies every repo is at cap, so a later candidate with headroom
// must still launch. The capped repo ("proj") is processed first (Repos()
// orders by name) and a session lands mid-tick, so its launch returns
// launchAtCap; the healthy repo ("zzz", default cap) must still get its run.
// Before the fix the launchAtCap `return` aborted the whole tick and the
// healthy repo idled until the next one.
func TestSchedule_perRepoCapDoesNotAbortTick(t *testing.T) {
	var hooked *listHookRunner
	f := newFixtureWrapped(t, "afk/<N>", func(fake *tmuxx.Fake) tmuxx.SessionRunner {
		hooked = &listHookRunner{Fake: fake}
		return hooked
	})

	// Capped repo (name "proj" sorts first) with a per-repo cap of 1.
	one := 1
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		MaxInstancesOverride: store.Set(&one),
	}); err != nil {
		t.Fatal(err)
	}
	f.trk.setReady(7)
	autoOn(f, f.repo)

	// Healthy repo (name "zzz" sorts last), default cap 6, its own ready queue.
	healthy, healthyTrk := f.addRepo("zzz", "afk/<N>")
	healthyTrk.setReady(7)
	autoOn(f, healthy)

	// A session appears AFTER the tick snapshot (first List) but before the
	// capped repo's launch() locked cap re-check (second List): the snapshot
	// counted 0 (< the cap of 1), the re-check counts 1 (== the cap), so the
	// capped repo's launch returns launchAtCap while the healthy repo (cap 6)
	// still has headroom.
	listCalls := 0
	hooked.before = func(fake *tmuxx.Fake) {
		listCalls++
		if listCalls >= 2 {
			fake.AddLive("intruder~x")
		}
	}

	f.svc.ScheduleOnce(t.Context())

	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 1 {
		t.Fatalf("active runs = %d, want 1 (the capped repo aborts only itself)", len(active))
	}
	if active[0].RepoID != healthy.ID {
		t.Errorf("launched repo = %s, want the healthy one (a per-repo cap must not abort the whole tick)", active[0].RepoID)
	}
	if f.branchExists(f.repo, "afk/7") {
		t.Error("capped repo created a claim despite hitting its cap")
	}
	if !f.branchExists(healthy, "afk/7") {
		t.Error("healthy repo did not claim its issue after the capped repo hit its cap")
	}
}

// --- ready hint ---------------------------------------------------------------

func TestClaimableIssuesFor(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7, 8, 9)
	f.createClaimBranch(f.repo, "afk/8")

	claimable, err := f.svc.ClaimableIssuesFor(t.Context(), f.repo)
	if err != nil {
		t.Fatalf("ClaimableIssuesFor: %v", err)
	}
	var ns []string
	for _, is := range claimable {
		ns = append(ns, strconv.Itoa(is.Number))
	}
	if strings.Join(ns, ",") != "7,9" {
		t.Errorf("claimable = %v, want [7 9] (claimed 8 drained around)", ns)
	}
}
