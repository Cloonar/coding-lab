package afk

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// recordingCmdRunner is the podmanx.CmdRunner fake: records every call,
// answers scripted output keyed by the space-joined command line.
type recordingCmdRunner struct {
	mu     sync.Mutex
	calls  [][]string
	script map[string]string
}

func newRecordingCmdRunner() *recordingCmdRunner {
	return &recordingCmdRunner{script: map[string]string{}}
}

func (r *recordingCmdRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	return []byte(r.script[strings.Join(argv, " ")]), nil
}

func (r *recordingCmdRunner) rmCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, c := range r.calls {
		if len(c) > 1 && c[1] == "rm" {
			out = append(out, append([]string(nil), c...))
		}
	}
	return out
}

// wireContainer configures the engine's container backstop seam
// (internal-package field pokes — the values cmd/lab passes through
// afk.Options when container config is present).
func (f *fixture) wireContainer() *recordingCmdRunner {
	rec := newRecordingCmdRunner()
	f.svc.podmanBin = "podman-test"
	f.svc.podmanRun = rec.run
	f.svc.containerPreflight = func() (podmanx.Result, bool) { return podmanx.Result{}, true }
	return rec
}

func wantRm(session string) []string {
	return []string{"podman-test", "rm", "--force", "--ignore", "--time", "5", podmanx.ContainerName(session)}
}

// The neutral Stop follows its tmux kill with the `podman rm` backstop,
// addressed by the deterministic container name (issue #205).
func TestStopAFK_containerBackstop(t *testing.T) {
	f := newFixture(t)
	rec := f.wireContainer()
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}

	if err := f.svc.StopAFK(t.Context(), run.SessionName); err != nil {
		t.Fatalf("StopAFK: %v", err)
	}
	if got, want := rec.rmCalls(), wantRm(run.SessionName); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("rm calls = %q, want exactly [%q]", got, want)
	}
}

// A reaper terminal outcome runs the same backstop — even for a dead session,
// where the container (under a SIGHUP-deaf CLI) can outlive its pane.
func TestReap_containerBackstop(t *testing.T) {
	f := newFixture(t)
	rec := f.wireContainer()
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}

	f.runner.Kill(run.SessionName) // crashed → reaps as death
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("outcome = %q, want death (so reapRun ran)", got.Outcome)
	}
	if got, want := rec.rmCalls(), wantRm(run.SessionName); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("rm calls = %q, want exactly [%q]", got, want)
	}
}

// Without container wiring the engine's stops never shell out to podman.
func TestStopAFK_noContainerWiringNoPodman(t *testing.T) {
	f := newFixture(t)
	rec := newRecordingCmdRunner()
	f.svc.podmanRun = rec.run // recorder present, but podmanBin/preflight absent
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if err := f.svc.StopAFK(t.Context(), run.SessionName); err != nil {
		t.Fatalf("StopAFK: %v", err)
	}
	if calls := rec.rmCalls(); len(calls) != 0 {
		t.Errorf("rm calls on an unwired server = %q, want none", calls)
	}
}
