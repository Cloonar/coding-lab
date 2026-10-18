package httpapi

// Previews on the run API (issue #105): every run response carries an
// "exposures" key — the run's `labctl expose` previews from the preview
// registry, [] (never null) when there are none or no listener at all. Also
// pins the transport property the tunnel upgrade depends on: a handler
// mounted as Options.AgentHandler can hijack its connection through the
// recover/requestID/logging/metrics chain the main listener wraps it in.

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/preview"
)

// fakeExposures is an ExposureSource keyed by run id. An unknown run answers
// nil, which the server must still render as [].
type fakeExposures struct {
	mu    sync.Mutex
	byRun map[string][]preview.Exposure
}

func (f *fakeExposures) Exposures(runID string) []preview.Exposure {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byRun[runID]
}

func (f *fakeExposures) set(runID string, exps []preview.Exposure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byRun[runID] = exps
}

// exposuresOf extracts a decoded run object's "exposures" value, failing on
// an absent key or a JSON null — both of which the SPA must never see.
func exposuresOf(t *testing.T, where string, run map[string]any) []any {
	t.Helper()
	raw, ok := run["exposures"]
	if !ok {
		t.Fatalf("%s: run JSON has no exposures key: %v", where, run)
	}
	list, ok := raw.([]any)
	if !ok || list == nil {
		t.Fatalf("%s: exposures = %#v, want a JSON array", where, raw)
	}
	return list
}

// wantExposures compares a decoded "exposures" array against exps.
func wantExposures(t *testing.T, where string, got []any, exps []preview.Exposure) {
	t.Helper()
	b, err := json.Marshal(exps)
	if err != nil {
		t.Fatal(err)
	}
	var want []any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: exposures = %v, want %v", where, got, want)
	}
}

func TestAPI_RunExposures(t *testing.T) {
	src := &fakeExposures{byRun: map[string][]preview.Exposure{}}
	x := newInstanceServerMod(t, func(o *Options) { o.Exposures = src })
	h := csrfHeaders(x.ts.URL)

	// A freshly created run renders [] — it cannot hold an exposure yet.
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	if got := exposuresOf(t, "POST instances", created); len(got) != 0 {
		t.Errorf("new run exposures = %v, want []", got)
	}
	runID := created["id"].(string)

	exps := []preview.Exposure{
		{Name: "api", URL: "http://api.lab.internal:8081", Port: 8080, Live: false},
		{Name: "web", URL: "http://web.lab.internal:8081", Port: 5173, Live: true},
	}
	src.set(runID, exps)

	// GET /runs (the history list).
	resp = x.do("GET", "/api/v1/runs?repo="+x.repo.ID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	runs, _ := decodeBody(t, resp)["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs list has %d rows, want 1", len(runs))
	}
	wantExposures(t, "GET runs", exposuresOf(t, "GET runs", runs[0].(map[string]any)), exps)

	// GET /runs/{id} (the chat header / run details).
	resp = x.do("GET", "/api/v1/runs/"+runID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	wantExposures(t, "GET run", exposuresOf(t, "GET run", decodeBody(t, resp)), exps)

	// The title PATCH echo is the same run view.
	resp = x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"title": "t"}, h)
	wantStatus(t, resp, http.StatusOK)
	wantExposures(t, "PATCH run", exposuresOf(t, "PATCH run", decodeBody(t, resp)), exps)

	// The instances rows embed runResponse, so they carry it too.
	resp = x.do("GET", "/api/v1/instances", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	instances, _ := decodeBody(t, resp)["instances"].([]any)
	if len(instances) != 1 {
		t.Fatalf("instances list has %d rows, want 1", len(instances))
	}
	wantExposures(t, "GET instances", exposuresOf(t, "GET instances", instances[0].(map[string]any)), exps)

	// A source answering nil for a run (the fake's unknown-run case) is [].
	src.set(runID, nil)
	resp = x.do("GET", "/api/v1/runs/"+runID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := exposuresOf(t, "GET run (source nil)", decodeBody(t, resp)); len(got) != 0 {
		t.Errorf("exposures with a nil answer = %v, want []", got)
	}
}

// A lab without a preview listener — Options.Exposures nil, or the typed-nil
// *preview.Registry New folds back to nil — renders "exposures": [] on every
// run, and never panics reaching for the registry.
func TestAPI_RunExposures_noListener(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Options)
	}{
		{"nil source", nil},
		{"typed-nil registry", func(o *Options) {
			var reg *preview.Registry
			o.Exposures = reg
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newInstanceServerMod(t, tc.mod)
			runID, _ := startRun(t, x)

			resp := x.do("GET", "/api/v1/runs?repo="+x.repo.ID, nil, nil)
			wantStatus(t, resp, http.StatusOK)
			runs, _ := decodeBody(t, resp)["runs"].([]any)
			if len(runs) != 1 {
				t.Fatalf("runs list has %d rows, want 1", len(runs))
			}
			if got := exposuresOf(t, "GET runs", runs[0].(map[string]any)); len(got) != 0 {
				t.Errorf("GET runs exposures = %v, want []", got)
			}

			resp = x.do("GET", "/api/v1/runs/"+runID, nil, nil)
			wantStatus(t, resp, http.StatusOK)
			if got := exposuresOf(t, "GET run", decodeBody(t, resp)); len(got) != 0 {
				t.Errorf("GET run exposures = %v, want []", got)
			}
		})
	}
}

// The `labctl expose` upgrade hijacks its connection inside the agent API
// handler, and on the main TCP listener that handler sits behind httpapi's
// whole middleware chain — every wrapper there must hand
// http.ResponseController the real connection (Unwrap). Exercised over a
// real TCP server: the fake agent handler hijacks, answers 101 itself, and
// echoes bytes — including the ones the client sent in the same write as its
// request headers, which net/http has already buffered by the time the
// handler runs.
func TestAgentMount_hijackThroughMiddleware(t *testing.T) {
	handlerErr := make(chan error, 1)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/x" {
			http.NotFound(w, r)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			handlerErr <- err
			http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: lab-tunnel\r\n\r\n")
		if err := brw.Flush(); err != nil {
			handlerErr <- err
			return
		}
		// Echo through the buffered reader, so bytes that arrived with the
		// request headers are not lost.
		_, _ = io.Copy(conn, brw.Reader)
		handlerErr <- nil
	})
	x := newTestServer(t, func(o *Options) { o.AgentHandler = agent })

	conn, err := net.DialTimeout("tcp", x.ts.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// The request and the first tunnel bytes in ONE write.
	req := "POST /agent/v1/x HTTP/1.1\r\nHost: lab\r\nConnection: Upgrade\r\nUpgrade: lab-tunnel\r\nContent-Length: 0\r\n\r\nearly"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 101", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if got := resp.Header.Get("Upgrade"); got != "lab-tunnel" {
		t.Errorf("Upgrade = %q, want lab-tunnel", got)
	}

	buf := make([]byte, len("early"))
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "early" {
		t.Fatalf("echo of the pre-buffered bytes = %q, %v; want \"early\"", buf, err)
	}
	if _, err := io.WriteString(conn, "later"); err != nil {
		t.Fatalf("write tunnel bytes: %v", err)
	}
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "later" {
		t.Fatalf("echo after the upgrade = %q, %v; want \"later\"", buf, err)
	}

	// Closing our write side ends the handler's copy loop.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	select {
	case err := <-handlerErr:
		if err != nil {
			t.Fatalf("agent handler: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent handler did not finish after the client closed")
	}
}
