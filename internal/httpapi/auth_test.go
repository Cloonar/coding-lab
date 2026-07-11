package httpapi

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

func TestProxyAuthMatrix(t *testing.T) {
	// httptest connects from 127.0.0.1, so 127.0.0.0/8 makes the peer a
	// trusted proxy.
	x := newTestServer(t, func(o *Options) {
		o.ProxyAuth = true
		o.ProxyAuthHeader = "Remote-User"
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	x.seedUser("op", "password123")

	// Trusted peer + the single user's username: authenticated.
	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil,
		map[string]string{"Remote-User": "op"})
	wantStatus(t, resp, http.StatusOK)
	if me := decodeBody(t, resp); me["username"] != "op" {
		t.Fatalf("me = %v", me)
	}

	// Trusted peer + a value that is NOT the user's username: falls
	// through to other auth, which there is none of → 401.
	resp = doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil,
		map[string]string{"Remote-User": "intruder"})
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	// Proxy-header identity is ambient: mutations still need CSRF.
	resp = doWith(t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/auth/logout", nil,
		map[string]string{"Remote-User": "op"})
	wantStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()
}

func TestProxyAuthUntrustedPeerIgnoresHeader(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.ProxyAuth = true
		o.ProxyAuthHeader = "Remote-User"
		// The httptest peer (127.0.0.1) is NOT in this range.
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	x.seedUser("op", "password123")

	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil,
		map[string]string{"Remote-User": "op"})
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestProxyAuthDisabledIgnoresHeader(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	x.seedUser("op", "password123")

	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil,
		map[string]string{"Remote-User": "op"})
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestPATAuth(t *testing.T) {
	x := newTestServer(t, nil)
	u := x.seedUser("op", "password123")
	pat := x.seedPAT(u.ID)

	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil,
		map[string]string{"Authorization": "Bearer " + pat})
	wantStatus(t, resp, http.StatusOK)
	if me := decodeBody(t, resp); me["username"] != "op" {
		t.Fatalf("me = %v", me)
	}

	// last_used_at was stamped.
	tok, err := x.st.APITokenByHash(context.Background(), ids.HashToken(pat))
	if err != nil {
		t.Fatalf("reload token: %v", err)
	}
	if tok.LastUsedAt == nil {
		t.Fatal("last_used_at not updated by PAT auth")
	}

	// A syntactically valid but unknown PAT: 401.
	ghost, _ := ids.NewToken("pat")
	resp = doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil,
		map[string]string{"Authorization": "Bearer " + ghost})
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	// A non-PAT bearer token: 401, no fallthrough.
	resp = doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil,
		map[string]string{"Authorization": "Bearer lab_run_notapat"})
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestForeignAuthSchemeFallsThroughToAmbient(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	// A non-Bearer Authorization scheme (e.g. stamped by an nginx
	// auth_basic proxy) is not ours to judge: the valid session cookie
	// riding alongside it must still authenticate the request.
	resp := x.do("GET", "/api/v1/me", nil,
		map[string]string{"Authorization": "Basic Zm9vOmJhcg=="})
	wantStatus(t, resp, http.StatusOK)
	if me := decodeBody(t, resp); me["username"] != "op" {
		t.Fatalf("me = %v", me)
	}

	// The Bearer scheme still decides alone: an invalid Bearer token is a
	// hard 401 even with a valid session cookie in the same request.
	resp = x.do("GET", "/api/v1/me", nil,
		map[string]string{"Authorization": "Bearer lab_run_notapat"})
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestForeignAuthSchemeFallsThroughToProxyHeader(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.ProxyAuth = true
		o.ProxyAuthHeader = "Remote-User"
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	x.seedUser("op", "password123")

	// Basic credentials from the gateway plus the proxy identity header:
	// proxy-header auth must still resolve.
	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil,
		map[string]string{"Authorization": "Basic Zm9vOmJhcg==", "Remote-User": "op"})
	wantStatus(t, resp, http.StatusOK)
	if me := decodeBody(t, resp); me["username"] != "op" {
		t.Fatalf("me = %v", me)
	}
}

