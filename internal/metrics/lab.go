package metrics

// Lab-domain collectors (brief §8 /metrics, §13 production bar; M8). The
// owning services report through the methods below at their single
// chokepoints: the AFK reaper/Stop terminal-outcome path, the tracker
// registry seam, and the reposvc clone job. Every label set is a fixed
// vocabulary — never run ids, repo ids, or free text — so cardinality stays
// bounded for the life of the process.
//
// All report methods are safe on a nil *Metrics: a service built without
// metrics (tests, degraded wiring) calls them unconditionally and they
// no-op. lab_instances_active deliberately is NOT a maintained gauge: it is
// a custom collector reading the live tmux+active-runs view at scrape time
// (RegisterInstances), so it can never drift from the truth the reaper and
// Stop act on — both source queries (one indexed runs scan, one tmux
// list-sessions) are cheap enough per scrape.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// afkDurationBuckets spans the AFK run lifetime range: seconds-fast deaths
// through multi-hour budgets (the default budget is 120 minutes — the
// histogram must resolve both sides of that boundary).
var afkDurationBuckets = []float64{60, 300, 600, 1200, 1800, 3600, 7200, 10800, 14400}

// registerLab builds and registers the lab-domain collectors; called once
// from New.
func (m *Metrics) registerLab() {
	m.afkRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lab_afk_runs_total",
		Help: "Terminal AFK run outcomes, by outcome (success|death|timeout|stopped) and run kind (afk_manual|afk_auto|lander).",
	}, []string{"outcome", "kind"})
	m.afkRunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "lab_afk_run_duration_seconds",
		Help:    "AFK run duration from started_at to its terminal outcome, by outcome.",
		Buckets: afkDurationBuckets,
	}, []string{"outcome"})
	m.trackerRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lab_tracker_requests_total",
		Help: "Tracker calls resolved through the registry, by binding (forge|builtin), operation and result (ok|error).",
	}, []string{"binding", "op", "result"})
	m.cloneJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lab_clone_jobs_total",
		Help: "Finished clone jobs, by result (ready|error). Jobs cancelled by a forced repo delete count neither.",
	}, []string{"result"})
	m.clonesInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lab_clones_in_flight",
		Help: "Clone jobs currently running.",
	})
	m.reg.MustRegister(m.afkRuns, m.afkRunDuration, m.trackerRequests, m.cloneJobs, m.clonesInFlight)
}

// AFKRunEnded records one terminal AFK run outcome at the reaper/stop
// chokepoint: the outcome counter and the started_at→ended_at duration
// histogram move together. kind is the run's kind (afk_manual|afk_auto|
// lander — every reaper-owned kind), outcome the runs.outcome vocabulary
// (success|death|timeout|stopped).
func (m *Metrics) AFKRunEnded(kind, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.afkRuns.WithLabelValues(outcome, kind).Inc()
	m.afkRunDuration.WithLabelValues(outcome).Observe(duration.Seconds())
}

// TrackerRequest counts one tracker call reported by the registry's observer
// seam. The signature matches tracker.Observer exactly, so cmd/lab wires
// `reg.SetObserver(m.TrackerRequest)` — only (binding, op, ok) ever crosses
// the seam, never error text or token bytes.
func (m *Metrics) TrackerRequest(binding, op string, ok bool) {
	if m == nil {
		return
	}
	result := "ok"
	if !ok {
		result = "error"
	}
	m.trackerRequests.WithLabelValues(binding, op, result).Inc()
}

// CloneStarted / CloneFinished bracket one clone job's lifetime for
// lab_clones_in_flight. reposvc calls Started when the job registers and
// Finished when the job goroutine exits, whatever its result.
func (m *Metrics) CloneStarted() {
	if m == nil {
		return
	}
	m.clonesInFlight.Inc()
}

// CloneFinished is CloneStarted's counterpart; see there.
func (m *Metrics) CloneFinished() {
	if m == nil {
		return
	}
	m.clonesInFlight.Dec()
}

// CloneJob counts a clone job's terminal result: "ready" or "error"
// (store.CloneStatusReady/CloneStatusError — the same vocabulary the repo
// row records). Cancelled jobs never reach here.
func (m *Metrics) CloneJob(result string) {
	if m == nil {
		return
	}
	m.cloneJobs.WithLabelValues(result).Inc()
}

// InstanceCounts is one scrape-time snapshot of the live-instance view,
// bucketed by run kind. instance.Service.LiveCounts produces it.
type InstanceCounts struct {
	Manual    int
	AFKManual int
	AFKAuto   int
	Lander    int
}

// instancesScrapeTimeout bounds the snapshot query a scrape triggers — a
// hung store or tmux must not wedge /metrics.
const instancesScrapeTimeout = 5 * time.Second

// RegisterInstances registers the lab_instances_active gauge, evaluated at
// scrape time from snapshot (design choice noted in the package comment:
// scrape-time query over a maintained gauge, so the value can never drift).
// A snapshot error yields no lab_instances_active series for that scrape —
// the endpoint itself stays 200 so the other collectors keep flowing;
// absence of the series is the failure signal.
func (m *Metrics) RegisterInstances(snapshot func(ctx context.Context) (InstanceCounts, error)) {
	m.reg.MustRegister(&instancesCollector{
		desc: prometheus.NewDesc(
			"lab_instances_active",
			"Active runs whose tmux session is live, by kind (manual|afk_manual|afk_auto|lander). Read at scrape time.",
			[]string{"kind"}, nil,
		),
		snapshot: snapshot,
	})
}

// instancesCollector adapts a snapshot func into a prometheus.Collector.
type instancesCollector struct {
	desc     *prometheus.Desc
	snapshot func(ctx context.Context) (InstanceCounts, error)
}

func (c *instancesCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *instancesCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), instancesScrapeTimeout)
	defer cancel()
	counts, err := c.snapshot(ctx)
	if err != nil {
		return // series absent this scrape; /metrics itself must stay healthy
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts.Manual), "manual")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts.AFKManual), "afk_manual")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts.AFKAuto), "afk_auto")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts.Lander), "lander")
}
