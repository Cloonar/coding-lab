package httpapi

// Port mode (issue #26): lab's own authenticated reverse proxy in front of the
// OneCLI sidecar's dashboard.
//
// The whole mode in one paragraph: --onecli-dashboard=port makes lab itself the
// way in. cmd/lab opens a SECOND http.Server on --onecli-dashboard-addr and
// serves the handler OneCLIDashboardProxy returns on it; that handler admits a
// request only if it carries a valid lab identity, then forwards it to the
// OneCLI origin — path, query, method and body verbatim, and every header but
// lab's own session cookie. This package opens no listener and knows no bind
// address — it produces a handler, and cmd/lab owns its lifecycle exactly as it
// owns Handler()'s. onecli_dashboard.go's resolveOneCLIDashboard already turned
// the operator's flags into the mode word this file switches on and the
// browser-facing URL the SPA reads; nothing is re-derived here.
//
// Why a whole ORIGIN and not a path prefix under lab's own listener: OneCLI's
// Next.js app has no basePath, so its asset and route URLs are absolute, and
// both apps claim /settings. ADR-0067 sized the upstream fork that would fix
// that at roughly six files and rejected it on permanence rather than size — a
// rebase obligation on every upstream release, for a UI lab has no stake in
// owning. Whole-origin proxying has neither problem, and that is why the
// handler below has no route table: on its listener, every path is the
// dashboard's.
//
// CSRF: the chain this file builds deliberately does NOT include
// s.csrfMiddleware, and needs no replacement for it. Lab's session cookie is
// SameSite=Strict, so a cross-site request never carries it at all and the gate
// answers 401 — the cookie attribute IS the CSRF defense on this listener. (A
// different port on the same host is still same-site, which is also exactly why
// the existing cookie works here in the first place: RFC 6265 cookies are
// host-scoped, not port-scoped, so lab's session rides to the second port with
// no cookie changes whatsoever.) Do not add a csrfMiddleware-style header
// requirement on the way past: X-Lab-Csrf is lab's SPA convention, and the
// dashboard is a foreign application that will never send it — the guard would
// reject every mutation the dashboard makes, which is all of them that matter.
//
// What this file does NOT do, stated plainly because the epic's acceptance
// criterion sounds like it might: "direct unauthenticated access is impossible
// when OneCLI binds loopback" is a property of the DEPLOYMENT, not of this
// code. This proxy is an ADDITIONAL, authenticated path to the dashboard; it
// cannot take away a route the operator left open. If the sidecar is published
// on a routable interface, that interface stays reachable with or without this
// handler. Binding OneCLI to loopback is the instruction that makes the
// criterion true, and docs/ops.md owns it.

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
)

// dashboardLoginNext is the fixed keyword the bounce-back carries as ?next=.
// It is a KEYWORD and not a URL, and that distinction is the open-redirect
// argument written out at the redirect itself — see oneCLIDashboardGate.
const dashboardLoginNext = "onecli-dashboard"

