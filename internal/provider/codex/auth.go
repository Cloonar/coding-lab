package codex

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// The auth check runs `codex login status` (through the CLI-runner seam,
// issue #206 — on a container-mode server that is the containerized CLI) and
// reads its plain-text verdict. Peeking at ~/.codex/auth.json directly is
// rejected for the same reason claudecode rejects its credentials file: it
// couples lab to codex's internal storage and skips the token validation the
// status command does.
//
// Pinned output shapes (live 0.133.0, 2026-07-10):
//
//	logged in  → exit 0, output contains "Logged in using ChatGPT"
//	logged out → exit 1, output contains "Not logged in"

// ParseAuthStatus parses the status command's combined output plus its
// exit-code verdict: logged in iff the command exited 0 AND the output
// carries "Logged in". Method is the lowercased token after "using " when
// present (e.g. "chatgpt"); Email stays "" — `codex login status` does not
// print one. CheckedAt is left zero — the caller stamps it. Exported for the
// compat fixture test.
func ParseAuthStatus(out []byte, exitOK bool) provider.AuthStatus {
	text := string(out)
	st := provider.AuthStatus{
		LoggedIn: exitOK && strings.Contains(text, "Logged in"),
	}
	if _, after, ok := strings.Cut(text, "using "); ok {
		method := after
		if i := strings.IndexFunc(method, func(r rune) bool { return r == '\n' || r == '\r' }); i >= 0 {
			method = method[:i]
		}
		st.Method = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(method), ".")))
	}
	return st
}

// runStatus executes `{codex} login status` through p.cli (issue #206),
// capturing output and the exit code. A plain non-zero exit is a DEFINITIVE
// logged-out answer (the pinned logged-out shape is exit 1 + "Not logged
// in") — recognized by the DIRECT *exec.ExitError type check the CLIRunner
// contract guarantees survives the seam unwrapped; only a run failure that
// isn't an exit status (binary missing, ctx cancelled) is an error.
//
// Pre-seam this parsed CombinedOutput; the runner separates the streams, so
// the parse runs over stdout then stderr concatenated. Losing the
// interleaving is safe by construction: ParseAuthStatus is a pure substring
// check ("Logged in", the "using " method cut) plus the exit-code verdict —
// never line-order sensitive.
func (p *Provider) runStatus(ctx context.Context) (provider.AuthStatus, error) {
	stdout, stderr, runErr := p.cli.Run(ctx, provider.CLIInvocation{
		Argv: []string{p.codexBin, "login", "status"},
	})
	out := append(append([]byte(nil), stdout...), stderr...)
	if runErr != nil {
		if _, isExit := runErr.(*exec.ExitError); !isExit {
			return provider.AuthStatus{}, fmt.Errorf("codex login status: %w", runErr)
		}
		return ParseAuthStatus(out, false), nil
	}
	return ParseAuthStatus(out, true), nil
}

// AuthStatus implements provider.AgentProvider. Without force, a result
// younger than authTTL (30s) is served from cache so renders cost nothing;
// force re-runs the check unconditionally — every spawn decision forces,
// because the render cache can hide a silent token expiry. On error the
// returned (and cached) status is logged-out; error results are cached
// exactly like successes (claudecode's pinned discipline).
func (p *Provider) AuthStatus(ctx context.Context, force bool) (provider.AuthStatus, error) {
	p.authMu.Lock()
	defer p.authMu.Unlock()
	if !force && time.Since(p.authChecked) <= p.authTTL {
		return p.authCache, nil
	}
	return p.refreshAuthLocked(ctx)
}

// refreshAuthLocked re-runs the status check under authMu and updates the
// cache. The error (if any) is returned so the caller can log it; the
// caller treats an error as logged-out — better to show the login banner
// than to spawn doomed sessions.
func (p *Provider) refreshAuthLocked(ctx context.Context) (provider.AuthStatus, error) {
	st, err := p.runStatus(ctx)
	if err != nil {
		st = provider.AuthStatus{} // unreadable status ⇒ logged out
	}
	st.CheckedAt = p.now()
	p.authCache = st
	p.authChecked = time.Now()
	return st, err
}
