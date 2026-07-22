package agentapi

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// socketDir returns a fresh directory short enough to hold a unix socket:
// sun_path caps socket paths at ~108 bytes, and t.TempDir() can sit under a
// deeply nested TMPDIR that blows past it. When the default temp dir would,
// the fallback goes under /tmp directly.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agentsock")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if len(SocketPath(dir)) <= 100 {
		return dir
	}
	dir, err = os.MkdirTemp("/tmp", "agentsock")
	if err != nil {
		t.Fatalf("mkdirtemp /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// mustListen asserts ListenSocket succeeds and that the resulting file is a
// 0700 socket.
func mustListen(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		t.Errorf("mode = %v, want a socket", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("perm = %o, want 0700", perm)
	}
	return ln
}

func TestSocketPath(t *testing.T) {
	if got, want := SocketPath("/var/lib/lab"), filepath.Join("/var/lib/lab", "agent.sock"); got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

// TestListenSocketReplacesStaleFile pins crash recovery: whatever a previous
// process left at the path — a regular file or an orphaned socket no one
// listens on — a new ListenSocket must replace it and serve.
func TestListenSocketReplacesStaleFile(t *testing.T) {
	t.Run("regular file", func(t *testing.T) {
		path := SocketPath(socketDir(t))
		if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
			t.Fatalf("write stale file: %v", err)
		}
		mustListen(t, path)
	})

	t.Run("orphaned socket", func(t *testing.T) {
		path := SocketPath(socketDir(t))
		// Simulate a crash: leave the socket file behind by disabling the
		// unlink-on-close a clean shutdown relies on.
		orphan, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("orphan listen: %v", err)
		}
		orphan.(*net.UnixListener).SetUnlinkOnClose(false)
		if err := orphan.Close(); err != nil {
			t.Fatalf("orphan close: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("orphan socket file should remain: %v", err)
		}
		mustListen(t, path)
	})

	t.Run("nothing at path", func(t *testing.T) {
		mustListen(t, SocketPath(socketDir(t)))
	})
}

// TestSocketServesAgentAPI round-trips the real Handler() over the unix
// socket: the run-token auth rule applies identically to socket traffic — the
// listener carries the same handler tree the TCP server mounts, nothing is
// re-derived per transport.
func TestSocketServesAgentAPI(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix the frobnicator", "It wobbles.")
	f.seedRun(t, "run_active", "repo_a", "active")
	token := f.seedToken(t, "run_active", nil)

	path := SocketPath(socketDir(t))
	ln := mustListen(t, path)

	srv := &http.Server{Handler: f.server().Handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// The URL host is a placeholder: DialContext ignores it and connects to
	// the socket, exactly how labctl's transport will.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}

	get := func(authz string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", "http://lab/agent/v1/issue", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request over socket: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	if status, body := get(""); status != http.StatusUnauthorized || !strings.Contains(body, "run token required") {
		t.Errorf("no token: status = %d, body = %q, want 401 with token-required error", status, body)
	}
	if status, body := get("Bearer " + token); status != http.StatusOK || !strings.Contains(body, `"number":7`) {
		t.Errorf("with token: status = %d, body = %q, want 200 with issue 7", status, body)
	}
}
