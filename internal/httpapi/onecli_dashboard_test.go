package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// operatorPAT seeds a user and a PAT for it. The dashboard tests authenticate
// with the token rather than the setup cookie because most of them configure an
// https --base-url, which makes lab's session cookie Secure — and Go's cookie
// jar (correctly) refuses to send a Secure cookie back over the httptest
// server's plain http. The endpoints accept any identity, so as far as they are
// concerned this is the same request.
func (x *testServer) operatorPAT() string {
	x.t.Helper()
	u := x.seedUser("op", "password123")
	return x.seedPAT(u.ID)
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// dashboard fetches the exposure endpoint as an authenticated operator,
// asserting the pinned 200 on the way through — every mode, off included, is a
// complete answer, so there is no status to branch on.
func (x *testServer) dashboard(token string) map[string]any {
	x.t.Helper()
	resp := doWith(x.t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/onecli/dashboard", nil, bearer(token))
	wantStatus(x.t, resp, http.StatusOK)
	return decodeBody(x.t, resp)
}

// wantExposure asserts the whole body: the mode, and the URL as a key that is
// either present with an exact value or absent entirely. An empty wantURL means
// "no url key at all" — `omitempty` is part of the contract, not an accident of
// the struct tag.
func wantExposure(t *testing.T, body map[string]any, wantMode, wantURL string) {
	t.Helper()
	if body["mode"] != wantMode {
		t.Fatalf("mode = %v, want %q (body %#v)", body["mode"], wantMode, body)
	}
	got, present := body["url"]
	if wantURL == "" {
		if present {
			t.Fatalf("body carries a url key with nothing exposed: %#v", body)
		}
		return
	}
	if !present || got != wantURL {
		t.Fatalf("url = %v (present=%v), want %q (body %#v)", got, present, wantURL, body)
	}
}

// TestAuthCheck pins the forward-auth contract subdomain mode is built on: 204
// with an EMPTY body for an authenticated caller, 401 for everyone else. nginx
// auth_request and caddy forward_auth read the status and nothing else, so the
// status is the whole API.
func TestAuthCheck(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123") // x.client's jar now holds a session

	t.Run("authenticated session is 204 and empty", func(t *testing.T) {
		resp := x.do("GET", "/api/v1/auth/check", nil, nil)
		wantStatus(t, resp, http.StatusNoContent)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		_ = resp.Body.Close()
		if len(body) != 0 {
			t.Fatalf("204 carried a body: %q", body)
		}
	})

	t.Run("no session is 401", func(t *testing.T) {
		// A fresh client with no jar entry — a user exists, so this is the auth
		// guard answering, not "nobody is set up yet".
		resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/auth/check", nil, nil)
		wantStatus(t, resp, http.StatusUnauthorized)
		// The 401 body is requireAuth's ordinary envelope: ignored by both
		// proxies, and what makes the endpoint debuggable with curl.
		if got := decodeBody(t, resp); got["error"] == "" {
			t.Fatalf("401 without a JSON error: %#v", got)
		}
	})

	t.Run("bogus cookie is 401", func(t *testing.T) {
		resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/auth/check", nil,
			map[string]string{"Cookie": sessionCookieName + "=not-a-real-session"})
		wantStatus(t, resp, http.StatusUnauthorized)
		_ = resp.Body.Close()
	})

	t.Run("a PAT is a valid identity too", func(t *testing.T) {
		// Forward-auth asks "is this principal authenticated", not "did it use
		// a cookie": every identity resolveIdentity produces passes.
		y := newTestServer(t, nil)
		token := y.operatorPAT()
		resp := doWith(t, http.DefaultClient, y.ts.URL, "GET", "/api/v1/auth/check", nil, bearer(token))
		wantStatus(t, resp, http.StatusNoContent)
		_ = resp.Body.Close()
	})
}

// TestOneCLIDashboardRequiresAuth proves requireAuth is really on the route: a
// user exists, the client carries no credential, and the answer is 401 — never
// the 200 the endpoint gives an authenticated caller. The body names an
// internal origin, so this guard is the only thing in front of it.
func TestOneCLIDashboardRequiresAuth(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.BaseURL = "https://lab.example.com"
		o.OneCLIDashboardMode = dashboardModePort
		o.OneCLIDashboardAddr = ":8443"
	})
	token := x.operatorPAT()

	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/onecli/dashboard", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	// Same request WITH a credential: 200. The 401 above was the auth guard,
	// not a missing route.
	wantExposure(t, x.dashboard(token), dashboardModePort, "https://lab.example.com:8443")
}

