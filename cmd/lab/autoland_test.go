package main

// In-process tests for `lab autoland rearm` (issue #188), in
// hashpassword_test.go's register: real writers, no subprocess, assertions on
// the exit code and the EXACT output shape. The API is an httptest.NewServer
// that records what it was handed, so the happy path pins the wire contract
// (method, escaped path, Authorization header) and every usage-error case can
// prove the stronger property that NO request was made at all — a re-arm is a
// state change, so "rejected before it left the process" is the thing worth
// testing, not merely "exited 2".

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// wantRearmedAt is a store.FormatTime-shaped instant (the pinned layout: UTC,
// exactly 3 fractional digits) so the printed line is byte-identical to what a
// real server would produce.
const wantRearmedAt = "2026-08-03T20:24:25.000Z"

// capturedRequest is one request the fake API saw. Path is the ESCAPED path —
// r.URL.Path is already percent-decoded, which would hide exactly the
// repo-id-escaping bug this asserts against.
type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   string
}

// rearmAPI starts a fake operator API answering every request with status/body
// and recording what it received. The recorder is mutex-guarded: the handler
// runs on the server's goroutine and the assertions on the test's, and the
// only thing joining them is a socket — which the race detector does not treat
// as a happens-before edge.
type rearmAPI struct {
	*httptest.Server
	mu   sync.Mutex
	seen []capturedRequest
}

func (a *rearmAPI) requests() []capturedRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]capturedRequest(nil), a.seen...)
}

func newRearmAPI(t *testing.T, status int, body string) *rearmAPI {
	t.Helper()
	api := &rearmAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		api.seen = append(api.seen, capturedRequest{
			Method: r.Method,
			Path:   r.URL.EscapedPath(),
			Auth:   r.Header.Get("Authorization"),
			Body:   string(b),
		})
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(api.Close)
	return api
}

// okBody is the handler's 200 envelope for repo/pull.
func okBody(repo string, pull int) string {
	b, err := json.Marshal(map[string]any{
		"repo_id":     repo,
		"pull_number": pull,
		"rearmed_at":  wantRearmedAt,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// fakeEnv is the injected getenv: an explicit map, so no test mutates the
// process environment and the flag-vs-env precedence cases stay independent.
func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// noEnv is the empty environment (nothing set).
func noEnv(string) string { return "" }

// TestRunAutolandRearmHappyPath: the full wire contract. A POST to the exact
// route with the PAT as a Bearer header and no body, exit 0, and stdout is
// EXACTLY the one tab-separated line echoed from the server's answer.
func TestRunAutolandRearmHappyPath(t *testing.T) {
	api := newRearmAPI(t, http.StatusOK, okBody("repo-1", 12))
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", api.URL, "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if want := "repo-1\t#12\trearmed\t" + wantRearmedAt + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.Method)
	}
	if want := "/api/v1/repos/repo-1/autoland/pulls/12/rearm"; got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}
	if want := "Bearer lab_pat_secret"; got.Auth != want {
		t.Errorf("Authorization = %q, want %q", got.Auth, want)
	}
	if got.Body != "" {
		t.Errorf("body = %q, want empty (the route takes no body)", got.Body)
	}
}

// TestRunAutolandRearmEscapesRepoID: the repo id is opaque caller input on a
// path segment, so '/' and ' ' must be percent-escaped rather than silently
// addressing a different route.
func TestRunAutolandRearmEscapesRepoID(t *testing.T) {
	const repo = "odd id/with-slash"
	api := newRearmAPI(t, http.StatusOK, okBody(repo, 3))
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", repo, "-pull", "3", "-url", api.URL, "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	if want := "/api/v1/repos/odd%20id%2Fwith-slash/autoland/pulls/3/rearm"; reqs[0].Path != want {
		t.Errorf("path = %q, want %q", reqs[0].Path, want)
	}
}

// TestRunAutolandRearmTrimsTrailingSlashOnURL: a base URL pasted with a
// trailing slash must not produce a doubled "//api".
func TestRunAutolandRearmTrimsTrailingSlashOnURL(t *testing.T) {
	api := newRearmAPI(t, http.StatusOK, okBody("repo-1", 7))
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "7", "-url", api.URL + "/", "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	if want := "/api/v1/repos/repo-1/autoland/pulls/7/rearm"; reqs[0].Path != want {
		t.Errorf("path = %q, want %q", reqs[0].Path, want)
	}
}

