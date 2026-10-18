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
	"sync"
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
	if err := r.Reserve("repo", "run2", "web", "s2", port); !errors.Is(err, ErrConflict) {
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

// attachVanished runs Attach for (runID, name) — which the caller has made
// sure is not reserved — against a real ServeTunnel, and returns what each
// side returned.
func attachVanished(t *testing.T, r *Registry, runID, name string) (res tunnelResult, attachErr error) {
	t.Helper()
	labEnd, agentEnd := net.Pipe()
	attached := make(chan error, 1)
	go func() { attached <- r.Attach(runID, name, labEnd) }()
	served := make(chan tunnelResult, 1)
	go func() {
		ctl, err := ServeTunnel(context.Background(), agentEnd, 1)
		served <- tunnelResult{ctl, err}
	}()
	for _, wait := range []func(){
		func() { attachErr = <-attached },
		func() { res = <-served },
	} {
		done := make(chan struct{})
		go func() { wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			_ = labEnd.Close()
			_ = agentEnd.Close()
			t.Fatalf("Attach(%q, %q) on a vanished reservation did not finish", runID, name)
		}
	}
	return res, attachErr
}

// An Attach that finds no reservation for its run — never made, held by
// another run, or released since Reserve by Stop or CloseRun (the agent API's
// Reserve → 101 → Attach is not atomic) — returns ErrUnknown AND gives labctl
// a final control message. A bare close would read as a dropped connection
// and send labctl reconnecting into a name that is gone.
func TestAttachVanishedReservation(t *testing.T) {
	r, _ := newTestRegistry(t)
	mustReserve(t, r, "repo", "run1", "web", 5173)

	mustReserve(t, r, "repo", "run3", "stopped", 5173)
	if err := r.Stop("run3", "stopped"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	mustReserve(t, r, "repo", "run4", "ended", 5173)
	r.CloseRun("run4")

	for _, tc := range []struct {
		why, runID, name string
		want             Control
	}{
		{"never reserved", "run1", "other", ControlStopped},
		{"held by another run", "run2", "web", ControlStopped},
		{"stopped since Reserve", "run3", "stopped", ControlStopped},
		{"run ended since Reserve", "run4", "ended", ControlRunEnded},
	} {
		t.Run(tc.why, func(t *testing.T) {
			res, attachErr := attachVanished(t, r, tc.runID, tc.name)
			if !errors.Is(attachErr, ErrUnknown) {
				t.Errorf("Attach = %v, want ErrUnknown", attachErr)
			}
			if res.ctl != tc.want || res.err != nil {
				t.Errorf("ServeTunnel = (%q, %v), want (%q, nil)", res.ctl, res.err, tc.want)
			}
		})
	}
	// The vanished attaches installed nothing anywhere.
	if isLive(r, "run1", "web") {
		t.Errorf("run1's reservation went live from another run's Attach")
	}
}

// wedgeConn is lab's end of a tunnel whose writes can be made to block, as
// they do on a connection whose peer stopped reading (a suspended laptop, a
// wedged labctl): after wedge, Write parks until the conn is closed.
type wedgeConn struct {
	net.Conn
	wedged    chan struct{}
	wedgeOnce sync.Once
	closed    chan struct{}
	closeOnce sync.Once
}

func newWedgeConn(c net.Conn) *wedgeConn {
	return &wedgeConn{Conn: c, wedged: make(chan struct{}), closed: make(chan struct{})}
}

func (c *wedgeConn) wedge() { c.wedgeOnce.Do(func() { close(c.wedged) }) }

func (c *wedgeConn) Write(p []byte) (int, error) {
	select {
	case <-c.wedged:
		<-c.closed
		return 0, net.ErrClosed
	default:
	}
	return c.Conn.Write(p)
}

func (c *wedgeConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// startWedgeableTunnel is startTunnel with lab's end wrapped in a wedgeConn,
// and the Registry's control-write bound set to writeTimeout first.
func startWedgeableTunnel(t *testing.T, r *Registry, runID, name string, port int, writeTimeout time.Duration) (*testTunnel, *wedgeConn) {
	t.Helper()
	r.mu.Lock()
	r.controlWriteTimeout = writeTimeout
	r.mu.Unlock()
	mustReserve(t, r, "repo", runID, name, port)

	labPipe, agentEnd := net.Pipe()
	labEnd := newWedgeConn(labPipe)
	ctx, cancel := context.WithCancel(context.Background())
	tt := &testTunnel{
		labEnd:   labEnd,
		agentEnd: agentEnd,
		cancel:   cancel,
		attached: make(chan error, 1),
		served:   make(chan tunnelResult, 1),
	}
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
	waitFor(t, "tunnel to go live", func() bool { return r.liveTunnel(name) != nil })
	return tt, labEnd
}

// Stop and CloseRun change the registry synchronously but never wait on the
// peer: with lab's writes wedged and a control-write bound far beyond the
// test's patience, both still return at once, with the name already free.
func TestStopAndCloseRunDoNotWaitOnPeer(t *testing.T) {
	for _, op := range []string{"Stop", "CloseRun"} {
		t.Run(op, func(t *testing.T) {
			_, port := newUpstream(t, http.NotFoundHandler())
			r, _ := newTestRegistry(t)
			tt, labEnd := startWedgeableTunnel(t, r, "run1", "web", port, time.Hour)
			labEnd.wedge()

			returned := make(chan error, 1)
			go func() {
				if op == "Stop" {
					returned <- r.Stop("run1", "web")
					return
				}
				r.CloseRun("run1")
				returned <- nil
			}()
			select {
			case err := <-returned:
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
			case <-time.After(waitTimeout):
				t.Fatalf("%s blocked on a peer that stopped reading", op)
			}
			if got := r.Exposures("run1"); len(got) != 0 {
				t.Errorf("Exposures right after %s = %v, want none", op, got)
			}
			mustReserve(t, r, "repo", "run2", "web", port)

			// Unwedge by closing: the parked control write fails, the
			// session ends, and labctl sees a drop (its reconnect would meet
			// the registry state set above).
			_ = labEnd.Close()
			if res := tt.waitServed(t); res.ctl != "" || !errors.Is(res.err, ErrTunnelDropped) {
				t.Errorf("ServeTunnel = (%q, %v), want a drop", res.ctl, res.err)
			}
			tt.waitAttach(t)
		})
	}
}

// tunnel.end is bounded by the Registry's control-write timeout, not by
// yamux's 10s ConnectionWriteTimeout: with lab's writes wedged, the session
// still closes — and Attach returns — well within waitTimeout.
func TestEndBoundedOnWedgedPeer(t *testing.T) {
	_, port := newUpstream(t, http.NotFoundHandler())
	r, _ := newTestRegistry(t)
	tt, labEnd := startWedgeableTunnel(t, r, "run1", "web", port, 50*time.Millisecond)
	labEnd.wedge()

	if err := r.Stop("run1", "web"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	tt.waitAttach(t) // fails if end waited out ConnectionWriteTimeout (10s)
	_ = tt.agentEnd.Close()
	tt.waitServed(t)
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
