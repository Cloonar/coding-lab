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
	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
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
// rows the engine reads — the reaper through bounded PullsForHead lookups
// (#176), never a Pulls listing — plus injectable errors and call counters.
// Reviews/PullComments (the autoland reads, #181) are scriptable per pull
// number and default to empty success.
type fakeTracker struct {
	mu                sync.Mutex
	ready             []tracker.Issue
	open              []tracker.Issue // the StateOpen set the blocked-by gate weighs (#136)
	pulls             []tracker.PullRef
	readyErr          error
	issuesErr         error // failIssues: Issues() error injection (open-set fetch)
	issuesCalls       int
	pullsCalls        int            // pullsCallCount: reaper ticks must add ZERO (#176); the autoland poller reads it
	pullsForHeadErr   error          // failPullsForHead: PullsForHead() error injection
	pullsForHeadCalls int            // pullsForHeadCallCount: the bounded-read counter (#176)
	pullsForHeadArgs  [][2]string    // recorded (head, base) per PullsForHead call
	pullTitles        map[int]string // Pull(n) detail title (done-signal notification body)
	detailErr         error          // failPullDetail: Pull() error injection
	detailCalls       int
	reviews           map[int][]tracker.Review  // Reviews(n) — the poller's native-review read
	pullComments      map[int][]tracker.Comment // PullComments(n) — verdict markers (#181)
	commentsErr       error                     // failPullComments: PullComments() error injection
	commentsCalls     int
}

func (f *fakeTracker) ReadyIssues(context.Context) ([]tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readyErr != nil {
		return nil, f.readyErr
	}
	return append([]tracker.Issue(nil), f.ready...), nil
}

// Issues serves the open-set the blocked-by gate weighs (#136). The engine only
// ever asks for StateOpen, so the state param is ignored — a copy of the
// scripted open set is returned, and the call is counted so a test can prove the
// fetch stayed lazy.
func (f *fakeTracker) Issues(context.Context, string) ([]tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issuesCalls++
	if f.issuesErr != nil {
		return nil, f.issuesErr
	}
	return append([]tracker.Issue(nil), f.open...), nil
}
func (f *fakeTracker) Issue(context.Context, int) (tracker.Issue, error) {
	return tracker.Issue{}, tracker.ErrNotFound
}
func (f *fakeTracker) CreateComment(context.Context, int, string) error { return nil }
func (f *fakeTracker) Pulls(context.Context) ([]tracker.PullRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullsCalls++
	return append([]tracker.PullRef(nil), f.pulls...), nil
}

