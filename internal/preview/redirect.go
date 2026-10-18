package preview

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// RedirectHandler is the handler for --preview-redirect-addr (issue #106):
// plain HTTP that only sends a browser on to the TLS preview listener. A
// request for the preview domain or any name under it gets a 308 Permanent
// Redirect to https://<same host>[:httpsPort]<same path and query>, the port
// left out when httpsPort is 443 (or 0, unknown); every other request —
// another host, a bare IP, an IPv6 literal, no Host at all — gets a plain
// 404.
//
// It serves nothing else, deliberately: no CA download, no status page, no
// preview content. This is the only unauthenticated plain-HTTP surface lab
// exposes, so anything it served would be served to anyone on the network,
// unencrypted, with no way to tell it was not tampered with in transit — the
// trust anchor in particular is only ever handed out behind lab's own auth.
//
// 308, not 301: a 308 keeps the method and body, so a form POSTed to an
// http:// preview URL arrives at the preview as the same POST. The answer is
// marked Cache-Control: no-store because browsers cache permanent redirects
// indefinitely by default, and a cached one would keep pointing at the old
// port after the operator moves the preview listener; the extra hop this
// costs happens only on a plain-http navigation, which the https URLs lab
// prints never cause.
func RedirectHandler(domain string, httpsPort int) http.Handler {
	port := ""
	if httpsPort != 0 && httpsPort != 443 {
		port = ":" + strconv.Itoa(httpsPort)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, ok := redirectHost(req.Host, domain)
		if !ok {
			http.NotFound(w, req)
			return
		}
		// RequestURI is the escaped path plus the query, exactly as the
		// browser sent them (an absolute-form request line contributes only
		// its path). Anything not starting with "/" — "*" from OPTIONS, an
		// opaque URL — has no path a preview could serve; send it to "/"
		// rather than glue it onto the host.
		uri := req.URL.RequestURI()
		if !strings.HasPrefix(uri, "/") {
			uri = "/"
		}
		h := w.Header()
		h.Set("Location", "https://"+host+port+uri)
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusPermanentRedirect)
	})
}

// redirectHost normalises a Host header — port and one trailing dot
// stripped, lowercased — and reports whether it is domain or a name under
// it. Every label in front of the domain must be non-empty and made only of
// [a-z0-9_-], so the host echoed into Location is always a plain DNS name:
// net/http already refuses control characters in Host, and this keeps
// anything stranger (userinfo, percent escapes, brackets) out of the
// redirect target too. An IPv6 literal fails that test and gets the 404.
func redirectHost(hostport, domain string) (string, bool) {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == domain {
		return host, true
	}
	prefix, ok := strings.CutSuffix(host, "."+domain)
	if !ok {
		return "", false
	}
	for label := range strings.SplitSeq(prefix, ".") {
		if label == "" || len(label) > 63 {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", false
			}
		}
	}
	return host, true
}
