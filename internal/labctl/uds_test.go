package labctl

// Tests for the unix:// LAB_URL scheme (issue #201): the same HTTP requests
// over a unix domain socket. The server side is the REAL agentapi handler —
// the fixture from labctl_test.go — served on a socket instead of TCP, so a
// pass here proves the whole chain (URL parsing, dialing, Bearer auth, the
// JSON and streaming request builders) end to end.

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// udsSocketPath returns a fresh socket path short enough for sun_path (~108
// bytes). t.TempDir() can sit under a very long TMPDIR in this sandbox, so a
// too-long candidate falls back to a fresh dir directly under /tmp; both are
// removed with the test.
func udsSocketPath(t *testing.T) string {
	t.Helper()
	mk := func(parent string) string {
		dir, err := os.MkdirTemp(parent, "labctl-uds-")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return filepath.Join(dir, "agent.sock")
	}
	sock := mk("")
	if len(sock) > 100 {
		sock = mk("/tmp")
	}
	return sock
}

// udsEnv serves the fixture's agent API on a unix domain socket and returns
// the session env pointing LAB_URL at it (unix://<abs path>).
func udsEnv(t *testing.T, f *agentFixture) map[string]string {
	t.Helper()
	sock := udsSocketPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: f.handler}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return map[string]string{"LAB_URL": "unix://" + sock, "LAB_TOKEN": f.token}
}

// TestUnixSchemeRelativePathRejected pins the validation seam: a unix:// URL
// whose socket path is not absolute (unix://agent.sock names "agent.sock")
// fails with the clear error BEFORE any dial, as the command's exit-1 error.
func TestUnixSchemeRelativePathRejected(t *testing.T) {
	env := map[string]string{"LAB_URL": "unix://agent.sock", "LAB_TOKEN": "lab_run_x"}
	code, stdout, stderr := run(t, []string{"issue", "list"}, env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "LAB_URL unix:// socket path must be absolute (unix:///abs/path)") {
		t.Errorf("stderr = %q, want the absolute-path error", stderr)
	}
	if strings.Contains(stderr, "dial") {
		t.Errorf("stderr = %q, want the validation error with no dial attempt", stderr)
	}
}

// TestUnixSchemeUnreachableSocket pins the transport-failure path: an absent
// socket is a plain dial error naming the socket path, exit 1 — no panic, no
// hang.
func TestUnixSchemeUnreachableSocket(t *testing.T) {
	env := map[string]string{"LAB_URL": "unix:///nonexistent/agent.sock", "LAB_TOKEN": "lab_run_x"}
	code, stdout, stderr := run(t, []string{"issue", "list"}, env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "/nonexistent/agent.sock") {
		t.Errorf("stderr = %q, want the dial error to name the socket path", stderr)
	}
}

// TestUnixSchemeRoundTrip drives the real agent API over a socket: a read
// (issue view, byte-identical to the TCP rendering), a JSON mutation (issue
// comment), and the read-back proving the write landed.
func TestUnixSchemeRoundTrip(t *testing.T) {
	f := newBuiltinFixture(t)
	env := udsEnv(t, f)

	code, stdout, stderr := run(t, []string{"issue", "view"}, env)
	if code != 0 {
		t.Fatalf("issue view: exit = %d, stderr %q", code, stderr)
	}
	if stdout != wantIssueView {
		t.Errorf("issue view stdout =\n%q\nwant\n%q", stdout, wantIssueView)
	}

	code, _, stderr = run(t, []string{"issue", "comment", "1", "over the socket"}, env)
	if code != 0 {
		t.Fatalf("issue comment: exit = %d, stderr %q", code, stderr)
	}

	code, stdout, stderr = run(t, []string{"issue", "view", "1"}, env)
	if code != 0 {
		t.Fatalf("issue view after comment: exit = %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "over the socket") {
		t.Errorf("stdout = %q, want the comment posted over the socket", stdout)
	}
}

// TestUnixSchemeSecretScan pins the hand-built streaming request (SecretScan
// does not go through do()) on the socket transport: a leaked value blocks
// with secret + file named, a clean range passes in silence — the pre-push
// hook shells out to `secret scan`, so this path MUST inherit the scheme.
func TestUnixSchemeSecretScan(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "DEPLOY_KEY", "prod deploy key", "s3cr3t-uds-v4lu3")
	env := udsEnv(t, f)

	dir := newScanRepo(t)
	base := scanRevParse(t, dir, "HEAD")
	commitScanFile(t, dir, "config/prod.env", "DEPLOY_KEY=s3cr3t-uds-v4lu3\n", "add config")
	leak := base + ".." + scanRevParse(t, dir, "HEAD")

	t.Chdir(dir)
	code, _, stderr := run(t, []string{"secret", "scan", leak}, env)
	if code != 1 {
		t.Fatalf("leaking range: exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "secret DEPLOY_KEY (exact form) found in config/prod.env") {
		t.Errorf("stderr = %q, want the finding naming secret and file", stderr)
	}
	if strings.Contains(stderr, "s3cr3t-uds-v4lu3") {
		t.Fatalf("stderr leaked the secret value: %q", stderr)
	}

	commitScanFile(t, dir, "main.go", "package main\n", "innocent change")
	clean := scanRevParse(t, dir, "HEAD~1") + ".." + scanRevParse(t, dir, "HEAD")
	code, stdout, stderr := run(t, []string{"secret", "scan", clean}, env)
	if code != 0 {
		t.Fatalf("clean range: exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("clean range: output = %q / %q, want silence", stdout, stderr)
	}
}
