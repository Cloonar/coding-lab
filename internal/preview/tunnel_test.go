package preview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// waitTimeout bounds every wait in these tests; a hang is a failure, not a
// stuck CI job.
const waitTimeout = 5 * time.Second

// tunnelResult is what ServeTunnel returned.
type tunnelResult struct {
	ctl Control
	err error
}

// testTunnel is one live tunnel between a Registry (lab's side) and
// ServeTunnel (labctl's side) over a net.Pipe.
type testTunnel struct {
	labEnd   net.Conn           // the pipe end Attach runs on
	agentEnd net.Conn           // the pipe end ServeTunnel runs on
	cancel   context.CancelFunc // cancels ServeTunnel's ctx
	attached chan error         // Attach's return value
	served   chan tunnelResult  // ServeTunnel's return values
}

// startTunnel reserves name for runID on port and attaches a tunnel to it
// whose labctl side forwards to 127.0.0.1:port. It waits until the exposure
// is live. Cleanup tears both sides down and waits for both goroutines, so
// no test leaks a session.
func startTunnel(t *testing.T, r *Registry, runID, name string, port int) *testTunnel {
	t.Helper()
	mustReserve(t, r, "repo", runID, name, port)

	labEnd, agentEnd := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	tt := &testTunnel{
		labEnd:   labEnd,
		agentEnd: agentEnd,
		cancel:   cancel,
		attached: make(chan error, 1),
		served:   make(chan tunnelResult, 1),
	}
	// Watch for this tunnel's own install: a replacing tunnel finds the name
	// already live from its predecessor, so "live" alone proves nothing.
	before := r.liveTunnel(name)
	go func() { tt.attached <- r.Attach(runID, name, labEnd) }()
	go func() {
		ctl, err := ServeTunnel(ctx, agentEnd, port)
		tt.served <- tunnelResult{ctl, err}
	}()
	t.Cleanup(func() {
		cancel()
		_ = labEnd.Close()
		_ = agentEnd.Close()
		tt.waitAttach(t)
		tt.waitServed(t)
	})

	waitFor(t, "tunnel to go live", func() bool {
		cur := r.liveTunnel(name)
		return cur != nil && cur != before
	})
	return tt
}

// waitAttach waits for Attach to return (at most once; later calls return
// the cached value) and asserts it returned nil.
func (tt *testTunnel) waitAttach(t *testing.T) {
	t.Helper()
	select {
	case err, ok := <-tt.attached:
		if ok {
			close(tt.attached)
			if err != nil {
				t.Errorf("Attach returned %v, want nil", err)
			}
		}
	case <-time.After(waitTimeout):
		t.Fatalf("Attach did not return")
	}
}

// waitServed waits for ServeTunnel to return and yields its result; a
// second call yields the zero result.
func (tt *testTunnel) waitServed(t *testing.T) tunnelResult {
	t.Helper()
	select {
	case res, ok := <-tt.served:
		if ok {
			close(tt.served)
		}
		return res
	case <-time.After(waitTimeout):
		t.Fatalf("ServeTunnel did not return")
		return tunnelResult{}
	}
}

// liveTunnel returns name's installed tunnel, or nil. Test-only peek.
func (r *Registry) liveTunnel(name string) *tunnel {
	tun, _, _ := r.lookup(name)
	return tun
}

// waitFor polls cond until it holds or waitTimeout passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// isLive reports the Live flag of runID's exposure called name.
func isLive(r *Registry, runID, name string) bool {
	for _, e := range r.Exposures(runID) {
		if e.Name == name {
			return e.Live
		}
	}
	return false
}

// newUpstream starts an HTTP server on 127.0.0.1 (tcp4 — the only address
// labctl dials) and returns it with its port.
func newUpstream(t *testing.T, h http.Handler) (*httptest.Server, int) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := httptest.NewUnstartedServer(h)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, ln.Addr().(*net.TCPAddr).Port
}