// TestOneCLIDashboardExposure walks the modes end to end through the API. The
// unconfigured case is the important one: the route is mounted (never a 404),
// the mode is "off", and there is no url key at all.
func TestOneCLIDashboardExposure(t *testing.T) {
	t.Run("unconfigured is off with no url", func(t *testing.T) {
		x := newTestServer(t, nil)
		token := x.operatorPAT()
		wantExposure(t, x.dashboard(token), dashboardModeOff, "")
	})

	t.Run("port mode derives the url from the base url", func(t *testing.T) {
		x := newTestServer(t, func(o *Options) {
			o.BaseURL = "https://lab.example.com"
			o.OneCLIDashboardMode = dashboardModePort
			o.OneCLIDashboardAddr = ":8443"
		})
		token := x.operatorPAT()
		wantExposure(t, x.dashboard(token), dashboardModePort, "https://lab.example.com:8443")
	})

	t.Run("port mode discards the bind host", func(t *testing.T) {
		// Same answer as the wildcard bind above: the listen address names an
		// interface, and only its port reaches a browser.
		x := newTestServer(t, func(o *Options) {
			o.BaseURL = "https://lab.example.com"
			o.OneCLIDashboardMode = dashboardModePort
			o.OneCLIDashboardAddr = "127.0.0.1:8443"
		})
		token := x.operatorPAT()
		wantExposure(t, x.dashboard(token), dashboardModePort, "https://lab.example.com:8443")
	})

	t.Run("port mode override wins verbatim", func(t *testing.T) {
		x := newTestServer(t, func(o *Options) {
			o.BaseURL = "https://lab.example.com"
			o.OneCLIDashboardMode = dashboardModePort
			o.OneCLIDashboardAddr = ":8443"
			o.OneCLIDashboardURL = "https://dash.example.com/"
		})
		token := x.operatorPAT()
		// Verbatim but for the trailing slash: the resolved URL is an origin a
		// consumer concatenates against, never a path.
		wantExposure(t, x.dashboard(token), dashboardModePort, "https://dash.example.com")
	})

	t.Run("subdomain mode reports the configured url", func(t *testing.T) {
		x := newTestServer(t, func(o *Options) {
			o.BaseURL = "https://lab.example.com"
			o.OneCLIDashboardMode = dashboardModeSubdomain
			o.OneCLIDashboardURL = "https://onecli.example.com"
		})
		token := x.operatorPAT()
		wantExposure(t, x.dashboard(token), dashboardModeSubdomain, "https://onecli.example.com")
	})
}

// TestResolveOneCLIDashboard table-tests the derivation directly — the decision
// table for what an operator's three settings resolve to.
func TestResolveOneCLIDashboard(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		addr     string
		url      string
		base     string
		wantMode string
		wantURL  string
	}{
		{"unset is off", "", "", "", "https://lab.example.com", dashboardModeOff, ""},
		{"off ignores everything else", dashboardModeOff, ":8443", "https://onecli.example.com", "https://lab.example.com", dashboardModeOff, ""},
		{"port from a wildcard bind", dashboardModePort, ":8443", "", "https://lab.example.com", dashboardModePort, "https://lab.example.com:8443"},
		{"port discards the bind host", dashboardModePort, "127.0.0.1:8443", "", "https://lab.example.com", dashboardModePort, "https://lab.example.com:8443"},
		// The base URL's own port is replaced, not appended to: the browser
		// reaches lab on 8080 and the dashboard on 8443.
		{"port replaces the base url port", dashboardModePort, ":8443", "", "http://lab.example.com:8080", dashboardModePort, "http://lab.example.com:8443"},
		// IPv6 on both halves: Hostname() strips the base URL's brackets and
		// JoinHostPort puts them back, so the literal is bracketed exactly once.
		{"port with an ipv6 base url", dashboardModePort, "[::1]:8443", "", "https://[2001:db8::1]", dashboardModePort, "https://[2001:db8::1]:8443"},
		{"port keeps http", dashboardModePort, ":8443", "", "http://lab.example.com", dashboardModePort, "http://lab.example.com:8443"},
		{"port override wins over the derivation", dashboardModePort, ":8443", "https://dash.example.com", "https://lab.example.com", dashboardModePort, "https://dash.example.com"},
		{"port override needs no base url", dashboardModePort, "", "https://dash.example.com", "", dashboardModePort, "https://dash.example.com"},
		{"port override trims one trailing slash", dashboardModePort, ":8443", "https://dash.example.com/", "https://lab.example.com", dashboardModePort, "https://dash.example.com"},
		{"subdomain takes the configured url", dashboardModeSubdomain, "", "https://onecli.example.com", "https://lab.example.com", dashboardModeSubdomain, "https://onecli.example.com"},
		{"subdomain trims one trailing slash", dashboardModeSubdomain, "", "https://onecli.example.com/", "", dashboardModeSubdomain, "https://onecli.example.com"},
		{"subdomain ignores the addr", dashboardModeSubdomain, ":8443", "https://onecli.example.com", "", dashboardModeSubdomain, "https://onecli.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, u, err := resolveOneCLIDashboard(tt.mode, tt.addr, tt.url, tt.base)
			if err != nil {
				t.Fatalf("resolveOneCLIDashboard(%q, %q, %q, %q) = error %v",
					tt.mode, tt.addr, tt.url, tt.base, err)
			}
			if mode != tt.wantMode || u != tt.wantURL {
				t.Errorf("resolveOneCLIDashboard(%q, %q, %q, %q) = (%q, %q), want (%q, %q)",
					tt.mode, tt.addr, tt.url, tt.base, mode, u, tt.wantMode, tt.wantURL)
			}
		})
	}
}

