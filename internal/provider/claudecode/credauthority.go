package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// This file implements the credential-authority seam (issue #222) — the
// single-refresher design that stops per-run OAuth snapshots from forking the
// provider's token family. Lab core is the ONE refresher per grant (against the
// machine's MASTER store); every instance is a consumer that receives the
// rotated family through InjectCredentials, never a refresher of its own. A
// per-instance CLI self-refresh rotates the shared refresh token and logs every
// other holder — the master and all sibling snapshots — out, which is the whole
// bug this seam exists to prevent.
//
// The division of labour is strict: CORE owns the schedule, the change
// comparison (CredentialsSig equality only), the fan-out ordering, and the
// adopt tie-breaks; this ADAPTER owns every file path and the CLI recipe, and
// core NEVER parses token contents. See provider.AgentProvider's
// "credential-authority seam" docs for the exact contracts.

// credentialsPathFor resolves the .credentials.json this seam stats/copies for a
// given home. An empty home means the machine's MASTER store (credentialsPath,
// the CLAUDE_CONFIG_DIR-aware resolver shared with logout.go); a non-empty home
// is that instance's private copy at <home>/.claude/.credentials.json — the
// SAME path InjectCredentials writes (instanceCredsUnder), so inject/sig/adopt
// all agree on one file.
func credentialsPathFor(home string) string {
	if home == "" {
		return credentialsPath()
	}
	return instanceCredsUnder(home)
}

// CredentialsSig implements provider.AgentProvider (issue #222): a cheap, opaque
// existence+mtime+size digest over the credential file under home, following the
// SpoolSig precedent (dialogspool.go). Core compares it for EQUALITY only and
// never parses it; "" doubles as "logged out / never injected", so
// CredentialsSig("") == "" on a host whose master store was never populated is a
// pinned acceptance criterion. The returned mtime is the file's modification
// time (zero when absent) — typed filesystem metadata core uses for the
// newest-mtime adopt tie-break, never a parsed token field.
//
// This deliberately covers ONLY .credentials.json — the token-family file
// OAuth rotation rewrites in place. ~/.claude.json's oauthAccount block (which
// InjectCredentials also mirrors) is account METADATA, not token material: any
// auth change that matters to this seam — a re-login or a token-family rotation
// — rewrites .credentials.json too, so stating the family file alone detects
// every rotation while ignoring incidental churn of unrelated .claude.json keys
// (folder trust, onboarding, projects). Widening the sig to .claude.json would
// make it flap on every trust-seed write and force needless fan-out/adopt.
func (p *Provider) CredentialsSig(home string) (string, time.Time) {
	path := credentialsPathFor(home)
	fi, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}
	}
	// Same digest shape as SpoolSig: "<base>:<mtimeNano>:<size>;".
	sig := fmt.Sprintf("%s:%d:%d;", filepath.Base(path), fi.ModTime().UnixNano(), fi.Size())
	return sig, fi.ModTime()
}

// AdoptCredentials implements provider.AgentProvider (issue #222): the reverse
// of InjectCredentials. It copies the instance HOME's .credentials.json back
// into the machine's MASTER store ATOMICALLY (writeAtomic creates its tmpfile
// 0600 inside the master dir via os.CreateTemp, fsyncs, then os.Renames it into
// place), so a live host-side CLI reading the master store never observes a torn
// file. Core calls it after detecting an instance self-refreshed (the instance's
// sig diverged from the sig core injected), so the master adopts that
// still-valid rotated family before a later HOME wipe destroys the only live
// copy. Core owns WHEN to adopt; this method owns only the atomic copy.
//
// An empty instanceHome is a programmer error (matching InjectCredentials). A
// MISSING instance credential file is an error too — unlike InjectCredentials'
// tolerant missing-master case: adoption is only ever requested after a sig
// proved the file existed, so a vanished file must be surfaced to the caller,
// never silently adopted as nothing.
func (p *Provider) AdoptCredentials(instanceHome string) error {
	if instanceHome == "" {
		return errors.New("claudecode: AdoptCredentials requires a non-empty instance HOME")
	}
	src := instanceCredsUnder(instanceHome)
	data, err := os.ReadFile(src)
	if err != nil {
		// Missing source included: adoption was requested on the strength of a
		// prior non-empty sig, so a read failure is a real error, never a no-op.
		return fmt.Errorf("claudecode: read instance credentials %s: %w", src, err)
	}
	dst := credentialsPath()
	// Create the master dir 0700 if absent (a host that has an instance copy but
	// no master dir yet), then copy atomically WITHIN it.
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("claudecode: mkdir master config dir %s: %w", filepath.Dir(dst), err)
	}
	if err := writeAtomic(dst, data); err != nil {
		return fmt.Errorf("claudecode: adopt instance credentials into %s: %w", dst, err)
	}
	return nil
}