// TestRunAutolandRearmUsageErrors: every argument mistake is exit 2, names the
// offending flag on stderr, prints nothing on stdout, and — the property that
// matters for a state-changing verb — never reaches the server.
func TestRunAutolandRearmUsageErrors(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantsMsg string
	}{
		{
			name:     "missing repo",
			args:     []string{"-pull", "12", "-token", "lab_pat_secret"},
			wantsMsg: "-repo is required",
		},
		{
			name:     "missing pull",
			args:     []string{"-repo", "repo-1", "-token", "lab_pat_secret"},
			wantsMsg: "-pull is required",
		},
		{
			name:     "pull zero",
			args:     []string{"-repo", "repo-1", "-pull", "0", "-token", "lab_pat_secret"},
			wantsMsg: "-pull is required",
		},
		{
			name:     "pull negative",
			args:     []string{"-repo", "repo-1", "-pull", "-4", "-token", "lab_pat_secret"},
			wantsMsg: "-pull is required",
		},
		{
			name:     "pull not a number",
			args:     []string{"-repo", "repo-1", "-pull", "twelve", "-token", "lab_pat_secret"},
			wantsMsg: "invalid value",
		},
		{
			name:     "unknown flag",
			args:     []string{"-repo", "repo-1", "-pull", "12", "-token", "lab_pat_secret", "-nope"},
			wantsMsg: "flag provided but not defined",
		},
		{
			name:     "positional arguments",
			args:     []string{"-repo", "repo-1", "-pull", "12", "-token", "lab_pat_secret", "extra"},
			wantsMsg: "unexpected arguments",
		},
		{
			name:     "missing token",
			args:     []string{"-repo", "repo-1", "-pull", "12"},
			wantsMsg: "operator PAT is required",
		},
		{
			name: "run token refused",
			args: []string{"-repo", "repo-1", "-pull", "12", "-token", "lab_run_whatever"},
			// The point of the verb's placement: an agent credential is
			// refused with an explanation, not a 401 from the server.
			wantsMsg: "not an operator PAT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Every case gets a live -url so "no request was made" is a real
			// assertion rather than a vacuous one.
			api := newRearmAPI(t, http.StatusOK, okBody("repo-1", 12))
			var stdout, stderr strings.Builder

			code := runAutolandRearm(append(tt.args, "-url", api.URL), noEnv, &stdout, &stderr)

			if code != 2 {
				t.Fatalf("exit code = %d, want 2; stderr = %q", code, stderr.String())
			}
			if stdout.String() != "" {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantsMsg) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), tt.wantsMsg)
			}
			if n := len(api.requests()); n != 0 {
				t.Errorf("server saw %d requests, want 0 — a rejected invocation must never reach the API", n)
			}
		})
	}
}

// TestRunAutolandRearmMissingURL: with no -url and no LAB_URL there is nothing
// to talk to — exit 2 naming the flag and the env var. Its own test because it
// is the one usage case that cannot be given a live server to prove silence
// against.
func TestRunAutolandRearmMissingURL(t *testing.T) {
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "-url is required") || !strings.Contains(stderr.String(), "LAB_URL") {
		t.Errorf("stderr = %q, want it to mention \"-url is required\" and LAB_URL", stderr.String())
	}
}

// TestRunAutolandRearmNoArgsPrintsUsage: the bare `lab autoland rearm` an
// operator types to remember the flags — exit 2 with the usage block, which is
// the verb's own (not the server's flag reference).
func TestRunAutolandRearmNoArgsPrintsUsage(t *testing.T) {
	var stdout, stderr strings.Builder

	code := runAutolandRearm(nil, noEnv, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Usage: lab autoland rearm") {
		t.Errorf("stderr = %q, want the verb's usage block", stderr.String())
	}
	// The server's own flag reference must NOT be dumped here.
	if strings.Contains(stderr.String(), "-master-key-file") {
		t.Errorf("stderr = %q, want the verb usage, not the server's flag reference", stderr.String())
	}
}

