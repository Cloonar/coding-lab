package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gaugeValue reads a gauge (with optional label match) from the registry;
// found=false when no matching series exists.
func gaugeValue(t *testing.T, m *Metrics, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := m.reg.Gather()
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
			return mt.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// histogramCountSum reads a histogram series' sample count and sum.
func histogramCountSum(t *testing.T, m *Metrics, name string, labels map[string]string) (uint64, float64) {
	t.Helper()
	families, err := m.reg.Gather()
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
			h := mt.GetHistogram()
			return h.GetSampleCount(), h.GetSampleSum()
		}
	}
	return 0, 0
}

func TestAFKRunEndedMovesCounterAndHistogram(t *testing.T) {
	m := New()
	m.AFKRunEnded("afk_auto", "timeout", 90*time.Minute)
	m.AFKRunEnded("afk_manual", "stopped", 30*time.Second)

	if got := counterValue(t, m, "lab_afk_runs_total", map[string]string{
		"outcome": "timeout", "kind": "afk_auto",
	}); got != 1 {
		t.Errorf("lab_afk_runs_total{timeout,afk_auto} = %v, want 1", got)
	}
	if got := counterValue(t, m, "lab_afk_runs_total", map[string]string{
		"outcome": "stopped", "kind": "afk_manual",
	}); got != 1 {
		t.Errorf("lab_afk_runs_total{stopped,afk_manual} = %v, want 1", got)
	}
	count, sum := histogramCountSum(t, m, "lab_afk_run_duration_seconds", map[string]string{"outcome": "timeout"})
	if count != 1 || sum != (90*time.Minute).Seconds() {
		t.Errorf("duration{timeout} count/sum = %d/%v, want 1/%v", count, sum, (90 * time.Minute).Seconds())
	}
}

func TestTrackerRequestResultLabels(t *testing.T) {
	m := New()
	m.TrackerRequest("forge", "pulls", true)
	m.TrackerRequest("forge", "pulls", true)
	m.TrackerRequest("builtin", "create_pull", false)

	if got := counterValue(t, m, "lab_tracker_requests_total", map[string]string{
		"binding": "forge", "op": "pulls", "result": "ok",
	}); got != 2 {
		t.Errorf("tracker{forge,pulls,ok} = %v, want 2", got)
	}
	if got := counterValue(t, m, "lab_tracker_requests_total", map[string]string{
		"binding": "builtin", "op": "create_pull", "result": "error",
	}); got != 1 {
		t.Errorf("tracker{builtin,create_pull,error} = %v, want 1", got)
	}
}

func TestCloneMetrics(t *testing.T) {
	m := New()
	m.CloneStarted()
	m.CloneStarted()
	if got, ok := gaugeValue(t, m, "lab_clones_in_flight", nil); !ok || got != 2 {
		t.Errorf("lab_clones_in_flight = %v (found %v), want 2", got, ok)
	}
	m.CloneFinished()
	m.CloneJob("ready")
	m.CloneFinished()
	m.CloneJob("error")

	if got, ok := gaugeValue(t, m, "lab_clones_in_flight", nil); !ok || got != 0 {
		t.Errorf("lab_clones_in_flight = %v (found %v), want 0 after both jobs finished", got, ok)
	}
	for _, result := range []string{"ready", "error"} {
		if got := counterValue(t, m, "lab_clone_jobs_total", map[string]string{"result": result}); got != 1 {
			t.Errorf("lab_clone_jobs_total{%s} = %v, want 1", result, got)
		}
	}
}

func TestInstancesCollectorReadsSnapshotAtScrape(t *testing.T) {
	m := New()
	counts := InstanceCounts{Manual: 2, AFKManual: 1, AFKAuto: 3}
	m.RegisterInstances(func(context.Context) (InstanceCounts, error) { return counts, nil })

	for kind, want := range map[string]float64{"manual": 2, "afk_manual": 1, "afk_auto": 3} {
		if got, ok := gaugeValue(t, m, "lab_instances_active", map[string]string{"kind": kind}); !ok || got != want {
			t.Errorf("lab_instances_active{%s} = %v (found %v), want %v", kind, got, ok, want)
		}
	}

	// Scrape-time semantics: a snapshot change is visible on the next
	// gather with no Set call anywhere.
	counts.AFKAuto = 0
	if got, _ := gaugeValue(t, m, "lab_instances_active", map[string]string{"kind": "afk_auto"}); got != 0 {
		t.Errorf("lab_instances_active{afk_auto} = %v after snapshot change, want 0", got)
	}
}

func TestInstancesCollectorErrorKeepsMetricsServing(t *testing.T) {
	m := New()
	m.RegisterInstances(func(context.Context) (InstanceCounts, error) {
		return InstanceCounts{}, errors.New("db down")
	})

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d with failing snapshot, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "lab_instances_active{") {
		t.Error("failing snapshot still produced lab_instances_active series")
	}
	if !strings.Contains(string(body), "go_goroutines") {
		t.Error("other collectors missing from /metrics while the snapshot fails")
	}
}

// TestNilMetricsReportsAreSafe pins the nil-receiver contract the services
// rely on: a service built without metrics calls every report method
// unconditionally.
func TestNilMetricsReportsAreSafe(t *testing.T) {
	var m *Metrics
	m.AFKRunEnded("afk_auto", "death", time.Minute)
	m.TrackerRequest("forge", "pulls", true)
	m.CloneStarted()
	m.CloneFinished()
	m.CloneJob("ready")
}

// TestMetricsHandlerCapsConcurrentScrapes pins the unauthenticated-endpoint
// guard: with the instances collector blocked mid-snapshot, at most
// maxScrapesInFlight scrapes run concurrently and the overflow gets an
// immediate 503 — a scrape flood cannot pile up unbounded snapshot work
// (one runs query + one tmux fork each) behind the blocked ones.
func TestMetricsHandlerCapsConcurrentScrapes(t *testing.T) {
	m := New()
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release) // failure path: unblock before srv.Close waits
		}
	}()
	m.RegisterInstances(func(ctx context.Context) (InstanceCounts, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return InstanceCounts{}, nil
	})
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	const total = maxScrapesInFlight + 3
	codes := make(chan int, total)
	for range total {
		go func() {
			resp, err := http.Get(srv.URL)
			if err != nil {
				codes <- -1
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}

	// Whatever the arrival order, exactly total-maxScrapesInFlight requests
	// bounce: an acquired slot blocks in the snapshot until release, so any
	// response arriving before release must be a 503.
	for i := 0; i < total-maxScrapesInFlight; i++ {
		if code := <-codes; code != http.StatusServiceUnavailable {
			t.Fatalf("overflow scrape %d: code = %d, want 503", i, code)
		}
	}
	close(release)
	released = true
	for i := 0; i < maxScrapesInFlight; i++ {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("blocked scrape %d: code = %d, want 200 after release", i, code)
		}
	}
}
