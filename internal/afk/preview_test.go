package afk

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// recordingCloser is a preview.RunCloser fake: it records every run id the
// run-end chokepoint released (issue #105), and runs onClose (when set) at
// the moment of the call so a test can inspect what had happened by then.
type recordingCloser struct {
	mu      sync.Mutex
	runs    []string
	onClose func(runID string)
}

func (c *recordingCloser) CloseRun(runID string) {
	if c.onClose != nil {
		c.onClose(runID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs = append(c.runs, runID)
}

func (c *recordingCloser) closed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.runs)
}

// The reaper is one of the run-end chokepoint's sites (issue #105): a
// terminal outcome releases the run's preview exposures, and does so AFTER
// the run's tokens are gone — so a labctl told "run-ended" can never race a
// re-register through a still-valid token.
func TestReap_closesRunExposuresAfterTokenDelete(t *testing.T) {
	f := newFixture(t)
	closer := &recordingCloser{}
	f.svc.exposures = closer
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := f.runner.Session(run.SessionName)
	token := envValue(sess.ExtraEnv, "LAB_TOKEN")
	tokenGone := false
	closer.onClose = func(string) {
		_, err := f.st.RunTokenByHash(t.Context(), ids.HashToken(token))
		tokenGone = errors.Is(err, store.ErrNotFound)
	}

	f.trk.addPull("afk/7", tracker.PullMerged)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	if got := closer.closed(); !slices.Equal(got, []string{run.ID}) {
		t.Fatalf("exposures closed by the reap = %v, want [%s]", got, run.ID)
	}
	if !tokenGone {
		t.Error("exposures were closed while the run's token still authenticated")
	}
}

// A reaper tick over a run still in progress releases nothing.
func TestReap_inProgressKeepsExposures(t *testing.T) {
	f := newFixture(t)
	closer := &recordingCloser{}
	f.svc.exposures = closer
	f.trk.setReady(7)
	if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if got := closer.closed(); len(got) != 0 {
		t.Errorf("exposures closed for an in-progress run: %v", got)
	}
}

// The neutral AFK Stop is the chokepoint's other afk site.
func TestStopAFK_closesRunExposures(t *testing.T) {
	f := newFixture(t)
	closer := &recordingCloser{}
	f.svc.exposures = closer
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if err := f.svc.StopAFK(t.Context(), run.SessionName); err != nil {
		t.Fatalf("StopAFK: %v", err)
	}
	if got := closer.closed(); !slices.Equal(got, []string{run.ID}) {
		t.Errorf("exposures closed by StopAFK = %v, want [%s]", got, run.ID)
	}
}

// With no preview listener (Exposures nil) both sites behave as before.
func TestStopAFK_noPreviewListener(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if err := f.svc.StopAFK(t.Context(), run.SessionName); err != nil {
		t.Fatalf("StopAFK: %v", err)
	}
}