// freePort returns a 127.0.0.1 port that nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// newPreviewListener serves r's Handler on a real listener, as cmd/lab does.
func newPreviewListener(t *testing.T, r *Registry) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// previewGet sends one GET through the preview listener with the browser
// Host host, plus any extra request headers.
func previewGet(t *testing.T, srv *httptest.Server, host string, hdr http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/hello?x=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	client := &http.Client{Timeout: waitTimeout, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET via preview: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp, string(body)
}

func TestProxyReachesUpstream(t *testing.T) {
	seen := make(chan *http.Request, 1)
	_, port := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen <- req.Clone(context.Background())
		// Connection-listed headers cannot be tested on the response: the
		// upstream's own server rewrites Connection to "close" for the
		// keepalive-less proxy transport. A fixed hop-by-hop header can.
		w.Header().Set("Proxy-Authenticate", "Basic")
		w.Header().Set("X-Upstream", "kept")
		_, _ = io.WriteString(w, "hello from the run")
	}))
	r, log := newTestRegistry(t)
	startTunnel(t, r, "run1", "web", port)
	if !isLive(r, "run1", "web") {
		t.Fatalf("exposure not live after Attach")
	}
	if log.count() < 2 {
		t.Errorf("onChange calls = %d, want reserve + attach", log.count())
	}
	srv := newPreviewListener(t, r)

	resp, body := previewGet(t, srv, "web.lab.internal:8081", http.Header{
		"Connection":          {"X-Client-Hop"},
		"X-Client-Hop":        {"1"},
		"Proxy-Authorization": {"Basic Zm9vOmJhcg=="},
		"X-Forwarded-For":     {"6.6.6.6"},
		"X-Forwarded-Host":    {"forged.example"},
	})
	if resp.StatusCode != http.StatusOK || body != "hello from the run" {
		t.Fatalf("response = %d %q, want 200 from the upstream", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Upstream") != "kept" {
		t.Errorf("end-to-end response header lost")
	}
	if resp.Header.Get("Proxy-Authenticate") != "" {
		t.Errorf("hop-by-hop response header Proxy-Authenticate reached the browser")
	}

	up := <-seen
	if up.Host != "web.lab.internal:8081" {
		t.Errorf("upstream Host = %q, want the browser's web.lab.internal:8081", up.Host)
	}
	if up.URL.RequestURI() != "/hello?x=1" {
		t.Errorf("upstream request URI = %q, want /hello?x=1", up.URL.RequestURI())
	}
	if got := up.Header.Get("X-Forwarded-Host"); got != "web.lab.internal:8081" {
		t.Errorf("X-Forwarded-Host = %q, want web.lab.internal:8081 (not the forged one)", got)
	}
	if got := up.Header.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http", got)
	}
	if got := up.Header.Get("X-Forwarded-For"); got != "127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q, want 127.0.0.1 only (forged hop dropped)", got)
	}
	for _, h := range []string{"X-Client-Hop", "Proxy-Authorization"} {
		if up.Header.Get(h) != "" {
			t.Errorf("hop-by-hop request header %s reached the upstream", h)
		}
	}
}

func TestProxyNothingListening(t *testing.T) {
	r, _ := newTestRegistry(t)
	port := freePort(t)
	startTunnel(t, r, "run1", "web", port)
	srv := newPreviewListener(t, r)

	resp, body := previewGet(t, srv, "web.lab.internal", nil)
	assertPage(t, resp, body, http.StatusBadGateway, "nothing is listening on port "+strconv.Itoa(port))

	// A refused dial is per request: the tunnel itself stays up.
	if !isLive(r, "run1", "web") {
		t.Errorf("exposure went not-live after a refused dial")
	}
}