// TestRunAutolandRearmUsageNeverPrintsTheToken pins the reason runAutolandRearm
// overrides fs.Usage instead of letting flag print its own.
//
// -token defaults to LAB_PAT so the everyday invocation keeps the credential
// out of argv — but flag's default Usage calls PrintDefaults, which renders
// every flag's DEFAULT VALUE. Under a normal operator environment that made
// `lab autoland rearm -h` print the PAT itself to stderr, straight into
// scrollback and CI logs. Every path that can reach a usage block is checked,
// because -h, an unknown flag, and an argument error each reach it differently
// (fs.Usage via flag's -h handling, fs.Usage via its parse error, and usageErr
// directly).
func TestRunAutolandRearmUsageNeverPrintsTheToken(t *testing.T) {
	const secret = "lab_pat_SUPERSECRET"
	env := func(k string) string {
		if k == "LAB_PAT" {
			return secret
		}
		return ""
	}
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"-nosuchflag"},
		nil,                          // missing -repo → usageErr
		{"-repo", "r"},               // missing -pull → usageErr
		{"-repo", "r", "-pull", "1"}, // missing -url → usageErr
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := runAutolandRearm(args, env, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if strings.Contains(stderr.String(), secret) || strings.Contains(stdout.String(), secret) {
				t.Errorf("the PAT leaked into usage output:\nstdout=%q\nstderr=%q", stdout.String(), stderr.String())
			}
			// The env var must still be NAMED — the operator has to learn
			// where the credential comes from, just never see its value.
			if !strings.Contains(stderr.String(), "LAB_PAT") {
				t.Errorf("stderr = %q, want it to name LAB_PAT", stderr.String())
			}
		})
	}
}

// TestRunAutolandRearmEnvFallback: LAB_URL and LAB_PAT supply -url and -token
// when the flags are absent — the everyday operator shape, and the one that
// keeps the PAT out of argv.
func TestRunAutolandRearmEnvFallback(t *testing.T) {
	api := newRearmAPI(t, http.StatusOK, okBody("repo-1", 12))
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12"},
		fakeEnv(map[string]string{"LAB_URL": api.URL, "LAB_PAT": "lab_pat_from_env"}),
		&stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if want := "repo-1\t#12\trearmed\t" + wantRearmedAt + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	if want := "Bearer lab_pat_from_env"; reqs[0].Auth != want {
		t.Errorf("Authorization = %q, want %q", reqs[0].Auth, want)
	}
}

// TestRunAutolandRearmFlagBeatsEnv: the repo's flag > env > default precedence
// — an explicit -url/-token wins over LAB_URL/LAB_PAT. The env points at a
// server that would fail the assertions, so a regression cannot pass silently.
func TestRunAutolandRearmFlagBeatsEnv(t *testing.T) {
	wanted := newRearmAPI(t, http.StatusOK, okBody("repo-1", 12))
	decoy := newRearmAPI(t, http.StatusOK, okBody("decoy", 99))
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", wanted.URL, "-token", "lab_pat_flag"},
		fakeEnv(map[string]string{"LAB_URL": decoy.URL, "LAB_PAT": "lab_pat_env"}),
		&stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if n := len(decoy.requests()); n != 0 {
		t.Errorf("LAB_URL server saw %d requests, want 0 — the -url flag must win", n)
	}
	reqs := wanted.requests()
	if len(reqs) != 1 {
		t.Fatalf("-url server saw %d requests, want 1", len(reqs))
	}
	if want := "Bearer lab_pat_flag"; reqs[0].Auth != want {
		t.Errorf("Authorization = %q, want %q — the -token flag must win over LAB_PAT", reqs[0].Auth, want)
	}
}

// TestRunAutolandRearmTokenFile: -token-file reads the PAT from disk (the
// systemd / secrets-manager shape that keeps the credential out of argv AND
// out of the environment), strips exactly one trailing newline, and wins over
// -token — the -seed-password-hash-file/-seed-password-hash precedent.
func TestRunAutolandRearmTokenFile(t *testing.T) {
	api := newRearmAPI(t, http.StatusOK, okBody("repo-1", 12))
	path := filepath.Join(t.TempDir(), "pat")
	if err := os.WriteFile(path, []byte("lab_pat_from_file\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", api.URL, "-token", "lab_pat_inline", "-token-file", path},
		fakeEnv(map[string]string{"LAB_PAT": "lab_pat_env"}), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	if want := "Bearer lab_pat_from_file"; reqs[0].Auth != want {
		t.Errorf("Authorization = %q, want %q", reqs[0].Auth, want)
	}
}

// TestRunAutolandRearmTokenFileUnreadable: a -token-file we were pointed at
// and cannot read is a configuration error (exit 2, nothing attempted) whose
// message names the PATH only — never the contents.
func TestRunAutolandRearmTokenFileUnreadable(t *testing.T) {
	api := newRearmAPI(t, http.StatusOK, okBody("repo-1", 12))
	path := filepath.Join(t.TempDir(), "absent")
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", api.URL, "-token-file", path},
		noEnv, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), path) {
		t.Errorf("stderr = %q, want it to name the token file path", stderr.String())
	}
	if n := len(api.requests()); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}

