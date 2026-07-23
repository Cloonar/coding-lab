package reconcile

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
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

func (r *recordingCmdRunner) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// wireContainer configures the fixture service's container backstop seam
// (internal-package field pokes — the values cmd/lab passes through
// reconcile.Options when container config is present).
func (f *recFixture) wireContainer() *recordingCmdRunner {
	rec := newRecordingCmdRunner()
	f.svc.podmanBin = "podman-test"
	f.svc.podmanRun = rec.run
	f.svc.containerPreflight = func() (podmanx.Result, bool) { return podmanx.Result{}, true }
	return rec
}

// The startup orphan sweep removes exactly the labrun- containers no live
// session accounts for: two listed, one owned by a live session → only the
// other is removed. Covers hard server-host crashes and pane-kill races —
// normal stops never leave one (their own backstops rm at kill time).
func TestStartupReconcile_sweepsOrphanContainers(t *testing.T) {
	f := newRecFixture(t)
	rec := f.wireContainer()

	liveSession := "proj~afk-7"
	f.runner.AddLive(liveSession)
	ownedName := podmanx.ContainerName(liveSession)
	const orphanName = "labrun-proj.afk-9-abc123"
	rec.script["podman-test ps --all --filter name=labrun- --format {{.Names}}"] =
		ownedName + "\n" + orphanName + "\n"

	if err := f.svc.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}

	var removed [][]string
	for _, call := range rec.recorded() {
		if len(call) > 1 && call[1] == "rm" {
			removed = append(removed, call)
		}
	}
	want := []string{"podman-test", "rm", "--force", "--ignore", "--time", "5", orphanName}
	if len(removed) != 1 || !slices.Equal(removed[0], want) {
		t.Errorf("rm calls = %q, want exactly [%q] (the live session's container must survive)", removed, want)
	}
}

// Without container wiring the startup pass never shells out to podman —
// host-only deployments stay podman-free.
func TestStartupReconcile_noContainerWiringNoPodman(t *testing.T) {
	f := newRecFixture(t)
	rec := newRecordingCmdRunner()
	f.svc.podmanRun = rec.run // recorder present, but podmanBin/preflight absent

	if err := f.svc.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("podman calls on an unwired server = %q, want none", calls)
	}
}

// Discard kills the session, rm's its container (the same backstop every
// session kill carries), and wipes the run's per-run tree IMMEDIATELY — not
// left to the age-based sweep (issue #205 restored the immediacy a prior
// refactor lost).
func TestDiscard_containerBackstopAndHomeWipe(t *testing.T) {
	f := newRecFixture(t)
	rec := f.wireContainer()

	f.addWorktree("lab/x-20260608-1530")
	name := "proj~x-20260608-1530"
	f.runner.AddLive(name)
	run := f.activeRun(name, "lab/x-20260608-1530", nil)
	if _, err := f.homes.Materialize(run.ID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	if err := f.svc.Discard(t.Context(), f.repo.ID, "lab/x-20260608-1530"); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	want := []string{"podman-test", "rm", "--force", "--ignore", "--time", "5", podmanx.ContainerName(name)}
	calls := rec.recorded()
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Errorf("podman calls = %q, want exactly [%q]", calls, want)
	}
	if recDirExists(f.homes.HomePath(run.ID)) {
		t.Error("discard left the run's per-run tree — the wipe must be immediate, not sweep-deferred")
	}
}