// upgradeEcho is an upstream that accepts `Upgrade: echo-test`, greets
// first (proving the upstream can talk unprompted, as an HMR socket does),
// then echoes every byte back — the shape of a WebSocket without needing a
// WebSocket library.
func upgradeEcho(w http.ResponseWriter, req *http.Request) {
	if !strings.EqualFold(req.Header.Get("Upgrade"), "echo-test") {
		http.Error(w, "want Upgrade: echo-test", http.StatusBadRequest)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo-test\r\n\r\nhello\n")
	if brw.Flush() != nil {
		return
	}
	_, _ = io.Copy(conn, brw)
}

func TestProxyUpgradeRoundTrip(t *testing.T) {
	_, port := newUpstream(t, http.HandlerFunc(upgradeEcho))
	r, _ := newTestRegistry(t)
	startTunnel(t, r, "run1", "web", port)
	srv := newPreviewListener(t, r)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial preview listener: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(waitTimeout))
	_, err = io.WriteString(conn, "GET /hmr HTTP/1.1\r\nHost: web.lab.internal:8081\r\nConnection: Upgrade\r\nUpgrade: echo-test\r\n\r\n")
	if err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); got != "echo-test" {
		t.Errorf("Upgrade response header = %q, want echo-test", got)
	}

	greeting, err := br.ReadString('\n')
	if err != nil || greeting != "hello\n" {
		t.Fatalf("greeting = %q, %v; want the upstream's hello", greeting, err)
	}
	for _, msg := range []string{"ping\n", "second message\n"} {
		if _, err := io.WriteString(conn, msg); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		echo, err := br.ReadString('\n')
		if err != nil || echo != msg {
			t.Fatalf("echo = %q, %v; want %q", echo, err, msg)
		}
	}
}

func TestStopDeliversStopped(t *testing.T) {
	_, port := newUpstream(t, http.NotFoundHandler())
	r, log := newTestRegistry(t)
	tt := startTunnel(t, r, "run1", "web", port)

	before := log.count()
	if err := r.Stop("run1", "web"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	res := tt.waitServed(t)
	if res.ctl != ControlStopped || res.err != nil {
		t.Errorf("ServeTunnel = (%q, %v), want (stopped, nil)", res.ctl, res.err)
	}
	tt.waitAttach(t)
	// Stop's own change only: the dying session must not announce a detach
	// on a name that is no longer there.
	if n := log.count(); n != before+1 {
		t.Errorf("onChange calls after Stop = %d, want %d", n, before+1)
	}
	if got := r.Exposures("run1"); len(got) != 0 {
		t.Errorf("Exposures after Stop = %v, want none", got)
	}
	resp, body := previewGet(t, newPreviewListener(t, r), "web.lab.internal", nil)
	assertPage(t, resp, body, http.StatusNotFound, "No such preview")
}

func TestCloseRunDeliversRunEnded(t *testing.T) {
	_, port := newUpstream(t, http.NotFoundHandler())
	r, _ := newTestRegistry(t)
	web := startTunnel(t, r, "run1", "web", port)
	api := startTunnel(t, r, "run1", "api", port)

	r.CloseRun("run1")
	for name, tt := range map[string]*testTunnel{"web": web, "api": api} {
		res := tt.waitServed(t)
		if res.ctl != ControlRunEnded || res.err != nil {
			t.Errorf("%s: ServeTunnel = (%q, %v), want (run-ended, nil)", name, res.ctl, res.err)
		}
		tt.waitAttach(t)
	}
	if got := r.Exposures("run1"); len(got) != 0 {
		t.Errorf("Exposures after CloseRun = %v, want none", got)
	}
	mustReserve(t, r, "repo", "run2", "web", port)
}

func TestSecondAttachReplacesFirst(t *testing.T) {
	_, port := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	r, _ := newTestRegistry(t)
	first := startTunnel(t, r, "run1", "web", port)
	second := startTunnel(t, r, "run1", "web", port)

	res := first.waitServed(t)
	if res.ctl != ControlReplaced || res.err != nil {
		t.Errorf("first ServeTunnel = (%q, %v), want (replaced, nil)", res.ctl, res.err)
	}
	first.waitAttach(t)

	// The replacement is the live one, and the first's death did not
	// un-live it.
	if !isLive(r, "run1", "web") {
		t.Fatalf("exposure not live after the replaced session died")
	}
	resp, body := previewGet(t, newPreviewListener(t, r), "web.lab.internal", nil)
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Errorf("response via the replacement = %d %q, want 200 ok", resp.StatusCode, body)
	}
	select {
	case res := <-second.served:
		t.Errorf("second ServeTunnel returned early: (%q, %v)", res.ctl, res.err)
	default:
	}
}