// PullsForHead serves the scripted pulls filtered by head — the reaper's
// bounded done-signal read (#176). addPull rows carry no base branch, so base
// always matches — but the (head, base) pair is recorded and the call
// counted, so the reaper tests can assert what was asked and how often,
// mirroring the pullsCalls/pullsCallCount pattern.
func (f *fakeTracker) PullsForHead(_ context.Context, head, base string) ([]tracker.PullRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullsForHeadCalls++
	f.pullsForHeadArgs = append(f.pullsForHeadArgs, [2]string{head, base})
	if f.pullsForHeadErr != nil {
		return nil, f.pullsForHeadErr
	}
	out := make([]tracker.PullRef, 0, len(f.pulls))
	for _, p := range f.pulls {
		if p.HeadBranch == head {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeTracker) Pull(_ context.Context, n int) (tracker.PullDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detailCalls++
	if f.detailErr != nil {
		return tracker.PullDetail{}, f.detailErr
	}
	return tracker.PullDetail{Number: n, Title: f.pullTitles[n]}, nil
}
func (f *fakeTracker) Checks(context.Context, int) ([]tracker.Check, error) { return nil, nil }
func (f *fakeTracker) CreatePull(context.Context, string, string, string, string) (tracker.PullRef, error) {
	return tracker.PullRef{}, errors.New("not implemented")
}
func (f *fakeTracker) MergePull(context.Context, int) (tracker.PullRef, error) {
	return tracker.PullRef{}, errors.New("not implemented")
}
func (f *fakeTracker) Reviews(_ context.Context, n int) ([]tracker.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tracker.Review(nil), f.reviews[n]...), nil
}
func (f *fakeTracker) RerequestReview(context.Context, int) error {
	return errors.New("not implemented")
}
func (f *fakeTracker) CommentPull(context.Context, int, string) error {
	return errors.New("not implemented")
}
func (f *fakeTracker) PullComments(_ context.Context, n int) ([]tracker.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentsCalls++
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	return append([]tracker.Comment(nil), f.pullComments[n]...), nil
}
func (f *fakeTracker) CloseIssue(context.Context, int) error { return nil }
func (f *fakeTracker) CreateIssue(context.Context, string, string, []string) (tracker.Issue, error) {
	return tracker.Issue{}, errors.New("not implemented")
}
func (f *fakeTracker) EditIssue(context.Context, int, tracker.IssueEdit) (tracker.Issue, error) {
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

// setReadyIssue APPENDS one ready issue carrying a body — the `## Blocked by`
// section the gate reads — leaving setReady's ref-less behaviour untouched so
// every existing test keeps passing.
func (f *fakeTracker) setReadyIssue(n int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready = append(f.ready, tracker.Issue{Number: n, Title: fmt.Sprintf("issue %d", n), Body: body})
}

// setOpen scripts the StateOpen set the blocked-by gate weighs a blocker ref
// against (#136).
func (f *fakeTracker) setOpen(ns ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = f.open[:0]
	for _, n := range ns {
		f.open = append(f.open, tracker.Issue{Number: n, Title: fmt.Sprintf("issue %d", n)})
	}
}

func (f *fakeTracker) failIssues(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issuesErr = err
}

func (f *fakeTracker) issuesCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issuesCalls
}

func (f *fakeTracker) addPull(head, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, tracker.PullRef{Number: len(f.pulls) + 1, HeadBranch: head, State: state})
}

func (f *fakeTracker) pullsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pullsCalls
}

func (f *fakeTracker) failPullsForHead(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullsForHeadErr = err
}

func (f *fakeTracker) pullsForHeadCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pullsForHeadCalls
}

// pullsForHeadCallArgs snapshots the recorded (head, base) pairs.
func (f *fakeTracker) pullsForHeadCallArgs() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.pullsForHeadArgs...)
}

func (f *fakeTracker) addReview(n int, state string, dismissed bool) {
	f.addReviewFrom(n, "human", state, dismissed, "")
}

// addReviewFrom is addReview with the reviewer and body scriptable — the #182
// rejected-state fold is per reviewer and RejectionContext quotes bodies, so
// the fix-forward tests need both.
func (f *fakeTracker) addReviewFrom(n int, reviewer, state string, dismissed bool, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reviews == nil {
		f.reviews = map[int][]tracker.Review{}
	}
	f.reviews[n] = append(f.reviews[n], tracker.Review{Reviewer: reviewer, State: state, Dismissed: dismissed, Body: body})
}

func (f *fakeTracker) addPullComment(n int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullComments == nil {
		f.pullComments = map[int][]tracker.Comment{}
	}
	f.pullComments[n] = append(f.pullComments[n], tracker.Comment{Author: "lander", Body: body, CreatedAt: clockTime})
}

func (f *fakeTracker) failPullComments(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentsErr = err
}

func (f *fakeTracker) pullCommentsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commentsCalls
}

func (f *fakeTracker) setPullTitle(n int, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullTitles == nil {
		f.pullTitles = map[int]string{}
	}
	f.pullTitles[n] = title
}

func (f *fakeTracker) failPullDetail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detailErr = err
}

