package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// The auth check runs `claude auth status --json` (through the CLI-runner
// seam, issue #206 — on a container-mode server that is the containerized
// CLI) and reads the top-level fields. Peeking at ~/.claude/.credentials.json
// directly was rejected in v0 and stays rejected: it couples lab to claude's
// internal storage and skips the token-expiry validation the status command
// does for us.

// statusJSON is the parsed subset of `claude auth status --json` stdout.
// Observed extras (apiProvider, orgId, orgName, subscriptionType) are
// ignored (compat probe 2.1.198).
type statusJSON struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
}

// ParseAuthStatus parses the status command's stdout. A missing loggedIn
// field defaults to false with no error; anything that is not a JSON object
// (including empty stdout) is an error. CheckedAt is left zero — the caller
// stamps it. Exported for the compat fixture test.
func ParseAuthStatus(data []byte) (provider.AuthStatus, error) {
	var s statusJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return provider.AuthStatus{}, err
	}
	return provider.AuthStatus{LoggedIn: s.LoggedIn, Email: s.Email, Method: s.AuthMethod}, nil
}

// runStatus executes `{claude} auth status --json` through p.cli (issue
// #206; inherited env, inherited cwd) and applies the v0 decision order,
// which is load-bearing:
//
//  1. Parse stdout REGARDLESS of the exit code — `claude auth status` may
//     exit non-zero when logged out while still emitting {"loggedIn":false},
//     and that is a definitive answer, not an error.
//  2. Stdout unparseable AND the command failed → the run error.
//  3. Stdout unparseable but the command succeeded → the parse error.
func (p *Provider) runStatus(ctx context.Context) (provider.AuthStatus, error) {
	out, _, runErr := p.cli.Run(ctx, provider.CLIInvocation{
		Argv: []string{p.claudeBin, "auth", "status", "--json"},
	})
	st, parseErr := ParseAuthStatus(out)
	if parseErr == nil {
		return st, nil
	}
	if runErr != nil {
		return provider.AuthStatus{}, fmt.Errorf("claude auth status: %w", runErr)
	}
	return provider.AuthStatus{}, fmt.Errorf("parse auth status: %w", parseErr)
}

// AuthStatus implements provider.AgentProvider. Without force, a result
// younger than authTTL (30s) is served from cache so renders cost nothing;
// force re-runs the check unconditionally — every spawn decision forces,
// because the render cache can hide a silent token expiry (v0-pinned).
// On error the returned (and cached) status is logged-out; error results
// are cached exactly like successes.
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
// than to spawn doomed remote-control sessions.
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
