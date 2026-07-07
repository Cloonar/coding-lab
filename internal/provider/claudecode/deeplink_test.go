package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRegistryEntry drops one <name>.json into dir the way claude's
// session registry does (the subset of fields lab reads).
func writeRegistryEntry(t *testing.T, dir, name string, e RegistryEntry) {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadPID returns the pid of a process that has already exited, for the
// stale-registry-file cases (claude only cleans its file on graceful
// exit).
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// Transcribed v0 contract: registry_test.go TestBridgeURLForDir
// (complete, 8 subtests).
func TestBridgeURLForDir(t *testing.T) {
	alive := os.Getpid()

	t.Run("matches cwd and normalises cse_ to session_", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_abc123",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_abc123"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
		if got := bridgeURLForDir(dir, "/work/other"); got != "" {
			t.Errorf("bridgeURLForDir for unmatched cwd = %q; want empty", got)
		}
	})

	t.Run("registry's session_ form passes through unchanged", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "session_xyz",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_xyz"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
	})

	t.Run("no bridge session yet means no URL", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1,
		})
		if got := bridgeURLForDir(dir, "/work/a"); got != "" {
			t.Errorf("bridgeURLForDir before bridge connect = %q; want empty", got)
		}
	})

	t.Run("stale file of a dead process is ignored", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: deadPID(t), Cwd: "/work/a", StartedAt: 9, BridgeSessionID: "cse_stale",
		})
		writeRegistryEntry(t, dir, "2.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_live",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_live"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q (dead pid must lose)", got, want)
		}
	})

	t.Run("newest startedAt wins on a reused worktree", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_old",
		})
		writeRegistryEntry(t, dir, "2.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 2, BridgeSessionID: "cse_new",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_new"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
	})

	t.Run("malformed file is skipped, valid sibling still found", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_ok",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_ok"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
	})

	t.Run("missing registry dir is a quiet miss", func(t *testing.T) {
		if got := bridgeURLForDir(filepath.Join(t.TempDir(), "nope"), "/work/a"); got != "" {
			t.Errorf("bridgeURLForDir on missing dir = %q; want empty", got)
		}
	})
}

// Transcribed v0 contract: the bridge connects seconds after spawn, so
// the capture must keep polling until the registry entry appears — a
// one-shot read at spawn time would always miss.
func TestCaptureBridgeURL_findsLateEntry(t *testing.T) {
	dir := t.TempDir()
	got := make(chan string, 1)
	go func() { got <- captureBridgeURL(context.Background(), dir, "/work/a", 5*time.Second) }()
	time.Sleep(300 * time.Millisecond)
	writeRegistryEntry(t, dir, "1.json", RegistryEntry{
		PID: os.Getpid(), Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_late",
	})
	if g, want := <-got, "https://claude.ai/code/session_late"; g != want {
		t.Errorf("captureBridgeURL = %q; want %q", g, want)
	}
}

func TestCaptureBridgeURL_timesOut(t *testing.T) {
	if got := captureBridgeURL(context.Background(), t.TempDir(), "/work/a", 300*time.Millisecond); got != "" {
		t.Errorf("captureBridgeURL with no entry = %q; want empty", got)
	}
}

func TestCaptureDeepLink_hit(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if err := os.MkdirAll(p.registryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRegistryEntry(t, p.registryDir, "1.json", RegistryEntry{
		PID: os.Getpid(), Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_abc",
	})
	got, err := p.CaptureDeepLink(context.Background(), "repo~x-1", "/work/a")
	if err != nil {
		t.Fatalf("CaptureDeepLink: %v", err)
	}
	if want := "https://claude.ai/code/session_abc"; got != want {
		t.Errorf("CaptureDeepLink = %q; want %q", got, want)
	}
}

// ADR-0017 / brief §11.2: a miss returns "" (the generic fallback is surfaced
// through FallbackOpen, never capture) AND logs loudly (v0 was silent).
func TestCaptureDeepLink_missReturnsEmptyAndWarnsLoudly(t *testing.T) {
	var logBuf bytes.Buffer
	run := newFakeRunner()
	p, _ := testProvider(t, run)
	p.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	p.bridgeTimeout = 100 * time.Millisecond

	got, err := p.CaptureDeepLink(context.Background(), "repo~x-1", "/work/nowhere")
	if err != nil {
		t.Fatalf("CaptureDeepLink: %v", err)
	}
	if got != "" {
		t.Errorf("CaptureDeepLink on miss = %q; want \"\" (fallback is FallbackOpen's job)", got)
	}
	if !strings.Contains(logBuf.String(), "deep-link capture missed") {
		t.Errorf("no loud warning logged on capture miss; log = %q", logBuf.String())
	}
}

// FallbackOpen owns the generic claude.ai open affordance (URL + v0 tooltip),
// the provider-owned metadata the SPA renders when no exact link was captured.
func TestFallbackOpen(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	fo := p.FallbackOpen()
	if fo.URL != GenericDeepLink {
		t.Errorf("FallbackOpen URL = %q; want %q", fo.URL, GenericDeepLink)
	}
	if fo.Title != genericLinkTitle {
		t.Errorf("FallbackOpen Title = %q; want %q", fo.Title, genericLinkTitle)
	}
}

// Idempotence: while a capture is in flight the session reads as
// "connecting" and a second CaptureDeepLink is an immediate no-op; the
// state clears when the capture ends.
func TestCaptureDeepLink_idempotentPerSessionAndConnecting(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	p.bridgeTimeout = 600 * time.Millisecond
	const name = "repo~x-1"

	done := make(chan string, 1)
	go func() {
		url, _ := p.CaptureDeepLink(context.Background(), name, "/work/a")
		done <- url
	}()

	// Wait for the in-flight mark.
	deadline := time.Now().Add(2 * time.Second)
	for !p.Connecting(name) {
		if time.Now().After(deadline) {
			t.Fatal("capture never marked connecting")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	got, err := p.CaptureDeepLink(context.Background(), name, "/work/a")
	if err != nil {
		t.Fatalf("second CaptureDeepLink: %v", err)
	}
	if got != "" {
		t.Errorf("second CaptureDeepLink while in flight = %q; want \"\" (no-op)", got)
	}
	if e := time.Since(start); e > 300*time.Millisecond {
		t.Errorf("second CaptureDeepLink took %v; want an immediate no-op", e)
	}

	<-done
	if p.Connecting(name) {
		t.Error("Connecting still true after capture finished")
	}
}