// TestRunAutolandRearmServerErrors: the API's error envelope is what the
// operator sees, verbatim, prefixed with the verb — for every status the route
// documents. Exit 1 (the operation failed), never 2 (the invocation was fine).
func TestRunAutolandRearmServerErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "404 unknown repo",
			status: http.StatusNotFound,
			body:   `{"error":"not found"}`,
			want:   "lab autoland rearm: not found\n",
		},
		{
			name:   "400 bad pull",
			status: http.StatusBadRequest,
			body:   `{"error":"pull must be a positive integer"}`,
			want:   "lab autoland rearm: pull must be a positive integer\n",
		},
		{
			name:   "401 unauthenticated",
			status: http.StatusUnauthorized,
			body:   `{"error":"authentication required"}`,
			want:   "lab autoland rearm: authentication required\n",
		},
		{
			// Not the {"error"} envelope: a proxy's HTML page, a plain-text
			// gateway error, anything. Must not panic, must not print an empty
			// message, must still exit 1 — so the status is the message.
			name:   "non-JSON error body falls back to the status",
			status: http.StatusBadGateway,
			body:   "<html><body>502 Bad Gateway</body></html>",
			want:   "lab autoland rearm: HTTP 502\n",
		},
		{
			name:   "empty error body falls back to the status",
			status: http.StatusInternalServerError,
			body:   "",
			want:   "lab autoland rearm: HTTP 500\n",
		},
		{
			// JSON, but not the envelope — the fallback keys on a non-empty
			// "error" field, not merely on parseable JSON.
			name:   "JSON without an error field falls back to the status",
			status: http.StatusForbidden,
			body:   `{"detail":"nope"}`,
			want:   "lab autoland rearm: HTTP 403\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newRearmAPI(t, tt.status, tt.body)
			var stdout, stderr strings.Builder

			code := runAutolandRearm(
				[]string{"-repo", "repo-1", "-pull", "12", "-url", api.URL, "-token", "lab_pat_secret"},
				noEnv, &stdout, &stderr)

			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
			}
			if stdout.String() != "" {
				t.Errorf("stdout = %q, want empty — a failed re-arm prints no success line", stdout.String())
			}
			if stderr.String() != tt.want {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

// TestRunAutolandRearmHTMLSuccessBody: a 200 whose body is a proxy's login
// page (issue #30's shape) must fail rather than print a half-empty line — a
// lost write can never read as success.
func TestRunAutolandRearmHTMLSuccessBody(t *testing.T) {
	api := newRearmAPI(t, http.StatusOK, "<html><body>please log in</body></html>")
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", api.URL, "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "decoding response") {
		t.Errorf("stderr = %q, want it to mention decoding the response", stderr.String())
	}
}

// TestRunAutolandRearmRedirectNotFollowed: the operator API never redirects,
// so a 3xx means something in front of lab answered. It is reported, not
// followed out to a login page.
func TestRunAutolandRearmRedirectNotFollowed(t *testing.T) {
	login := newRearmAPI(t, http.StatusOK, `{"repo_id":"repo-1","pull_number":12,"rearmed_at":"never"}`)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, login.URL+"/login", http.StatusFound)
	}))
	t.Cleanup(api.Close)
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", api.URL, "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "HTTP 302") {
		t.Errorf("stderr = %q, want it to report the 302", stderr.String())
	}
	if n := len(login.requests()); n != 0 {
		t.Errorf("redirect target saw %d requests, want 0 — redirects must not be followed", n)
	}
}

