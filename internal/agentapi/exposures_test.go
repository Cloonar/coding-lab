package agentapi

// Exposure routes (issue #105): the pre-switch error contract on a recorder,
// and the full upgrade — register, 101, yamux tunnel, a browser request
// through the preview handler reaching the run's port, stop — over BOTH
// transports the agent API is served on (the unix socket and TCP).

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/preview"
)

// exposeFixture is a fixture with two active runs in one repo and a server
// wired to a real preview Registry.
type exposeFixture struct {
	*testFixture
	reg     *preview.Registry
	handler http.Handler
	tokenA  string
	tokenB  string
}

func newExposeFixture(t *testing.T) *exposeFixture {
	t.Helper()
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	f.seedRun(t, "run_b", "repo_a", "active")
	reg := preview.NewRegistry("lab.internal", 8081, nil)
	s := f.server()
	s.SetPreviews(reg)
	return &exposeFixture{
		testFixture: f,
		reg:         reg,
		handler:     s.Handler(),
		tokenA:      f.seedToken(t, "run_a", nil),
		tokenB:      f.seedToken(t, "run_b", nil),
	}
}

// exposeRecorded sends one exposure request through h on a recorder (only the
// pre-switch answers can be observed there: a recorder cannot be hijacked).
func exposeRecorded(h http.Handler, method, path, token, body string, upgrade bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if upgrade {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "lab-tunnel")
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func assertJSONError(t *testing.T, rr *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rr.Code, status, rr.Body.String())
	}
	if want := fmt.Sprintf(`{"error":%q}`, msg); strings.TrimSpace(rr.Body.String()) != want {
		t.Fatalf("body = %s, want %s", rr.Body.String(), want)
	}
}

func TestExposuresDisabled(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)

	unset := f.server()
	explicitNil := f.server()
	explicitNil.SetPreviews(nil)
	for name, s := range map[string]*Server{"never set": unset, "set to nil": explicitNil} {
		t.Run(name, func(t *testing.T) {
			h := s.Handler()
			assertJSONError(t, exposeRecorded(h, "POST", "/agent/v1/exposures", token, `{"name":"web","port":5173}`, true),
				http.StatusNotFound, previewsDisabledMessage)
			assertJSONError(t, exposeRecorded(h, "DELETE", "/agent/v1/exposures/web", token, "", false),
				http.StatusNotFound, previewsDisabledMessage)
		})
	}
}

