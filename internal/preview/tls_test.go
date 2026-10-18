package preview

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"testing"
	"time"
)

// The TLS front door end to end (issue #106): the preview listener as
// cmd/lab runs it — an http.Server configured by CertManager.ConfigureServer
// and started with ServeTLS — in front of a real tunnel, reached by clients
// that trust nothing but the minted lab CA.

// startTLSPreview serves r's Handler over TLS with m's certificate on a
// loopback listener and returns its address.
func startTLSPreview(t *testing.T, r *Registry, m *CertManager) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	must(t, err)
	srv := &http.Server{
		Handler: r.Handler(),
		// The untrusting client's failed handshakes would otherwise be
		// logged to stderr.
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: waitTimeout,
	}
	m.ConfigureServer(srv)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("ServeTLS: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return ln.Addr().String()
}

// caPool is a pool holding only m's CA.
func caPool(t *testing.T, m *CertManager) *x509.CertPool {
	t.Helper()
	pemBytes, ok := m.CACertPEM()
	if !ok {
		t.Fatalf("no lab CA")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("CA PEM did not parse")
	}
	return pool
}

// previewClient is an HTTPS client whose every dial goes to addr, whatever
// host the URL names — the wildcard DNS record, in miniature — trusting only
// roots. h2 selects HTTP/2 only, otherwise HTTP/1.1 only.
func previewClient(addr string, roots *x509.CertPool, h2 bool) *http.Client {
	var protos http.Protocols
	if h2 {
		protos.SetHTTP2(true)
	} else {
		protos.SetHTTP1(true)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		Protocols:         &protos,
		DisableKeepAlives: true,
	}
	return &http.Client{Transport: tr, Timeout: waitTimeout}
}

// tlsFixture is a lab-CA preview listener for lab.internal with "web"
// exposed through a real tunnel to an upstream answering "hello over tls"
// (and echoing the protocol it was reached by from the preview side).
func tlsFixture(t *testing.T) (m *CertManager, r *Registry, addr string) {
	t.Helper()
	_, port := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Forwarded-Proto-Seen", req.Header.Get("X-Forwarded-Proto"))
		_, _ = io.WriteString(w, "hello over tls")
	}))
	m = newLabCA(t, t.TempDir(), "lab.internal", newTestClock(time.Now()))
	r, _ = newTestRegistry(t)
	startTunnel(t, r, "run1", "web", port)
	return m, r, startTLSPreview(t, r, m)
}

func getBody(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	must(t, err)
	return resp, string(body)
}

func TestTLSPreviewHTTP1(t *testing.T) {
	m, _, addr := tlsFixture(t)
	resp, body := getBody(t, previewClient(addr, caPool(t, m), false), "https://web.lab.internal/hello?x=1")
	if resp.StatusCode != http.StatusOK || body != "hello over tls" {
		t.Fatalf("response = %d %q, want the upstream's 200", resp.StatusCode, body)
	}
	if resp.ProtoMajor != 1 {
		t.Errorf("proto = %s, want HTTP/1.1", resp.Proto)
	}
	if got := resp.Header.Get("X-Forwarded-Proto-Seen"); got != "https" {
		t.Errorf("upstream saw X-Forwarded-Proto %q, want https", got)
	}
}

func TestTLSPreviewHTTP2(t *testing.T) {
	if extendedConnectEnabled() {
		t.Skip("GODEBUG=http2xconnect=1 pins the preview listener to HTTP/1.1")
	}
	m, _, addr := tlsFixture(t)
	resp, body := getBody(t, previewClient(addr, caPool(t, m), true), "https://web.lab.internal/hello")
	if resp.StatusCode != http.StatusOK || body != "hello over tls" {
		t.Fatalf("response = %d %q, want the upstream's 200", resp.StatusCode, body)
	}
	if resp.ProtoMajor != 2 || resp.TLS == nil || resp.TLS.NegotiatedProtocol != "h2" {
		t.Errorf("proto = %s, want HTTP/2 negotiated by ALPN", resp.Proto)
	}
}

func TestTLSPreviewUntrustedClientFails(t *testing.T) {
	_, _, addr := tlsFixture(t)
	for _, roots := range []*x509.CertPool{x509.NewCertPool(), nil /* system roots */} {
		_, err := previewClient(addr, roots, false).Get("https://web.lab.internal/")
		var unknown x509.UnknownAuthorityError
		if !errors.As(err, &unknown) {
			t.Errorf("roots %v: err = %v, want an unknown-authority verification failure", roots != nil, err)
		}
	}
}

func TestTLSPreviewWrongNameFails(t *testing.T) {
	m, _, addr := tlsFixture(t)
	_, err := previewClient(addr, caPool(t, m), false).Get("https://web.example.com/")
	var invalid x509.HostnameError
	if !errors.As(err, &invalid) {
		t.Errorf("err = %v, want a hostname mismatch", err)
	}
}