// TestRunAutolandRearmUnreachableServer: a transport failure is exit 1 with
// the reason on stderr, never a panic and never a success line.
func TestRunAutolandRearmUnreachableServer(t *testing.T) {
	api := newRearmAPI(t, http.StatusOK, okBody("repo-1", 12))
	dead := api.URL
	api.Close() // the port is now closed; the dial fails
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", dead, "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "lab autoland rearm: ") {
		t.Errorf("stderr = %q, want the verb-prefixed error form", stderr.String())
	}
}

// TestRunAutolandRearmRelativeUnixSocket: unix:// is followed directly by the
// path, so unix://foo.sock names the RELATIVE "foo.sock". Rejected up front as
// a usage error rather than dialed into a confusing ENOENT.
func TestRunAutolandRearmRelativeUnixSocket(t *testing.T) {
	var stdout, stderr strings.Builder

	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", "unix://agent.sock", "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "must be absolute") {
		t.Errorf("stderr = %q, want it to demand an absolute socket path", stderr.String())
	}
}

// TestRunAutolandRearmUnixSocket: an operator API reachable over a unix socket
// works end to end — the requests stay plain HTTP, only the dial changes, and
// whatever host the URL nominally names is never resolved.
func TestRunAutolandRearmUnixSocket(t *testing.T) {
	// t.TempDir() can be long enough to blow the ~104-byte sun_path limit on
	// some platforms; os.MkdirTemp under the system temp dir keeps it short.
	dir, err := os.MkdirTemp("", "labrearm")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "lab.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listening on %s: %v", sock, err)
	}
	api := &rearmAPI{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.seen = append(api.seen, capturedRequest{Method: r.Method, Path: r.URL.EscapedPath(), Auth: r.Header.Get("Authorization")})
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody("repo-1", 12))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	var stdout, stderr strings.Builder
	code := runAutolandRearm(
		[]string{"-repo", "repo-1", "-pull", "12", "-url", "unix://" + sock, "-token", "lab_pat_secret"},
		noEnv, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if want := "repo-1\t#12\trearmed\t" + wantRearmedAt + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	if want := "/api/v1/repos/repo-1/autoland/pulls/12/rearm"; reqs[0].Path != want {
		t.Errorf("path = %q, want %q", reqs[0].Path, want)
	}
}

// TestRunAutolandSubcommands: the `autoland` group's dispatch — a bare group,
// an unknown verb, and the verb itself.
func TestRunAutolandSubcommands(t *testing.T) {
	t.Run("no subcommand", func(t *testing.T) {
		var stdout, stderr strings.Builder
		code := runAutoland(nil, noEnv, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if !strings.Contains(stderr.String(), "missing subcommand") ||
			!strings.Contains(stderr.String(), "Usage: lab autoland rearm") {
			t.Errorf("stderr = %q, want the missing-subcommand message and usage", stderr.String())
		}
	})

	t.Run("unknown subcommand", func(t *testing.T) {
		var stdout, stderr strings.Builder
		code := runAutoland([]string{"bogus"}, noEnv, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if stdout.String() != "" {
			t.Errorf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), `unknown subcommand "bogus"`) ||
			!strings.Contains(stderr.String(), "Usage: lab autoland rearm") {
			t.Errorf("stderr = %q, want the unknown-subcommand message and usage", stderr.String())
		}
	})

	t.Run("rearm routes through", func(t *testing.T) {
		api := newRearmAPI(t, http.StatusOK, okBody("repo-1", 12))
		var stdout, stderr strings.Builder
		code := runAutoland(
			[]string{"rearm", "-repo", "repo-1", "-pull", "12", "-url", api.URL, "-token", "lab_pat_secret"},
			noEnv, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
		}
		if n := len(api.requests()); n != 1 {
			t.Errorf("server saw %d requests, want 1", n)
		}
	})
}

// TestUsageDocumentsAutolandRearm pins that `lab -help` advertises the verb
// and its env vars, in the style of TestUsageDocumentsSeedFlags. usage is the
// package-level const printed on flag.ErrHelp, so assert against it directly.
func TestUsageDocumentsAutolandRearm(t *testing.T) {
	for _, want := range []string{
		"lab autoland rearm",
		"-repo",
		"-pull",
		"LAB_PAT",
		"LAB_PAT_FILE",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not document %q", want)
		}
	}
}
