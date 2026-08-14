package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dashboardEcho is what the stub dashboard reports about the request it
// received. Everything the proxy is responsible for rewriting (or deliberately
// NOT rewriting) is in here, so a test asserts on the upstream's view rather
// than on the proxy's internals.
type dashboardEcho struct {
	Method  string `json:"method"`
	URI     string `json:"uri"` // RequestURI: path AND query, as the upstream saw it
	Host    string `json:"host"`
	Cookie  string `json:"cookie"`
	XFF     string `json:"xff"`
	XFHost  string `json:"xf_host"`
	XFProto string `json:"xf_proto"`
}

// newStubDashboard stands in for the OneCLI sidecar: it echoes back what it
// received. /teapot is the one path that answers with something other than 200,
// so a test can prove the upstream's status and body are passed through rather
// than synthesized by the proxy.
func newStubDashboard(t *testing.T) *httptest.Server {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/teapot" {
			http.Error(w, "i am a teapot", http.StatusTeapot)
			return
		}
		w.Header().Set("X-Stub-Dashboard", "1")
		writeJSON(w, http.StatusOK, dashboardEcho{
			Method:  r.Method,
			URI:     r.URL.RequestURI(),
			Host:    r.Host,
			Cookie:  r.Header.Get("Cookie"),
			XFF:     r.Header.Get("X-Forwarded-For"),
			XFHost:  r.Header.Get("X-Forwarded-Host"),
			XFProto: r.Header.Get("X-Forwarded-Proto"),
		})
	}))
	t.Cleanup(stub.Close)
	return stub
}

// proxyFixture is a lab server plus the two other listeners port mode needs: the
// stub dashboard upstream, and lab's own dashboard proxy on a SECOND port —
// which is where cmd/lab will serve it.
type proxyFixture struct {
	*testServer
	stub  *httptest.Server
	proxy *httptest.Server
}

// newProxyFixture wires port mode end to end.
//
// --onecli-url deliberately carries a /v1 path, exactly as a real one does: the
// REST client's base is the API path, and the proxy must reduce it to the
// origin. BaseURL is plain http so lab's session cookie is not Secure and Go's
// cookie jar will send it back over httptest's plain-http listeners.
func newProxyFixture(t *testing.T, mod func(*Options)) *proxyFixture {
	t.Helper()
	stub := newStubDashboard(t)
	x := newTestServer(t, func(o *Options) {
		o.OneCLIAPIURL = stub.URL + "/v1"
		o.OneCLIDashboardMode = dashboardModePort
		o.OneCLIDashboardAddr = ":8443"
		o.BaseURL = "http://lab.example.test"
		if mod != nil {
			mod(o)
		}
	})
	h, err := x.srv.OneCLIDashboardProxy()
	if err != nil {
		t.Fatalf("OneCLIDashboardProxy: %v", err)
	}
	if h == nil {
		t.Fatal("OneCLIDashboardProxy returned no handler in port mode")
	}
	proxy := httptest.NewServer(h)
	t.Cleanup(proxy.Close)
	return &proxyFixture{testServer: x, stub: stub, proxy: proxy}
}

// via sends a request to the PROXY listener through the fixture's cookie-jar
// client — the same client, holding the same jar, that authenticated against
// lab's MAIN listener.
//
// That reuse is the whole point of the fixture, not a convenience: it is the
// direct proof of the property port mode rests on. RFC 6265 cookies are
// host-scoped, not port-scoped, so lab's existing session cookie rides to the
// second port with no cookie changes at all — no Domain, no re-issue, nothing
// for the operator to configure. (It is also why SameSite=Strict is untouched
// and sufficient here: a different port on the same host is still same-site.)
func (f *proxyFixture) via(method, path string, headers map[string]string) *http.Response {
	f.t.Helper()
	return doWith(f.t, f.client, f.proxy.URL, method, path, nil, headers)
}

// htmlAccept is what a browser sends on a top-level navigation.
func htmlAccept() map[string]string {
	return map[string]string{"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}
}

// noRedirectClient is a jar-less client that stops at the first response, so a
// test can read the bounce-to-login's Location instead of watching the client
// chase it into a host that does not resolve.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func decodeEcho(t *testing.T, resp *http.Response) dashboardEcho {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var e dashboardEcho
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode dashboard echo: %v", err)
	}
	return e
}

