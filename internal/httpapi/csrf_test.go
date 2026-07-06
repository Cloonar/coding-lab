package httpapi

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestCSRFMatrix(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")
	pat := x.seedPAT(mustUserID(x))

	// Mutating request with ambient (cookie) auth and no CSRF header: 403.
	resp := x.do("POST", "/api/v1/auth/logout", nil, nil)
	wantStatus(t, resp, http.StatusForbidden)
	if got := decodeBody(t, resp); got["error"] != "missing X-Lab-Csrf header" {
		t.Fatalf("error = %q", got["error"])
	}

	// Header present but foreign Origin: 403.
	resp = x.do("POST", "/api/v1/auth/logout", nil,
		map[string]string{"X-Lab-Csrf": "1", "Origin": "https://evil.example"})
	wantStatus(t, resp, http.StatusForbidden)
	if got := decodeBody(t, resp); got["error"] != "origin not allowed" {
		t.Fatalf("error = %q", got["error"])
	}

	// Header present, Origin ABSENT: 403 (design §5 — browsers always send
	// Origin on non-GET fetch).
	resp = x.do("POST", "/api/v1/auth/logout", nil, map[string]string{"X-Lab-Csrf": "1"})
	wantStatus(t, resp, http.StatusForbidden)
	if got := decodeBody(t, resp); got["error"] != "missing Origin header" {
		t.Fatalf("error = %q", got["error"])
	}

	// GET is not guarded: no CSRF headers needed.
	resp = x.do("GET", "/api/v1/me", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	// Bearer PAT bypasses CSRF entirely (no cookie client, no CSRF headers).
	resp = doWith(t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/auth/logout", nil,
		map[string]string{"Authorization": "Bearer " + pat})
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	// Header + matching Origin: the session logout goes through.
	resp = x.do("POST", "/api/v1/auth/logout", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	resp = x.do("GET", "/api/v1/me", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

// mustUserID pulls the single seeded operator's id out of the store.
func mustUserID(x *testServer) string {
	x.t.Helper()
	u, err := x.st.UserByUsername(x.t.Context(), "op")
	if err != nil {
		x.t.Fatalf("load op: %v", err)
	}
	return u.ID
}

func TestCSRFHonorsBaseURLOrigin(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.BaseURL = "https://lab.example.com"
	})
	x.setup("op", "password123")

	// The httptest origin no longer matches: --base-url wins.
	resp := x.do("POST", "/api/v1/auth/logout", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()

	// The configured origin (with default port normalized) matches.
	resp = x.do("POST", "/api/v1/auth/logout", nil, csrfHeaders("https://lab.example.com:443"))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
}

func TestCSRFMixedCaseBaseURLHost(t *testing.T) {
	// Browsers serialize Origin with a lowercase host; a mixed-case
	// --base-url must canonicalize to the same origin or every mutating
	// request 403s.
	x := newTestServer(t, func(o *Options) {
		o.BaseURL = "https://Lab.Example.com"
	})
	x.setup("op", "password123")

	resp := x.do("POST", "/api/v1/auth/logout", nil, csrfHeaders("https://lab.example.com"))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
}

func TestUnauthenticatedMutationsSkipCSRF(t *testing.T) {
	// Login and setup carry no ambient credential, so the guard does not
	// apply — the SPA can call them before it has any session.
	x := newTestServer(t, nil)
	x.seedUser("op", "password123")

	resp := doWith(t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/auth/login",
		map[string]any{"username": "op", "password": "password123"}, nil)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
}

func TestCanonicalOrigin(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://lab.example.com", "https://lab.example.com", true},
		{"https://Lab.Example.com", "https://lab.example.com", true},
		{"https://lab.example.com:443", "https://lab.example.com", true},
		{"http://lab.example.com:80", "http://lab.example.com", true},
		{"http://lab.example.com:8080", "http://lab.example.com:8080", true},
		{"https://lab.example.com/path", "https://lab.example.com", true},
		{"null", "", false},
		{"", "", false},
		{"ftp://x", "", false},
	}
	for _, tt := range tests {
		got, ok := canonicalOrigin(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("canonicalOrigin(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestClientIPDerivation(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	s := &Server{trusted: trusted}

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{"untrusted peer ignores xff", "5.5.5.5:1000", "1.2.3.4", "5.5.5.5"},
		{"trusted peer takes rightmost non-trusted hop", "10.0.0.1:1000", "1.2.3.4, 10.0.0.2", "1.2.3.4"},
		{"trusted peer, single hop", "10.0.0.1:1000", "9.9.9.9", "9.9.9.9"},
		{"trusted peer, no xff", "10.0.0.1:1000", "", "10.0.0.1"},
		{"trusted peer, garbage xff", "10.0.0.1:1000", "not-an-ip", "10.0.0.1"},
		{"all hops trusted", "10.0.0.1:1000", "10.0.0.3, 10.0.0.2", "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("POST", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := s.clientIP(r); got != tt.want {
				t.Fatalf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}
