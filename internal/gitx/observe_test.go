package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// fetchLog collects what the fetch observer was told.
type fetchLog struct {
	calls []fetchCall
}

type fetchCall struct {
	attr FetchAttribution
	err  error
}

func (l *fetchLog) observe(a FetchAttribution, err error) {
	l.calls = append(l.calls, fetchCall{a, err})
}

// take returns the calls observed since the last take.
func (l *fetchLog) take() []fetchCall {
	calls := l.calls
	l.calls = nil
	return calls
}

// newObservedFixture is a real bare clone of a real origin, on an engine
// whose fetch observer records into the returned log.
func newObservedFixture(t *testing.T) (f *wtFixture, log *fetchLog) {
	t.Helper()
	f = newWtFixture(t)
	log = &fetchLog{}
	f.eng.SetFetchObserver(log.observe)
	return f, log
}

// A fetch reports its outcome only when its caller attributed it — and that
// is how the reconcile sweep's credential-less fetch, which fails on every
// private remote by design, stays out of the readiness records.
func TestFetchObserver_OnlyAttributedFetchesAreReported(t *testing.T) {
	f, log := newObservedFixture(t)
	ctx := t.Context()
	attr := FetchAttribution{RepoID: "repo_1", Credential: "cred_1@2026-07-01T12:00:00.000Z"}

	// Unattributed (the sweep's shape): fetched, not reported.
	if err := f.eng.Fetch(ctx, f.bare, f.env); err != nil {
		t.Fatalf("unattributed Fetch: %v", err)
	}
	if calls := log.take(); len(calls) != 0 {
		t.Fatalf("an unattributed fetch was reported: %+v", calls)
	}

	// Attributed: one report, success, carrying the attribution verbatim.
	if err := f.eng.Fetch(AttributeFetch(ctx, attr), f.bare, f.env); err != nil {
		t.Fatalf("attributed Fetch: %v", err)
	}
	calls := log.take()
	if len(calls) != 1 || calls[0].err != nil || calls[0].attr != attr {
		t.Fatalf("attributed fetch reported %+v, want one success for %+v", calls, attr)
	}

	// The remote goes away (the stand-in for a rejected credential: either
	// way `git fetch origin` exits non-zero). The failure is reported with
	// git's own words — and exactly once, though Fetch runs two git commands.
	if err := os.RemoveAll(f.origin); err != nil {
		t.Fatal(err)
	}
	err := f.eng.Fetch(AttributeFetch(ctx, attr), f.bare, f.env)
	if err == nil {
		t.Fatal("Fetch against a vanished origin succeeded")
	}
	calls = log.take()
	if len(calls) != 1 || calls[0].attr != attr {
		t.Fatalf("failed fetch reported %+v, want exactly one call for %+v", calls, attr)
	}
	if calls[0].err == nil || calls[0].err.Error() != err.Error() {
		t.Fatalf("reported error = %v, want the fetch's own error %v", calls[0].err, err)
	}
	if !strings.Contains(calls[0].err.Error(), "git fetch origin") || !strings.Contains(calls[0].err.Error(), "fatal:") {
		t.Fatalf("reported error lacks git's verdict: %v", calls[0].err)
	}

	// The same failing fetch, unattributed, is still silent.
	if err := f.eng.Fetch(ctx, f.bare, f.env); err == nil {
		t.Fatal("unattributed Fetch against a vanished origin succeeded")
	}
	if calls := log.take(); len(calls) != 0 {
		t.Fatalf("an unattributed failing fetch was reported: %+v", calls)
	}
}

// Every fetch-carrying operation reports through the one primitive: the
// spawn's worktree fetch, the lander's adopt fetch, the import snapshot's and
// the pull's all reach the observer under their caller's attribution.
func TestFetchObserver_EveryFetchCarryingOpReports(t *testing.T) {
	f, log := newObservedFixture(t)
	attr := FetchAttribution{RepoID: "repo_1", Credential: "none", OnBehalfOf: "repo_2"}
	ctx := AttributeFetch(t.Context(), attr)
	wantOne := func(op string) {
		t.Helper()
		calls := log.take()
		if len(calls) != 1 || calls[0].err != nil || calls[0].attr != attr {
			t.Fatalf("%s reported %+v, want one successful fetch for %+v", op, calls, attr)
		}
	}

	wt := filepath.Join(f.wtRoot, "wt-new")
	if err := f.eng.AddWorktree(ctx, f.bare, wt, "lab/new", "main", f.env); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	wantOne("AddWorktree")

	if err := f.eng.AddWorktreeExisting(ctx, f.bare, filepath.Join(f.wtRoot, "wt-adopt"), "main", f.env); err != nil {
		t.Fatalf("AddWorktreeExisting: %v", err)
	}
	wantOne("AddWorktreeExisting")

	if _, err := f.eng.MaterializeSnapshot(ctx, f.bare, filepath.Join(f.wtRoot, "snapshot"), "main", f.env); err != nil {
		t.Fatalf("MaterializeSnapshot: %v", err)
	}
	wantOne("MaterializeSnapshot")

	if _, err := f.eng.PullBase(ctx, f.bare, wt, "main", "Op", "op@example.com", f.env); err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	wantOne("PullBase")

	// The attribution survives context.WithoutCancel — the pull and merge
	// services detach from their caller before they fetch.
	if err := f.eng.Fetch(context.WithoutCancel(ctx), f.bare, f.env); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	wantOne("Fetch under WithoutCancel")
}