// labSession logs in on lab's main listener and returns the session cookie, for
// the tests that have to compose a Cookie header by hand rather than let the
// jar do it.
func (x *testServer) labSession(username, password string) *http.Cookie {
	x.t.Helper()
	x.seedUser(username, password)
	resp := doWith(x.t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/auth/login",
		map[string]any{"username": username, "password": password}, nil)
	wantStatus(x.t, resp, http.StatusOK)
	defer func() { _ = resp.Body.Close() }()
	return sessionCookieFrom(x.t, resp)
}

// TestOneCLIDashboardProxyEndToEnd is the epic's acceptance criterion for port
// mode: the dashboard loads through lab's proxy with a valid session. The
// session was obtained from lab's main listener and is used, unchanged, on the
// proxy's own port.
func TestOneCLIDashboardProxyEndToEnd(t *testing.T) {
	f := newProxyFixture(t, nil)
	f.setup("op", "password123") // the jar now holds lab's session cookie

	t.Run("a nested path and its query reach the upstream verbatim", func(t *testing.T) {
		resp := f.via("GET", "/settings?tab=secrets", htmlAccept())
		wantStatus(t, resp, http.StatusOK)
		if got := resp.Header.Get("X-Stub-Dashboard"); got != "1" {
			t.Errorf("upstream response header did not survive: X-Stub-Dashboard = %q", got)
		}
		echo := decodeEcho(t, resp)
		if echo.Method != http.MethodGet || echo.URI != "/settings?tab=secrets" {
			t.Fatalf("upstream saw %s %q, want GET %q", echo.Method, echo.URI, "/settings?tab=secrets")
		}
	})

	t.Run("the onecli url's path is dropped", func(t *testing.T) {
		// --onecli-url is <stub>/v1 (the REST base). Only its ORIGIN is the proxy
		// target: /settings must arrive as /settings, never /v1/settings.
		// Prefix-joining the REST path is the single most likely "fix" someone
		// applies to this file, and this is what it would break.
		echo := decodeEcho(t, f.via("GET", "/settings", htmlAccept()))
		if echo.URI != "/settings" {
			t.Fatalf("upstream saw %q, want %q — the --onecli-url path leaked into the proxy target",
				echo.URI, "/settings")
		}
	})

	t.Run("the upstream status and body come back", func(t *testing.T) {
		resp := f.via("GET", "/teapot", htmlAccept())
		wantStatus(t, resp, http.StatusTeapot)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), "i am a teapot") {
			t.Fatalf("upstream body did not come back: %q", body)
		}
	})

	t.Run("the root path works", func(t *testing.T) {
		echo := decodeEcho(t, f.via("GET", "/", htmlAccept()))
		if echo.URI != "/" {
			t.Fatalf("upstream saw %q, want %q", echo.URI, "/")
		}
	})
}