// RefreshCredentials implements provider.AgentProvider (issue #222): the loop's
// blind periodic poke against the MASTER store. It runs claude's OWN CLI
// non-interactively — a real API call in print mode, `claude -p --model haiku`
// with a trivial prompt — pinned to the master config dir via CLAUDE_CONFIG_DIR.
// The CLI reads credential state from disk, decides for ITSELF whether the token
// is near expiry, and rotates the store in place if so; a valid-token poke is a
// no-op costing one trivial haiku call. Lab never parses token contents and
// never speaks the OAuth protocol — it pokes, then observes via CredentialsSig.
// The probe was confirmed live 2026-07-24 (compat §3b): with the master's
// expiresAt forged 60 s into the past, this exact recipe rotated the master
// store in place.
//
// Callers detect a resulting rotation PURELY via CredentialsSig("") changing,
// never from this return value: a poke that rotated nothing and one that rotated
// the whole family are indistinguishable here by design. The error channel
// reports only that the poke itself could not run. Callers gate on
// CredentialsSig("") != "" and bound ctx with a timeout, since the CLI is a live
// subprocess.
func (p *Provider) RefreshCredentials(ctx context.Context) error {
	dir := masterConfigDir()

	// Argv is the probe-confirmed non-interactive refresh trigger (compat §3b):
	// print mode + a real (cheap) model + a minimal prompt. `-p` makes it a
	// one-shot API call with no TTY, so it runs unattended and exits.
	//
	// Env: OVERRIDE CLAUDE_CONFIG_DIR to the master dir. The lab server's own
	// environment may carry a stray/instance CLAUDE_CONFIG_DIR, and the CLI
	// resolves that variable OVER HOME (compat §3a) — so the pin must be explicit
	// and must never be empty, or the poke would refresh the wrong (or a
	// relative, pre-2.1.214) store. The runner's filter-then-append semantics
	// (provider.CLIInvocation.Env) guarantee exactly one, master-pointing entry
	// regardless of exec's dedup order; dir is always non-empty here
	// (masterConfigDir falls back to $HOME/.claude), preserving the never-empty
	// pin invariant compat §3a documents.
	//
	// Working dir: the master config dir (stable, exists after a login). We do
	// NOT create it here — if it is absent the host never logged in and the CLI's
	// own chdir/auth failure surfaces as the returned error; the caller gates the
	// poke on CredentialsSig("") != "" so this is only ever reached for a
	// populated store.
	//
	// Stdout is discarded (the model's reply is irrelevant — only the on-disk
	// rotation matters); stderr feeds the failure message.
	_, stderr, err := p.cli.Run(ctx, provider.CLIInvocation{
		Argv: []string{p.claudeBin, "-p", "--model", "haiku", "ok"},
		Env:  []string{"CLAUDE_CONFIG_DIR=" + dir},
		Dir:  dir,
	})
	if err != nil {
		return fmt.Errorf("claudecode: credential refresh poke (claude -p --model haiku) failed: %w: %s",
			err, stderrTail(string(stderr)))
	}
	return nil
}

// stderrTail trims a captured stderr buffer to a short, log-friendly tail for a
// wrapped CLI error (mirroring how logout.go threads `claude`'s own words into
// its failure message). "" collapses to a fixed marker so the error never ends
// in a dangling colon.
func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no stderr captured"
	}
	const max = 500
	if len(s) > max {
		s = "…" + s[len(s)-max:]
	}
	return s
}