// TestTLSPreviewUpgrade is the WebSocket path over TLS: a browser's
// WebSocket handshake offers only http/1.1 in ALPN and sends an HTTP/1.1
// Upgrade, which the reverse proxy hijacks and forwards through the tunnel.
func TestTLSPreviewUpgrade(t *testing.T) {
	_, port := newUpstream(t, http.HandlerFunc(upgradeEcho))
	m := newLabCA(t, t.TempDir(), "lab.internal", newTestClock(time.Now()))
	r, _ := newTestRegistry(t)
	startTunnel(t, r, "run1", "web", port)
	addr := startTLSPreview(t, r, m)

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		ServerName: "web.lab.internal",
		RootCAs:    caPool(t, m),
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(waitTimeout))
	if p := conn.ConnectionState().NegotiatedProtocol; p != "http/1.1" {
		t.Errorf("ALPN = %q, want http/1.1", p)
	}
	_, err = io.WriteString(conn, "GET /hmr HTTP/1.1\r\nHost: web.lab.internal\r\nConnection: Upgrade\r\nUpgrade: echo-test\r\n\r\n")
	must(t, err)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if greeting, err := br.ReadString('\n'); err != nil || greeting != "hello\n" {
		t.Fatalf("greeting = %q, %v; want the upstream's hello", greeting, err)
	}
	for _, msg := range []string{"ping\n", "over tls\n"} {
		_, err := io.WriteString(conn, msg)
		must(t, err)
		if echo, err := br.ReadString('\n'); err != nil || echo != msg {
			t.Fatalf("echo = %q, %v; want %q", echo, err, msg)
		}
	}
}

// TestTLSPreviewNoExtendedConnect pins the ALPN decision's premise
// (nextProtos): the HTTP/2 preview listener must not advertise
// SETTINGS_ENABLE_CONNECT_PROTOCOL (RFC 8441), or browsers would send
// WebSockets as extended CONNECT streams the reverse proxy cannot forward.
// It reads the server's first SETTINGS frame straight off the wire — no
// HTTP/2 library — so a Go release that flips net/http's default fails here.
func TestTLSPreviewNoExtendedConnect(t *testing.T) {
	m, _, addr := tlsFixture(t)
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		ServerName: "web.lab.internal",
		RootCAs:    caPool(t, m),
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(waitTimeout))
	p := conn.ConnectionState().NegotiatedProtocol
	if extendedConnectEnabled() {
		// Go would advertise extended CONNECT: the listener must not offer h2.
		if p == "h2" {
			t.Fatalf("ALPN = h2 with GODEBUG=http2xconnect=1; want HTTP/2 refused")
		}
		return
	}
	if p != "h2" {
		t.Fatalf("ALPN = %q, want h2", p)
	}

	// Client preface, then an empty SETTINGS frame (length 0, type 4).
	_, err = io.WriteString(conn, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	must(t, err)
	_, err = conn.Write([]byte{0, 0, 0, 4, 0, 0, 0, 0, 0})
	must(t, err)

	const (
		frameSettings          = 0x4
		flagAck                = 0x1
		settingEnableConnProto = 0x8
	)
	br := bufio.NewReader(conn)
	for {
		var hdr [9]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			t.Fatalf("reading a frame header: %v", err)
		}
		length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		payload := make([]byte, length)
		if _, err := io.ReadFull(br, payload); err != nil {
			t.Fatalf("reading a frame payload: %v", err)
		}
		if hdr[3] != frameSettings || hdr[4]&flagAck != 0 {
			continue
		}
		for p := payload; len(p) >= 6; p = p[6:] {
			id, val := binary.BigEndian.Uint16(p), binary.BigEndian.Uint32(p[2:])
			if id == settingEnableConnProto && val != 0 {
				t.Fatalf("the preview listener advertises SETTINGS_ENABLE_CONNECT_PROTOCOL=%d; "+
					"browsers would send WebSockets over HTTP/2 extended CONNECT, which the proxy cannot forward — "+
					"pin ALPN to http/1.1 in nextProtos", val)
			}
		}
		return
	}
}

// TestTLSPreviewHotSwap: a renewed leaf reaches the next handshake on the
// running server — GetCertificate is the whole swap; nothing is rebuilt.
func TestTLSPreviewHotSwap(t *testing.T) {
	clock := newTestClock(time.Now())
	m, err := NewCertManager(CertOptions{
		Domain: "lab.internal", Dir: t.TempDir(), Now: clock.now, Logger: quietLogger(),
		RenewInterval: 5 * time.Millisecond,
	})
	must(t, err)
	r, _ := newTestRegistry(t)
	addr := startTLSPreview(t, r, m)
	roots := caPool(t, m)

	peerLeaf := func() []byte {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "web.lab.internal", RootCAs: roots, MinVersion: tls.VersionTLS12,
			Time: clock.now})
		if err != nil {
			t.Fatalf("TLS dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		return conn.ConnectionState().PeerCertificates[0].Raw
	}
	before := peerLeaf()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	clock.set(m.Status().NotAfter.Add(-RenewBefore + time.Hour))
	waitFor(t, "the renewed leaf on the wire", func() bool { return !bytes.Equal(peerLeaf(), before) })
}
