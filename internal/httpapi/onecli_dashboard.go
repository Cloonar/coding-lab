package httpapi

// OneCLI dashboard exposure (issue #26): everything the operator API owes the
// dashboard epic, minus the port-mode reverse proxy itself. Two endpoints:
//
//   - GET /api/v1/onecli/dashboard — the RESOLVED exposure: which mode is
//     active and, when the dashboard is exposed at all, the browser-facing
//     URL. The SPA's grant picker (issue #25) renders or hides its link-out
//     from this and nothing else, so no consumer has to know the topology, and
//     lab never has to teach the browser the difference between "lab proxies it
//     on a second port" and "your nginx fronts onecli.<domain>".
//   - GET /api/v1/auth/check — the forward-auth probe subdomain mode is built
//     on. It lives here rather than beside handleAuthState in handlers.go
//     because subdomain mode is the entire reason it exists: it ships with,
//     and is read with, the exposure it serves.
//
// Why the exposure is a SEPARATE endpoint from /api/v1/onecli/health rather
// than one more field on that payload: exposure is static process
// configuration, resolved once in New out of flags that cannot change while
// the process lives, whereas health spends up to oneCLIProbeTimeout (3s) on
// two live network probes. Folding them together would make a consumer that
// only wants "where is the dashboard" — the SPA, on every screen that renders
// the link — pay that probe budget, and would couple a constant answer to a
// fallible one, so a dead sidecar could take the link with it.
//
// "off" is a complete, healthy answer here exactly as it is for health: the
// default lab exposes nothing, says so in the same body shape it uses for
// every other mode, and that is not a failure to report.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The three exposure modes. They mirror the exported constants in
// internal/config: cmd/lab resolves --onecli-dashboard there and passes the
// resolved string through verbatim.
//
// httpapi deliberately does NOT import config. config is a leaf package parsed
// out of argv and the environment, and the API server has to stay constructible
// without one — every test in this package builds an Options literal, and so
// does any other embedder. The price is these three literals; the guard against
// them drifting is resolveOneCLIDashboard, which rejects any word it does not
// recognize by name instead of quietly treating it as off.
const (
	dashboardModeOff       = "off"
	dashboardModePort      = "port"
	dashboardModeSubdomain = "subdomain"
)

// resolveOneCLIDashboard turns the three raw --onecli-dashboard* settings plus
// --base-url into the pair the Server stores: the canonical mode word and the
// browser-facing URL, "" when nothing is exposed. New calls it once, and the
// handler then answers from two strings — no parsing, no allocation, nothing
// that can fail at request time.
//
// Every problem it finds is a startup error, never a fallback to off. config
// has already validated the mode by the time cmd/lab gets here, so anything
// rejected below is a wiring bug between the two packages — and the failure a
// silent fallback buys is the worst one on offer: an operator who asked for the
// dashboard, saw no error, and discovers weeks later that it was never exposed.
func resolveOneCLIDashboard(mode, addr, dashURL, baseURL string) (string, string, error) {
	switch mode {
	case "", dashboardModeOff:
		// Unset is off, and must be: Options's zero value has to build a
		// working server. "" is a spelling of off, never a third state.
		return dashboardModeOff, "", nil
	case dashboardModeSubdomain:
		if dashURL == "" {
			return "", "", fmt.Errorf("httpapi: onecli dashboard mode subdomain: no browser-facing URL configured")
		}
		// Nothing to derive here, ever: in this mode the operator's own reverse
		// proxy owns the origin, and only the operator knows what it is.
		return dashboardModeSubdomain, trimTrailingSlash(dashURL), nil
	case dashboardModePort:
		u, err := oneCLIDashboardPortURL(addr, dashURL, baseURL)
		if err != nil {
			return "", "", err
		}
		return dashboardModePort, u, nil
	default:
		return "", "", fmt.Errorf("httpapi: onecli dashboard mode %q: want off, port or subdomain", mode)
	}
}

