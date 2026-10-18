package preview

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedirectHandler(t *testing.T) {
	for _, tc := range []struct {
		port   int
		method string
		host   string
		target string // request-target as sent on the request line
		want   string // Location
	}{
		// The acceptance criterion's example, without and with a port.
		{443, http.MethodGet, "web.lab.internal", "/some/path?q=1", "https://web.lab.internal/some/path?q=1"},
		{0, http.MethodGet, "web.lab.internal", "/some/path?q=1", "https://web.lab.internal/some/path?q=1"},
		{8443, http.MethodGet, "web.lab.internal", "/some/path?q=1", "https://web.lab.internal:8443/some/path?q=1"},
		// The redirect listener's own port in Host is dropped, not carried over.
		{443, http.MethodGet, "web.lab.internal:80", "/", "https://web.lab.internal/"},
		{8443, http.MethodGet, "web.lab.internal:8080", "/", "https://web.lab.internal:8443/"},
		// Normalised like the preview listener: case, trailing dot.
		{443, http.MethodGet, "WEB.Lab.Internal.", "/x", "https://web.lab.internal/x"},
		// The escaped form survives verbatim.
		{443, http.MethodGet, "web.lab.internal", "/a%2Fb/c%20d?x=%26y&z", "https://web.lab.internal/a%2Fb/c%20d?x=%26y&z"},
		// An empty query marker is kept.
		{443, http.MethodGet, "web.lab.internal", "/p?", "https://web.lab.internal/p?"},
		// The bare domain and deeper names are under the domain too.
		{443, http.MethodGet, "lab.internal", "/", "https://lab.internal/"},
		{443, http.MethodGet, "a.web.lab.internal", "/", "https://a.web.lab.internal/"},
		// Method does not matter; 308 keeps it.
		{443, http.MethodPost, "web.lab.internal", "/form", "https://web.lab.internal/form"},
		{443, http.MethodHead, "web.lab.internal", "/", "https://web.lab.internal/"},
		// An absolute-form request line contributes only its path and query.
		{443, http.MethodGet, "web.lab.internal", "http://web.lab.internal/abs?q=2", "https://web.lab.internal/abs?q=2"},
		// A path that looks like a host stays a path.
		{443, http.MethodGet, "web.lab.internal", "//evil.example/x", "https://web.lab.internal//evil.example/x"},
		// No path to keep: "/".
		{443, http.MethodOptions, "web.lab.internal", "*", "https://web.lab.internal/"},
	} {
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader("body"))
		req.Host = tc.host
		rec := httptest.NewRecorder()
		RedirectHandler("lab.internal", tc.port).ServeHTTP(rec, req)
		if rec.Code != http.StatusPermanentRedirect {
			t.Errorf("%s %s %s (port %d): status = %d, want 308", tc.method, tc.host, tc.target, tc.port, rec.Code)
			continue
		}
		if got := rec.Header().Get("Location"); got != tc.want {
			t.Errorf("%s %s %s (port %d): Location = %q, want %q", tc.method, tc.host, tc.target, tc.port, got, tc.want)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s %s: Cache-Control = %q, want no-store", tc.host, tc.target, cc)
		}
	}
}

func TestRedirectHandlerNotFound(t *testing.T) {
	h := RedirectHandler("lab.internal", 443)
	for _, host := range []string{
		"web.example.com",
		"weblab.internal",
		"lab.internal.example",
		"192.0.2.1",
		"192.0.2.1:80",
		"[::1]",
		"[::1]:80",
		"[2001:db8::1]:80",
		".lab.internal",
		"a..lab.internal",
		"a%2e.lab.internal",
		"user@web.lab.internal",
		"",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, "/some/path?q=1", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s Host %q: status = %d, want 404", method, host, rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("%s Host %q: Location %q on a 404", method, host, loc)
			}
		}
	}
}

// TestRedirectHandlerOverTheWire checks the handler behind a real listener,
// where the request line is parsed by net/http itself.
func TestRedirectHandlerOverTheWire(t *testing.T) {
	srv := httptest.NewServer(RedirectHandler("lab.internal", 8443))
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/some/path?q=1", nil)
	must(t, err)
	req.Host = "web.lab.internal"
	resp, err := client.Do(req)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Location"), "https://web.lab.internal:8443/some/path?q=1"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	// The listener's own address (an IP) is not a preview host.
	resp, err = client.Get(srv.URL + "/ca.crt")
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("by IP: status = %d, want 404", resp.StatusCode)
	}
}
