package preview

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// get serves one request with the given Host through h and returns the
// recorded response and its body.
func get(t *testing.T, h http.Handler, host string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp, string(body)
}

// assertPage asserts resp is an HTML page with the given status whose body
// contains want.
func assertPage(t *testing.T, resp *http.Response, body string, code int, want string) {
	t.Helper()
	if resp.StatusCode != code {
		t.Errorf("status = %d, want %d (body %q)", resp.StatusCode, code, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	if !strings.Contains(body, want) {
		t.Errorf("body %q does not contain %q", body, want)
	}
}

func TestHandlerNotFound(t *testing.T) {
	r, _ := newTestRegistry(t)
	mustReserve(t, r, "repo", "run1", "web", 5173)
	h := r.Handler()

	for _, host := range []string{
		"nope.lab.internal",        // unreserved name
		"nope.lab.internal:8081",   // with the listener's port
		"lab.internal",             // the bare domain
		"lab.internal:8081",        // bare domain with port
		"web.example.com",          // another domain entirely
		"web.lab.internal.example", // the domain as a non-suffix
		"a.web.lab.internal",       // multi-label name
		"weblab.internal",          // no dot before the domain
		"",                         // no Host at all
	} {
		resp, body := get(t, h, host)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("Host %q: status = %d, want 404", host, resp.StatusCode)
			continue
		}
		assertPage(t, resp, body, http.StatusNotFound, "No such preview")
	}
}

func TestHandlerReservedNotLive(t *testing.T) {
	r, _ := newTestRegistry(t)
	mustReserve(t, r, "repo", "run1", "web", 5173)
	h := r.Handler()

	// Routing is case-insensitive and ignores the port and a trailing dot,
	// the forms a browser may legitimately send.
	for _, host := range []string{"web.lab.internal", "WEB.Lab.Internal:8081", "web.lab.internal."} {
		resp, body := get(t, h, host)
		assertPage(t, resp, body, http.StatusServiceUnavailable, "reconnecting")
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Host %q: Cache-Control = %q, want no-store", host, cc)
		}
	}

	// Stopped → the same host is a 404.
	if err := r.Stop("run1", "web"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	resp, body := get(t, h, "web.lab.internal")
	assertPage(t, resp, body, http.StatusNotFound, "No such preview")
}

func TestHandlerEscapesEchoedHost(t *testing.T) {
	r, _ := newTestRegistry(t)
	resp, body := get(t, r.Handler(), `<script>alert(1)</script>.lab.internal`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(body, "<script>") {
		t.Errorf("body echoes the host unescaped: %q", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("body %q does not carry the escaped host", body)
	}
}
