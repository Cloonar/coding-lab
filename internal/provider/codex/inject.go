package codex

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// InjectCredentials implements provider.AgentProvider (issue #202): install the
// machine's MASTER codex credentials into the run's private instance HOME and
// return the env pin that keeps the spawned session resolving its state there.
// Exactly one call site exists (the launch path), so a server-side credential
// proxy can later replace the copy wholesale.
//
// It copies the master auth.json (credentialsPath, the CODEX_HOME-aware master
// resolution the logout escalation shares) → <home>/.codex/auth.json (parent
// dir 0700, file 0600). It ALWAYS returns CODEX_HOME=<home>/.codex — even when
// the master auth.json is missing — because the pin governs PATH RESOLUTION, not
// authentication: lab supports $CODEX_HOME for the master store (codexHomeDir),
// so a value inherited through tmux would otherwise point the instance back at
// that master store; the pin redirects it to the instance home, which coincides
// with codex's own HOME-derived default.
//
// A MISSING master auth.json (the operator never logged in) is NOT an error: it
// is logged loudly and not written, the env pin is still returned, and the
// spawn proceeds — codex shows its own login prompt in the pane, exactly
// today's behavior on an unauthenticated host. An empty home is a programmer
// error. The injector never deletes anything; wipe-on-stop of the instance HOME
// shreds the copy.
func (p *Provider) InjectCredentials(instanceHome string) ([]string, error) {
	if instanceHome == "" {
		return nil, errors.New("codex: InjectCredentials requires a non-empty instance HOME")
	}
	codexHome := instanceCodexHome(instanceHome)
	if err := p.copyMasterAuth(codexHome); err != nil {
		return nil, err
	}
	// Unconditional: the pin is about where codex reads/writes its state, not
	// whether an auth.json landed, so it fires even on the missing-master path.
	return []string{"CODEX_HOME=" + codexHome}, nil
}

// copyMasterAuth copies the master auth.json (credentialsPath) into
// <codexHome>/auth.json (parent dir 0700, file 0600). A missing master file is
// logged and skipped — not an error.
func (p *Provider) copyMasterAuth(codexHome string) error {
	src := p.credentialsPath()
	data, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			p.log.Warn("master codex auth.json absent — the run will show codex's login prompt in its pane (unauthenticated host)",
				"component", "provider.codex", "path", src)
			return nil
		}
		return fmt.Errorf("read master codex auth %s: %w", src, err)
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", codexHome, err)
	}
	// MkdirAll leaves an ALREADY-EXISTING directory's mode untouched — and
	// SeedWorkspace (seed.go) creates this same <home>/.codex dir at 0o755
	// (world-readable, appropriate for its own config.toml/AGENTS.md) via
	// writeAtomic's own MkdirAll. Whichever of SeedWorkspace/InjectCredentials
	// runs first must not decide this credential's parent-dir mode, so it is
	// forced to 0700 here unconditionally.
	if err := os.Chmod(codexHome, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", codexHome, err)
	}
	dst := filepath.Join(codexHome, "auth.json")
	// writeAtomic (seed.go) normalizes to 0644 for world-readable config;
	// auth.json is a credential, so tighten it to 0600 after the rename.
	if err := writeAtomic(dst, data); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	return nil
}