// TestOneCLIDashboardProxyUnauthenticated pins the other half of the criterion:
// without a session, a browser is bounced to lab's login and everything else
// gets a legible 401.
func TestOneCLIDashboardProxyUnauthenticated(t *testing.T) {
	f := newProxyFixture(t, nil)
	// A user exists, so every refusal below is the gate answering — not "nobody
	// is set up yet".
	f.seedUser("op", "password123")

	t.Run("a browser navigation is redirected to lab's login", func(t *testing.T) {
		resp := doWith(t, noRedirectClient(), f.proxy.URL, "GET", "/settings?foo=bar", nil, htmlAccept())
		wantStatus(t, resp, http.StatusFound)
		defer func() { _ = resp.Body.Close() }()
		// Asserted as an exact string because the SPA depends on it: `next` is a
		// fixed KEYWORD and `path` is a path-and-query only, never an absolute
		// URL. The SPA composes the destination from the keyword plus the
		// browser-facing origin it reads from GET /api/v1/onecli/dashboard, so the
		// origin comes from lab's configuration and never from the query string —
		// which is what makes this bounce structurally incapable of being an open
		// redirect. A ?next=<absolute URL> here would need an allowlist to be safe;
		// this needs nothing.
		want := "http://lab.example.test/login?next=onecli-dashboard&path=%2Fsettings%3Ffoo%3Dbar"
		if got := resp.Header.Get("Location"); got != want {
			t.Fatalf("Location = %q, want %q", got, want)
		}
	})

	t.Run("a non-browser GET is 401 with the JSON envelope", func(t *testing.T) {
		resp := doWith(t, noRedirectClient(), f.proxy.URL, "GET", "/api/agents", nil,
			map[string]string{"Accept": "application/json"})
		wantStatus(t, resp, http.StatusUnauthorized)
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("401 carried a Location header %q — an XHR cannot follow a login page", loc)
		}
		if got := decodeBody(t, resp); got["error"] != "authentication required" {
			t.Fatalf("401 body = %#v", got)
		}
	})

	t.Run("a POST is 401 even from a browser", func(t *testing.T) {
		// The Accept header says HTML, but a 302 to a login page answers a form
		// POST with a confusing broken-looking failure instead of a legible
		// refusal. Only GET and HEAD are navigations.
		resp := doWith(t, noRedirectClient(), f.proxy.URL, "POST", "/settings", nil, htmlAccept())
		wantStatus(t, resp, http.StatusUnauthorized)
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("POST was redirected to %q", loc)
		}
		_ = resp.Body.Close()
	})

	t.Run("a bogus session cookie is 401, not a pass", func(t *testing.T) {
		resp := doWith(t, noRedirectClient(), f.proxy.URL, "GET", "/", nil,
			map[string]string{"Accept": "application/json", "Cookie": sessionCookieName + "=not-a-real-session"})
		wantStatus(t, resp, http.StatusUnauthorized)
		_ = resp.Body.Close()
	})

	t.Run("nothing unauthenticated reaches the upstream", func(t *testing.T) {
		// The stub answers 200 with an echo body on every path but /teapot, so a
		// leak past the gate would have shown up as a 200 above. This asserts the
		// same thing from the other side: the refusals carried no upstream marker.
		resp := doWith(t, noRedirectClient(), f.proxy.URL, "GET", "/", nil,
			map[string]string{"Accept": "application/json"})
		defer func() { _ = resp.Body.Close() }()
		if got := resp.Header.Get("X-Stub-Dashboard"); got != "" {
			t.Fatalf("an unauthenticated request reached the dashboard (X-Stub-Dashboard = %q)", got)
		}
	})
}

// TestOneCLIDashboardProxyPAT: the gate takes any identity resolveIdentity
// produces, exactly like /api/v1/auth/check. The question is "is this principal
// authenticated to lab", not "did it use a cookie".
func TestOneCLIDashboardProxyPAT(t *testing.T) {
	f := newProxyFixture(t, nil)
	token := f.operatorPAT()

	resp := doWith(t, noRedirectClient(), f.proxy.URL, "GET", "/settings", nil, bearer(token))
	wantStatus(t, resp, http.StatusOK)
	if echo := decodeEcho(t, resp); echo.URI != "/settings" {
		t.Fatalf("upstream saw %q, want %q", echo.URI, "/settings")
	}
}

