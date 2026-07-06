package reposvc

// M8 observability: real clone jobs (file:// origins, hermetic git) must
// move lab_clone_jobs_total at their terminal status and return
// lab_clones_in_flight to zero.

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// metricValue reads one counter/gauge series from m's registry (0 when the
// series does not exist yet).
func metricValue(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
	metric:
		for _, mt := range fam.GetMetric() {
			got := map[string]string{}
			for _, lp := range mt.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metric
				}
			}
			switch fam.GetType() {
			case dto.MetricType_COUNTER:
				return mt.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				return mt.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func TestCloneJobMetrics(t *testing.T) {
	e := newTestEnv(t)
	m := metrics.New()
	e.svc.metrics = m

	// Successful clone → result "ready".
	origin := makeOrigin(t, e.home, "main", 1)
	repo, err := e.svc.Add(context.Background(), AddParams{RemoteURL: "file://" + origin})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	waitFor(t, "ready clone job counted", func() bool {
		return metricValue(t, m, "lab_clone_jobs_total", map[string]string{"result": "ready"}) == 1
	})

	// Failing clone (missing origin) → result "error".
	bad, err := e.svc.Add(context.Background(), AddParams{RemoteURL: "file:///nonexistent/void.git", Name: "void"})
	if err != nil {
		t.Fatalf("Add bad: %v", err)
	}
	e.waitCloneStatus(t, bad.ID, store.CloneStatusError)
	waitFor(t, "error clone job counted", func() bool {
		return metricValue(t, m, "lab_clone_jobs_total", map[string]string{"result": "error"}) == 1
	})

	// Both jobs are done: the in-flight gauge is back to zero.
	waitFor(t, "clones in flight back to 0", func() bool {
		return metricValue(t, m, "lab_clones_in_flight", nil) == 0
	})
}

// TestCloneOutcome pins the metric classification table — in particular that
// cancellation wins over success (regression: a force-delete cancelling the
// job just after the clone succeeded still counted ready).
func TestCloneOutcome(t *testing.T) {
	cancelled := context.Canceled
	failed := store.ErrNotFound
	cases := []struct {
		name        string
		err, ctxErr error
		result      string
		counted     bool
	}{
		{"success", nil, nil, store.CloneStatusReady, true},
		{"failure", failed, nil, store.CloneStatusError, true},
		{"cancelled after failure", failed, cancelled, "", false},
		{"cancelled after success", nil, cancelled, "", false},
	}
	for _, tc := range cases {
		result, counted := cloneOutcome(tc.err, tc.ctxErr)
		if result != tc.result || counted != tc.counted {
			t.Errorf("%s: cloneOutcome = (%q, %v), want (%q, %v)", tc.name, result, counted, tc.result, tc.counted)
		}
	}
}
