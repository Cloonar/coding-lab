package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Claude stopped printing the remote-control deep link into the terminal
// between 2.1.156 and 2.1.170: the "/remote-control is active · … or at
// <url>" transcript message was removed (only the status-bar "Remote Control
// active" chip remains), so scraping the tmux pane can never find the link
// again. The replacement source is claude's own session registry:
// ~/.claude/sessions/<pid>.json, one file per live claude process, holding the
// process's cwd and — written the moment the Remote Control bridge connects,
// before any user input — its bridgeSessionId. The deep link is
// https://claude.ai/code/<bridgeSessionId>, the same construction claude uses
// internally. Worktrees are per-instance (ADR-0017), so a cwd match identifies
// exactly one lab session.

// registryEntry is the subset lab reads of one ~/.claude/sessions/<pid>.json.
type registryEntry struct {
	PID             int    `json:"pid"`
	Cwd             string `json:"cwd"`
	StartedAt       int64  `json:"startedAt"` // unix millis
	BridgeSessionID string `json:"bridgeSessionId"`
}

// captureBridgeURL polls the registry up to timeout for the deep link of the
// claude process running in dir, returning "" if none appears in time. The
// poll covers claude's full boot plus the bridge connect, both of which happen
// after tmux reports the session live.
func captureBridgeURL(registryDir, dir string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		if url := bridgeURLForDir(registryDir, dir); url != "" {
			return url
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// bridgeURLForDir is a single registry pass: the deep link of the newest live
// claude process whose cwd is dir, or "". The cwd comparison is an exact
// string match — claude records its kernel-reported (symlink-resolved) cwd,
// and lab's worktree paths are absolute and symlink-free by construction.
// Newest-alive wins because a worktree path can be reused across runs (an AFK
// requeue reuses afk-<N>'s worktree) and a SIGKILLed predecessor leaves its
// registry file behind — claude cleans up on graceful exit only.
func bridgeURLForDir(registryDir, dir string) string {
	files, err := os.ReadDir(registryDir)
	if err != nil {
		return ""
	}
	var best registryEntry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(registryDir, f.Name()))
		if err != nil {
			continue
		}
		var e registryEntry
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		if e.BridgeSessionID == "" || e.Cwd != dir || !pidAlive(e.PID) {
			continue
		}
		if best.BridgeSessionID == "" || e.StartedAt > best.StartedAt {
			best = e
		}
	}
	if best.BridgeSessionID == "" {
		return ""
	}
	return bridgeURL(best.BridgeSessionID)
}

// pidAlive reports whether pid is a live process. Signal 0 probes existence
// without delivering anything; EPERM still proves liveness (the pid exists,
// just owned by someone else).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// bridgeURL renders the claude.ai deep link for a bridge session id. The
// registry stores the URL-ready session_… form, but claude's transcripts carry
// the same id as cse_…; normalise defensively so either spelling yields the
// link claude itself would build (its toCompatSessionId: cse_X → session_X).
func bridgeURL(id string) string {
	if rest, ok := strings.CutPrefix(id, "cse_"); ok {
		id = "session_" + rest
	}
	return "https://claude.ai/code/" + id
}
