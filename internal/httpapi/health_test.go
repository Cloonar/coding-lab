package httpapi

import (
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestHealthzAndReadyz(t *testing.T) {
	x := newTestServer(t, nil)

	for _, path := range []string{"/healthz", "/readyz"} {
		resp := x.do("GET", path, nil, nil)
		wantStatus(t, resp, http.StatusOK)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "ok" {
			t.Fatalf("%s body = %q, want ok", path, body)
		}
	}

	// Kill the database: readyz flips to 503 with a JSON error, healthz
	// stays 200 (liveness, not readiness).
	if err := x.st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	resp := x.do("GET", "/readyz", nil, nil)
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Fatal("readyz 503 without JSON error")
	}

	resp = x.do("GET", "/healthz", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
}

func TestHealthEndpointsBypassAuthAndStore(t *testing.T) {
	// Probes carrying ambient credentials (session cookie AND a proxy
	// header from a trusted peer) with the store closed: if auth touched
	// the store, these would 500. healthz must stay 200 (liveness), readyz
	// must answer its designed 503, /metrics must scrape.
	x := newTestServer(t, func(o *Options) {
		o.ProxyAuth = true
		o.ProxyAuthHeader = "Remote-User"
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	x.setup("op", "password123") // jar now holds a session cookie
	if err := x.st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	hdr := map[string]string{"Remote-User": "op"}

	resp := x.do("GET", "/healthz", nil, hdr)
	wantStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("healthz body = %q, want ok", body)
	}

	resp = x.do("GET", "/readyz", nil, hdr)
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Fatal("readyz 503 without JSON error")
	}

	resp = x.do("GET", "/metrics", nil, hdr)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
}

func TestMetricsRouteLabelSurvivesAuthClone(t *testing.T) {
	// authMiddleware clones the request for authenticated calls; the
	// pattern recorder must still deliver the api mux's route to the
	// metrics middleware instead of the root catch-all "/".
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	resp := x.do("GET", "/api/v1/me", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	resp = x.do("GET", "/metrics", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if !strings.Contains(string(body), `route="/api/v1/me"`) {
		t.Fatal(`metrics output missing route="/api/v1/me" for an authenticated request`)
	}
}

func TestMetricsEndpointServes(t *testing.T) {
	x := newTestServer(t, nil)

	// Generate one observation, then scrape.
	resp := x.do("GET", "/healthz", nil, nil)
	_ = resp.Body.Close()

	resp = x.do("GET", "/metrics", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if !strings.Contains(string(body), "lab_http_requests_total") {
		t.Fatal("metrics output missing lab_http_requests_total")
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	x := newTestServer(t, nil)
	resp := x.do("GET", "/api/v1/does-not-exist", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want JSON", ct)
	}
	if got := decodeBody(t, resp); got["error"] != "not found" {
		t.Fatalf("body = %v", got)
	}
}

func TestAgentMountBypassesOperatorMiddleware(t *testing.T) {
	// A fake agent handler proves the mount contract: /agent/v1 requests
	// reach it even on mutating methods with a session cookie and no CSRF
	// header (the real run-token auth is agentapi's own, tested there).
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("agent-ok"))
	})
	x := newTestServer(t, func(o *Options) {
		o.AgentHandler = agent
	})
	x.setup("op", "password123")

	resp := x.do("POST", "/agent/v1/prs", map[string]any{"title": "t"}, nil)
	wantStatus(t, resp, http.StatusTeapot)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "agent-ok" {
		t.Fatalf("agent handler not reached: body = %q", body)
	}
}
