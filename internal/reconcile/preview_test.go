package reconcile

import (
	"slices"
	"sync"
	"testing"
)

// recordingCloser is a preview.RunCloser fake: it records every run id the
// run-end chokepoint released (issue #105).
type recordingCloser struct {
	mu   sync.Mutex
	runs []string
}

func (c *recordingCloser) CloseRun(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs = append(c.runs, runID)
}

func (c *recordingCloser) closed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.runs)
}

// The Discard kill is one of the run-end chokepoint's sites (issue #105): the
// killed run's tokens are deleted, and right after, its preview exposures are
// released.
func TestDiscard_closesRunExposures(t *testing.T) {
	f := newRecFixture(t)
	closer := &recordingCloser{}
	f.svc.exposures = closer
	f.addWorktree("lab/x-20260608-1530")
	name := "proj~x-20260608-1530"
	f.runner.AddLive(name)
	run := f.activeRun(name, "lab/x-20260608-1530", nil)

	if err := f.svc.Discard(t.Context(), f.repo.ID, "lab/x-20260608-1530"); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got := closer.closed(); !slices.Equal(got, []string{run.ID}) {
		t.Errorf("exposures closed by Discard = %v, want [%s]", got, run.ID)
	}
}

// A Discard on a lab without a preview listener (Exposures nil) behaves as
// before: the nil-safe helper is a no-op, never a panic.
func TestDiscard_noPreviewListener(t *testing.T) {
	f := newRecFixture(t)
	f.addWorktree("lab/x-20260608-1530")
	name := "proj~x-20260608-1530"
	f.runner.AddLive(name)
	f.activeRun(name, "lab/x-20260608-1530", nil)
	if err := f.svc.Discard(t.Context(), f.repo.ID, "lab/x-20260608-1530"); err != nil {
		t.Fatalf("Discard: %v", err)
	}
}

// The dead-session sweep ends a run whose session vanished, and so releases
// its previews too (issue #105) — otherwise a name would stay reserved, and
// refused to every other run, until lab restarts. A live run keeps its own.
func TestSweepDeadSessions_closesRunExposures(t *testing.T) {
	f := newRecFixture(t)
	closer := &recordingCloser{}
	f.svc.exposures = closer
	f.activeRun("proj~live-20260608-1530", "lab/live-20260608-1530", nil)
	dead := f.activeRun("proj~dead-20260608-1530", "lab/dead-20260608-1530", nil)
	f.runner.AddLive("proj~live-20260608-1530")

	f.svc.sweepDeadSessions(t.Context())

	if got := closer.closed(); !slices.Equal(got, []string{dead.ID}) {
		t.Errorf("exposures closed by the dead-session sweep = %v, want [%s]", got, dead.ID)
	}
}
