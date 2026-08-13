package onecli

// ProbeGateway's decision table. The probe is a TCP dial and nothing more, so
// the tests are a real listener on 127.0.0.1:0 and the same listener closed —
// no network, no OneCLI, no credential spent.

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// listenLoopback starts a listener on an ephemeral loopback port and returns
// it with a gateway URL addressing it.
func listenLoopback(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln, "http://" + ln.Addr().String()
}

func TestProbeGatewayReachesALiveListener(t *testing.T) {
	ln, gatewayURL := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	// Accept and drop: the probe only needs the handshake, never a byte of
	// protocol — it deliberately does not speak CONNECT (see ProbeGateway).
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if err := ProbeGateway(context.Background(), gatewayURL); err != nil {
		t.Fatalf("ProbeGateway(%s): %v", gatewayURL, err)
	}
}

// TestProbeGatewayFailsOnAClosedPort: the failure the probe exists to catch.
// The listener is closed first, so nothing is bound on that port — exactly the
// "sidecar is not running" case that must stop a spawn (#24's fail-closed
// rule) rather than let a run start with a dead proxy.
func TestProbeGatewayFailsOnAClosedPort(t *testing.T) {
	ln, gatewayURL := listenLoopback(t)
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err := ProbeGateway(context.Background(), gatewayURL)
	if err == nil {
		t.Fatal("ProbeGateway succeeded against a closed port, want an error")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error %q does not name the address that failed (%s)", err, addr)
	}
	if !strings.Contains(err.Error(), "OneCLI gateway") {
		t.Errorf("error %q is not actionable — it does not say what was being probed", err)
	}
}

// TestProbeGatewayRejectsBadURLsWithoutDialing: a typo must be a configuration
// error at once, not a DNS lookup or a connect timeout. The assertion that it
// did not dial is the absence of "dialing" in the message — every dial failure
// this package produces carries that word.
func TestProbeGatewayRejectsBadURLsWithoutDialing(t *testing.T) {
	for _, tc := range []struct {
		name, url, wantWord string
	}{
		{"empty", "", "is empty"},
		{"whitespace", "   ", "is empty"},
		{"unparseable", "http://[::1", "not a valid"},
		{"control character in url", "http://127.0.0.1:10255/\x7f\x00", "not a valid"},
		{"no scheme", "127.0.0.1:10255", "http(s)"},
		{"socks scheme", "socks5://127.0.0.1:1080", "http(s)"},
		{"unix scheme", "unix:///run/onecli/gateway.sock", "http(s)"},
		{"no host", "http:///path", "must include a host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A deadline already in the past: if the implementation dialed
			// despite the malformed URL, the error would be the context's, not
			// the validation message asserted below.
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()

			err := ProbeGateway(ctx, tc.url)
			if err == nil {
				t.Fatalf("ProbeGateway(%q) succeeded, want refusal", tc.url)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not mention %q", err, tc.wantWord)
			}
			if strings.Contains(err.Error(), "dialing") {
				t.Errorf("error %q shows a dial was attempted for an invalid URL", err)
			}
		})
	}
}

// TestProbeGatewayHonorsContext: the probe runs on lab's status path, so a
// canceled context must abort it rather than sit in a connect timeout.
func TestProbeGatewayHonorsContext(t *testing.T) {
	ln, gatewayURL := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ProbeGateway(ctx, gatewayURL)
	if err == nil {
		t.Fatal("ProbeGateway succeeded with a canceled context, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not unwrap to context.Canceled", err)
	}
}

// TestProbeGatewayDefaultsThePortFromTheScheme: a gateway URL without an
// explicit port must dial the scheme's default, which is what the run's own
// HTTP client would do. 127.0.0.1:80 is almost certainly not listening in a
// test environment, so the assertion is on the ADDRESS in the error, not on
// the outcome.
func TestProbeGatewayDefaultsThePortFromTheScheme(t *testing.T) {
	for _, tc := range []struct{ url, wantAddr string }{
		{"http://127.0.0.1", "127.0.0.1:80"},
		{"https://127.0.0.1", "127.0.0.1:443"},
		{"http://127.0.0.1:10255", "127.0.0.1:10255"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := ProbeGateway(ctx, tc.url)
		cancel()
		if err == nil {
			// Something really is listening there; the port default is then
			// unobservable from here, and a false failure would be worse than
			// skipping the row.
			continue
		}
		if !strings.Contains(err.Error(), tc.wantAddr) {
			t.Errorf("ProbeGateway(%s) error %q does not name %s", tc.url, err, tc.wantAddr)
		}
	}
}
