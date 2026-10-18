package preview

import (
	"context"
	"errors"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
)

// routeKey carries a proxied request's route — the tunnel it must go
// through and the reserved port it is for — from serveHTTP to the
// transport's DialContext, which has nothing else to go on: the dial address
// is not a real address.
type routeKey struct{}

type route struct {
	tun  *tunnel
	name string
	port int
}

// serveHTTP routes one preview request by Host. Only <label>.<domain> with a
// single label that is currently reserved gets anywhere near a tunnel;
// everything else — the bare domain, another host entirely, a dotted
// multi-label name, a released name — is the same 404. There is no
// authentication: reachable is allowed, the stance lab takes for its own LAN
// path too (issue #105).
func (r *Registry) serveHTTP(w http.ResponseWriter, req *http.Request) {
	name, ok := r.nameFromHost(req.Host)
	if !ok {
		writePage(w, http.StatusNotFound, "No such preview",
			"There is no preview at "+req.Host+".")
		return
	}
	tun, port, ok := r.lookup(name)
	if !ok {
		writePage(w, http.StatusNotFound, "No such preview",
			"There is no preview named "+name+" on this lab.")
		return
	}
	if tun == nil {
		writePage(w, http.StatusServiceUnavailable, "Preview reconnecting",
			"The preview "+name+" is reserved, but its tunnel is reconnecting. Try again in a moment.")
		return
	}
	ctx := context.WithValue(req.Context(), routeKey{}, route{tun: tun, name: name, port: port})
	r.proxy.ServeHTTP(w, req.WithContext(ctx))
}

// nameFromHost extracts the preview name from a Host header value:
// lowercased, port and one trailing dot stripped, and required to be exactly
// one valid label in front of ".<domain>".
func (r *Registry) nameFromHost(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	name, ok := strings.CutSuffix(host, "."+r.domain)
	if !ok || !ValidName(name) {
		return "", false
	}
	return name, true
}

// newProxy builds the ReverseProxy behind every live preview.
func (r *Registry) newProxy() *httputil.ReverseProxy {
	transport := &http.Transport{
		// No Proxy: a fresh Transport ignores HTTPS_PROXY/HTTP_PROXY (only
		// DefaultTransport reads them), and the dial below is the only route.
		DialContext: dialRoute,
		// One yamux stream per proxied request or upgrade. Pooling would let a
		// connection opened for one request outlive it inside the transport,
		// keyed by an address that means nothing, and a stream is cheap.
		DisableKeepAlives: true,
		// The upstream is a dev server over HTTP/1.1; a WebSocket upgrade
		// needs HTTP/1.1, and h2c is never what a dev server speaks.
		ForceAttemptHTTP2: false,
	}
	return &httputil.ReverseProxy{
		// Rewrite, not Director: on this path ReverseProxy drops any
		// client-supplied Forwarded/X-Forwarded-* before the hook runs, so a
		// browser cannot forge a hop; SetXForwarded then sets the real ones.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			// The dial address is never dialled (dialRoute goes by the route in
			// the context); the preview's name keeps the transport's per-host
			// bookkeeping separate per preview.
			pr.Out.URL.Host = pr.Out.Context().Value(routeKey{}).(route).name
			// Keep the browser's Host: a dev server builds absolute URLs (HMR
			// endpoints, redirects) from it, and they must point back at the
			// preview hostname, not at a meaningless internal address.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport:    transport,
		ErrorHandler: proxyError,
		ErrorLog:     slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
}

// dialRoute is the proxy transport's DialContext: it opens a data stream on
// the tunnel the request was routed to.
func dialRoute(ctx context.Context, _, _ string) (net.Conn, error) {
	rt, ok := ctx.Value(routeKey{}).(route)
	if !ok {
		return nil, errors.New("preview: proxied request carries no route")
	}
	return rt.tun.dial(ctx, rt.port)
}

// proxyError answers a failed proxied request. labctl's "nothing listening"
// status gets its specific page; anything else — the session dying
// mid-request, a malformed upstream response — is a generic 502, with the
// detail kept for the log rather than echoed to whoever reached the
// listener.
func proxyError(w http.ResponseWriter, req *http.Request, err error) {
	var refused *refusedError
	if errors.As(err, &refused) {
		writePage(w, http.StatusBadGateway, "Nothing listening",
			"The preview's tunnel is up, but nothing is listening on port "+strconv.Itoa(refused.port)+" in the run.")
		return
	}
	if errors.Is(err, context.Canceled) {
		// The browser went away; there is nobody to answer.
		return
	}
	slog.Warn("proxying a preview request", "component", "preview", "host", req.Host, "err", err)
	writePage(w, http.StatusBadGateway, "Preview unreachable",
		"The preview's tunnel failed while forwarding this request.")
}

// writePage writes one of the preview listener's short HTML pages. Every
// piece of text is escaped: the host and name echoed into them come straight
// from the request. no-store keeps a browser from caching a 503 over the
// preview once its tunnel is back.
func writePage(w http.ResponseWriter, code int, title, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	t := html.EscapeString(title)
	_, _ = w.Write([]byte("<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>" + t +
		"</title></head><body><h1>" + t + "</h1><p>" + html.EscapeString(msg) + "</p></body></html>\n"))
}
