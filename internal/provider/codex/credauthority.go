package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// This file implements the credential-authority seam (issue #222): the
// single-refresher design that stops per-run OAuth snapshots from forking
// codex's token family. See provider.AgentProvider's RefreshCredentials /
// CredentialsSig / AdoptCredentials doc for the full contract; this file
// owns every file path and CLI recipe, core owns the schedule and the
// change-comparison/adopt policy.

// authJSONPath resolves the auth.json this seam stats/copies for a given
// home: "" means the MASTER store — the exact resolution credentialsPath
// uses (logout's rm-escalation target); a non-empty home is that instance's
// private copy under instanceCodexHome — the exact path InjectCredentials
// writes. auth.json is only ever stat-ed or copied whole here, NEVER parsed
// (it holds OpenAI's token JSON; lab's no-token-parsing stance).
func (p *Provider) authJSONPath(home string) string {
	if home == "" {
		return p.credentialsPath()
	}
	return filepath.Join(instanceCodexHome(home), "auth.json")
}

// CredentialsSig implements provider.AgentProvider: a cheap existence+mtime+
// size digest of auth.json under home (the SpoolSig precedent —
// internal/provider/claudecode/dialogspool.go). "" (with a zero mtime) when
// the file is missing — which is exactly the "logged out / never injected"
// signal, including on a master store that was never populated (the issue
// #222 acceptance criterion). The returned mtime is the file's own
// modification time, never a parsed token field.
func (p *Provider) CredentialsSig(home string) (string, time.Time) {
	path := p.authJSONPath(home)
	fi, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}
	}
	return fmt.Sprintf("%s:%d:%d;", filepath.Base(path), fi.ModTime().UnixNano(), fi.Size()), fi.ModTime()
}

// AdoptCredentials implements provider.AgentProvider: the reverse copy of
// InjectCredentials — instance HOME's auth.json back into the MASTER store,
// ATOMICALLY (tmp file in the master's own directory, then os.Rename, so a
// live host-side `codex` process reading the master store never observes a
// torn file). An empty instanceHome is a programmer error; a missing
// instance auth.json is an error too (adoption is only ever requested after
// a CredentialsSig proved the file existed — the caller must learn if it
// vanished, never silently adopt nothing).
func (p *Provider) AdoptCredentials(instanceHome string) error {
	if instanceHome == "" {
		return errors.New("codex: AdoptCredentials requires a non-empty instance HOME")
	}
	src := p.authJSONPath(instanceHome)
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("codex: AdoptCredentials: read instance auth.json %s: %w", src, err)
	}
	dst := p.credentialsPath()
	dir := filepath.Dir(dst)
	// Mirror copyMasterAuth's permission discipline: the credential
	// directory is created 0700 when absent. Unlike copyMasterAuth (which
	// forces the mode unconditionally to settle a seeding race), an
	// already-existing master home dir is left exactly as the operator/CLI
	// set it — this seam never "repairs" a directory it did not create.
	if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return fmt.Errorf("codex: AdoptCredentials: mkdir %s: %w", dir, mkErr)
		}
	} else if statErr != nil {
		return fmt.Errorf("codex: AdoptCredentials: stat %s: %w", dir, statErr)
	}
	if err := atomicWriteCredential(dst, data); err != nil {
		return fmt.Errorf("codex: AdoptCredentials: write master auth.json: %w", err)
	}
	return nil
}

// atomicWriteCredential writes data to dst via tmpfile+fsync+rename WITHIN
// dst's own directory (writeAtomic's pinned pattern, seed.go), but at 0600 —
// dst is always a credential file here (auth.json), never the world-readable
// config writeAtomic itself serves.
func atomicWriteCredential(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp.*")
	if err != nil {
		return fmt.Errorf("tmpfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// refreshProbeArgs is the fixed non-interactive probe recipe RefreshCredentials
// runs (issue #222) — a CANDIDATE recipe, NOT live-verified (see credauthority.go
// doc and internal/compat/codex/compat.md's "Non-interactive refresh trigger"
// section): one real `codex exec` turn, sandboxed read-only (the probe must
// never be able to write through the agent's own tool calls — it exists to
// trigger the CLI's OWN token-refresh check, not to run agent work),
// `--skip-git-repo-check` so the probe runs from the master codex home
// directory (not necessarily a git repo), and a minimal one-word prompt that
// needs no tool call to answer. Confirmed against the installed codex-cli
// 0.144.4's `codex exec --help`; `-c` config overrides were deliberately
// avoided in favor of the narrower purpose-built flags.
var refreshProbeArgs = []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "ok"}

// RefreshCredentials implements provider.AgentProvider: the loop's blind
// periodic poke against the MASTER store. It runs `codex exec` (refreshProbeArgs)
// with CODEX_HOME forced to the master codex home directory (credentialsPath's
// own directory) — overriding, never merely inheriting, because the lab
// server's own environment may carry a stray/instance CODEX_HOME (the same
// tmux-inheritance vector InjectCredentials defends against for a spawned
// session's env). The working directory is the master codex home when it
// exists; otherwise the command still runs (from the caller's cwd) and its
// own failure surfaces — callers are expected to gate the call on
// CredentialsSig("") != "" beforehand.
//
// Discards stdout. On failure, returns an error carrying a trimmed tail of
// the command's stderr (this package's CLI error-wrapping convention — see
// ProbeModels/runLogoutCommand).
func (p *Provider) RefreshCredentials(ctx context.Context) error {
	masterDir := filepath.Dir(p.credentialsPath())

	cmd := exec.CommandContext(ctx, p.codexBin, refreshProbeArgs...)
	cmd.Env = overrideEnv(os.Environ(), "CODEX_HOME", masterDir)
	if info, err := os.Stat(masterDir); err == nil && info.IsDir() {
		cmd.Dir = masterDir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "no stderr captured"
		}
		return fmt.Errorf("codex exec (credential refresh probe): %w: %s", err, msg)
	}
	return nil
}

// overrideEnv returns environ with every existing entry for key removed and
// key=value appended. A plain append onto an environ slice that ALREADY
// carries key (as os.Environ() might) is not enough: some C library
// getenv() implementations resolve the FIRST matching entry in the exec'd
// process's environ array, so a naive append could leave a stray inherited
// value in effect despite the "override" — filtering first is what makes
// this a genuine override rather than a same-key duplicate.
func overrideEnv(environ []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ)+1)
	for _, e := range environ {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return append(out, key+"="+value)
}