func TestSessionSlidingTouchAndExpiry(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	clk := testutil.NewFakeClock(base)
	x := newTestServer(t, func(o *Options) {
		o.Now = clk.Now
	})
	x.setup("op", "password123")

	// Find the session row through the cookie the jar holds.
	u, err := url.Parse(x.ts.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	var sessionID string
	for _, c := range x.client.Jar.Cookies(u) {
		if c.Name == "lab_session" {
			sessionID = ids.HashToken(c.Value)
		}
	}
	if sessionID == "" {
		t.Fatal("no session cookie in jar")
	}

	me := func(wantCode int) {
		t.Helper()
		resp := x.do("GET", "/api/v1/me", nil, nil)
		wantStatus(t, resp, wantCode)
		_ = resp.Body.Close()
	}
	lastSeen := func() *time.Time {
		t.Helper()
		ws, err := x.st.WebSession(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("load session: %v", err)
		}
		return ws.LastSeenAt
	}

	// First authenticated request stamps last_seen_at.
	me(http.StatusOK)
	first := lastSeen()
	if first == nil || !first.Equal(base) {
		t.Fatalf("last_seen_at after first request = %v, want %v", first, base)
	}

	// Within the 5-minute window: no touch.
	clk.Advance(time.Minute)
	me(http.StatusOK)
	if got := lastSeen(); !got.Equal(base) {
		t.Fatalf("last_seen_at touched too early: %v", got)
	}

	// At the window boundary: touched again.
	clk.Advance(4 * time.Minute)
	me(http.StatusOK)
	if got := lastSeen(); !got.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("last_seen_at after 5min = %v, want %v", got, base.Add(5*time.Minute))
	}

	// Past expiry the session is dead.
	clk.Advance(sessionTTL)
	me(http.StatusUnauthorized)
}

func TestSecureCookieWithHTTPSBaseURL(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.BaseURL = "https://lab.example.com"
	})
	resp := x.do("POST", "/api/v1/auth/setup",
		map[string]any{"username": "op", "password": "password123"}, nil)
	wantStatus(t, resp, http.StatusCreated)
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if c.Name == "lab_session" {
			if !c.Secure {
				t.Fatal("cookie not Secure despite https --base-url")
			}
			return
		}
	}
	t.Fatal("no lab_session cookie")
}

func TestPasswordHashRoundTrip(t *testing.T) {
	h := hashPasswordWith(testArgon, "correct horse")
	if ok, err := VerifyPassword(h, "correct horse"); err != nil || !ok {
		t.Fatalf("verify correct = (%v, %v)", ok, err)
	}
	if ok, err := VerifyPassword(h, "wrong horse"); err != nil || ok {
		t.Fatalf("verify wrong = (%v, %v)", ok, err)
	}
	// VerifyPassword still verifies a fresh HashPassword output (production
	// params, not the test-speed ones above).
	prod := HashPassword("correct horse battery staple")
	if ok, err := VerifyPassword(prod, "correct horse battery staple"); err != nil || !ok {
		t.Fatalf("verify HashPassword output = (%v, %v)", ok, err)
	}
	// A malformed hash still errors, and the error still names the caller
	// ("verify password") even though the check itself now lives in
	// parsePHC (issue #137).
	if _, err := VerifyPassword("$2y$whatever", "x"); err == nil || !strings.Contains(err.Error(), "verify password") {
		t.Fatalf("malformed hash err = %v, want error mentioning %q", err, "verify password")
	}
	if _, err := VerifyPassword("", "x"); err == nil || !strings.Contains(err.Error(), "verify password") {
		t.Fatalf("empty hash err = %v, want error mentioning %q", err, "verify password")
	}
}

// TestValidatePasswordHash pins parsePHC's tolerance and rejections via its
// exported wrapper: ValidatePasswordHash is what startup seeding of a
// pre-hashed user (issue #137) runs to catch a broken hash before it's ever
// written to the users table, so every rejection here is a rejection the
// seed path gets for free.
func TestValidatePasswordHash(t *testing.T) {
	valid := hashPasswordWith(testArgon, "some password")
	parts := strings.Split(valid, "$")
	// parts: ["", "argon2id", "v=19", "m=…,t=…,p=…", "<salt>", "<key>"]
	if len(parts) != 6 {
		t.Fatalf("test hash has %d parts, want 6: %q", len(parts), valid)
	}
	mutate := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}

	if err := ValidatePasswordHash(valid); err != nil {
		t.Fatalf("ValidatePasswordHash(valid) = %v, want nil", err)
	}

	tests := []struct {
		name string
		hash string
	}{
		{"truncated", strings.Join(parts[:5], "$")},
		{"wrong scheme argon2i", mutate(1, "argon2i")},
		{"wrong scheme bcrypt", mutate(1, "bcrypt")},
		{"wrong version", mutate(2, "v=1")},
		{"bad base64 salt", mutate(4, "not-valid-base64!!!")},
		{"bad base64 key", mutate(5, "not-valid-base64!!!")},
		{"empty key", mutate(5, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidatePasswordHash(tt.hash); err == nil {
				t.Fatalf("ValidatePasswordHash(%q) = nil, want error", tt.hash)
			}
		})
	}
}

func TestHashPasswordUsesPinnedParams(t *testing.T) {
	// Design §12: time=3, memory=64MiB, threads=4 — pinned in the PHC string.
	h := HashPassword("pw-for-params")
	const wantPrefix = "$argon2id$v=19$m=65536,t=3,p=4$"
	if len(h) < len(wantPrefix) || h[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("hash prefix = %q, want %q", h, wantPrefix)
	}
	if ok, err := VerifyPassword(h, "pw-for-params"); err != nil || !ok {
		t.Fatalf("verify = (%v, %v)", ok, err)
	}
}