func TestExposureCreateErrors(t *testing.T) {
	f := newExposeFixture(t)

	t.Run("no token", func(t *testing.T) {
		rr := exposeRecorded(f.handler, "POST", "/agent/v1/exposures", "", `{"name":"web","port":5173}`, true)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("missing upgrade", func(t *testing.T) {
		assertJSONError(t, exposeRecorded(f.handler, "POST", "/agent/v1/exposures", f.tokenA, `{"name":"web","port":5173}`, false),
			http.StatusBadRequest, "expected Upgrade: lab-tunnel")
	})

	t.Run("wrong upgrade protocol", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/agent/v1/exposures", strings.NewReader(`{"name":"web","port":5173}`))
		req.Header.Set("Authorization", "Bearer "+f.tokenA)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		rr := httptest.NewRecorder()
		f.handler.ServeHTTP(rr, req)
		assertJSONError(t, rr, http.StatusBadRequest, "expected Upgrade: lab-tunnel")
	})

	t.Run("upgrade without connection upgrade", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/agent/v1/exposures", strings.NewReader(`{"name":"web","port":5173}`))
		req.Header.Set("Authorization", "Bearer "+f.tokenA)
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Upgrade", "LAB-TUNNEL")
		rr := httptest.NewRecorder()
		f.handler.ServeHTTP(rr, req)
		assertJSONError(t, rr, http.StatusBadRequest, "expected Upgrade: lab-tunnel")
	})

	t.Run("bad json", func(t *testing.T) {
		assertJSONError(t, exposeRecorded(f.handler, "POST", "/agent/v1/exposures", f.tokenA, `{"name":`, true),
			http.StatusBadRequest, "invalid JSON body")
	})

	t.Run("invalid name", func(t *testing.T) {
		assertJSONError(t, exposeRecorded(f.handler, "POST", "/agent/v1/exposures", f.tokenA, `{"name":"Web.x","port":5173}`, true),
			http.StatusBadRequest, `invalid name "Web.x": must be a single DNS label (a-z, 0-9, '-', at most 63 characters)`)
	})

	t.Run("invalid port", func(t *testing.T) {
		assertJSONError(t, exposeRecorded(f.handler, "POST", "/agent/v1/exposures", f.tokenA, `{"name":"web","port":70000}`, true),
			http.StatusBadRequest, "invalid port 70000: must be 1–65535")
		assertJSONError(t, exposeRecorded(f.handler, "POST", "/agent/v1/exposures", f.tokenA, `{"name":"web"}`, true),
			http.StatusBadRequest, "invalid port 0: must be 1–65535")
	})

	t.Run("conflict with another run", func(t *testing.T) {
		if err := f.reg.Reserve("repo_a", "run_b", "taken", 3000); err != nil {
			t.Fatalf("reserve for run_b: %v", err)
		}
		assertJSONError(t, exposeRecorded(f.handler, "POST", "/agent/v1/exposures", f.tokenA, `{"name":"taken","port":5173}`, true),
			http.StatusConflict, `name "taken" is already exposed by another run`)
	})

	t.Run("limit", func(t *testing.T) {
		for i := range preview.MaxPerRun {
			if err := f.reg.Reserve("repo_a", "run_a", "n"+strconv.Itoa(i), 3000); err != nil {
				t.Fatalf("reserve %d: %v", i, err)
			}
		}
		assertJSONError(t, exposeRecorded(f.handler, "POST", "/agent/v1/exposures", f.tokenA, `{"name":"one-too-many","port":5173}`, true),
			http.StatusUnprocessableEntity, "exposure limit reached (8 per run)")
	})
}

func TestExposureDelete(t *testing.T) {
	f := newExposeFixture(t)

	assertJSONError(t, exposeRecorded(f.handler, "DELETE", "/agent/v1/exposures/nope", f.tokenA, "", false),
		http.StatusNotFound, `no exposure named "nope" in this run`)

	// Another run's name is just as unknown — and survives the attempt.
	if err := f.reg.Reserve("repo_a", "run_b", "theirs", 3000); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	assertJSONError(t, exposeRecorded(f.handler, "DELETE", "/agent/v1/exposures/theirs", f.tokenA, "", false),
		http.StatusNotFound, `no exposure named "theirs" in this run`)
	if got := f.reg.Exposures("run_b"); len(got) != 1 {
		t.Fatalf("run_b exposures = %+v, want its name kept", got)
	}

	if err := f.reg.Reserve("repo_a", "run_a", "web", 5173); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	rr := exposeRecorded(f.handler, "DELETE", "/agent/v1/exposures/web", f.tokenA, "", false)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rr.Code, rr.Body.String())
	}
	if got := f.reg.Exposures("run_a"); len(got) != 0 {
		t.Fatalf("run_a exposures after stop = %+v, want none", got)
	}
}