// TestOneCLIDashboardProxyStripsLabSession pins the cookie surgery: lab's own
// session cookie is a lab credential and stops at the proxy, while every other
// cookie — OneCLI's own session above all — rides through untouched, or the
// dashboard cannot keep a visitor logged in.
func TestOneCLIDashboardProxyStripsLabSession(t *testing.T) {
	f := newProxyFixture(t, nil)
	session := f.labSession("op", "password123")

	// Composed by hand rather than left to the jar: the point is a request that
	// carries lab's cookie AND the dashboard's own beside it.
	resp := doWith(t, noRedirectClient(), f.proxy.URL, "GET", "/", nil, map[string]string{
		"Accept": "application/json",
		"Cookie": sessionCookieName + "=" + session.Value + "; onecli_sid=abc; other=1",
	})
	wantStatus(t, resp, http.StatusOK)
	echo := decodeEcho(t, resp)

	if strings.Contains(echo.Cookie, sessionCookieName) || strings.Contains(echo.Cookie, session.Value) {
		t.Fatalf("lab's session cookie reached the dashboard: Cookie = %q", echo.Cookie)
	}
	for _, want := range []string{"onecli_sid=abc", "other=1"} {
		if !strings.Contains(echo.Cookie, want) {
			t.Errorf("cookie %q did not survive the strip: Cookie = %q", want, echo.Cookie)
		}
	}

	t.Run("a request without lab's cookie is passed through untouched", func(t *testing.T) {
		resp := doWith(t, noRedirectClient(), f.proxy.URL, "GET", "/", nil, map[string]string{
			"Accept":        "application/json",
			"Authorization": "Bearer " + f.seedPAT(f.seedUser("scripted", "password123").ID),
			"Cookie":        "onecli_sid=abc; other=1",
		})
		wantStatus(t, resp, http.StatusOK)
		if echo := decodeEcho(t, resp); echo.Cookie != "onecli_sid=abc; other=1" {
			t.Fatalf("Cookie = %q, want it forwarded verbatim", echo.Cookie)
		}
	})
}

// TestOneCLIDashboardProxyForwardedHeaders pins what the upstream is told about
// the hop. The client-supplied X-Forwarded-For is the security half: on the
// Rewrite path ReverseProxy drops the inbound Forwarded/X-Forwarded-* before
// the hook runs and SetXForwarded then writes fresh values, so a caller cannot
// smuggle a forged hop through lab.
func TestOneCLIDashboardProxyForwardedHeaders(t *testing.T) {
	f := newProxyFixture(t, nil)
	f.setup("op", "password123")

	headers := htmlAccept()
	headers["X-Forwarded-For"] = "1.2.3.4"
	echo := decodeEcho(t, f.via("GET", "/", headers))

	if echo.XFProto != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want %q", echo.XFProto, "http")
	}
	if want := strings.TrimPrefix(f.proxy.URL, "http://"); echo.XFHost != want {
		t.Errorf("X-Forwarded-Host = %q, want the inbound host %q", echo.XFHost, want)
	}
	if echo.XFF == "" {
		t.Error("X-Forwarded-For was not set at all")
	}
	if strings.Contains(echo.XFF, "1.2.3.4") {
		t.Errorf("the client's X-Forwarded-For survived verbatim: %q", echo.XFF)
	}
	// The outbound Host is the TARGET's, which is Go's default for SetURL and
	// what a Next.js app behind a proxy expects; the browser-facing host is not
	// lost, it is in X-Forwarded-Host asserted above.
	if want := strings.TrimPrefix(f.stub.URL, "http://"); echo.Host != want {
		t.Errorf("upstream Host = %q, want the target's host %q", echo.Host, want)
	}
}

// TestOneCLIDashboardProxyUpstreamDown: a dead sidecar is a 502 in lab's own
// JSON envelope, and the dial error — which names an internal address — stays
// in the log where the operator reads it, not in a response this listener may
// well serve to a network the sidecar is not on.
func TestOneCLIDashboardProxyUpstreamDown(t *testing.T) {
	f := newProxyFixture(t, nil)
	f.setup("op", "password123")
	f.stub.Close()

	resp := f.via("GET", "/settings", htmlAccept())
	wantStatus(t, resp, http.StatusBadGateway)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("502 body is not lab's JSON envelope (%v): %q", err, body)
	}
	if got["error"] != "onecli dashboard is unreachable" {
		t.Fatalf("502 error = %q", got["error"])
	}
	if host := strings.TrimPrefix(f.stub.URL, "http://"); strings.Contains(string(body), host) {
		t.Fatalf("the 502 leaked the upstream address %q: %q", host, body)
	}
	if strings.Contains(string(body), "connection refused") {
		t.Fatalf("the 502 leaked the dial error: %q", body)
	}
}