// OneCLIDashboardProxy returns the authenticated reverse proxy for
// --onecli-dashboard=port, or (nil, nil) when the dashboard is not exposed on
// a port. cmd/lab serves a non-nil handler on its own http.Server, bound to
// --onecli-dashboard-addr, alongside the one serving Handler().
//
// A nil handler with a nil error is the ordinary answer for two of the three
// modes: off exposes nothing, and subdomain is fronted by the operator's own
// reverse proxy delegating auth to /api/v1/auth/check. Neither has a second
// listener, and neither is a failure to report.
//
// Every other problem is an error, i.e. a refusal to start, for the reason
// resolveOneCLIDashboard states at length: an operator who asked for the
// dashboard, saw no error, and got no dashboard is the worst outcome available.
func (s *Server) OneCLIDashboardProxy() (http.Handler, error) {
	if s.oneCLIDashboardMode != dashboardModePort {
		return nil, nil
	}

	// The upstream is --onecli-url — the SAME address lab's REST client already
	// dials, and NOT a new setting. Read that twice before "fixing" it: OneCLI
	// serves its Next.js dashboard and its REST API on ONE port (10254; ADR-0067
	// describes the sidecar's two ports, and the second one is the gateway
	// proxy, not a second HTTP surface). So the dashboard's origin is already in
	// lab's configuration, and adding --onecli-dashboard-target would introduce
	// a second name for one address that could then disagree with itself —
	// exactly the address surface ADR-0067 pins closed ("the client binding is
	// deliberately partial", "three optional settings", the gateway URL's
	// separateness argued as an exception rather than a pattern).
	//
	// config guarantees --onecli-url is set whenever the mode is not off, so an
	// empty value here is a wiring bug between the packages rather than an
	// operator mistake. It still gets a named refusal instead of a nil-target
	// panic at the first request.
	if s.oneCLIAPIURL == "" {
		return nil, fmt.Errorf("httpapi: onecli dashboard mode port: no OneCLI URL to proxy to")
	}
	u, err := url.Parse(s.oneCLIAPIURL)
	if err != nil {
		return nil, fmt.Errorf("httpapi: onecli dashboard mode port: --onecli-url %q: %w", s.oneCLIAPIURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// url.Parse rejects far less than a reader expects — "localhost:10254"
		// parses happily as scheme "localhost" with the opaque body "10254", and
		// no host at all — so the real guard is on the two halves an origin is
		// made of, the same pair canonicalOrigin tests in csrf.go. Without it a
		// scheme-less address would reach the transport as a target with no host
		// and fail once per request instead of once at startup.
		return nil, fmt.Errorf("httpapi: onecli dashboard mode port: --onecli-url %q: want an absolute http(s) URL", s.oneCLIAPIURL)
	}
	// The ORIGIN only. --onecli-url may (and typically does) end in /v1: that
	// path is the REST client's business, and joining it here would send the
	// browser's /settings to /v1/settings. Proxying the bare origin is precisely
	// what makes this work where the rejected path-prefix proxy could not — the
	// dashboard's absolute URLs land back on the same origin they came from.
	target := &url.URL{Scheme: u.Scheme, Host: u.Host}

	// The base URL is load-bearing here, not defensive noise: it is the origin
	// the unauthenticated bounce sends a browser to, and without it the gate
	// would have nowhere to redirect. This is a REACHABLE configuration inside
	// this package, which is why the guard exists at all —
	// oneCLIDashboardPortURL succeeds with no --base-url whenever
	// --onecli-dashboard-url is set as an override, so New builds a port-mode
	// Server with an empty baseOrigin. internal/config refuses that combination
	// at the CLI today; this package must not depend on it continuing to.
	if s.baseOrigin == "" {
		return nil, fmt.Errorf("httpapi: onecli dashboard mode port: no base URL to redirect unauthenticated browsers to for login")
	}

	rp := &httputil.ReverseProxy{
		// Rewrite, not the deprecated Director. The difference is not stylistic:
		// on the Rewrite path ReverseProxy DELETES any client-supplied Forwarded
		// and X-Forwarded-{For,Host,Proto} from the outbound request before this
		// hook runs, so a caller cannot smuggle a forged hop through lab; on the
		// Director path those headers are inherited from the inbound request and
		// appended to. That is a security property of the choice, not a
		// formatting detail.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// SetURL clears pr.Out.Host, which makes the outbound Host header the
			// TARGET's host. Keep that (it is Go's default): a Next.js app behind a
			// proxy expects to be addressed by the host it is actually served on,
			// and SetXForwarded below hands it the inbound host as X-Forwarded-Host
			// so it can still see the browser-facing name if it cares.
			pr.SetXForwarded()
			stripLabSession(pr.Out)
		},
		// ReverseProxy's default error handler is a bare 502 with an EMPTY body
		// plus a line on the stdlib logger. Replacing it keeps two properties:
		// the JSON envelope every other lab error uses, and the upstream error
		// text staying out of the response — it names an internal address (and
		// on a dial failure, the exact host and port the sidecar is on), which
		// this listener may well be exposed to a network the sidecar is not.
		// The operator gets the whole error in the log, keyed by the same
		// request_id loggingMiddleware prints.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("proxying to the onecli dashboard",
				"component", "httpapi",
				"err", err,
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", logx.RequestID(r.Context()))
			writeError(w, http.StatusBadGateway, "onecli dashboard is unreachable")
		},
	}

	// Outermost first: recover → request-id → logging, the same three lab's own
	// tree opens with, reusing the same methods.
	//
	// s.metrics.Middleware and metrics.RecordPattern are deliberately NOT here,
	// and the omission is a decision rather than an oversight. Both record by
	// lab's ROUTE PATTERN; this listener has no route table, so every dashboard
	// request — /settings, /_next/static/…, a long-poll on an approval — would
	// land in one meaningless bucket, while diluting lab's own API histograms
	// with a foreign app's traffic. The request log above still records every
	// proxied request with its status and duration, which is the observability
	// that is actually legible here.
	h := s.oneCLIDashboardGate(rp)
	h = s.loggingMiddleware(h)
	h = s.requestIDMiddleware(h)
	h = s.recoverMiddleware(h)
	return h, nil
}

