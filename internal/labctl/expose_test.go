package labctl

// `labctl expose` against the REAL agent API wired to a REAL preview
// Registry: the URL line, a browser request through Registry.Handler()
// reaching a local upstream, the reconnect loop, every final outcome and its
// exit code, the signal path, and both LAB_URL transports.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/agentapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/preview"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// syncBuffer is a goroutine-safe output sink: Run writes from its own
// goroutine while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// recordingPreviews is the real Registry with every Attach's connection
// handed to the test, so it can drop a tunnel from lab's side.
type recordingPreviews struct {
	*preview.Registry
	mu       sync.Mutex
	attaches int
	conns    chan net.Conn
}

func (p *recordingPreviews) Attach(runID, name string, conn net.Conn) error {
	p.mu.Lock()
	p.attaches++
	p.mu.Unlock()
	p.conns <- conn
	return p.Registry.Attach(runID, name, conn)
}

func (p *recordingPreviews) attachCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attaches
}

// newExposeFixture is the builtin fixture with previews enabled on a
// registry for "lab.internal" behind port 8081.
func newExposeFixture(t *testing.T) (*agentFixture, *recordingPreviews) {
	t.Helper()
	p := &recordingPreviews{
		Registry: preview.NewRegistry("lab.internal", 8081, nil),
		conns:    make(chan net.Conn, 32),
	}
	var st *store.Store
	f := newAgentFixture(t, store.TrackerBindingBuiltin, builtinResolver(&st),
		func(s *agentapi.Server) { s.SetPreviews(p) })
	st = f.st
	return f, p
}

// fastReconnect shrinks the reconnect backoff for one test.
func fastReconnect(t *testing.T) {
	t.Helper()
	initial, maxWait := reconnectInitial, reconnectMax
	reconnectInitial, reconnectMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { reconnectInitial, reconnectMax = initial, maxWait })
}

// upstreamPort starts the "dev server" on 127.0.0.1 and returns its port.
func upstreamPort(t *testing.T) int {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "dev server: %s host=%s", r.URL.Path, r.Host)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	port, _ := strconv.Atoi(u.Port())
	return port
}

// exposeProc is one `labctl expose` running in the background.
type exposeProc struct {
	stdout, stderr *syncBuffer
	sigs           chan os.Signal
	done           chan int
}

func startExpose(t *testing.T, env map[string]string, args ...string) *exposeProc {
	t.Helper()
	p := &exposeProc{stdout: &syncBuffer{}, stderr: &syncBuffer{}, sigs: make(chan os.Signal, 1), done: make(chan int, 1)}
	go func() {
		p.done <- Run(append([]string{"expose"}, args...), Env{
			Getenv:  func(k string) string { return env[k] },
			Stdout:  p.stdout,
			Stderr:  p.stderr,
			Version: "test",
			Signals: p.sigs,
		})
	}()
	t.Cleanup(func() {
		select {
		case p.sigs <- syscall.SIGTERM:
		default:
		}
		select {
		case code := <-p.done:
			p.done <- code
		case <-time.After(10 * time.Second):
			t.Error("labctl expose did not exit at cleanup")
		}
	})
	return p
}

// wait returns the exit code, failing the test if Run does not return.
func (p *exposeProc) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-p.done:
		p.done <- code
		return code
	case <-time.After(10 * time.Second):
		t.Fatalf("labctl expose did not exit (stdout %q, stderr %q)", p.stdout.String(), p.stderr.String())
		return -1
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitLive waits until the run's exposure name is attached.
func waitLive(t *testing.T, reg *recordingPreviews, runID, name string) {
	t.Helper()
	waitFor(t, name+" live", func() bool {
		for _, e := range reg.Exposures(runID) {
			if e.Name == name && e.Live {
				return true
			}
		}
		return false
	})
}

