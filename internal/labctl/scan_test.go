package labctl

// Tests for `labctl secret scan` (issue #106). The server side is a plain
// httptest fake — NOT the real agentapi package (built in parallel) — and the
// diffs come from real git repos in t.TempDir() (no network, identity pinned
// per command).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// scanGit runs one git command in dir under testutil.HermeticGitEnv (pinned
// identity, no global/system config, no detached maintenance children); it
// fails the test on error and returns stdout.
func scanGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(dir)...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, errb.String())
	}
	return out.String()
}

// newScanRepo creates a repo with one initial commit (README.md) and returns
// its path.
func newScanRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	scanGit(t, dir, "init", "-b", "main")
	commitScanFile(t, dir, "README.md", "hello\n", "init")
	return dir
}

func writeScanFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitScanFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	writeScanFile(t, dir, name, content)
	scanGit(t, dir, "add", ".")
	scanGit(t, dir, "commit", "-m", msg)
}

func scanRevParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(scanGit(t, dir, "rev-parse", rev))
}

// scanRecorder captures what reached the fake scan endpoint.
type scanRecorder struct {
	mu       sync.Mutex
	requests int
	method   string
	path     string
	auth     string
	ctype    string
	body     string
}

func (r *scanRecorder) snapshot() scanRecorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return scanRecorder{requests: r.requests, method: r.method, path: r.path,
		auth: r.auth, ctype: r.ctype, body: r.body}
}

// newScanServer serves the fake scan endpoint: it drains and records each
// request, then answers with the given status and body (as JSON).
func newScanServer(t *testing.T, status int, response string) (*scanRecorder, map[string]string) {
	t.Helper()
	rec := &scanRecorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests++
		rec.method, rec.path = r.Method, r.URL.Path
		rec.auth = r.Header.Get("Authorization")
		rec.ctype = r.Header.Get("Content-Type")
		rec.body = string(body)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(ts.Close)
	return rec, map[string]string{"LAB_URL": ts.URL, "LAB_TOKEN": "lab_run_scan"}
}

// TestSecretScanCleanPass pins the happy path: the request is a POST to
// /agent/v1/secrets/scan with the Bearer token and a text/x-diff body carrying
// the outgoing patch, and a zero-findings answer is a SILENT exit 0.
func TestSecretScanCleanPass(t *testing.T) {
	dir := newScanRepo(t)
	commitScanFile(t, dir, "app.go", "package app // added-marker-alpha\n", "change")
	rng := scanRevParse(t, dir, "HEAD~1") + ".." + scanRevParse(t, dir, "HEAD")
	rec, env := newScanServer(t, http.StatusOK, `{"findings":[]}`)

	t.Chdir(dir)
	code, stdout, stderr := run(t, []string{"secret", "scan", rng}, env)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("output = %q / %q, want silence on a clean pass", stdout, stderr)
	}

	got := rec.snapshot()
	if got.requests != 1 {
		t.Fatalf("requests = %d, want 1", got.requests)
	}
	if got.method != http.MethodPost || got.path != "/agent/v1/secrets/scan" {
		t.Errorf("request = %s %s, want POST /agent/v1/secrets/scan", got.method, got.path)
	}
	if got.auth != "Bearer lab_run_scan" {
		t.Errorf("Authorization = %q, want the Bearer token", got.auth)
	}
	if got.ctype != "text/x-diff" {
		t.Errorf("Content-Type = %q, want text/x-diff", got.ctype)
	}
	if !strings.Contains(got.body, "added-marker-alpha") {
		t.Errorf("request body missing the added line; body = %q", got.body)
	}
}

// TestSecretScanFindings pins the blocking path: findings print one line each
// (name, form, file) plus the rewrite instruction, exit 1 — and the planted
// value itself NEVER appears (the server returns names only).
func TestSecretScanFindings(t *testing.T) {
	dir := newScanRepo(t)
	commitScanFile(t, dir, "app.env", "API_KEY=swordfish-9911-plaintext\n", "oops")
	rng := scanRevParse(t, dir, "HEAD~1") + ".." + scanRevParse(t, dir, "HEAD")
	_, env := newScanServer(t, http.StatusOK,
		`{"findings":[{"secret":"API_KEY","file":"app.env","form":"exact"},{"secret":"DB_URL","file":"config/dev.yaml","form":"base64"}]}`)

	t.Chdir(dir)
	code, stdout, stderr := run(t, []string{"secret", "scan", rng}, env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (findings go to stderr)", stdout)
	}
	for _, want := range []string{
		"labctl secret scan: secret API_KEY (exact form) found in app.env",
		"labctl secret scan: secret DB_URL (base64 form) found in config/dev.yaml",
		"rewrite the offending commits",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "swordfish") {
		t.Errorf("stderr leaked the planted value: %q", stderr)
	}
}

// TestSecretScanRevArgPassthrough pins that rev-args pass to git VERBATIM —
// including ones that start with `--`, the hook's no-remote-ref shape
// `<sha> --not --remotes=origin` — with no flag parsing or reordering.
func TestSecretScanRevArgPassthrough(t *testing.T) {
	dir := newScanRepo(t)
	rec, env := newScanServer(t, http.StatusOK, `{"findings":[]}`)

	t.Chdir(dir)
	args := []string{"secret", "scan", scanRevParse(t, dir, "HEAD"), "--not", "--remotes=origin"}
	code, stdout, stderr := run(t, args, env)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (git must accept the verbatim rev-args; stderr %q)", code, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("output = %q / %q, want silence", stdout, stderr)
	}
	got := rec.snapshot()
	if got.requests != 1 {
		t.Fatalf("requests = %d, want 1", got.requests)
	}
	// No remotes exist, so the whole history is outgoing: the initial
	// commit's added line must be in the stream.
	if !strings.Contains(got.body, "+hello") {
		t.Errorf("request body missing the initial commit's diff; body = %q", got.body)
	}
}

