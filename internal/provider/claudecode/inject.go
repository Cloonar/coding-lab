package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// InjectCredentials implements provider.AgentProvider (issue #202): install the
// machine's MASTER claude credentials into the run's private instance HOME so
// the spawned session authenticates as the logged-in operator without ever
// reading — or writing — the master store at runtime. Exactly one call site
// exists (the launch path), so a server-side credential proxy can later replace
// this copy wholesale. It copies two things, each independently tolerant of a
// missing master file:
//
//   - the master credentials file (credentialsPath, the CLAUDE_CONFIG_DIR-aware
//     resolution logout.go shares) → <home>/.claude/.credentials.json, the file
//     claude reads for its OAuth tokens (compat §3);
//   - the master ~/.claude.json's oauthAccount key (account metadata) merged
//     into <home>/.claude/.claude.json create-or-update style — the CLI treats
//     that block as part of "logged in", so mirroring it is cheap insurance. This
//     writer and SeedWorkspace's trust seed both preserve each other's keys via
//     the generic map round-trip, so injector/seed order does not matter.
//
// It ALWAYS returns CLAUDE_CONFIG_DIR=<home>/.claude (explicitly SET) — the
// claude CLI resolves CLAUDE_CONFIG_DIR *over* HOME for ALL of its state
// (credentials, .claude.json, projects/ — compat §3a), and lab itself honors a
// service-wide value for the master store (credentialsPath, shared with
// logout.go), so a value inherited through tmux would point the instance's
// claude straight back at that master store. Pinning the instance's OWN
// .claude dir both neutralizes the inherited value and keeps resolution
// version-proof: the previous CLAUDE_CONFIG_DIR= (empty) pin relied on
// empty-behaves-as-unset, which only holds from ~2.1.214 — on 2.1.198 the
// credentials path joins the empty dir into a RELATIVE ./.credentials.json,
// so every instance spawned unauthenticated and chat replies died at "Not
// logged in" (live-straced 2026-07-23; compat §3a). The same defense codex
// mounts with its unconditional CODEX_HOME pin. All adapter readers/writers
// (globalConfigUnder, projectsDirUnder, registryDirUnder, instanceCredsUnder)
// resolve inside the pinned dir, so the CLI and lab agree on one layout.
//
// A MISSING master file (the operator never logged in) is NOT an
// error: it is logged loudly and simply not written, so the spawn proceeds and
// the CLI shows its own login prompt in the pane, exactly today's behavior on
// an unauthenticated host. An empty home is a programmer error. The injector
// never deletes anything; wipe-on-stop of the instance HOME shreds the copy.
func (p *Provider) InjectCredentials(instanceHome string) ([]string, error) {
	if instanceHome == "" {
		return nil, errors.New("claudecode: InjectCredentials requires a non-empty instance HOME")
	}
	if err := p.copyMasterCredentials(instanceHome); err != nil {
		return nil, err
	}
	if err := p.mirrorOAuthAccount(instanceHome); err != nil {
		return nil, err
	}
	// Unconditional, and deliberately the instance's own .claude dir: an
	// inherited CLAUDE_CONFIG_DIR outranks HOME in the CLI's resolution, so it
	// must be overridden for the instance — with an explicit dir, never empty
	// (empty is treated as a REAL config dir by pre-2.1.214 CLIs; compat §3a).
	return []string{"CLAUDE_CONFIG_DIR=" + instanceConfigDirUnder(instanceHome)}, nil
}

// copyMasterCredentials copies the master .credentials.json (credentialsPath,
// shared with the logout escalation) into <home>/.claude/.credentials.json
// (parent dir 0700, file 0600 via the package's atomic tmp+rename writer). A
// missing master file is logged and skipped — not an error.
func (p *Provider) copyMasterCredentials(instanceHome string) error {
	src := credentialsPath()
	data, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			p.log.Warn("master claude credentials absent — the run will show claude's login prompt in its pane (unauthenticated host)",
				"component", "provider.claudecode", "path", src)
			return nil
		}
		return fmt.Errorf("read master credentials %s: %w", src, err)
	}
	dst := instanceCredsUnder(instanceHome)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	// writeAtomic (trust.go) creates its tmpfile 0600 via os.CreateTemp and
	// renames it into place — the same private mode the master file carries.
	return writeAtomic(dst, data)
}

// mirrorOAuthAccount merges the master ~/.claude.json's oauthAccount key into
// the instance <home>/.claude/.claude.json, preserving every other key on both sides
// (the same read-modify-write seedGlobalConfig performs). A missing master
// config, or one carrying no oauthAccount, writes nothing — not an error.
func (p *Provider) mirrorOAuthAccount(instanceHome string) error {
	master, err := os.ReadFile(p.configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			p.log.Warn("master claude config absent — no oauthAccount to mirror into the instance home",
				"component", "provider.claudecode", "path", p.configPath)
			return nil
		}
		return fmt.Errorf("read master config %s: %w", p.configPath, err)
	}
	var masterObj map[string]any
	if len(master) > 0 {
		if err := json.Unmarshal(master, &masterObj); err != nil {
			return fmt.Errorf("parse master config %s: %w", p.configPath, err)
		}
	}
	acct, ok := masterObj["oauthAccount"]
	if !ok {
		return nil // no account metadata to mirror (logged out, or a lean config)
	}
	dst := globalConfigUnder(instanceHome)
	cfg, err := readJSONObject(dst)
	if err != nil {
		return err
	}
	cfg["oauthAccount"] = acct
	return marshalAtomic(dst, cfg)
}