// browse sends one browser request for path to the preview host name.
func browse(reg *recordingPreviews, name, path string) (int, string) {
	req := httptest.NewRequest("GET", path, nil)
	req.Host = name + ".lab.internal:8081"
	rr := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func TestExposeArguments(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStderr string
	}{
		{"no args", []string{"expose"}, agentEnv, 2, "want <name> <port>"},
		{"name only", []string{"expose", "web"}, agentEnv, 2, "want <name> <port>"},
		{"too many", []string{"expose", "web", "1", "2"}, agentEnv, 2, "want <name> <port>"},
		{"stop no name", []string{"expose", "stop"}, agentEnv, 2, "want <name>"},
		{"stop too many", []string{"expose", "stop", "a", "b"}, agentEnv, 2, "want <name>"},
		{"invalid name", []string{"expose", "Web", "5173"}, agentEnv, 1,
			`labctl expose: invalid name "Web": must be a single DNS label (a-z, 0-9, '-', at most 63 characters)`},
		{"dotted name", []string{"expose", "a.b", "5173"}, agentEnv, 1, `invalid name "a.b"`},
		{"port not an integer", []string{"expose", "web", "http"}, agentEnv, 1, `invalid port "http": must be an integer 1–65535`},
		{"port zero", []string{"expose", "web", "0"}, agentEnv, 1, "invalid port 0: must be 1–65535"},
		{"port too big", []string{"expose", "web", "65536"}, agentEnv, 1, "invalid port 65536: must be 1–65535"},
		{"stop invalid name", []string{"expose", "stop", "-web"}, agentEnv, 1, `labctl expose stop: invalid name "-web"`},
		{"missing env", []string{"expose", "web", "5173"}, nil, 2, "LAB_URL and LAB_TOKEN must be set"},
		{"stop missing env", []string{"expose", "stop", "web"}, nil, 2, "LAB_URL and LAB_TOKEN must be set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(t, tt.args, tt.env)
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, tt.wantCode, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

// TestExposeServesAndStops is the happy path over both LAB_URL transports:
// the URL is the first (and only) stdout line, a browser request reaches the
// run's port, and `expose stop` ends the foreground command with exit 0.
func TestExposeServesAndStops(t *testing.T) {
	for _, transport := range []string{"tcp", "unix"} {
		t.Run(transport, func(t *testing.T) {
			f, reg := newExposeFixture(t)
			env := f.env()
			if transport == "unix" {
				env = udsEnv(t, f)
			}
			port := upstreamPort(t)

			p := startExpose(t, env, "web", strconv.Itoa(port))
			waitFor(t, "the URL line", func() bool { return strings.Contains(p.stdout.String(), "\n") })
			if got := p.stdout.String(); got != "https://web.lab.internal:8081\n" {
				t.Fatalf("stdout = %q, want exactly the URL line", got)
			}
			waitLive(t, reg, f.runID, "web")

			code, body := browse(reg, "web", "/index.html")
			if code != http.StatusOK || body != "dev server: /index.html host=web.lab.internal:8081" {
				t.Fatalf("preview answer = %d %q", code, body)
			}

			stopCode, stopOut, stopErr := run(t, []string{"expose", "stop", "web"}, env)
			if stopCode != 0 || stopOut != "" || stopErr != "" {
				t.Fatalf("expose stop = %d, stdout %q, stderr %q; want a silent 0", stopCode, stopOut, stopErr)
			}
			if code := p.wait(t); code != 0 {
				t.Fatalf("expose exit = %d, want 0 (stderr %q)", code, p.stderr.String())
			}
			if !strings.Contains(p.stderr.String(), `exposure "web" stopped`) {
				t.Errorf("stderr = %q, want the stopped note", p.stderr.String())
			}
			if code, _ := browse(reg, "web", "/"); code != http.StatusNotFound {
				t.Errorf("preview after stop = %d, want 404", code)
			}
		})
	}
}

func TestExposeRefusals(t *testing.T) {
	t.Run("previews disabled", func(t *testing.T) {
		f := newBuiltinFixture(t)
		for _, args := range [][]string{{"expose", "web", "5173"}, {"expose", "stop", "web"}} {
			code, stdout, stderr := run(t, args, f.env())
			if code != 1 || stdout != "" {
				t.Fatalf("%v: exit = %d, stdout %q; want 1 and no URL", args, code, stdout)
			}
			if !strings.Contains(stderr, "previews are not enabled on this lab (no preview listener configured)") {
				t.Errorf("%v: stderr = %q, want the disabled message", args, stderr)
			}
		}
	})

	t.Run("name held by another run", func(t *testing.T) {
		f, reg := newExposeFixture(t)
		if err := reg.Reserve(f.repo, "run_other", "web", "other-session", 3000); err != nil {
			t.Fatalf("reserve for the other run: %v", err)
		}
		code, stdout, stderr := run(t, []string{"expose", "web", "5173"}, f.env())
		if code != 1 || stdout != "" {
			t.Fatalf("exit = %d, stdout %q; want 1 and no URL", code, stdout)
		}
		if !strings.Contains(stderr, `labctl expose: name "web" is already exposed by another run`) {
			t.Errorf("stderr = %q, want the 409 message", stderr)
		}
	})

	t.Run("lab unreachable on the first attempt", func(t *testing.T) {
		env := map[string]string{"LAB_URL": "unix://" + udsSocketPath(t), "LAB_TOKEN": "lab_run_x"}
		code, stdout, stderr := run(t, []string{"expose", "web", "5173"}, env)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "labctl expose: ") {
			t.Fatalf("exit = %d, stdout %q, stderr %q; want 1 with the transport error", code, stdout, stderr)
		}
	})

	t.Run("stop unknown name", func(t *testing.T) {
		f, _ := newExposeFixture(t)
		code, _, stderr := run(t, []string{"expose", "stop", "nope"}, f.env())
		if code != 1 || !strings.Contains(stderr, `labctl expose stop: no exposure named "nope" in this run`) {
			t.Fatalf("exit = %d, stderr %q; want 1 with the 404 message", code, stderr)
		}
	})
}

// TestExposeReconnects drops the tunnel from lab's side: labctl re-registers
// through a transient 503 and reattaches, the URL is not printed again, the
// preview works again, and a later stop is still a clean 0.
func TestExposeReconnects(t *testing.T) {
	fastReconnect(t)
	f, reg := newExposeFixture(t)

	var mu sync.Mutex
	posts := 0
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/agent/v1/exposures" {
			mu.Lock()
			posts++
			n := posts
			mu.Unlock()
			if n == 2 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"lab is restarting"}`)
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(flaky.Close)
	env := map[string]string{"LAB_URL": flaky.URL, "LAB_TOKEN": f.token}
	port := upstreamPort(t)

	p := startExpose(t, env, "web", strconv.Itoa(port))
	waitLive(t, reg, f.runID, "web")
	first := <-reg.conns
	_ = first.Close() // the connection drops (lab restart, network blip)

	waitFor(t, "the second attach", func() bool { return reg.attachCount() == 2 })
	waitLive(t, reg, f.runID, "web")
	if code, body := browse(reg, "web", "/again"); code != http.StatusOK || !strings.Contains(body, "/again") {
		t.Fatalf("preview after reattach = %d %q", code, body)
	}
	mu.Lock()
	gotPosts := posts
	mu.Unlock()
	if gotPosts != 3 {
		t.Errorf("registration attempts = %d, want 3 (first, the 503, the reattach)", gotPosts)
	}
	if got := p.stdout.String(); got != "https://web.lab.internal:8081\n" {
		t.Errorf("stdout = %q, want the URL line once", got)
	}
	stderr := p.stderr.String()
	for _, want := range []string{"connection to lab dropped", "reconnect failed (lab is restarting)", `reattached "web"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}

	if err := reg.Stop(f.runID, "web"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if code := p.wait(t); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, p.stderr.String())
	}
}

