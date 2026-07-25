package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Logout drops the machine's current Codex account so an operator can log a
// fresh one in — the symmetric counterpart to the device-code login flow.
// Auth is machine-level, so this is a machine-wide cut: running instances
// survive on their in-memory token until it refreshes; new spawns fail.
//
// Success is decided from a force-refreshed status, NEVER the command's exit
// code (the seam contract; claudecode pinned the same discipline). The flow:
//
//  1. Run `codex logout` (plain exec, defensive timeout — non-interactive).
//     Its exit/stderr are captured for logs and the failure message only.
//  2. Force-refresh status; a logged-out reading (which unreadable status
//     also yields) ends it as success.
//  3. Still logged in ⇒ escalate: delete <codexHome>/auth.json directly and
//     force-refresh once more.
//  4. Success iff status is now logged-out; else an error carrying the
//     captured stderr.
//
// provider.auth.changed is published when the account actually dropped.
// Every force-refresh updates the auth cache, so on return the cache already
// reflects the logged-out truth.
func (p *Provider) Logout(ctx context.Context) error {
	stderr := p.runLogoutCommand(ctx)

	if st, _ := p.AuthStatus(ctx, true); !st.LoggedIn {
		p.publishAuthChanged()
		return nil
	}

	// Residue: the command left us logged in. Escalate by removing the creds
	// file directly, then re-check.
	creds := p.credentialsPath()
	if err := os.Remove(creds); err != nil && !os.IsNotExist(err) {
		p.log.Warn("remove codex credentials during logout escalation",
			"component", "provider.codex", "path", creds, "err", err)
	}
	if st, _ := p.AuthStatus(ctx, true); !st.LoggedIn {
		p.publishAuthChanged()
		return nil
	}

	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = "no stderr captured"
	}
	return fmt.Errorf("codex logout: still logged in after credentials removal: %s", msg)
}

// runLogoutCommand runs `codex logout` through p.cli (issue #206) under a
// defensive timeout and returns its captured stderr; stdout is discarded —
// only the force-refreshed status decides success. The exit code is
// deliberately ignored for the success decision; a non-zero exit is logged,
// and the stderr is threaded into the failure message so a genuine stuck
// logout surfaces the tool's own words.
func (p *Provider) runLogoutCommand(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, p.logoutTimeout)
	defer cancel()

	_, stderr, err := p.cli.Run(cctx, provider.CLIInvocation{
		Argv: []string{p.codexBin, "logout"},
	})
	if err != nil {
		p.log.Warn("codex logout exited non-zero (success is decided from status, not exit code)",
			"component", "provider.codex", "err", err, "stderr", strings.TrimSpace(string(stderr)))
	}
	return string(stderr)
}

// credentialsPath resolves codex's machine-level credentials file the same
// way the codex binary does — <codexHome>/auth.json under the adapter's own
// home resolution ($CODEX_HOME when set, else <loginDir>/.codex). The
// rm-escalation must target the exact file `codex logout` would.
func (p *Provider) credentialsPath() string {
	return filepath.Join(codexHomeDir(p.loginDir), "auth.json")
}