// TestSecretScanMultipleCommits proves the diff covers the WHOLE range (git
// log -p, not a single endpoint diff): two commits, both markers in the body.
func TestSecretScanMultipleCommits(t *testing.T) {
	dir := newScanRepo(t)
	base := scanRevParse(t, dir, "HEAD")
	commitScanFile(t, dir, "one.txt", "marker-one-8f1\n", "first")
	commitScanFile(t, dir, "two.txt", "marker-two-3c9\n", "second")
	rec, env := newScanServer(t, http.StatusOK, `{"findings":[]}`)

	t.Chdir(dir)
	code, _, stderr := run(t, []string{"secret", "scan", base + "..HEAD"}, env)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	got := rec.snapshot()
	for _, marker := range []string{"marker-one-8f1", "marker-two-3c9"} {
		if !strings.Contains(got.body, marker) {
			t.Errorf("request body missing %q (want patches for every commit in the range)", marker)
		}
	}
}

// TestSecretScanServerError pins fail-closed on a non-2xx: the envelope's
// message surfaces and the exit is 1 — the hook must not let the push pass.
func TestSecretScanServerError(t *testing.T) {
	dir := newScanRepo(t)
	_, env := newScanServer(t, http.StatusInternalServerError, `{"error":"scanner exploded"}`)

	t.Chdir(dir)
	code, stdout, stderr := run(t, []string{"secret", "scan", "HEAD"}, env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "labctl secret scan: scan failed:") ||
		!strings.Contains(stderr, "scanner exploded") {
		t.Errorf("stderr = %q, want the scan-failed prefix with the envelope message", stderr)
	}
}

// TestSecretScanServerUnreachable pins fail-closed on a transport error: a
// guard that cannot reach the scanner has NOT passed.
func TestSecretScanServerUnreachable(t *testing.T) {
	dir := newScanRepo(t)
	env := map[string]string{"LAB_URL": "http://127.0.0.1:1", "LAB_TOKEN": "lab_run_scan"}

	t.Chdir(dir)
	code, _, stderr := run(t, []string{"secret", "scan", "HEAD"}, env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "labctl secret scan: scan failed:") {
		t.Errorf("stderr = %q, want the scan-failed message", stderr)
	}
}

// TestSecretScanGitFailure pins fail-closed on a git error (bogus range): the
// git failure wins — even if a request slipped out and the server answered a
// clean 200 over the empty stream, "pass" over a broken diff proves nothing.
func TestSecretScanGitFailure(t *testing.T) {
	dir := newScanRepo(t)
	_, env := newScanServer(t, http.StatusOK, `{"findings":[]}`)

	t.Chdir(dir)
	code, stdout, stderr := run(t, []string{"secret", "scan", "deadbeef..HEAD"}, env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.HasPrefix(stderr, "labctl secret scan: ") {
		t.Errorf("stderr = %q, want the labctl secret scan: prefix", stderr)
	}
	if !strings.Contains(stderr, "git log failed") || !strings.Contains(stderr, "deadbeef") {
		t.Errorf("stderr = %q, want the git failure with git's own stderr", stderr)
	}
}

// TestSecretScanEndToEnd drives the WHOLE chain with no fakes: a real git
// repo's outgoing range, the real `secret scan` verb, and the real agentapi
// handler (behind httptest) matching against a vault-sealed secret. This is
// the integration proof for issue #106's client/server halves: a leaked value
// blocks with secret + file named (never the value), a clean range passes in
// silence.
func TestSecretScanEndToEnd(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "DEPLOY_KEY", "prod deploy key", "s3cr3t-dpl0y-v4lu3")

	dir := newScanRepo(t)
	base := scanRevParse(t, dir, "HEAD")
	commitScanFile(t, dir, "config/prod.env", "DEPLOY_KEY=s3cr3t-dpl0y-v4lu3\n", "add config")
	leak := base + ".." + scanRevParse(t, dir, "HEAD")

	t.Chdir(dir)
	code, stdout, stderr := run(t, []string{"secret", "scan", leak}, f.env())
	if code != 1 {
		t.Fatalf("leaking range: exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("leaking range: stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "secret DEPLOY_KEY (exact form) found in config/prod.env") {
		t.Errorf("stderr = %q, want the finding naming secret and file", stderr)
	}
	if strings.Contains(stderr, "s3cr3t-dpl0y-v4lu3") {
		t.Fatalf("stderr leaked the secret value: %q", stderr)
	}

	// A commit on top WITHOUT the value: scanning just that range is clean.
	commitScanFile(t, dir, "main.go", "package main\n", "innocent change")
	clean := scanRevParse(t, dir, "HEAD~1") + ".." + scanRevParse(t, dir, "HEAD")
	code, stdout, stderr = run(t, []string{"secret", "scan", clean}, f.env())
	if code != 0 {
		t.Fatalf("clean range: exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("clean range: output = %q / %q, want silence", stdout, stderr)
	}
}