// TestExposeStopDuringReconnect (issue #105): `labctl expose stop web` run
// while the foreground `labctl expose web` sits in reconnect backoff frees
// the name for good. The foreground's next attempt carries its session, lab
// answers 410, and it exits 0 as if it had read "stopped" — instead of
// re-reserving the name and bringing the preview back.
func TestExposeStopDuringReconnect(t *testing.T) {
	fastReconnect(t)
	f, reg := newExposeFixture(t)

	// Every registration after the first is held at 503 until the stop has
	// landed, pinning the foreground in backoff.
	var mu sync.Mutex
	posts, refused := 0, 0
	gate := make(chan struct{})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/agent/v1/exposures" {
			mu.Lock()
			posts++
			n := posts
			mu.Unlock()
			select {
			case <-gate:
			default:
				if n > 1 {
					mu.Lock()
					refused++
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":"lab is restarting"}`)
					return
				}
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	env := map[string]string{"LAB_URL": front.URL, "LAB_TOKEN": f.token}

	p := startExpose(t, env, "web", strconv.Itoa(upstreamPort(t)))
	waitLive(t, reg, f.runID, "web")
	_ = (<-reg.conns).Close()
	waitFor(t, "the foreground in backoff", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return refused >= 1
	})

	if code, _, stderr := run(t, []string{"expose", "stop", "web"}, f.env()); code != 0 {
		t.Fatalf("expose stop = %d (stderr %q), want 0", code, stderr)
	}
	close(gate)

	if code := p.wait(t); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, p.stderr.String())
	}
	if !strings.Contains(p.stderr.String(), `exposure "web" stopped`) {
		t.Errorf("stderr = %q, want the stopped note", p.stderr.String())
	}
	if got := reg.Exposures(f.runID); len(got) != 0 {
		t.Errorf("exposures after the foreground exited = %+v, want none", got)
	}
	if n := reg.attachCount(); n != 1 {
		t.Errorf("attaches = %d, want 1 (the stopped exposure never re-attached)", n)
	}
	if code, _ := browse(reg, "web", "/"); code != http.StatusNotFound {
		t.Errorf("preview after stop = %d, want 404", code)
	}

	// A NEW `labctl expose web` is the agent asking again, and gets it.
	again := startExpose(t, f.env(), "web", strconv.Itoa(upstreamPort(t)))
	waitLive(t, reg, f.runID, "web")
	waitFor(t, "the new URL line", func() bool { return again.stdout.String() == "https://web.lab.internal:8081\n" })
}