func (f *fakeTracker) detailCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detailCalls
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
	homes    *instancehome.Manager

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
	if err := st.SeedDefaultSettings(t.Context(), 6, "claude-code"); err != nil {
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
	homes := instancehome.New(filepath.Join(stateDir, "instances"))
	inst, err := instance.New(instance.Options{
		Store: st, Git: git, Runner: svcRunner, Providers: reg, Vault: vlt, Materializer: mat,
		Homes: homes, Guard: guard, Bus: bus, ReposDir: reposDir, WorktreeRoot: worktreeRoot,
		LabURL: "http://127.0.0.1:8080", GitEnv: env, CaptureCtx: context.Background(),
		Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("instance.New: %v", err)
	}

	trackers := newFakeResolver()
	svc, err := New(Options{
		Store: st, Git: git, Runner: svcRunner, Trackers: trackers,
		Instances: inst, Homes: homes, Bus: bus, Guard: guard,
		ReposDir: reposDir, WorktreeRoot: worktreeRoot, GitEnv: env, Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("afk.New: %v", err)
	}
	inst.SetAFKStopper(svc)

	f := &fixture{
		t: t, svc: svc, inst: inst, st: st, runner: runner, prov: prov, bus: bus,
		clock: clock, trackers: trackers, git: git, guard: guard, homes: homes,
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
		AFKBranchPattern: pattern, ManualBranchPrefix: "lab/",
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
	if last := sess.Argv[len(sess.Argv)-1]; last != SeedPrompt(7, "afk/7", false, "") {
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

// TestStartManualAFK_repoOverrideReachesSeedPrompt pins the #52/ADR-0027 launch
// wiring end-to-end within the engine: a repo-level afk_prompt override is
// resolved on the locked launch path (ResolveAFKPrompt) and rendered into the
// seed prompt carried as the spawn's trailing argv positional — with <N>/<BRANCH>
// interpolated and the built-in template fully replaced.
func TestStartManualAFK_repoOverrideReachesSeedPrompt(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)

	override := "Custom playbook: resolve <N> on <BRANCH>, then stop."
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID,
		store.RepoSettingsUpdate{AFKPrompt: store.Set(&override)}); err != nil {
		t.Fatalf("set afk_prompt override: %v", err)
	}

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatalf("session %q not live", run.SessionName)
	}
	want := "Custom playbook: resolve 7 on afk/7, then stop."
	if last := sess.Argv[len(sess.Argv)-1]; last != want {
		t.Errorf("seed argv = %q, want the rendered repo override %q", last, want)
	}
}

// The AFK launch path resolves the remote-control knob through the instance
// service (issue #163) and carries it OPAQUELY into the spawn: the afk package
// never reads the value — it neither branches on it nor spawns differently
// because of it, exactly as it carries the ultracode options bag. Here the
// AFK-override layer (spawn_remote_default_afk) turns it on for unattended runs
// and the resolved bool must land in BOTH places lab later reads it: the
// provider's SpawnSpec and the persisted run row (the deep-link gate's source of
// truth across a restart).
func TestStartManualAFK_remoteOverrideReachesSpawnAndRunRow(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	// The base stays "false" (seeded) — only the AFK override says true, so this
	// also pins that the AFK layer resolves BEFORE the base.
	if err := f.st.SetSetting(t.Context(), store.SettingSpawnRemoteDefaultAFK, "true"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	specs := f.prov.SpawnSpecs()
	if len(specs) != 1 || !specs[0].Remote {
		t.Fatalf("SpawnSpecs = %+v, want one spec with Remote=true", specs)
	}
	if !run.Remote || !f.runRow(run.ID).Remote {
		t.Errorf("run.Remote = %v / persisted %v, want true on both (the row is what survives a restart)",
			run.Remote, f.runRow(run.ID).Remote)
	}

	// And with no override anywhere, an AFK run inherits the seeded false base.
	f2 := newFixture(t)
	f2.trk.setReady(7)
	run2, err := f2.svc.StartManualAFK(t.Context(), f2.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK (no override): %v", err)
	}
	if run2.Remote || f2.runRow(run2.ID).Remote {
		t.Error("an AFK run with no remote layers set recorded remote=true, want the false floor")
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

	// The done-signal was read via the bounded per-branch lookup — the reaper
	// NEVER lists the repo's pull history (#176).
	if n := f.trk.pullsCallCount(); n != 0 {
		t.Errorf("reaper listed pulls %d times; want 0 (bounded PullsForHead only — #176)", n)
	}

	// A second sweep is a total no-op: the run is terminal, so not even the
	// bounded per-branch read happens.
	before := f.trk.pullsForHeadCallCount()
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if f.trk.pullsForHeadCallCount() != before {
		t.Error("second sweep re-read pulls for a reaped run")
	}
}

// TestReap_boundedPullReadsPerRun pins the issue #176 acceptance criterion:
// one reaper tick over N active runs on one repo makes exactly N PullsForHead
// calls — one per run, each asking (run.Branch, repo.DefaultBranch), the base
// the agent API pins for every lab-created PR — and ZERO Pulls calls; the
// request count is O(active runs), independent of how many merged PRs the
// repo has accumulated. A second tick pays the same bounded price again (the
// read is per tick, never memoized — a PR can appear at any moment).
func TestReap_boundedPullReadsPerRun(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7, 8, 9)

	var runs []store.Run
	for range 3 {
		run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}

	// All alive, under budget, no PRs: the tick classifies nothing, but every
	// run got exactly one bounded done-signal read.
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if n := f.trk.pullsCallCount(); n != 0 {
		t.Errorf("tick listed pulls %d times; want 0 (bounded PullsForHead only — #176)", n)
	}
	if n := f.trk.pullsForHeadCallCount(); n != len(runs) {
		t.Errorf("tick made %d PullsForHead calls; want %d (one per active run)", n, len(runs))
	}
	args := f.trk.pullsForHeadCallArgs()
	if len(args) != len(runs) {
		t.Fatalf("recorded %d arg pairs; want %d", len(args), len(runs))
	}
	// Runs come back sorted by session name, so the tick is deterministic and
	// the args line up run-for-run.
	for i, run := range runs {
		if want := [2]string{run.Branch, f.repo.DefaultBranch}; args[i] != want {
			t.Errorf("call %d args = %v; want %v (head = run branch, base = repo default)", i, args[i], want)
		}
	}

	// Still-active runs are re-read next tick at the same bounded price.
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(2*time.Minute))
	if n := f.trk.pullsForHeadCallCount(); n != 2*len(runs) {
		t.Errorf("after two ticks %d PullsForHead calls; want %d", n, 2*len(runs))
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
	f.trk.failPullsForHead(errors.New("forge is down"))

	// The bounded per-head read failed: the run is never classified on
	// missing tracker data — it stays active and waits for the next tick.
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Fatalf("outcome = %q — classified on missing tracker data", got.Outcome)
	}
	// Next tick, tracker healthy again → real death.
	f.trk.failPullsForHead(nil)
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
	f.svc.SpawnOnce(t.Context())
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
// draining the same row-less session once it is unmarked. Also pins that two
// providers' login sessions, plus the legacy bare name, coexist in the live
// list across both drain passes without ever being touched (issue #77).
func TestReap_zombieDrainRespectsStartguard(t *testing.T) {
	f := newFixture(t)
	name := gitx.ComposeSessionName(f.repo.Name, "afk-9") // belongs to repo "proj"
	f.runner.AddLive(name)                                // live, but no active run row
	f.guard.Mark(name)

	logins := []string{tmuxx.LoginSessionName("claude-code"), tmuxx.LoginSessionName("codex"), "lab-login"}
	for _, login := range logins {
		f.runner.AddLive(login)
	}

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
	for _, login := range logins {
		if _, live := f.runner.Session(login); !live {
			t.Errorf("drain killed login session %q", login)
		}
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
	// does not even read pulls for it (no candidates).
	before := f.trk.pullsForHeadCallCount()
	f.svc.ReapOnce(t.Context(), clockTime.Add(500*time.Minute))
	if f.trk.pullsForHeadCallCount() != before {
		t.Error("sweep read pulls for a stopped run")
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
	f.svc.SpawnOnce(t.Context())
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
	f.svc.SpawnOnce(t.Context())
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
	f.svc.SpawnOnce(t.Context())
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

	f.svc.SpawnOnce(t.Context())
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
	f.svc.SpawnOnce(t.Context())
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
	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 2 {
		t.Errorf("second sweep launched a second auto run (%d active)", len(active))
	}
}

func TestSchedule_gates(t *testing.T) {
	t.Run("auto off does nothing", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		f.svc.SpawnOnce(t.Context())
		if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
			t.Error("auto-off repo launched")
		}
	})
	t.Run("no ready idles", func(t *testing.T) {
		f := newFixture(t)
		autoOn(f, f.repo)
		f.svc.SpawnOnce(t.Context())
		if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
			t.Error("empty queue launched")
		}
	})
	t.Run("parked issue reads zero and cannot loop", func(t *testing.T) {
		f := newFixture(t)
		f.trk.setReady(7)
		f.createClaimBranch(f.repo, "afk/7")
		autoOn(f, f.repo)
		f.svc.SpawnOnce(t.Context())
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
		f.svc.SpawnOnce(t.Context())
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
		f.svc.SpawnOnce(t.Context())
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

	f.svc.SpawnOnce(t.Context())
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
			f.svc.SpawnOnce(context.Background())
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
// test can make a session appear mid-pass — between the spawn pass's initial
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

// Regression (D12c): an at-cap launch must skip only the capped repo, not
// abort the whole spawn pass. Under per-repo cap overrides one repo hitting
// its cap no longer implies every repo is at cap, so a later candidate with
// headroom must still launch. The capped repo ("proj") is processed first
// (Repos() orders by name) and a session lands mid-pass, so its launch
// returns atCap; the healthy repo ("zzz", default cap) must still get its
// run. Before the fix the at-cap `return` aborted the whole tick and the
// healthy repo idled until the next one; the spawn pass keeps the per-repo
// floor-raise semantics (#185).
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

	f.svc.SpawnOnce(t.Context())

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

// --- blocked-by gate (#136, ADR-0042) ----------------------------------------

// The headline acceptance test: the scheduler holds back a lower-numbered ready
// issue while an open `## Blocked by` ref keeps it blocked, claims the next
// unblocked issue instead, and — once the blocker closes on a later sweep —
// claims the previously-blocked issue.
func TestSchedule_blockedIssueSkippedThenClaimedAfterBlockerCloses(t *testing.T) {
	f := newFixture(t)
	f.trk.setReadyIssue(87, "## Blocked by\n\n- #74\n")
	f.trk.setReadyIssue(90, "Ready to go, no blockers here.\n")
	f.trk.setOpen(74, 87, 90)
	autoOn(f, f.repo)

	// #87 is the LOWER number but its blocker #74 is open, so #90 is claimed.
	f.svc.SpawnOnce(t.Context())
	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 1 {
		t.Fatalf("scheduled %d runs, want 1 (#87 held back by open #74)", len(active))
	}
	run := active[0]
	if run.IssueNumber == nil || *run.IssueNumber != 90 || run.Branch != "afk/90" {
		t.Fatalf("claimed run = %+v, want issue 90 on afk/90 (the lower #87 was skipped)", run)
	}
	if f.branchExists(f.repo, "afk/87") {
		t.Error("blocked issue #87 was claimed (a claim branch exists)")
	}

	// Complete the #90 run via a success reap so the serial auto gate releases:
	// real work in the worktree + an open PR on the run branch (the done-signal).
	f.commitInWorktree(run.WorktreePath)
	f.trk.addPull("afk/90", tracker.PullOpen)
	f.clock.Advance(10 * time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("reap outcome = %q, want success (needed to release the serial auto gate)", got.Outcome)
	}

	// Blocker #74 has closed and #90 is done: only #87 remains ready, now
	// unblocked. The next sweep claims it.
	f.trk.setReady() // clear the queue…
	f.trk.setReadyIssue(87, "## Blocked by\n\n- #74\n")
	f.trk.setOpen(87) // …#74 no longer open
	f.svc.SpawnOnce(t.Context())
	active, _ = f.st.ActiveRuns(t.Context())
	if len(active) != 1 {
		t.Fatalf("second sweep active = %d, want 1 (#87 unblocked once #74 closed)", len(active))
	}
	if active[0].IssueNumber == nil || *active[0].IssueNumber != 87 || active[0].Branch != "afk/87" {
		t.Fatalf("second claim = %+v, want issue 87 on afk/87", active[0])
	}
}

// An all-blocked queue is indistinguishable from an empty one: the auto sweep
// launches nothing and never strikes the failure counter, and a manual start
// returns the pinned empty-queue sentinel.
func TestSchedule_allBlockedIsEmptyQueue(t *testing.T) {
	f := newFixture(t)
	f.trk.setReadyIssue(87, "## Blocked by\n\n- #74\n")
	f.trk.setOpen(74, 87)

	// (a) auto sweep: nothing launches AND the counter is untouched.
	autoOn(f, f.repo)
	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Fatalf("all-blocked sweep launched %d runs, want 0", len(active))
	}
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("failures = %d, want 0 (a blocked queue is not a failed run)", n)
	}

	// (b) manual start: the same empty-queue outcome, the pinned 409 sentinel.
	if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); !errors.Is(err, ErrNoReady) {
		t.Fatalf("manual start = %v, want ErrNoReady (all-blocked ≡ empty)", err)
	}
}

// The open-set fetch is lazy: neither the auto path nor the manual path issues a
// Tracker.Issues(StateOpen) call when no claimable issue carries a blocker ref.
func TestFilterClaimable_noOpenFetchWithoutRefs(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7, 8) // setReady writes no Body → no `## Blocked by` refs
	autoOn(f, f.repo)

	f.svc.SpawnOnce(t.Context()) // auto path filters (claims #7)
	if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); err != nil {
		t.Fatalf("StartManualAFK: %v", err) // manual path filters too (claims #8)
	}
	if calls := f.trk.issuesCallCount(); calls != 0 {
		t.Errorf("Issues(StateOpen) fired %d times with no blocker refs in the queue, want 0 (the fetch is lazy)", calls)
	}
}