// TestExposureTunnelEndToEnd drives the whole path over each transport:
// labctl's half is preview.ServeTunnel on the 101 body, the browser's is a
// request through Registry.Handler(), and the run's port is an upstream
// httptest server on 127.0.0.1.
func TestExposureTunnelEndToEnd(t *testing.T) {
	transports := map[string]func(t *testing.T, h http.Handler) func(ctx context.Context, network, addr string) (net.Conn, error){
		"unix socket": func(t *testing.T, h http.Handler) func(ctx context.Context, network, addr string) (net.Conn, error) {
			path := SocketPath(socketDir(t))
			ln := mustListen(t, path)
			srv := &http.Server{Handler: h, ReadHeaderTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })
			return func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			}
		},
		"tcp": func(t *testing.T, h http.Handler) func(ctx context.Context, network, addr string) (net.Conn, error) {
			ts := httptest.NewUnstartedServer(h)
			// Timeouts far shorter than the test: the tunnel must not
			// inherit the server's deadlines.
			ts.Config.ReadHeaderTimeout = 100 * time.Millisecond
			ts.Config.ReadTimeout = 100 * time.Millisecond
			ts.Config.WriteTimeout = 100 * time.Millisecond
			ts.Start()
			t.Cleanup(ts.Close)
			addr := ts.Listener.Addr().String()
			return func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", addr)
			}
		},
	}

	for name, serve := range transports {
		t.Run(name, func(t *testing.T) {
			f := newExposeFixture(t)
			dial := serve(t, f.handler)
			client := &http.Client{Transport: &http.Transport{DialContext: dial}}

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, "upstream saw %s %s host=%s", r.Method, r.URL.Path, r.Host)
			}))
			t.Cleanup(upstream.Close)
			u, _ := url.Parse(upstream.URL)
			port, _ := strconv.Atoi(u.Port())

			req, _ := http.NewRequest("POST", "http://lab/agent/v1/exposures",
				strings.NewReader(fmt.Sprintf(`{"name":"web","port":%d}`, port)))
			req.Header.Set("Authorization", "Bearer "+f.tokenA)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "lab-tunnel")
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("upgrade request: %v", err)
			}
			if resp.StatusCode != http.StatusSwitchingProtocols {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 101 (body %s)", resp.StatusCode, body)
			}
			if got := resp.Header.Get("Lab-Preview-URL"); got != "http://web.lab.internal:8081" {
				t.Fatalf("Lab-Preview-URL = %q", got)
			}
			if got := resp.Header.Get("Upgrade"); got != "lab-tunnel" {
				t.Fatalf("Upgrade = %q", got)
			}
			rwc, ok := resp.Body.(io.ReadWriteCloser)
			if !ok {
				t.Fatalf("101 body is %T, not an io.ReadWriteCloser", resp.Body)
			}

			type result struct {
				ctl preview.Control
				err error
			}
			done := make(chan result, 1)
			go func() {
				ctl, err := preview.ServeTunnel(context.Background(), rwc, port)
				done <- result{ctl, err}
			}()

			waitLive(t, f.reg, "run_a", true)
			// Outlive the server's timeouts: a deadline left on the hijacked
			// connection would have killed the tunnel by now.
			time.Sleep(300 * time.Millisecond)

			preq := httptest.NewRequest("GET", "/hello", nil)
			preq.Host = "web.lab.internal:8081"
			prr := httptest.NewRecorder()
			f.reg.Handler().ServeHTTP(prr, preq)
			if prr.Code != http.StatusOK || prr.Body.String() != "upstream saw GET /hello host=web.lab.internal:8081" {
				t.Fatalf("preview answer = %d %q", prr.Code, prr.Body.String())
			}

			// Stop over the same transport: 204, and ServeTunnel hears "stopped".
			dreq, _ := http.NewRequest("DELETE", "http://lab/agent/v1/exposures/web", nil)
			dreq.Header.Set("Authorization", "Bearer "+f.tokenA)
			dresp, err := client.Do(dreq)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			_ = dresp.Body.Close()
			if dresp.StatusCode != http.StatusNoContent {
				t.Fatalf("delete status = %d, want 204", dresp.StatusCode)
			}
			select {
			case r := <-done:
				if r.err != nil || r.ctl != preview.ControlStopped {
					t.Fatalf("ServeTunnel = (%q, %v), want (stopped, nil)", r.ctl, r.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("ServeTunnel did not return after stop")
			}
		})
	}
}

// waitLive polls until runID's single exposure is (or is no longer) live.
func waitLive(t *testing.T, reg *preview.Registry, runID string, live bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		exps := reg.Exposures(runID)
		if len(exps) == 1 && exps[0].Live == live {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("exposures of %s = %+v, want one with live=%v", runID, exps, live)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