// TestExposeOpenReleasesFailedConnections: each ExposeOpen builds its own
// Transport, so a non-101 answer must not leave its connection in that
// Transport's pool — a reconnect loop retrying through 503s would otherwise
// pin one connection (a goroutine and an fd on lab) per attempt.
func TestExposeOpenReleasesFailedConnections(t *testing.T) {
	var mu sync.Mutex
	open := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"lab is restarting"}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch st {
		case http.StateNew:
			open++
		case http.StateClosed, http.StateHijacked:
			open--
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c := &Client{BaseURL: srv.URL, Token: "lab_run_x"}
	const attempts = 5
	for range attempts {
		_, _, err := c.ExposeOpen(context.Background(), "web", "session", 5173)
		var se *apiStatusError
		if !errors.As(err, &se) || se.status != http.StatusServiceUnavailable {
			t.Fatalf("ExposeOpen = %v, want the 503", err)
		}
	}
	waitFor(t, "every failed attempt's connection to close", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return open == 0
	})
}

// TestExposeReconnectRefused: a 4xx on reconnect is final — here a 401
// because the run's tokens are gone.
func TestExposeReconnectRefused(t *testing.T) {
	fastReconnect(t)
	f, reg := newExposeFixture(t)
	p := startExpose(t, f.env(), "web", strconv.Itoa(upstreamPort(t)))
	waitLive(t, reg, f.runID, "web")

	if err := f.st.DeleteRunTokens(context.Background(), f.runID); err != nil {
		t.Fatalf("DeleteRunTokens: %v", err)
	}
	_ = (<-reg.conns).Close()

	if code := p.wait(t); code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, p.stderr.String())
	}
	if !strings.Contains(p.stderr.String(), "labctl expose: invalid run token") {
		t.Errorf("stderr = %q, want the 401 message", p.stderr.String())
	}
	if n := reg.attachCount(); n != 1 {
		t.Errorf("attaches = %d, want 1", n)
	}
}

