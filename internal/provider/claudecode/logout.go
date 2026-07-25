package claudecode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Logout drops the machine's current Claude account so an operator can log a
// fresh one in — the symmetric counterpart to LoginStart/LoginSubmitCode. Auth
// is machine-level/per-OS-user (ADR-0001/0008), so this is a machine-wide cut:
// running instances survive on their in-memory token until it refreshes/401s;
// new spawns fail immediately. It does NOT stop instances (bare by decision)
// and does NOT publish provider.auth.changed — the HTTP layer owns the SSE
// announcement and the confirm gate, exactly as the DoD assigns them.
//
// Success is decided from a force-refreshed status, NEVER the command's exit
// code: `claude auth logout`'s exit code and idempotency are undocumented and
// unreliable (upstream #30924), and a non-zero exit alongside a logged-out
// status is still a success. The flow:
//
//  1. Run `claude auth logout` (plain exec, defensive timeout — non-interactive,
//     no TTY). Its exit/stderr are captured for logs and the failure message only.
//  2. Force-refresh status; a logged-out reading (which unreadable status also
//     yields, per the package-wide invariant) ends it as success.
//  3. Still logged in ⇒ escalate: delete the credentials file directly and
//     force-refresh once more.
//  4. Success iff status is now logged-out; else an error carrying the captured
//     stderr.
//
// Every force-refresh updates the auth cache, so on return the cache already
// reflects the new (logged-out) truth — no separate invalidation is needed.
func (p *Provider) Logout(ctx context.Context) error {
	stderr := p.runLogoutCommand(ctx)

	// Success is the status, not the exit code (D: "Success = status").
	if st, _ := p.AuthStatus(ctx, true); !st.LoggedIn {
		return nil
	}

	// Residue: the command left us logged in. Escalate by removing the creds
	// file directly, then re-check.
	creds := credentialsPath()
	if err := os.Remove(creds); err != nil && !os.IsNotExist(err) {
		p.log.Warn("remove claude credentials during logout escalation",
			"component", "provider.claudecode", "path", creds, "err", err)
	}
	if st, _ := p.AuthStatus(ctx, true); !st.LoggedIn {
		return nil
	}

	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = "no stderr captured"
	}
	return fmt.Errorf("claude logout: still logged in after credentials removal: %s", msg)
}

// runLogoutCommand runs `claude auth logout` through p.cli (issue #206)
// under a defensive timeout and returns its captured stderr; stdout is
// discarded — only the force-refreshed status decides success. The exit code
// is deliberately ignored for the success decision (undocumented/unreliable —
// upstream #30924); a non-zero exit is logged, and the stderr is threaded
// into the failure message so a genuine stuck logout surfaces the tool's own
// words.
func (p *Provider) runLogoutCommand(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, p.logoutTimeout)
	defer cancel()

	_, stderr, err := p.cli.Run(cctx, provider.CLIInvocation{
		Argv: []string{p.claudeBin, "auth", "logout"},
	})
	if err != nil {
		p.log.Warn("claude auth logout exited non-zero (success is decided from status, not exit code)",
			"component", "provider.claudecode", "err", err, "stderr", strings.TrimSpace(string(stderr)))
	}
	return string(stderr)
}

// masterConfigDir resolves claude's machine-level config dir the same way the
// claude binary does: CLAUDE_CONFIG_DIR when set (the isolation seam the compat
// test relies on — it outranks HOME for ALL claude state, compat §3a), else
// $HOME/.claude. Master-store operations (the rm-escalation, InjectCredentials'
// source, the credential-authority seam's master reads/writes and the refresh
// poke's CLAUDE_CONFIG_DIR pin) all derive from this one resolver so lab and the
// CLI agree on exactly one master layout.
func masterConfigDir() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	return dir
}

// credentialsPath resolves claude's machine-level credentials file. The
// rm-escalation must target the exact file `claude auth logout` would, so this
// mirrors claude's own resolution (masterConfigDir) rather than deriving from
// lab's injected registry/config paths.
func credentialsPath() string {
	return filepath.Join(masterConfigDir(), ".credentials.json")
}
