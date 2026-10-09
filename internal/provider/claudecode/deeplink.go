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

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
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

// GenericDeepLink is claude-code's fallback open affordance (ADR-0017): it
// opens the claude.ai session picker instead of the exact session. It is
// provider-owned metadata surfaced through FallbackOpen — never returned
// from CaptureDeepLink, which yields "" on a miss so the write-only-on-hit
// rule needs no cross-package constant.
const GenericDeepLink = "https://claude.ai/code"

// genericLinkTitle is the human tooltip on the fallback open link, pinned
// verbatim from v0 (the SPA rendered this exact text as the anchor title).
const genericLinkTitle = "Opens the claude.ai session picker — the exact deep link wasn't captured"

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
	// the ignored registry keys; read since ADR-0016. /clear rewrites it in the
	// same step that rotates the session — before the fresh transcript file
	// exists (issue #79 decision 5).
	SessionID string `json:"sessionId"`
	// Status is the CLI-maintained activity status, rewritten on every
	// transition (issue #79, compat §2): "busy" (a query is loading OR
	// delegated agents are still active), "waiting" (a dialog/prompt is on
	// screen; WaitingFor says which), or "idle" (neither). ReadChat composes
	// it over the transcript fold (registryStatus); any other value — or none
	// — is unusable and leaves the fold alone.
	Status string `json:"status"`
	// WaitingFor is the human reason accompanying Status "waiting" (observed:
	// "input needed", "dialog open", "worker request", "sandbox request", or a
	// permission prompt's own text) — surfaced as Chat.StateDetail.
	WaitingFor string `json:"waitingFor"`
	// StatusUpdatedAt is when Status last changed (unix millis). Read for the
	// record only: the status is event-driven, not a heartbeat, so lab applies
	// NO age bound (issue #79 decision 3) — a dead CLI is caught by its pid
	// (pidAlive) and tmux liveness, never by a stale timestamp.
	StatusUpdatedAt int64 `json:"statusUpdatedAt"`
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

// newestLiveEntry is the single registry pass both registry readers share:
// the entry of the newest live claude process whose cwd is dir and that
// passes want, or false. The cwd comparison is an exact string match —
// claude records its kernel-reported (symlink-resolved) cwd, and lab's
// worktree paths are absolute and symlink-free by construction. Newest-alive
// wins because a worktree path can be reused across runs and a SIGKILLed
// predecessor leaves its registry file behind — claude cleans up on graceful
// exit only.
func newestLiveEntry(registryDir, dir string, want func(RegistryEntry) bool) (RegistryEntry, bool) {
	files, err := os.ReadDir(registryDir)
	if err != nil {
		return RegistryEntry{}, false
	}
	var (
		best  RegistryEntry
		found bool
	)
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
		if !want(e) || e.Cwd != dir || !pidAlive(e.PID) {
			continue
		}
		if !found || e.StartedAt > best.StartedAt {
			best, found = e, true
		}
	}
	return best, found
}

// bridgeURLForDir is the deep-link registry pass: the link of the newest live
// claude process in dir that has connected its bridge, or "".
func bridgeURLForDir(registryDir, dir string) string {
	e, ok := newestLiveEntry(registryDir, dir, func(e RegistryEntry) bool { return e.BridgeSessionID != "" })
	if !ok {
		return ""
	}
	return BridgeURL(e.BridgeSessionID)
}

// registryEntryForDir is the chat surface's registry pass: the entry of the
// newest live claude process in dir that carries a sessionId. Unlike the deep
// link, sessionId is present from process start (it is the transcript
// filename), so it needs no bridge-connect wait. LocateTranscript (the
// transcript identity) and ReadChat (the status overlay) both select through
// this one function, so transcript and state always come from the SAME
// session entry (issue #79 decision 3).
func registryEntryForDir(registryDir, dir string) (RegistryEntry, bool) {
	return newestLiveEntry(registryDir, dir, func(e RegistryEntry) bool { return e.SessionID != "" })
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

// CaptureDeepLink implements provider.DeepLinker: poll the registry under the
// instance HOME home (issue #202) up to bridgeTimeout for the deep link of the
// session running in worktree. On a miss it logs LOUDLY (brief §11.2 — v0 was
// silent) and returns "" — the generic fallback is surfaced through
// FallbackOpen, not capture, so the caller's write-only-on-hit rule needs no
// cross-package constant. An empty home is a quiet miss: a run with no per-run
// home has no instance registry to read, and lab never falls back to the master
// store (isolation by construction).
//
// Idempotent per session: while a capture is in flight, a second call is a
// no-op returning ("", nil), so callers can't stack polls on one session.
// The in-flight set doubles as the "connecting…" render state, exposed via
// Connecting.
func (p *Provider) CaptureDeepLink(ctx context.Context, sessionName, worktree, home string) (string, error) {
	if home == "" {
		return "", nil
	}
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

	registryDir := registryDirUnder(home)
	if url := captureBridgeURL(ctx, registryDir, worktree, p.bridgeTimeout); url != "" {
		return url, nil
	}
	p.log.Warn("deep-link capture missed — the row will show the generic claude.ai fallback link",
		"component", "provider.claudecode", "session", sessionName,
		"worktree", worktree, "registry_dir", registryDir, "timeout", p.bridgeTimeout)
	return "", nil
}

// FallbackOpen implements provider.DeepLinker: claude-code's generic open
// affordance (the claude.ai session picker + its v0 tooltip), rendered by the
// SPA when no exact link was captured.
func (p *Provider) FallbackOpen() provider.OpenAffordance {
	return provider.OpenAffordance{URL: GenericDeepLink, Title: genericLinkTitle}
}

// Connecting reports whether sessionName has a deep-link capture in
// flight — the "connecting…" state the UI shows before a link is known.
func (p *Provider) Connecting(sessionName string) bool {
	p.captureMu.Lock()
	defer p.captureMu.Unlock()
	return p.capturing[sessionName]
}