// TestExposeLabEnds: control messages other than "stopped" are final and
// exit 1 — the run ended, or a newer expose of the same name took over.
func TestExposeLabEnds(t *testing.T) {
	t.Run("run ended", func(t *testing.T) {
		f, reg := newExposeFixture(t)
		p := startExpose(t, f.env(), "web", strconv.Itoa(upstreamPort(t)))
		waitLive(t, reg, f.runID, "web")
		reg.CloseRun(f.runID)
		if code := p.wait(t); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(p.stderr.String(), `the run ended; exposure "web" released`) {
			t.Errorf("stderr = %q", p.stderr.String())
		}
	})

	t.Run("replaced", func(t *testing.T) {
		f, reg := newExposeFixture(t)
		port := strconv.Itoa(upstreamPort(t))
		older := startExpose(t, f.env(), "web", port)
		waitFor(t, "first attach", func() bool { return reg.attachCount() == 1 })
		waitLive(t, reg, f.runID, "web")
		newer := startExpose(t, f.env(), "web", port)
		if code := older.wait(t); code != 1 {
			t.Fatalf("older exit = %d, want 1", code)
		}
		if !strings.Contains(older.stderr.String(), `exposure "web" was taken over`) {
			t.Errorf("older stderr = %q", older.stderr.String())
		}
		waitFor(t, "the newer URL line", func() bool { return newer.stdout.String() == "https://web.lab.internal:8081\n" })
		waitLive(t, reg, f.runID, "web")
	})
}

// TestExposeSignal: SIGTERM stops the exposure on lab (best-effort DELETE)
// and exits 0.
func TestExposeSignal(t *testing.T) {
	f, reg := newExposeFixture(t)
	var mu sync.Mutex
	deletes := 0
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/agent/v1/exposures/web" {
			mu.Lock()
			deletes++
			mu.Unlock()
		}
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(counting.Close)

	p := startExpose(t, map[string]string{"LAB_URL": counting.URL, "LAB_TOKEN": f.token}, "web", strconv.Itoa(upstreamPort(t)))
	waitLive(t, reg, f.runID, "web")
	p.sigs <- syscall.SIGTERM

	if code := p.wait(t); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, p.stderr.String())
	}
	mu.Lock()
	n := deletes
	mu.Unlock()
	if n != 1 {
		t.Errorf("DELETE requests = %d, want 1", n)
	}
	if got := reg.Exposures(f.runID); len(got) != 0 {
		t.Errorf("exposures after signal = %+v, want the name released", got)
	}
	if !strings.Contains(p.stderr.String(), `interrupted; stopping exposure "web"`) {
		t.Errorf("stderr = %q", p.stderr.String())
	}
}

func TestExposeRetryable(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{errors.New("dial unix: connection refused"), true},
		{&apiStatusError{status: 502, err: errors.New("bad gateway")}, true},
		{&apiStatusError{status: 503, err: errors.New("restarting")}, true},
		{&apiStatusError{status: 401, err: errors.New("invalid run token")}, false},
		{&apiStatusError{status: 404, err: errors.New("previews are not enabled")}, false},
		{&apiStatusError{status: 409, err: errors.New("taken")}, false},
		{&apiStatusError{status: 410, err: errors.New("stopped")}, false},
		{&apiStatusError{status: 200, err: errors.New("no upgrade")}, false},
		{fmt.Errorf("wrapped: %w", &apiStatusError{status: 422, err: errors.New("limit")}), false},
	} {
		if got := retryable(tt.err); got != tt.want {
			t.Errorf("retryable(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
