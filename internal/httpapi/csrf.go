package httpapi

import (
	"net/http"
	"net/url"
	"strings"
)

// csrfHeader must be sent (with value "1") on every mutating request that
// was authenticated via ambient credentials. The SPA sets it on all fetches.
const csrfHeader = "X-Lab-Csrf"

// csrfMiddleware implements design §5 exactly:
//   - only mutating methods are guarded;
//   - only ambient-credential requests (session cookie, proxy header) are
//     guarded — Bearer requests bypass, unauthenticated requests (login,
//     setup) carry no ambient credential to ride on;
//   - the custom header is required — EXCEPT for the presence beacon (issue
//     #160), which cannot set it (navigator.sendBeacon allows no headers);
//     the Origin checks below still guard that endpoint in full;
//   - Origin, when present, must equal the configured origin (--base-url,
//     else scheme://Host); an ABSENT Origin is rejected — browsers always
//     send it on non-GET fetch, so its absence means a non-browser client
//     replaying ambient credentials.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		id := identityFrom(r.Context())
		if id == nil || !id.method.ambient() {
			next.ServeHTTP(w, r)
			return
		}
		// The presence beacon (issue #160) is the one mutating endpoint that
		// cannot carry the custom header: navigator.sendBeacon(...) allows no
		// header control. Waiving ONLY the header check is safe because the
		// Origin checks below still apply in full — a cross-site page cannot
		// forge a matching Origin, so browser CSRF stays impossible; the custom
		// header is redundant defense here, not the defense. The worst
		// forgeable outcome is a wrong presence bit (an extra or a suppressed
		// notification), never a data change.
		beacon := r.Method == http.MethodPost && r.URL.Path == "/api/v1/presence"
		if !beacon && r.Header.Get(csrfHeader) != "1" {
			writeError(w, http.StatusForbidden, "missing X-Lab-Csrf header")
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			writeError(w, http.StatusForbidden, "missing Origin header")
			return
		}
		if !s.originAllowed(origin, r) {
			writeError(w, http.StatusForbidden, "origin not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed compares the Origin header against the expected origin:
// --base-url's origin when configured, else <scheme>://<Host>.
func (s *Server) originAllowed(origin string, r *http.Request) bool {
	got, ok := canonicalOrigin(origin)
	if !ok {
		return false
	}
	want := s.baseOrigin
	if want == "" {
		var wok bool
		want, wok = canonicalOrigin(s.requestScheme(r) + "://" + r.Host)
		if !wok {
			return false
		}
	}
	return got == want
}

// canonicalOrigin reduces a URL to scheme://host with default ports
// stripped; ok is false for anything that is not an absolute http(s) URL
// (including the literal "null" origin).
func canonicalOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	// Lowercase the host: browsers serialize Origin with a lowercase host,
	// while --base-url arrives verbatim. Ports are digits, so lowercasing
	// host:port is safe.
	host := strings.ToLower(u.Host)
	switch u.Scheme {
	case "http":
		host = strings.TrimSuffix(host, ":80")
	case "https":
		host = strings.TrimSuffix(host, ":443")
	}
	return u.Scheme + "://" + host, true
}