// The open-set fetch fails CLOSED: a Tracker.Issues error skips the repo's tick
// (no launch, no strike) and surfaces as *TrackerError on a manual start.
func TestFilterClaimable_openFetchFailureFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.trk.setReadyIssue(87, "## Blocked by\n\n- #74\n") // a real local ref arms the fetch
	f.trk.failIssues(errors.New("forge is down"))

	// (a) auto sweep: skip the repo — no run, no panic, no failure strike (a
	// fetch error is infrastructure, not a run outcome).
	autoOn(f, f.repo)
	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Fatalf("fetch-failed sweep launched %d runs, want 0 (fail closed)", len(active))
	}
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("failures = %d, want 0 (an open-set fetch error is not a run failure)", n)
	}

	// (b) manual start: the failure surfaces as *TrackerError (API → 502).
	_, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	var te *TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("manual start err = %v, want *TrackerError (fail closed on infra)", err)
	}
}

// --- instance HOME wipe (issue #202) ------------------------------------------

// A neutral Stop of an AFK run wipes its private instance HOME (the credential
// copy) while the park — worktree + claim branch — survives untouched.
func TestStopAFK_wipesInstanceHome(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	home := f.homes.HomePath(run.ID)
	if !dirExists(home) {
		t.Fatalf("instance home %s not materialized by launch", home)
	}

	if err := f.svc.StopAFK(t.Context(), run.SessionName); err != nil {
		t.Fatalf("StopAFK: %v", err)
	}
	if dirExists(home) {
		t.Error("instance home survived StopAFK (the credential copy must go)")
	}
	// The park is untouched: the claim branch still exists.
	if !f.branchExists(f.repo, "afk/7") {
		t.Error("StopAFK deleted the claim branch — the park must survive")
	}
}

// A reaper terminal outcome (here a death) wipes the run's private instance
// HOME beside the credential cleanup it already does.
func TestReap_wipesInstanceHome(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	home := f.homes.HomePath(run.ID)
	if !dirExists(home) {
		t.Fatalf("instance home %s not materialized by launch", home)
	}

	f.runner.Kill(run.SessionName) // crashed → reaps as death
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("outcome = %q, want death (so reapRun ran)", got.Outcome)
	}
	if dirExists(home) {
		t.Error("instance home survived the reap (the credential copy must go)")
	}
}
