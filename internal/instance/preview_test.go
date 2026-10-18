package instance

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
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

// The manual Stop is one of the run-end chokepoint's sites (issue #105):
// right after the run's tokens are deleted, the run's preview exposures are
// released, so the name frees up and its tunnel hears "run-ended".
func TestStop_closesRunExposures(t *testing.T) {
	f := newFixture(t)
	closer := &recordingCloser{}
	f.svc.exposures = closer
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := closer.closed(); len(got) != 0 {
		t.Fatalf("exposures closed before Stop: %v", got)
	}
	if _, err := f.svc.Stop(t.Context(), run.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := closer.closed(); !slices.Equal(got, []string{run.ID}) {
		t.Errorf("exposures closed by Stop = %v, want [%s]", got, run.ID)
	}
}

// StopAll (repo delete) ends each manual run through the same teardown, so it
// releases each run's exposures too.
func TestStopAll_closesRunExposures(t *testing.T) {
	f := newFixture(t)
	closer := &recordingCloser{}
	f.svc.exposures = closer
	a, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "a"})
	if err != nil {
		t.Fatalf("Start a: %v", err)
	}
	b, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "b"})
	if err != nil {
		t.Fatalf("Start b: %v", err)
	}
	if _, err := f.svc.StopAll(t.Context(), f.repo.ID); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	got := closer.closed()
	slices.Sort(got)
	want := []string{a.ID, b.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("exposures closed by StopAll = %v, want %v", got, want)
	}
}

// A lab without a preview listener (Exposures nil) stops exactly as before:
// the nil-safe helper makes the chokepoint a no-op, never a panic.
func TestStop_noPreviewListener(t *testing.T) {
	f := newFixture(t)
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := f.svc.Stop(t.Context(), run.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Options.Preview reaches the seeder (issue #105): a lab with a preview
// listener seeds every run's worktree notes with the Previews section and the
// real URL shape; a lab without one seeds none.
func TestLaunch_previewSectionFollowsListener(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		f := newFixture(t)
		f.svc.previewRef = &seeder.PreviewRef{Domain: "lab.internal", Port: 8081}
		run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		local := contextFile(t, run.WorktreePath)
		for _, want := range []string{"## Previews", "`https://<name>.lab.internal:8081`"} {
			if !strings.Contains(local, want) {
				t.Errorf("context file lacks %q:\n%s", want, local)
			}
		}
	})
	t.Run("unconfigured", func(t *testing.T) {
		f := newFixture(t)
		run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if local := contextFile(t, run.WorktreePath); strings.Contains(local, "## Previews") {
			t.Errorf("context file of a lab without a preview listener carries a Previews section:\n%s", local)
		}
	})
}

// New carries both Options fields through to the service.
func TestNew_carriesPreviewWiring(t *testing.T) {
	f := newFixture(t)
	ref := &seeder.PreviewRef{Domain: "lab.internal", Port: 80}
	closer := &recordingCloser{}
	svc, err := New(Options{
		Store: f.st, Git: f.svc.git, Runner: f.runner, Providers: f.svc.providers,
		Vault: f.svc.vault, Materializer: f.svc.mat, Homes: f.homes, Guard: f.guard,
		Bus: f.bus, ReposDir: f.reposDir, WorktreeRoot: f.worktreeRoot,
		Preview: ref, Exposures: closer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.previewRef != ref {
		t.Errorf("previewRef = %v, want the Options ref", svc.previewRef)
	}
	if svc.exposures != closer {
		t.Errorf("exposures = %v, want the Options closer", svc.exposures)
	}
}