// A fetch that died with its caller's context is not a failed fetch: nothing
// is reported, so a dropped request can never mark a repo's credential as
// broken.
func TestFetchObserver_CancelledFetchIsNotReported(t *testing.T) {
	f, log := newObservedFixture(t)
	attr := FetchAttribution{RepoID: "repo_1", Credential: "none"}

	ctx, cancel := context.WithCancel(AttributeFetch(t.Context(), attr))
	cancel()
	if err := f.eng.Fetch(ctx, f.bare, f.env); err == nil {
		t.Fatal("Fetch under a cancelled context succeeded")
	}
	if calls := log.take(); len(calls) != 0 {
		t.Fatalf("a cancelled fetch was reported: %+v", calls)
	}

	// The caller's own deadline is the caller going away too.
	ctx, cancel = context.WithDeadline(AttributeFetch(t.Context(), attr), time.Now().Add(-time.Second))
	defer cancel()
	if err := f.eng.Fetch(ctx, f.bare, f.env); err == nil {
		t.Fatal("Fetch past its caller's deadline succeeded")
	}
	if calls := log.take(); len(calls) != 0 {
		t.Fatalf("a fetch past its caller's deadline was reported: %+v", calls)
	}
}

// The engine's OWN timeout is a real failure of the remote — the caller is
// still there, waiting — and is reported.
func TestFetchObserver_EngineTimeoutIsReported(t *testing.T) {
	f, log := newObservedFixture(t)
	f.eng.timeout = time.Nanosecond // every git subprocess now exceeds the bound
	attr := FetchAttribution{RepoID: "repo_1", Credential: "none"}

	err := f.eng.Fetch(AttributeFetch(t.Context(), attr), f.bare, f.env)
	if err == nil || !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("Fetch = %v, want the engine's timeout error", err)
	}
	calls := log.take()
	if len(calls) != 1 || calls[0].err == nil || !strings.Contains(calls[0].err.Error(), "timed out after") {
		t.Fatalf("engine timeout reported %+v, want one timeout failure", calls)
	}
}

// A completed clone is reported as a successful fetch; a failed or cancelled
// one is not (the repo's clone_status already says so).
func TestFetchObserver_Clone(t *testing.T) {
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeOrigin(t, home, "main", 2)
	env := testutil.HermeticGitEnv(home)
	eng := New("git")
	log := &fetchLog{}
	eng.SetFetchObserver(log.observe)
	attr := FetchAttribution{RepoID: "repo_1", Credential: "none"}
	ctx := AttributeFetch(t.Context(), attr)

	if err := eng.CloneBare(ctx, "file://"+origin, filepath.Join(t.TempDir(), "ok.git"), env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	if calls := log.take(); len(calls) != 1 || calls[0].err != nil || calls[0].attr != attr {
		t.Fatalf("completed clone reported %+v, want one success", calls)
	}

	if err := eng.CloneBare(ctx, "file://"+filepath.Join(home, "no-such-origin"), filepath.Join(t.TempDir(), "bad.git"), env, nil); err == nil {
		t.Fatal("clone of a missing origin succeeded")
	}
	if calls := log.take(); len(calls) != 0 {
		t.Fatalf("a failed clone was reported: %+v", calls)
	}

	// Unattributed clone: silent.
	if err := eng.CloneBare(t.Context(), "file://"+origin, filepath.Join(t.TempDir(), "plain.git"), env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	if calls := log.take(); len(calls) != 0 {
		t.Fatalf("an unattributed clone was reported: %+v", calls)
	}
}

// An engine without an observer fetches exactly as before.
func TestFetchObserver_NoObserver(t *testing.T) {
	f := newWtFixture(t)
	ctx := AttributeFetch(t.Context(), FetchAttribution{RepoID: "repo_1"})
	if err := f.eng.Fetch(ctx, f.bare, f.env); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}