// TestResolveOneCLIDashboardErrors pins the refusals, message text included.
// Every one of them fails startup rather than falling back to off: config
// already validated the mode, so anything here is a wiring bug, and the silent
// fallback would turn a typo into an invisibly-unexposed dashboard.
func TestResolveOneCLIDashboardErrors(t *testing.T) {
	tests := []struct {
		name string
		mode string
		addr string
		url  string
		base string
		want string
	}{
		{
			"unknown mode", "on", "", "", "https://lab.example.com",
			`httpapi: onecli dashboard mode "on": want off, port or subdomain`,
		},
		{
			// Case matters: the mode word is compared, not interpreted.
			"wrong case mode", "Port", ":8443", "", "https://lab.example.com",
			`httpapi: onecli dashboard mode "Port": want off, port or subdomain`,
		},
		{
			"subdomain without a url", dashboardModeSubdomain, ":8443", "", "https://lab.example.com",
			"httpapi: onecli dashboard mode subdomain: no browser-facing URL configured",
		},
		{
			"port without a base url", dashboardModePort, ":8443", "", "",
			"httpapi: onecli dashboard mode port: no base URL to derive the browser-facing URL from",
		},
		{
			"port with an unsplittable addr", dashboardModePort, "8443", "", "https://lab.example.com",
			`httpapi: onecli dashboard addr "8443": want [host]:port`,
		},
		{
			// SplitHostPort accepts a bare trailing colon; an origin ending in
			// one is not usable, so it is the same refusal.
			"port with an empty port", dashboardModePort, "127.0.0.1:", "", "https://lab.example.com",
			`httpapi: onecli dashboard addr "127.0.0.1:": want [host]:port`,
		},
		{
			"port with no addr at all", dashboardModePort, "", "", "https://lab.example.com",
			`httpapi: onecli dashboard addr "": want [host]:port`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, u, err := resolveOneCLIDashboard(tt.mode, tt.addr, tt.url, tt.base)
			if err == nil {
				t.Fatalf("resolveOneCLIDashboard(%q, %q, %q, %q) = (%q, %q), want an error",
					tt.mode, tt.addr, tt.url, tt.base, mode, u)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err, tt.want)
			}
		})
	}
}

// TestNewRejectsDashboardConfig proves the refusals above actually abort
// startup, rather than being a helper nobody's failure path reaches.
func TestNewRejectsDashboardConfig(t *testing.T) {
	newWith := func(t *testing.T, mod func(*Options)) error {
		t.Helper()
		o := Options{
			Store:  testutil.TempStore(t),
			Bus:    events.NewBus(),
			Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		}
		mod(&o)
		_, err := New(o)
		return err
	}
	tests := []struct {
		name string
		mod  func(*Options)
	}{
		{"unknown mode", func(o *Options) {
			o.OneCLIDashboardMode = "enabled"
		}},
		{"subdomain without a url", func(o *Options) {
			o.OneCLIDashboardMode = dashboardModeSubdomain
		}},
		{"port without a base url", func(o *Options) {
			o.OneCLIDashboardMode = dashboardModePort
			o.OneCLIDashboardAddr = ":8443"
		}},
		{"port with an unsplittable addr", func(o *Options) {
			o.OneCLIDashboardMode = dashboardModePort
			o.OneCLIDashboardAddr = "8443"
			o.BaseURL = "https://lab.example.com"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := newWith(t, tt.mod); err == nil {
				t.Fatal("New accepted the configuration, want an error")
			}
		})
	}

	// The control: the same shapes, configured correctly, build a server.
	for _, ok := range []func(*Options){
		func(o *Options) {
			o.OneCLIDashboardMode = dashboardModeSubdomain
			o.OneCLIDashboardURL = "https://onecli.example.com"
		},
		func(o *Options) {
			o.OneCLIDashboardMode = dashboardModePort
			o.OneCLIDashboardAddr = ":8443"
			o.BaseURL = "https://lab.example.com"
		},
	} {
		if err := newWith(t, ok); err != nil {
			t.Fatalf("New rejected a valid dashboard configuration: %v", err)
		}
	}
}

