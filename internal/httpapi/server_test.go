package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// testArgon keeps password hashing fast in tests. VerifyPassword reads
// parameters from the PHC string, so mixed-parameter hashes verify fine.
var testArgon = argonParams{time: 1, memoryKiB: 1024, threads: 1, keyLen: 32, saltLen: 16}

type testServer struct {
	t      *testing.T
	srv    *Server
	st     *store.Store
	bus    *events.Bus
	ts     *httptest.Server
	client *http.Client // cookie-jar client (the "browser")
}

func newTestServer(t *testing.T, mod func(*Options)) *testServer {
	t.Helper()
	st := testutil.TempStore(t)
	bus := events.NewBus()
	o := Options{
		Store:             st,
		Bus:               bus,
		Logger:            slog.New(slog.NewJSONHandler(io.Discard, nil)),
		HeartbeatInterval: time.Hour, // tests that want heartbeats override
	}
	if mod != nil {
		mod(&o)
	}
	srv, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.argon = testArgon

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &testServer{t: t, srv: srv, st: st, bus: bus, ts: ts, client: &http.Client{Jar: jar}}
}

// do sends a request through the cookie-jar client. body non-nil is sent as
// JSON. The response body stays open; use decodeBody or close it.
func (x *testServer) do(method, path string, body any, headers map[string]string) *http.Response {
	x.t.Helper()
	return doWith(x.t, x.client, x.ts.URL, method, path, body, headers)
}

func doWith(t *testing.T, client *http.Client, baseURL, method, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// csrfHeaders is what the SPA sends on mutating requests.
func csrfHeaders(origin string) map[string]string {
	return map[string]string{"X-Lab-Csrf": "1", "Origin": origin}
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return m
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("%s %s: status = %d, want %d (body %s)",
			resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, b)
	}
}

func (x *testServer) seedUser(username, password string) store.User {
	x.t.Helper()
	u, err := x.st.CreateUser(context.Background(), username, hashPasswordWith(testArgon, password))
	if err != nil {
		x.t.Fatalf("seed user: %v", err)
	}
	return u
}

func (x *testServer) seedPAT(userID string) string {
	x.t.Helper()
	token, hash := ids.NewToken("pat")
	if _, err := x.st.CreateAPIToken(context.Background(), userID, "test", hash); err != nil {
		x.t.Fatalf("seed pat: %v", err)
	}
	return token
}

// setup runs the first-run setup through the API so the jar holds the
// auto-login cookie.
func (x *testServer) setup(username, password string) {
	x.t.Helper()
	resp := x.do("POST", "/api/v1/auth/setup", map[string]any{"username": username, "password": password}, nil)
	wantStatus(x.t, resp, http.StatusCreated)
	_ = resp.Body.Close()
}

func TestSetupFlow(t *testing.T) {
	x := newTestServer(t, nil)

	// Fresh DB: state says setup is required, nobody authenticated.
	resp := x.do("GET", "/api/v1/auth/state", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	state := decodeBody(t, resp)
	if state["setup_required"] != true || state["authenticated"] != false {
		t.Fatalf("initial auth state = %v", state)
	}

	// Setup succeeds and auto-logs-in (cookie set).
	resp = x.do("POST", "/api/v1/auth/setup", map[string]any{"username": "op", "password": "password123"}, nil)
	wantStatus(t, resp, http.StatusCreated)
	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "lab_session" {
			sessionCookie = c
		}
	}
	_ = resp.Body.Close()
	if sessionCookie == nil {
		t.Fatal("setup response set no lab_session cookie")
	}
	if !sessionCookie.HttpOnly || sessionCookie.Path != "/" || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attributes wrong: %+v", sessionCookie)
	}
	if sessionCookie.Secure {
		t.Fatal("cookie Secure over plain http without https base-url")
	}
	if sessionCookie.MaxAge != int(sessionTTL.Seconds()) {
		t.Fatalf("cookie MaxAge = %d, want %d", sessionCookie.MaxAge, int(sessionTTL.Seconds()))
	}

	// The auto-login works.
	resp = x.do("GET", "/api/v1/me", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if me := decodeBody(t, resp); me["username"] != "op" {
		t.Fatalf("me = %v", me)
	}

	// Second setup (fresh client, no cookie) is refused.
	resp = doWith(t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/auth/setup",
		map[string]any{"username": "second", "password": "password123"}, nil)
	wantStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()

	// State reflects the logged-in operator.
	resp = x.do("GET", "/api/v1/auth/state", nil, nil)
	state = decodeBody(t, resp)
	if state["setup_required"] != false || state["authenticated"] != true || state["username"] != "op" {
		t.Fatalf("post-setup auth state = %v", state)
	}
}