// TestOneCLIDashboardProxyConstructor is the decision table for what a
// configuration produces: a handler, nothing at all, or a named refusal.
func TestOneCLIDashboardProxyConstructor(t *testing.T) {
	// Two of the three modes have no second listener, and that is an ordinary
	// configuration rather than an error: off exposes nothing, and subdomain is
	// fronted by the operator's own proxy delegating to /api/v1/auth/check.
	t.Run("no handler and no error when the dashboard is not on a port", func(t *testing.T) {
		tests := []struct {
			name string
			mod  func(*Options)
		}{
			{"unconfigured", func(*Options) {}},
			{"explicitly off", func(o *Options) {
				o.OneCLIDashboardMode = dashboardModeOff
				o.OneCLIAPIURL = "http://127.0.0.1:10254/v1"
				o.BaseURL = "https://lab.example.com"
			}},
			{"subdomain", func(o *Options) {
				o.OneCLIDashboardMode = dashboardModeSubdomain
				o.OneCLIDashboardURL = "https://onecli.example.com"
				o.OneCLIAPIURL = "http://127.0.0.1:10254/v1"
				o.BaseURL = "https://lab.example.com"
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				x := newTestServer(t, tt.mod)
				h, err := x.srv.OneCLIDashboardProxy()
				if err != nil {
					t.Fatalf("OneCLIDashboardProxy: %v", err)
				}
				if h != nil {
					t.Fatal("a handler was built for a mode with no second listener")
				}
			})
		}
	})

	t.Run("refusals", func(t *testing.T) {
		tests := []struct {
			name string
			mod  func(*Options)
			want string
		}{
			{
				// config pairs --onecli-url with the rest of the OneCLI settings, so
				// this is a wiring bug between the packages rather than an operator
				// mistake — and still a named refusal, not a nil target that panics
				// on the first request.
				"port with no onecli url",
				func(o *Options) {
					o.OneCLIDashboardMode = dashboardModePort
					o.OneCLIDashboardAddr = ":8443"
					o.BaseURL = "https://lab.example.com"
				},
				"httpapi: onecli dashboard mode port: no OneCLI URL to proxy to",
			},
			{
				// The reachable-inside-this-package case: port mode resolves its
				// browser-facing URL from the --onecli-dashboard-url override with no
				// --base-url at all, so New succeeds and leaves baseOrigin empty —
				// and the gate would then have nowhere to bounce a browser to.
				// internal/config refuses this at the CLI today; this package does not
				// get to depend on that.
				"port with an override url but no base url",
				func(o *Options) {
					o.OneCLIDashboardMode = dashboardModePort
					o.OneCLIDashboardURL = "https://dash.example.com"
					o.OneCLIAPIURL = "http://127.0.0.1:10254/v1"
				},
				"httpapi: onecli dashboard mode port: no base URL to redirect unauthenticated browsers to for login",
			},
			{
				"port with an unparseable onecli url",
				func(o *Options) {
					o.OneCLIDashboardMode = dashboardModePort
					o.OneCLIDashboardAddr = ":8443"
					o.BaseURL = "https://lab.example.com"
					o.OneCLIAPIURL = "http://[::1"
				},
				`httpapi: onecli dashboard mode port: --onecli-url "http://[::1"`,
			},
			{
				// The scheme-less address url.Parse does NOT reject: it reads as
				// scheme "localhost" with the opaque body "10254" and no host at all.
				// That is why the origin's two halves are checked separately from the
				// parse — otherwise this would reach the transport and fail once per
				// request instead of once at startup.
				"port with an onecli url that has no origin",
				func(o *Options) {
					o.OneCLIDashboardMode = dashboardModePort
					o.OneCLIDashboardAddr = ":8443"
					o.BaseURL = "https://lab.example.com"
					o.OneCLIAPIURL = "localhost:10254"
				},
				`httpapi: onecli dashboard mode port: --onecli-url "localhost:10254": want an absolute http(s) URL`,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				x := newTestServer(t, tt.mod)
				h, err := x.srv.OneCLIDashboardProxy()
				if err == nil {
					t.Fatalf("OneCLIDashboardProxy = (%v, nil), want an error", h)
				}
				if h != nil {
					t.Errorf("a handler was returned alongside the error: %v", h)
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Errorf("error = %q, want it to contain %q", err, tt.want)
				}
			})
		}
	})
}
