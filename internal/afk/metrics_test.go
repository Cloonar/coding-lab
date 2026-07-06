package afk

// M8 observability: drive real reaper/stop terminal outcomes through the
// engine fixture (fake tmux, real store/git) and scrape /metrics, asserting
// lab_afk_runs_total and lab_afk_run_duration_seconds moved with the pinned
// labels. The metrics handle is injected the same way cmd/lab wires it
// (afk.Options.Metrics).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// scrape fetches the /metrics exposition for m.
func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	return string(body)
}

func TestReap_metricsCountOutcomeAndDuration(t *testing.T) {
	f := newFixture(t)
	m := metrics.New()
	f.svc.metrics = m

	// A death: session crashes, reaped one minute after start.
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	f.runner.Kill(run.SessionName)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("outcome = %q, want death", got.Outcome)
	}

	// A success: next issue's run opens its PR, reaped 10 minutes in.
	f.trk.setReady(8)
	run2, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK #8: %v", err)
	}
	f.commitInWorktree(run2.WorktreePath)
	f.trk.addPull("afk/8", tracker.PullOpen)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(10*time.Minute))

	body := scrape(t, m)
	for _, want := range []string{
		`lab_afk_runs_total{kind="afk_manual",outcome="death"} 1`,
		`lab_afk_runs_total{kind="afk_manual",outcome="success"} 1`,
		// started_at→ended_at: reaped exactly 60s / 600s after the frozen start.
		`lab_afk_run_duration_seconds_count{outcome="death"} 1`,
		`lab_afk_run_duration_seconds_sum{outcome="death"} 60`,
		`lab_afk_run_duration_seconds_count{outcome="success"} 1`,
		`lab_afk_run_duration_seconds_sum{outcome="success"} 600`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

func TestStopAFK_metricsCountStoppedOutcome(t *testing.T) {
	f := newFixture(t)
	m := metrics.New()
	f.svc.metrics = m

	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	f.clock.Advance(5 * time.Minute)
	if err := f.svc.StopAFK(t.Context(), run.SessionName); err != nil {
		t.Fatalf("StopAFK: %v", err)
	}

	body := scrape(t, m)
	for _, want := range []string{
		`lab_afk_runs_total{kind="afk_manual",outcome="stopped"} 1`,
		`lab_afk_run_duration_seconds_count{outcome="stopped"} 1`,
		`lab_afk_run_duration_seconds_sum{outcome="stopped"} 300`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}

	// A second Stop is already-terminal (ErrNotFound) and must not double
	// count.
	if err := f.svc.StopAFK(t.Context(), run.SessionName); err == nil {
		t.Fatal("second StopAFK succeeded, want ErrNotFound")
	}
	if body := scrape(t, m); !strings.Contains(body, `lab_afk_runs_total{kind="afk_manual",outcome="stopped"} 1`) {
		t.Error("double Stop double-counted the stopped outcome")
	}
}
