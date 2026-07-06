package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Claude stopped printing the remote-control deep link into the terminal
// between 2.1.156 and 2.1.170, so the pane can never be scraped for it
// again. The replacement source is claude's own session registry:
// ~/.claude/sessions/<pid>.json, one file per live claude process, holding
// the process's cwd and — written the moment the Remote Control bridge
// connects, before any user input — its bridgeSessionId. The deep link is
// https://claude.ai/code/<bridgeSessionId>, the same construction claude
// uses internally. Worktrees are per-instance, so a cwd match identifies
// exactly one lab session. (v0 registry.go, verified against 2.1.198.)

// GenericDeepLink is the fallback when capture misses: it opens the
// claude.ai session picker instead of the exact session. Callers must
// never persist it over a previously captured real link.
const GenericDeepLink = "https://claude.ai/code"

// RegistryEntry is the subset lab reads of one ~/.claude/sessions/<pid>.json.
// Exported for the compat fixture test; unknown registry fields are
// ignored by construction.
type RegistryEntry struct {
	PID             int    `json:"pid"`
	Cwd             string `json:"cwd"`
	StartedAt       int64  `json:"startedAt"` // unix millis
	BridgeSessionID string `json:"bridgeSessionId"`
	// SessionID is claude's transcript filename stem: the chat surface reads
	// <projects>/<cwd-slug>/<SessionID>.jsonl (compat.md §5). Formerly one of
	// the ignored registry keys; read since ADR-0016.
	SessionID string `json:"sessionId"`
}

// BridgeURL renders the claude.ai deep link for a bridge session id. The
// registry stores the URL-ready session_… form, but claude's transcripts
// carry the same id as cse_…; normalise defensively so either spelling
// yields the link claude itself would build (its toCompatSessionId:
// cse_X → session_X). Any other prefix passes through unchanged.
func BridgeURL(id string) string {
	if rest, ok := strings.CutPrefix(id, "cse_"); ok {
		id = "session_" + rest
	}
	return "https://claude.ai/code/" + id
}

// bridgeURLForDir is a single registry pass: the deep link of the newest
// live claude process whose cwd is dir, or "". The cwd comparison is an
// exact string match — claude records its kernel-reported
// (symlink-resolved) cwd, and lab's worktree paths are absolute and
// symlink-free by construction. Newest-alive wins because a worktree path
// can be reused across runs and a SIGKILLed predecessor leaves its
// registry file behind — claude cleans up on graceful exit only.
func bridgeURLForDir(registryDir, dir string) string {
	files, err := os.ReadDir(registryDir)
	if err != nil {
		return ""
	}
	var best RegistryEntry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(registryDir, f.Name()))
		if err != nil {
			continue
		}
		var e RegistryEntry
		if json.Unmarshal(b, &e) != nil {
			continue // malformed sibling never poisons the scan
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
	return BridgeURL(best.BridgeSessionID)
}

// pidAlive reports whether pid is a live process. Signal 0 probes
// existence without delivering anything; EPERM still proves liveness (the
// pid exists, just owned by someone else).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// captureBridgeURL polls the registry every 200ms up to timeout for the
// deep link of the claude process running in dir, returning "" if none
// appears in time (or ctx is cancelled). The poll covers claude's full
// boot plus the bridge connect, both of which happen after tmux reports
// the session live — a one-shot read at spawn time would always miss. The
// check runs before the deadline test, so at least one pass always
// happens even with timeout 0.
func captureBridgeURL(ctx context.Context, registryDir, dir string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		if url := bridgeURLForDir(registryDir, dir); url != "" {
			return url
		}
		if time.Now().After(deadline) {
			return ""
		}
		if !sleepOrDone(ctx, pollInterval) {
			return ""
		}
	}
}

// CaptureDeepLink implements provider.AgentProvider: poll the registry up
// to bridgeTimeout for the deep link of the session running in worktree.
// On a miss it logs LOUDLY (brief §11.2 — v0 was silent) and returns
// GenericDeepLink; the caller must not overwrite a real stored link with
// it.
//
// Idempotent per session: while a capture is in flight, a second call is a
// no-op returning ("", nil), so callers can't stack polls on one session.
// The in-flight set doubles as the "connecting…" render state, exposed via
// Connecting.
func (p *Provider) CaptureDeepLink(ctx context.Context, sessionName, worktree string) (string, error) {
	p.captureMu.Lock()
	if p.capturing[sessionName] {
		p.captureMu.Unlock()
		return "", nil
	}
	p.capturing[sessionName] = true
	p.captureMu.Unlock()
	defer func() {
		p.captureMu.Lock()
		delete(p.capturing, sessionName)
		p.captureMu.Unlock()
	}()

	if url := captureBridgeURL(ctx, p.registryDir, worktree, p.bridgeTimeout); url != "" {
		return url, nil
	}
	p.log.Warn("deep-link capture missed — falling back to the generic claude.ai link",
		"component", "provider.claudecode", "session", sessionName,
		"worktree", worktree, "registry_dir", p.registryDir, "timeout", p.bridgeTimeout)
	return GenericDeepLink, nil
}

// Connecting reports whether sessionName has a deep-link capture in
// flight — the "connecting…" state the UI shows before a link is known.
func (p *Provider) Connecting(sessionName string) bool {
	p.captureMu.Lock()
	defer p.captureMu.Unlock()
	return p.capturing[sessionName]
}
