package onecli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ProbeGateway reports whether the OneCLI GATEWAY proxy (port 10255 by
// default) accepts a TCP connection at gatewayURL. It is the reachability half
// of issue #23's health signal, and it is deliberately the weakest check that
// still proves the thing lab needs to know.
//
// What it proves and why that is the right amount:
//
//   - The gateway lab is about to point a run's HTTPS_PROXY at has a listener.
//     That is the failure this guards — the runs epic (#24) refuses to spawn
//     when the gateway is unreachable, and a run that starts against a dead
//     proxy fails later, opaquely, with every outbound request broken.
//   - It does NOT speak CONNECT and does NOT authenticate. A CONNECT probe
//     would need an agent's proxy token, which means minting one — a WRITE
//     that regenerates the agent's credential (see AgentToken) — just to
//     answer "is it up". Spending a credential on a health check is how health
//     checks start causing outages.
//
// The gateway URL is a proxy URL, not the API base, so it never goes through
// apiRoot: no /v1, no path. Only scheme, host and port are read. An
// unparseable URL, a non-http(s) scheme, or a URL with no host is an error
// BEFORE any dial — a typo must not become a DNS timeout.
//
// The returned error names the address that failed so an operator can act on
// it (that address, not the raw URL, is what a firewall or a compose bind
// setting is expressed in). The URL itself is only ever rendered redacted,
// because a proxy URL may legitimately carry userinfo credentials.
func ProbeGateway(ctx context.Context, gatewayURL string) error {
	if strings.TrimSpace(gatewayURL) == "" {
		return errors.New("onecli gateway probe: gateway URL is empty")
	}
	u, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil {
		return fmt.Errorf("onecli gateway probe: gateway URL is not a valid http(s) URL (e.g. http://127.0.0.1:10255): %w", urlErrReason(err))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("onecli gateway probe: gateway URL must be an http(s) URL, got scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("onecli gateway probe: gateway URL must include a host (e.g. http://127.0.0.1:10255)")
	}
	port := u.Port()
	if port == "" {
		// A gateway URL without an explicit port is unusual (OneCLI's proxy
		// listens on 10255), but the scheme's default is the only defensible
		// reading, and it keeps the probe's address identical to the one the
		// run's HTTP client would dial.
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	addr := net.JoinHostPort(host, port)

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("onecli gateway probe: dialing %s: %w; check that the OneCLI gateway is running and that its proxy port is bound on an interface lab can reach", addr, urlErrReason(err))
	}
	// The connection proved the point; holding it open would leak a socket per
	// probe on a signal lab polls.
	_ = conn.Close()
	return nil
}