// sessionCookieFrom pulls the session cookie out of a response, failing when
// there is none.
func sessionCookieFrom(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("%s carried no %s cookie", resp.Request.URL.Path, sessionCookieName)
	return nil
}

// TestSessionCookieDomain pins --session-cookie-domain on BOTH halves of the
// session's life. The logout half is the one that rots silently: a browser
// deletes a cookie only when the expiring Set-Cookie matches its Domain, so a
// clear that omits the attribute leaves the parent-domain cookie alive in the
// browser (see clearSessionCookie).
//
// The requests run on a jar-less client because Go's cookie jar refuses a
// Domain=example.com cookie from a 127.0.0.1 origin — correctly, and irrelevant
// here: what is under test is the Set-Cookie lab writes.
func TestSessionCookieDomain(t *testing.T) {
	login := func(t *testing.T, x *testServer) *http.Cookie {
		t.Helper()
		x.seedUser("op", "password123")
		resp := doWith(t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/auth/login",
			map[string]any{"username": "op", "password": "password123"}, nil)
		wantStatus(t, resp, http.StatusOK)
		defer func() { _ = resp.Body.Close() }()
		return sessionCookieFrom(t, resp)
	}
	logout := func(t *testing.T, x *testServer, session *http.Cookie) *http.Cookie {
		t.Helper()
		headers := csrfHeaders(x.ts.URL)
		headers["Cookie"] = session.Name + "=" + session.Value
		resp := doWith(t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/auth/logout", nil, headers)
		wantStatus(t, resp, http.StatusNoContent)
		defer func() { _ = resp.Body.Close() }()
		return sessionCookieFrom(t, resp)
	}

	t.Run("configured domain is set on both cookies", func(t *testing.T) {
		x := newTestServer(t, func(o *Options) { o.SessionCookieDomain = "example.com" })

		set := login(t, x)
		if set.Domain != "example.com" {
			t.Fatalf("login cookie Domain = %q, want %q", set.Domain, "example.com")
		}
		// Widening the Domain must not have cost anything else: cross-subdomain
		// is still same-site, so Strict stays, and the cookie stays HttpOnly.
		if set.SameSite != http.SameSiteStrictMode {
			t.Errorf("login cookie SameSite = %v, want Strict", set.SameSite)
		}
		if !set.HttpOnly {
			t.Error("login cookie is not HttpOnly with a Domain set")
		}
		if set.Path != "/" {
			t.Errorf("login cookie Path = %q, want %q", set.Path, "/")
		}

		// The clearing cookie must match on Domain or the browser keeps the
		// live one beside it.
		clear := logout(t, x, set)
		if clear.Domain != "example.com" {
			t.Fatalf("logout cookie Domain = %q, want %q — the browser will not delete a cookie it does not match",
				clear.Domain, "example.com")
		}
		if clear.MaxAge >= 0 {
			t.Errorf("logout cookie MaxAge = %d, want a negative (expiring) value", clear.MaxAge)
		}
		if clear.Value != "" {
			t.Errorf("logout cookie Value = %q, want empty", clear.Value)
		}
		if clear.SameSite != http.SameSiteStrictMode || !clear.HttpOnly {
			t.Errorf("logout cookie attributes drifted: %+v", clear)
		}
	})

	t.Run("unset domain stays host-only on both cookies", func(t *testing.T) {
		x := newTestServer(t, nil)

		set := login(t, x)
		if set.Domain != "" {
			t.Fatalf("login cookie Domain = %q, want no Domain attribute", set.Domain)
		}
		clear := logout(t, x, set)
		if clear.Domain != "" {
			t.Fatalf("logout cookie Domain = %q, want no Domain attribute", clear.Domain)
		}
	})
}
