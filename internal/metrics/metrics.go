// Package metrics owns lab's prometheus registry and the HTTP middleware
// that records per-route request counts and latencies (design §5: the
// middleware sits between logging and auth in the chain).
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics bundles the registry with lab's HTTP collectors and the
// lab-domain collectors (lab.go): AFK run outcomes/durations, tracker
// requests, clone jobs.
type Metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec

	// Lab-domain collectors, registered by registerLab (lab.go).
	afkRuns         *prometheus.CounterVec
	afkRunDuration  *prometheus.HistogramVec
	trackerRequests *prometheus.CounterVec
	cloneJobs       *prometheus.CounterVec
	clonesInFlight  prometheus.Gauge
}

// New builds a fresh registry with the Go/process collectors and lab's HTTP
// request collectors registered.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lab_http_requests_total",
			Help: "HTTP requests served, by route pattern, method and status code.",
		}, []string{"route", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "lab_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, by route pattern and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
	}
	reg.MustRegister(m.requests, m.duration)
	m.registerLab()
	return m
}

// Registry exposes the underlying registry for additional collectors and
// for test assertions (Gather).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// /metrics is unauthenticated (probes and scrapers hit it) and the
// instances collector does real work per scrape — a runs query and a tmux
// fork (lab.go). The cap turns a scrape flood into immediate 503s instead
// of unbounded concurrent forks; the timeout cuts off a scrape wedged on a
// hung Gather. Both are far above what one Prometheus needs.
const (
	maxScrapesInFlight = 3
	scrapeTimeout      = 10 * time.Second
)

// Handler serves the /metrics endpoint for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		MaxRequestsInFlight: maxScrapesInFlight,
		Timeout:             scrapeTimeout,
	})
}

// routeHolder carries the innermost ServeMux pattern from RecordPattern
// back to Middleware. It must travel via context because intervening
// middleware may clone the request (WithContext): a mux then stamps
// r.Pattern on the clone only, which the outer middleware never sees —
// context values survive the clone, request mutation does not.
type routeHolder struct{ pattern string }

type routeHolderKey struct{}

// RecordPattern wraps the innermost mux: after dispatch it copies the
// pattern the mux stamped on the request into the holder that Middleware
// placed in the context. Without it, any request-cloning middleware between
// Middleware and the mux hides the route label.
func RecordPattern(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if h, ok := r.Context().Value(routeHolderKey{}).(*routeHolder); ok {
			h.pattern = r.Pattern
		}
	})
}

// Middleware records one observation per request. The route label is the
// ServeMux pattern that ultimately handled the request, reported by a
// RecordPattern shim wrapped around the innermost mux; when no shim ran it
// falls back to r.Pattern on its own request, except the root catch-all "/"
// (that pattern says nothing about the route — a middleware short-circuited
// before dispatch), which buckets as "unmatched". Both fallbacks keep label
// cardinality bounded, as does the method allowlist.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		holder := &routeHolder{}
		r = r.WithContext(context.WithValue(r.Context(), routeHolderKey{}, holder))
		next.ServeHTTP(rec, r)

		pattern := holder.pattern
		if pattern == "" && r.Pattern != "/" {
			pattern = r.Pattern
		}
		route := patternRoute(pattern)
		method := methodLabel(r.Method)
		m.requests.WithLabelValues(route, method, strconv.Itoa(rec.code)).Inc()
		m.duration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
	})
}

// methodLabel keeps the method label bounded: any RFC 7230 token parses as
// a method, so nonstandard verbs collapse into "OTHER" instead of minting
// unbounded time series.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return m
	}
	return "OTHER"
}

// patternRoute strips the method prefix from a ServeMux pattern
// ("GET /api/v1/me" → "/api/v1/me"); the method is its own label.
func patternRoute(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	if method, path, ok := strings.Cut(pattern, " "); ok && !strings.Contains(method, "/") {
		return path
	}
	return pattern
}

// statusRecorder captures the response code while passing Flush through so
// SSE keeps working behind the middleware.
type statusRecorder struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.code = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer
// (Flush for SSE, deadlines, …).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush forwards to the underlying writer when it supports flushing.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