// oneCLIDashboardPortURL derives port mode's browser-facing origin: lab's own
// externally visible scheme and host, with the second listener's port.
func oneCLIDashboardPortURL(addr, override, baseURL string) (string, error) {
	// An explicit --onecli-dashboard-url wins verbatim. The derivation below is
	// a convenience for the common deployment, not a claim to know better than
	// the operator: a lab whose second listener is itself fronted by a proxy,
	// or published under a different name, is exactly why the override exists.
	if override != "" {
		return trimTrailingSlash(override), nil
	}
	if baseURL == "" {
		return "", fmt.Errorf("httpapi: onecli dashboard mode port: no base URL to derive the browser-facing URL from")
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("httpapi: parse base url: %w", err)
	}
	if base.Hostname() == "" {
		// Unreachable from New (it validated the base URL as an absolute
		// http(s) URL first), but this function is also the unit under test:
		// a base URL with no host is as unusable as no base URL at all.
		return "", fmt.Errorf("httpapi: onecli dashboard mode port: no base URL to derive the browser-facing URL from")
	}

	// The listen address's HOST half is thrown away deliberately, and that is
	// the non-obvious half of this derivation. --onecli-dashboard-addr says
	// which INTERFACE lab binds — ":8443", "127.0.0.1:8443", "[::1]:8443" are
	// all ordinary values — and none of that is anything a browser can type.
	// The browser-facing host comes from --base-url, the one setting that
	// already means "the address this lab is reached at"; only the port carries
	// over. Deriving the host from the bind address instead would hand every
	// loopback-bound lab a dashboard URL of https://127.0.0.1:8443, correct
	// only from the machine lab happens to run on.
	//
	// An empty port after a successful split ("127.0.0.1:", which SplitHostPort
	// accepts) is rejected with the same error rather than rendered: a URL
	// ending in a bare colon is not a usable origin, and the operator's typo is
	// better named at startup than pasted into someone's address bar.
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", fmt.Errorf("httpapi: onecli dashboard addr %q: want [host]:port", addr)
	}

	// url.URL{Scheme, Host}.String() renders exactly scheme://host:port — no
	// path, no trailing slash, which is the shape a consumer can concatenate
	// against. net.JoinHostPort brackets an IPv6 literal the way a URL needs it
	// ([2001:db8::1]:8443), and Hostname() has already stripped the brackets
	// the base URL carried, so the two compose instead of double-bracketing.
	derived := url.URL{Scheme: base.Scheme, Host: net.JoinHostPort(base.Hostname(), port)}
	return derived.String(), nil
}

// trimTrailingSlash normalizes an operator-supplied origin to the shape every
// consumer can concatenate against: no trailing slash. One is enough —
// "https://onecli.example.com/" is what a browser's address bar hands an
// operator to paste — and anything past that is a path prefix, a configuration
// this epic explicitly does not support: OneCLI's Next.js app has no basePath,
// which is why path-prefix exposure was ruled out in favour of whole-origin
// proxying in the first place (issue #26).
func trimTrailingSlash(raw string) string { return strings.TrimSuffix(raw, "/") }

// oneCLIDashboardResponse is the exposure endpoint's one body shape, in every
// mode. URL is omitted rather than sent empty when nothing is exposed: "off"
// has no URL to report, so a consumer testing for the key and one testing for a
// non-empty string reach the same conclusion.
type oneCLIDashboardResponse struct {
	Mode string `json:"mode"`
	URL  string `json:"url,omitempty"`
}

// handleOneCLIDashboard is GET /api/v1/onecli/dashboard: the resolved exposure,
// read straight off the two fields New computed. Always 200 — every mode,
// including off, is a complete answer.
func (s *Server) handleOneCLIDashboard(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, oneCLIDashboardResponse{
		Mode: s.oneCLIDashboardMode,
		URL:  s.oneCLIDashboardURL,
	})
}

// handleAuthCheck is GET /api/v1/auth/check: 204 iff the request carries a
// valid lab identity, 401 otherwise. It is the whole lab-side contract of
// subdomain mode — nginx's `auth_request /api/v1/auth/check;` and caddy's
// `forward_auth` both work by reissuing each incoming request against this
// path and reading only its status code.
//
// 204, not 200 with a body, and the emptiness is the point three times over:
// nginx's auth_request discards the response body outright, caddy's
// forward_auth passes on any 2xx, and an endpoint that returns nothing cannot
// be made into a data leak by a proxy that is — by design — allowed to call it
// on behalf of an unauthenticated visitor. There is no username, no expiry and
// no session id in the answer, because nothing that consumes it could read one
// anyway. Resist adding any: the caller that wants identity has /api/v1/me.
//
// The 401 body is requireAuth's ordinary JSON envelope rather than an empty
// response. Both proxies ignore it, so it costs nothing where it matters, and
// it keeps the endpoint debuggable by hand — curl against it is how an operator
// verifies a forward-auth setup before pointing a browser at the subdomain.
//
// It accepts ANY identity resolveIdentity produces — session cookie, PAT, or
// trusted-proxy header — not the session cookie alone. The question a
// forward-auth proxy is asking is "is this principal authenticated to lab", not
// "did it use a cookie", and narrowing it would refuse a PAT-authenticated
// client for no security gain: each of those identities already passed the same
// checks that admit it to the rest of /api/v1.
//
// It is a GET, so csrfMiddleware waives it by method (csrf.go) and requireAuth
// is the entire guard. That is correct here rather than a gap: the handler
// mutates nothing and returns nothing, so there is no forgeable effect and no
// payload to steal.
func (s *Server) handleAuthCheck(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
