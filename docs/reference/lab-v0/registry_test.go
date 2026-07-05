package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// writeRegistryEntry drops one <name>.json into dir the way claude's session
// registry does (the subset of fields lab reads).
func writeRegistryEntry(t *testing.T, dir, name string, e registryEntry) {
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
// stale-registry-file cases (claude only cleans its file on graceful exit).
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestBridgeURLForDir(t *testing.T) {
	alive := os.Getpid()

	t.Run("matches cwd and normalises cse_ to session_", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", registryEntry{
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
		writeRegistryEntry(t, dir, "1.json", registryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "session_xyz",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_xyz"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
	})

	t.Run("no bridge session yet means no URL", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", registryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1,
		})
		if got := bridgeURLForDir(dir, "/work/a"); got != "" {
			t.Errorf("bridgeURLForDir before bridge connect = %q; want empty", got)
		}
	})

	t.Run("stale file of a dead process is ignored", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", registryEntry{
			PID: deadPID(t), Cwd: "/work/a", StartedAt: 9, BridgeSessionID: "cse_stale",
		})
		writeRegistryEntry(t, dir, "2.json", registryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_live",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_live"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q (dead pid must lose)", got, want)
		}
	})

	t.Run("newest startedAt wins on a reused worktree", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", registryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_old",
		})
		writeRegistryEntry(t, dir, "2.json", registryEntry{
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
		writeRegistryEntry(t, dir, "1.json", registryEntry{
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

// The bridge connects seconds after spawn, so the capture must keep polling
// until the registry entry appears — a one-shot read at spawn time would
// always miss. The capture runs in a goroutine (as it does under startCapture)
// so the entry can be written from the test goroutine mid-poll.
func TestCaptureBridgeURL_findsLateEntry(t *testing.T) {
	dir := t.TempDir()
	got := make(chan string, 1)
	go func() { got <- captureBridgeURL(dir, "/work/a", 5*time.Second) }()
	time.Sleep(300 * time.Millisecond)
	writeRegistryEntry(t, dir, "1.json", registryEntry{
		PID: os.Getpid(), Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_late",
	})
	if g, want := <-got, "https://claude.ai/code/session_late"; g != want {
		t.Errorf("captureBridgeURL = %q; want %q", g, want)
	}
}

func TestCaptureBridgeURL_timesOut(t *testing.T) {
	if got := captureBridgeURL(t.TempDir(), "/work/a", 300*time.Millisecond); got != "" {
		t.Errorf("captureBridgeURL with no entry = %q; want empty", got)
	}
}