// oneCLIDashboardGate is port mode's whole authorization story: a valid lab
// identity, or nothing gets through to the dashboard. It accepts ANY identity
// resolveIdentity produces — session cookie, PAT, or trusted-proxy header — for
// the same reason /api/v1/auth/check does: the question is "is this principal
// authenticated to lab", not "did it use a cookie".
//
// The identity is deliberately not stashed in the request context: nothing
// behind this gate reads it. The dashboard is a separate application with its
// own sessions, and lab's principal is not part of what it is sent.
func (s *Server) oneCLIDashboardGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.resolveIdentity(r)
		if err != nil {
			// A store failure is not an authorization answer; matching
			// authMiddleware, it is a 500 rather than a redirect or a 401 — both
			// of which would tell the caller something untrue about its
			// credential.
			s.log.Error("resolving identity", "component", "httpapi", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if id != nil {
			next.ServeHTTP(w, r)
			return
		}
		if !browserNavigation(r) {
			// Everything that is not a navigation gets the plain refusal.
			// Answering a POST or an XHR with a 302 to a login page produces a
			// confusing broken-looking failure — a redirect chain the caller
			// follows into HTML it cannot parse, or a silently swallowed error —
			// where a 401 is legible to every client ever written.
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		// The bounce to lab's login, and the single most important line in this
		// file: the originally requested URL is NOT carried as a URL.
		//
		// `next` is a fixed KEYWORD and `path` is a path-and-query only. The SPA
		// composes the destination from that keyword plus the browser-facing
		// origin it reads back from GET /api/v1/onecli/dashboard — so the origin
		// always comes from lab's own configuration and NEVER from the query
		// string a stranger sent. That makes the bounce-back structurally
		// incapable of being an open redirect: there is no place in the parameter
		// where an attacker's origin could be written, so there is nothing for a
		// validator to get wrong, and no validator to forget to run.
		//
		// The obvious-looking ?next=<absolute URL> is exactly the design this
		// avoids. It reduces every login page that implements it to one
		// carefully-maintained allowlist standing between a phishing link and a
		// credential prompt that redirects wherever the link said, and the
		// allowlist is only ever one refactor away from being wrong. Keep the
		// keyword. If a second bounce destination is ever needed, add a second
		// keyword — never a URL.
		loginURL := s.baseOrigin + "/login?next=" + dashboardLoginNext +
			"&path=" + url.QueryEscape(r.URL.RequestURI())
		http.Redirect(w, r, loginURL, http.StatusFound)
	})
}

// browserNavigation reports whether r is a top-level navigation a human would
// expect to be redirected: a GET or HEAD that says it will take HTML. Anything
// else — a form POST, an XHR, a fetch for JSON, an asset request from a page
// whose session expired — is a caller that wants a status code, not a login
// page.
func browserNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

// stripLabSession removes lab's own session cookie from the outbound request
// and leaves EVERY other cookie in place — OneCLI's own session cookies above
// all, which must ride through untouched or the dashboard cannot keep a visitor
// logged in.
//
// Why bother, given the dashboard would simply ignore an unknown cookie: lab's
// session token is a lab CREDENTIAL. Anything holding it can act as the
// operator against lab's entire API. The dashboard has no use for it and is a
// separate application in a separate process — forwarding it would hand a
// bearer credential for lab to something that never needed one, and every log
// line, crash dump, and request trace on the far side would then carry it. This
// is defense in depth against a compromised or over-logging sidecar, not a fix
// for a known bug.
//
// One named cookie, and not the whole credential-bearing header class: a
// PAT-authenticated caller's Authorization header IS forwarded, deliberately.
// OneCLI's REST API shares this origin with its dashboard (that is why
// --onecli-url is the proxy target at all), so a script pointed at lab's proxy
// may legitimately be carrying OneCLI's own Bearer key — stripping the header
// wholesale would break that while protecting a credential no browser ever
// sends. The cookie is the browser's ambient credential and the one the
// dashboard would receive without anybody choosing to send it; that asymmetry is
// the line this function draws.
//
// The no-op case is left strictly alone: when no lab cookie is present the
// Cookie header is not rewritten at all. Re-serializing would lose nothing
// important, but skipping the work keeps the common case — every asset request
// from a PAT- or proxy-authenticated caller — honest about doing nothing.
func stripLabSession(out *http.Request) {
	cookies := out.Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			found = true
			break
		}
	}
	if !found {
		return
	}
	out.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			continue
		}
		out.AddCookie(c)
	}
}
