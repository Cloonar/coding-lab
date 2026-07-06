package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func counterValue(t *testing.T, m *Metrics, name string, labels map[string]string) float64 {
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
			if fam.GetType() == dto.MetricType_COUNTER {
				return mt.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestMiddlewareRecordsRouteMethodCode(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(m.Middleware(mux))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()

	got := counterValue(t, m, "lab_http_requests_total", map[string]string{
		"route": "/api/v1/me", "method": "GET", "code": "418",
	})
	if got != 1 {
		t.Fatalf("lab_http_requests_total{route=/api/v1/me,GET,418} = %v, want 1", got)
	}
}

func TestRecordPatternSurvivesRequestClone(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {})

	// A request-cloning middleware (like httpapi's authMiddleware) between
	// Middleware and the mux: the mux stamps r.Pattern on the clone only,
	// so without the RecordPattern shim the route label degrades to the
	// fallback.
	type cloneKey struct{}
	clone := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), cloneKey{}, "cloned")))
		})
	}
	srv := httptest.NewServer(m.Middleware(clone(RecordPattern(mux))))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()

	got := counterValue(t, m, "lab_http_requests_total", map[string]string{
		"route": "/api/v1/me", "method": "GET", "code": "200",
	})
	if got != 1 {
		t.Fatalf("lab_http_requests_total{route=/api/v1/me,GET,200} = %v, want 1 (clone hid the pattern)", got)
	}
}

func TestMethodLabelBoundsNonstandardVerbs(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(m.Middleware(mux))
	defer srv.Close()

	req, err := http.NewRequest("BOGUS", srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()

	got := counterValue(t, m, "lab_http_requests_total", map[string]string{
		"route": "/x", "method": "OTHER", "code": "200",
	})
	if got != 1 {
		t.Fatalf("BOGUS method not bucketed as OTHER (counter = %v)", got)
	}
}

func TestMiddlewareBucketsUnmatched(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m.Middleware(http.NewServeMux()))
	defer srv.Close()

	for _, path := range []string{"/nope/1", "/nope/2", "/other"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}

	got := counterValue(t, m, "lab_http_requests_total", map[string]string{
		"route": "unmatched", "method": "GET", "code": "404",
	})
	if got != 3 {
		t.Fatalf("unmatched counter = %v, want 3 (cardinality must stay bounded)", got)
	}
}

func TestHandlerServesRegistry(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(m.Middleware(mux))
	defer srv.Close()

	if resp, err := http.Get(srv.URL + "/x"); err != nil {
		t.Fatalf("get /x: %v", err)
	} else {
		_ = resp.Body.Close()
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, want := range []string{"lab_http_requests_total", "lab_http_request_duration_seconds", "go_goroutines"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("/metrics output missing %q", want)
		}
	}
}

func TestPatternRoute(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "unmatched"},
		{"GET /api/v1/me", "/api/v1/me"},
		{"POST /api/v1/auth/login", "/api/v1/auth/login"},
		{"/healthz", "/healthz"},
		{"/agent/v1/", "/agent/v1/"},
	}
	for _, tt := range tests {
		if got := patternRoute(tt.in); got != tt.want {
			t.Errorf("patternRoute(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