func TestDroppedConnectionKeepsReservation(t *testing.T) {
	_, port := newUpstream(t, http.NotFoundHandler())
	r, log := newTestRegistry(t)
	tt := startTunnel(t, r, "run1", "web", port)

	before := log.count()
	_ = tt.labEnd.Close() // lab restarts / the connection dies
	res := tt.waitServed(t)
	if res.ctl != "" || !errors.Is(res.err, ErrTunnelDropped) {
		t.Errorf("ServeTunnel = (%q, %v), want (\"\", ErrTunnelDropped)", res.ctl, res.err)
	}
	tt.waitAttach(t)

	got := r.Exposures("run1")
	if len(got) != 1 || got[0].Name != "web" || got[0].Live {
		t.Fatalf("Exposures after drop = %+v, want web reserved and not live", got)
	}
	if n := log.count(); n != before+1 {
		t.Errorf("onChange calls after drop = %d, want %d (the detach)", n, before+1)
	}
	resp, body := previewGet(t, newPreviewListener(t, r), "web.lab.internal", nil)
	assertPage(t, resp, body, http.StatusServiceUnavailable, "reconnecting")

	// Another run still cannot take the name while labctl reconnects.
	if err := r.Reserve("repo", "run2", "web", port); !errors.Is(err, ErrConflict) {
		t.Errorf("other run during reconnect: err = %v, want ErrConflict", err)
	}
	// The holder reattaches.
	startTunnel(t, r, "run1", "web", port)
	if !isLive(r, "run1", "web") {
		t.Errorf("exposure not live after reattach")
	}
}

func TestServeTunnelContextCancel(t *testing.T) {
	_, port := newUpstream(t, http.NotFoundHandler())
	r, _ := newTestRegistry(t)
	tt := startTunnel(t, r, "run1", "web", port)

	tt.cancel()
	res := tt.waitServed(t)
	if res.ctl != "" || !errors.Is(res.err, context.Canceled) {
		t.Errorf("ServeTunnel = (%q, %v), want (\"\", context.Canceled)", res.ctl, res.err)
	}
	tt.waitAttach(t)
	if isLive(r, "run1", "web") {
		t.Errorf("exposure still live after labctl went away")
	}
}

func TestAttachWithoutReserve(t *testing.T) {
	r, _ := newTestRegistry(t)
	mustReserve(t, r, "repo", "run1", "web", 5173)
	for _, tc := range []struct{ runID, name string }{
		{"run1", "other"}, // never reserved
		{"run2", "web"},   // held by another run
	} {
		labEnd, agentEnd := net.Pipe()
		if err := r.Attach(tc.runID, tc.name, labEnd); !errors.Is(err, ErrUnknown) {
			t.Errorf("Attach(%q, %q) = %v, want ErrUnknown", tc.runID, tc.name, err)
		}
		// Attach owns conn: it closed the lab end.
		_ = agentEnd.SetReadDeadline(time.Now().Add(waitTimeout))
		if _, err := agentEnd.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("Attach(%q, %q) left conn open: read err = %v, want EOF", tc.runID, tc.name, err)
		}
		_ = agentEnd.Close()
	}
}

func TestServeTunnelRejectsInvalidPort(t *testing.T) {
	labEnd, agentEnd := net.Pipe()
	defer func() { _ = labEnd.Close() }()
	ctl, err := ServeTunnel(context.Background(), agentEnd, 0)
	if ctl != "" || !errors.Is(err, ErrInvalidPort) {
		t.Errorf("ServeTunnel(port 0) = (%q, %v), want ErrInvalidPort", ctl, err)
	}
}

// Many concurrent requests share one tunnel, each on its own stream.
func TestProxyConcurrentRequests(t *testing.T) {
	_, port := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, req.URL.Query().Get("i"))
	}))
	r, _ := newTestRegistry(t)
	startTunnel(t, r, "run1", "web", port)
	srv := newPreviewListener(t, r)

	client := &http.Client{Timeout: waitTimeout}
	errs := make(chan error, 20)
	for i := range 20 {
		go func() {
			req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/?i=%d", srv.URL, i), nil)
			req.Host = "web.lab.internal"
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if string(body) != strconv.Itoa(i) {
				errs <- fmt.Errorf("request %d got %q", i, body)
				return
			}
			errs <- nil
		}()
	}
	for range 20 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	client.CloseIdleConnections()
}
