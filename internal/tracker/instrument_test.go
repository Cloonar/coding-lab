package tracker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// failingTracker errs on every call — the observer must report ok=false for
// each op.
type failingTracker struct{ err error }

func (f failingTracker) ReadyIssues(context.Context) ([]Issue, error)    { return nil, f.err }
func (f failingTracker) Issues(context.Context, string) ([]Issue, error) { return nil, f.err }
func (f failingTracker) Issue(context.Context, int) (Issue, error)       { return Issue{}, f.err }
func (f failingTracker) CreateComment(context.Context, int, string) error {
	return f.err
}
func (f failingTracker) Pulls(context.Context) ([]PullRef, error) { return nil, f.err }
func (f failingTracker) Pull(context.Context, int) (PullDetail, error) {
	return PullDetail{}, f.err
}
func (f failingTracker) Checks(context.Context, int) ([]Check, error) { return nil, f.err }
func (f failingTracker) CreatePull(context.Context, string, string, string, string) (PullRef, error) {
	return PullRef{}, f.err
}
func (f failingTracker) MergePull(context.Context, int) (PullRef, error) { return PullRef{}, f.err }
func (f failingTracker) CloseIssue(context.Context, int) error           { return f.err }
func (f failingTracker) CreateIssue(context.Context, string, string, []string) (Issue, error) {
	return Issue{}, f.err
}
func (f failingTracker) EditIssue(context.Context, int, IssueEdit) (Issue, error) {
	return Issue{}, f.err
}
func (f failingTracker) AddIssueLabels(context.Context, int, []string) error    { return f.err }
func (f failingTracker) RemoveIssueLabels(context.Context, int, []string) error { return f.err }
func (f failingTracker) Labels(context.Context) ([]Label, error)                { return nil, f.err }
func (f failingTracker) EnsureLabel(context.Context, string, string, string) (Label, error) {
	return Label{}, f.err
}

// observerLog records every (binding, op, ok) the seam reports.
type observerLog struct {
	mu   sync.Mutex
	got  []obsCall
	next int
}

type obsCall struct {
	binding, op string
	ok          bool
}

func (l *observerLog) observe(binding, op string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, obsCall{binding, op, ok})
}

// take pops the next recorded call (order of the driving loop below).
func (l *observerLog) take(t *testing.T) obsCall {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.next >= len(l.got) {
		t.Fatalf("observer recorded only %d calls", len(l.got))
	}
	c := l.got[l.next]
	l.next++
	return c
}

// driveAll calls every Tracker method once, in the fixed op order the
// assertions below use.
func driveAll(t *testing.T, trk Tracker) {
	t.Helper()
	ctx := context.Background()
	_, _ = trk.ReadyIssues(ctx)
	_, _ = trk.Issues(ctx, StateOpen)
	_, _ = trk.Issue(ctx, 1)
	_ = trk.CreateComment(ctx, 1, "body")
	_, _ = trk.Pulls(ctx)
	_, _ = trk.Pull(ctx, 1)
	_, _ = trk.Checks(ctx, 1)
	_, _ = trk.CreatePull(ctx, "afk/1", "main", "t", "b")
	_, _ = trk.MergePull(ctx, 1)
	_ = trk.CloseIssue(ctx, 1)
	_, _ = trk.CreateIssue(ctx, "t", "b", []string{"bug"})
	_, _ = trk.EditIssue(ctx, 1, IssueEdit{})
	_ = trk.AddIssueLabels(ctx, 1, []string{"bug"})
	_ = trk.RemoveIssueLabels(ctx, 1, []string{"bug"})
	_, _ = trk.Labels(ctx)
	_, _ = trk.EnsureLabel(ctx, "bug", "", "")
}

// opOrder mirrors driveAll.
var opOrder = []string{OpReadyIssues, OpIssues, OpIssue, OpCreateComment, OpPulls, OpPull, OpChecks, OpCreatePull,
	OpMergePull, OpCloseIssue, OpCreateIssue, OpEditIssue, OpAddIssueLabels, OpRemoveIssueLabels, OpLabels, OpEnsureLabel}

