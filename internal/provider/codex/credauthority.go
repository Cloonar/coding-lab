package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
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
// writes. auth.json is copied whole here and core NEVER parses it (it holds
// OpenAI's token JSON; lab's no-token-parsing stance); the adapter alone
// reads it, and only for the emptiness test authLoggedIn applies (issue #101).
func (p *Provider) authJSONPath(home string) string {
	if home == "" {
		return p.credentialsPath()
	}
	return filepath.Join(instanceCodexHome(home), "auth.json")
}

// authLoggedIn reports whether an auth.json body is a usable login — the ONE
// emptiness test CredentialsSig and AdoptCredentials share (issue #101), so
// the sig scan and the adopt agree on what "logged out" means. A body is
// logged in when it parses as a JSON object AND carries either a non-empty
// string OPENAI_API_KEY (API-key login; may have no tokens) or a `tokens`
// object with BOTH a non-empty access_token and a non-empty refresh_token
// (ChatGPT/OAuth login). Anything else — non-JSON, `{}`, a null API key with
// no tokens, emptied token strings — is logged out.
//
// UNVERIFIED: how codex-cli treats a failed refresh on disk (claude empties
// its tokens in place, issue #101; codex may delete the file, empty it, or
// leave it) has not been observed. This is the MINIMUM guard — reject an
// unparseable or token-less file — not a model of codex's clear behaviour;
// tighten it once a real cleared auth.json is captured. Token values are
// only tested for emptiness, never returned, logged, or put in an error.
func authLoggedIn(body []byte) bool {
	var f struct {
		APIKey *string `json:"OPENAI_API_KEY"`
		Tokens *struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(body, &f) != nil {
		return false
	}
	if f.APIKey != nil && *f.APIKey != "" {
		return true
	}
	return f.Tokens != nil && f.Tokens.Access != "" && f.Tokens.Refresh != ""
}

// CredentialsSig implements provider.AgentProvider: a cheap existence+mtime+
// size digest of auth.json under home (the SpoolSig precedent —
// internal/provider/claudecode/dialogspool.go). "" (with a zero mtime) when
// the file is missing — which is exactly the "logged out / never injected"
// signal, including on a master store that was never populated (the issue
// #222 acceptance criterion) — AND when the file exists but authLoggedIn
// rejects it (issue #101), so a cleared/garbage file reads like an absent one
// to every core gate and never becomes an adopt candidate. The cost is one
// small read+parse instead of a bare stat. The returned mtime is the file's
// own modification time, never a parsed token field.
func (p *Provider) CredentialsSig(home string) (string, time.Time) {
	path := p.authJSONPath(home)
	fi, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}
	}
	if body, err := os.ReadFile(path); err != nil || !authLoggedIn(body) {
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
// vanished, never silently adopt nothing). A file that authLoggedIn rejects is
// refused with an error wrapping provider.ErrCredentialsLoggedOut before ANY
// master-side effect (no mkdir, no write): adopting it would fan a dead
// family out over every live home (issue #101). The path is named, never the
// content.
func (p *Provider) AdoptCredentials(instanceHome string) error {
	if instanceHome == "" {
		return errors.New("codex: AdoptCredentials requires a non-empty instance HOME")
	}
	src := p.authJSONPath(instanceHome)
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("codex: AdoptCredentials: read instance auth.json %s: %w", src, err)
	}
	if !authLoggedIn(data) {
		return fmt.Errorf("codex: AdoptCredentials: %s: %w", src, provider.ErrCredentialsLoggedOut)
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
// through p.cli (issue #206) with CODEX_HOME forced to the master codex home
// directory (credentialsPath's own directory) — overriding, never merely
// inheriting, because the lab server's own environment may carry a
// stray/instance CODEX_HOME (the same tmux-inheritance vector
// InjectCredentials defends against for a spawned session's env); the
// runner's filter-then-append semantics (provider.CLIInvocation.Env) make the
// pin a genuine override, never a same-key duplicate a getenv-first-match
// libc would resolve wrong. The working directory is the master codex home
// when it exists; otherwise the command still runs (from the inherited cwd)
// and its own failure surfaces — callers are expected to gate the call on
// CredentialsSig("") != "" beforehand.
//
// Discards stdout. On failure, returns an error carrying a trimmed tail of
// the command's stderr (this package's CLI error-wrapping convention — see
// ProbeModels/runLogoutCommand).
func (p *Provider) RefreshCredentials(ctx context.Context) error {
	masterDir := filepath.Dir(p.credentialsPath())

	inv := provider.CLIInvocation{
		Argv: append([]string{p.codexBin}, refreshProbeArgs...),
		Env:  []string{"CODEX_HOME=" + masterDir},
	}
	if info, err := os.Stat(masterDir); err == nil && info.IsDir() {
		inv.Dir = masterDir
	}
	_, stderr, err := p.cli.Run(ctx, inv)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = "no stderr captured"
		}
		return fmt.Errorf("codex exec (credential refresh probe): %w: %s", err, msg)
	}
	return nil
}