func TestSetupValidation(t *testing.T) {
	x := newTestServer(t, nil)
	tests := []struct {
		name string
		body map[string]any
	}{
		{"empty username", map[string]any{"username": "", "password": "password123"}},
		{"long username", map[string]any{"username": string(bytes.Repeat([]byte("a"), 65)), "password": "password123"}},
		{"short password", map[string]any{"username": "op", "password": "seven77"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := x.do("POST", "/api/v1/auth/setup", tt.body, nil)
			wantStatus(t, resp, http.StatusBadRequest)
			body := decodeBody(t, resp)
			if body["error"] == "" {
				t.Fatal("400 without error message")
			}
		})
	}
	// Nothing was created: setup still required.
	resp := x.do("GET", "/api/v1/auth/state", nil, nil)
	if state := decodeBody(t, resp); state["setup_required"] != true {
		t.Fatalf("state after invalid setups = %v", state)
	}
}

func TestLoginSuccessAndBadCreds(t *testing.T) {
	x := newTestServer(t, nil)
	u := x.seedUser("op", "password123")

	// Wrong password and unknown user share one message (design §5).
	for _, body := range []map[string]any{
		{"username": "op", "password": "wrong-password"},
		{"username": "ghost", "password": "password123"},
	} {
		resp := x.do("POST", "/api/v1/auth/login", body, nil)
		wantStatus(t, resp, http.StatusUnauthorized)
		if got := decodeBody(t, resp); got["error"] != badCredsMessage {
			t.Fatalf("error = %q, want %q", got["error"], badCredsMessage)
		}
	}

	// /me is still locked.
	resp := x.do("GET", "/api/v1/me", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	// Correct credentials log in.
	resp = x.do("POST", "/api/v1/auth/login", map[string]any{"username": "op", "password": "password123"}, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["username"] != "op" {
		t.Fatalf("login body = %v", got)
	}

	resp = x.do("GET", "/api/v1/me", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	me := decodeBody(t, resp)
	if me["username"] != "op" || me["created_at"] == "" {
		t.Fatalf("me = %v", me)
	}
	// created_at is reported exactly as stored: the pinned fixed-width
	// formatter (design §2), milliseconds included — never ad-hoc RFC3339.
	if want := store.FormatTime(u.CreatedAt); me["created_at"] != want {
		t.Fatalf("me created_at = %q, want stored form %q", me["created_at"], want)
	}
}

func TestLoginOversizedUsernameSkipsLimiter(t *testing.T) {
	x := newTestServer(t, nil)
	x.seedUser("op", "password123")

	// A ~1 MiB username (kept inside the 1 MiB JSON body cap): the answer
	// is the generic bad-creds 401 (no length oracle), and the limiter
	// table must not retain the attacker-sized string as a key.
	huge := strings.Repeat("a", maxJSONBody-1024)
	resp := x.do("POST", "/api/v1/auth/login",
		map[string]any{"username": huge, "password": "wrong-password"}, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	if got := decodeBody(t, resp); got["error"] != badCredsMessage {
		t.Fatalf("error = %q, want %q", got["error"], badCredsMessage)
	}
	if n := x.srv.limiter.Len(); n != 0 {
		t.Fatalf("limiter grew to %d entries on an oversized username", n)
	}

	// Empty username: same generic answer, still no limiter entry.
	resp = x.do("POST", "/api/v1/auth/login",
		map[string]any{"username": "", "password": "wrong-password"}, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	if got := decodeBody(t, resp); got["error"] != badCredsMessage {
		t.Fatalf("error = %q, want %q", got["error"], badCredsMessage)
	}
	if n := x.srv.limiter.Len(); n != 0 {
		t.Fatalf("limiter grew to %d entries on an empty username", n)
	}

	// An in-range username still consumes the limiter as before.
	resp = x.do("POST", "/api/v1/auth/login",
		map[string]any{"username": "op", "password": "wrong-password"}, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
	if n := x.srv.limiter.Len(); n == 0 {
		t.Fatal("limiter recorded nothing for a valid-length attempt")
	}
}

func TestParallelLoginsAllComplete(t *testing.T) {
	x := newTestServer(t, nil)
	x.seedUser("op", "password123")

	// More parallel attempts than the argon2 semaphore admits: overflow
	// must wait for a slot — every request completes, none errors.
	const parallel = 3 * argonConcurrency
	if parallel > loginIPBurst {
		t.Fatalf("test bug: %d attempts would trip the per-IP burst %d", parallel, loginIPBurst)
	}
	type outcome struct {
		code int
		err  error
	}
	results := make(chan outcome, parallel)
	for i := 0; i < parallel; i++ {
		go func(i int) {
			user, pass := "op", "password123"
			if i%2 == 1 {
				// Unknown users burn the dummy hash — also under the cap.
				user, pass = fmt.Sprintf("ghost%d", i), "wrong-password"
			}
			b, err := json.Marshal(map[string]any{"username": user, "password": pass})
			if err != nil {
				results <- outcome{err: err}
				return
			}
			resp, err := http.Post(x.ts.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(b))
			if err != nil {
				results <- outcome{err: err}
				return
			}
			_ = resp.Body.Close()
			results <- outcome{code: resp.StatusCode}
		}(i)
	}
	deadline := time.After(30 * time.Second)
	for i := 0; i < parallel; i++ {
		select {
		case res := <-results:
			if res.err != nil {
				t.Fatalf("parallel login failed: %v", res.err)
			}
			if res.code != http.StatusOK && res.code != http.StatusUnauthorized {
				t.Fatalf("parallel login status = %d, want 200 or 401", res.code)
			}
		case <-deadline:
			t.Fatalf("only %d/%d parallel logins completed", i, parallel)
		}
	}
}

func TestRememberMeCookieTTL(t *testing.T) {
	x := newTestServer(t, nil)
	x.seedUser("op", "password123")

	resp := x.do("POST", "/api/v1/auth/login",
		map[string]any{"username": "op", "password": "password123", "remember": true}, nil)
	wantStatus(t, resp, http.StatusOK)
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if c.Name == "lab_session" {
			if c.MaxAge != int(rememberTTL.Seconds()) {
				t.Fatalf("remember-me MaxAge = %d, want %d", c.MaxAge, int(rememberTTL.Seconds()))
			}
			return
		}
	}
	t.Fatal("no lab_session cookie on remember-me login")
}

func TestLogout(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	resp := x.do("POST", "/api/v1/auth/logout", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	resp = x.do("GET", "/api/v1/me", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	// Logout without any session is a 401 (requireAuth), not a crash.
	resp = doWith(t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/auth/logout", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestLoginRateLimitPerUsername(t *testing.T) {
	x := newTestServer(t, nil)
	x.seedUser("op", "password123")

	body := map[string]any{"username": "op", "password": "wrong-password"}
	for i := 0; i < loginPairBurst; i++ {
		resp := x.do("POST", "/api/v1/auth/login", body, nil)
		wantStatus(t, resp, http.StatusUnauthorized)
		_ = resp.Body.Close()
	}
	resp := x.do("POST", "/api/v1/auth/login", body, nil)
	wantStatus(t, resp, http.StatusTooManyRequests)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Fatal("429 without JSON error")
	}

	// The correct password is throttled too — the limiter sits in front of
	// verification.
	resp = x.do("POST", "/api/v1/auth/login", map[string]any{"username": "op", "password": "password123"}, nil)
	wantStatus(t, resp, http.StatusTooManyRequests)
	_ = resp.Body.Close()
}

func TestLoginRateLimitPerIPAggregate(t *testing.T) {
	x := newTestServer(t, nil)

	// Spraying distinct usernames from one IP exhausts the aggregate bucket.
	n := 0
	for i := 0; i < loginIPBurst; i++ {
		resp := x.do("POST", "/api/v1/auth/login",
			map[string]any{"username": fmt.Sprintf("user%d", i), "password": "wrong-password"}, nil)
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("attempt %d already throttled", i+1)
		}
		_ = resp.Body.Close()
		n++
	}
	resp := x.do("POST", "/api/v1/auth/login",
		map[string]any{"username": "another", "password": "wrong-password"}, nil)
	wantStatus(t, resp, http.StatusTooManyRequests)
	_ = resp.Body.Close()
	if n != loginIPBurst {
		t.Fatalf("allowed %d attempts before aggregate limit, want %d", n, loginIPBurst)
	}
}