func TestObserverReportsEveryOp(t *testing.T) {
	f := newRegistryFixture(t)
	log := &observerLog{}
	f.reg.SetObserver(log.observe)

	trk, err := f.reg.TrackerFor(context.Background(),
		store.Repo{ID: "repo_ok", TrackerBinding: store.TrackerBindingBuiltin})
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	driveAll(t, trk)

	for _, op := range opOrder {
		c := log.take(t)
		if c.binding != store.TrackerBindingBuiltin || c.op != op || !c.ok {
			t.Errorf("observed %+v, want {builtin %s true}", c, op)
		}
	}
}

func TestObserverReportsErrorsWithoutErrorContent(t *testing.T) {
	f := newRegistryFixture(t)
	log := &observerLog{}
	f.reg.SetObserver(log.observe)
	// Swap in a failing backend behind the builtin binding: the observer
	// must see ok=false per op — and by construction (Observer receives no
	// error value) nothing of the error text can cross the seam.
	f.reg.newBuiltin = func(BuiltinConfig) Tracker {
		return failingTracker{err: errors.New("secret-carrying failure")}
	}

	trk, err := f.reg.TrackerFor(context.Background(),
		store.Repo{ID: "repo_fail", TrackerBinding: store.TrackerBindingBuiltin})
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	driveAll(t, trk)

	for _, op := range opOrder {
		c := log.take(t)
		if c.binding != store.TrackerBindingBuiltin || c.op != op || c.ok {
			t.Errorf("observed %+v, want {builtin %s false}", c, op)
		}
	}
}

// rescopedFake is a Tracker+RunScoper backend: ForRun stamps the run id on
// a copy, so the test can tell which identity the wrapper handed back. Its
// embedded failingTracker carries a nil err, so every op succeeds.
type rescopedFake struct {
	failingTracker
	runID string
}

func (r rescopedFake) ForRun(runID string) Tracker {
	r.runID = runID
	return r
}

// TestObservedForwardsRunScoper pins the decorator's rescoping seam
// (regression: with the observer wired, the agent API's ForRun rescope was
// silently skipped — the wrapper hid the concrete backend — misattributing
// agent comments to the operator): the wrapper forwards ForRun to the
// backend and re-wraps the result, so rescoped calls still report.
func TestObservedForwardsRunScoper(t *testing.T) {
	f := newRegistryFixture(t)
	log := &observerLog{}
	f.reg.SetObserver(log.observe)
	f.reg.newBuiltin = func(BuiltinConfig) Tracker { return rescopedFake{} }

	trk, err := f.reg.TrackerFor(context.Background(),
		store.Repo{ID: "repo_scoped", TrackerBinding: store.TrackerBindingBuiltin})
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}

	rs, ok := trk.(RunScoper)
	if !ok {
		t.Fatalf("observed wrapper (%T) does not expose RunScoper", trk)
	}
	scoped := rs.ForRun("run_1")
	wrapper, ok := scoped.(*observed)
	if !ok {
		t.Fatalf("ForRun returned %T, want the re-wrapped decorator", scoped)
	}
	backend, ok := wrapper.t.(rescopedFake)
	if !ok || backend.runID != "run_1" {
		t.Fatalf("rescoped backend = %#v, want rescopedFake bound to run_1", wrapper.t)
	}

	// The rescoped tracker still reports through the observer.
	_ = scoped.CreateComment(context.Background(), 1, "body")
	if c := log.take(t); c.binding != store.TrackerBindingBuiltin || c.op != OpCreateComment || !c.ok {
		t.Errorf("rescoped call observed %+v, want {builtin comment true}", c)
	}

	// A backend without the seam stays wrapped, unchanged.
	plain := &observed{t: failingTracker{}, binding: store.TrackerBindingForge, obs: log.observe}
	if got := plain.ForRun("run_2"); got != Tracker(plain) {
		t.Errorf("ForRun on a non-RunScoper backend returned %T, want the wrapper itself", got)
	}
}

// TestNoObserverReturnsBackendUnwrapped pins the compatibility contract:
// without SetObserver the registry hands back the bare backend (existing
// callers type-assert the concrete trackers).
func TestNoObserverReturnsBackendUnwrapped(t *testing.T) {
	f := newRegistryFixture(t)
	trk, err := f.reg.TrackerFor(context.Background(),
		store.Repo{ID: "repo_plain", TrackerBinding: store.TrackerBindingBuiltin})
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	if _, ok := trk.(fakeBuiltin); !ok {
		t.Fatalf("TrackerFor without observer returned %T, want the bare backend", trk)
	}
}
