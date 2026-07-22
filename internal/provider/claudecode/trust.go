package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
)

// gitExcludeEntries are the per-repo ignore lines SeedWorkspace appends to
// the worktree's .git/info/exclude: the .claude/ dir holds the seeded
// settings.local.json (and later run-local state), which must never show
// up as dirt in the run's own git status. info/exclude is shared through
// the common git dir, so one append covers every worktree of the repo.
// (The append/dedup mechanics were lifted into internal/seeder in M7 —
// seeder.EnsureExcludes — which lab's own seeding shares.)
var gitExcludeEntries = []string{".claude/"}

// SeedWorkspace implements provider.AgentProvider: pre-approve the
// worktree so neither claude's first-run onboarding, the workspace-trust
// dialog, nor the project MCP-server prompt blocks an unattended run, for
// incogni repos additionally disable claude's own attribution output (D15 §9
// measure 1 — merged into the same settings.local.json through the same
// preserve-unknown-keys atomic writer), then keep the seeded files out of
// git status via .git/info/exclude. Called after the worktree exists and
// before claude spawns; any failure aborts the Start (caller rolls back
// the worktree + branch).
//
// The two HOME-GLOBAL grants (onboarding + folder trust in ~/.claude.json)
// write ONLY under the run's private instance HOME opts.Home
// (<opts.Home>/.claude.json, issue #202) — never the machine's master
// ~/.claude.json — so a run's trust state stays inside its own home. An empty
// opts.Home skips those two grants entirely (a run with no per-run home gets no
// global-config write, never a fallback to the master store); the worktree-
// local grants — MCP approval, attribution-off, and the excludes — apply
// either way.
func (p *Provider) SeedWorkspace(worktree string, opts provider.SeedOpts) error {
	if opts.Home != "" {
		if err := seedGlobalConfig(globalConfigUnder(opts.Home), worktree); err != nil {
			return err
		}
	}
	if err := seedProjectMcpApproval(worktree); err != nil {
		return err
	}
	if opts.Incogni {
		if err := SeedAttributionOff(worktree); err != nil {
			return err
		}
	}
	return seeder.EnsureExcludes(worktree, gitExcludeEntries)
}

// SeedTrust pre-approves dir so none of claude's spawn-time prompts — the
// first-run onboarding wizard, the workspace-trust dialog, or the project
// MCP-server prompt — appears when claude launches there unattended.
// (Ported from v0 trust.go; the onboarding grant is new — see compat §4a.
// Exported for the compat trust-key test.)
//
// The grants go to two different files, because claude reads them from two
// different places:
//
//   - The global config at configPath (~/.claude.json) carries two grants,
//     written together by seedGlobalConfig: hasCompletedOnboarding (top-level,
//     machine-global) skips the first-run onboarding wizard, and
//     projects.<dir>.hasTrustDialogAccepted (per-project, keyed by dir) skips
//     the workspace-trust dialog. `claude auth login` (login.go) authenticates
//     without completing onboarding, so on a fresh install the first
//     --remote-control spawn would otherwise block on the theme picker ("Let's
//     get started — choose the text style…") with no one to answer it.
//   - MCP approval (enableAllProjectMcpServers) is a *project-local*
//     setting in dir/.claude/settings.local.json — NOT the global config.
//     A fresh git worktree inherits the tracked .mcp.json but not that
//     gitignored settings file, where the human's approval lives, so
//     without seeding it an AFK run stalls forever on the "New MCP server
//     found in this project" prompt with no one to clear it. (An earlier
//     fix wrote enableAllProjectMcpServers into the ~/.claude.json project
//     entry instead; claude does not read MCP approval from there, which
//     is why the prompt kept appearing.)
//
// Both writes tolerate a missing file (creating it), preserve unknown keys
// via a generic map round-trip, and write atomically via tmpfile + fsync +
// rename.
//
// Not safe against a concurrent writer of the same file: a parallel claude
// instance writing at the same instant could clobber our update, in which
// case a dialog appears once and re-resolves on next click. Accepted risk
// for a single-user dev machine (v0 note preserved).
func SeedTrust(configPath, dir string) error {
	if err := seedGlobalConfig(configPath, dir); err != nil {
		return err
	}
	return seedProjectMcpApproval(dir)
}

// seedGlobalConfig writes the two grants that live in claude's global config
// (~/.claude.json at configPath) in a single atomic pass:
//
//   - hasCompletedOnboarding=true (top-level, machine-global): skips claude's
//     first-run onboarding wizard. NOT keyed by dir. theme is deliberately not
//     seeded — it reads null even on a fully onboarded host, so this flag is
//     the sole onboarding gate (compat §4a; live 2.1.198).
//   - projects.<dir>.hasTrustDialogAccepted=true (per-project): skips the
//     workspace-trust dialog for dir (compat §4).
//
// The file is read once and rewritten only when a grant is actually missing —
// an already-satisfied config is left untouched so a needless rewrite can't
// clobber a concurrent claude writer. Folding onboarding into the trust pass
// keeps this to the one read+write the trust seed already performed each spawn.
func seedGlobalConfig(configPath, dir string) error {
	cfg, err := readJSONObject(configPath)
	if err != nil {
		return err
	}
	dirty := false

	// Onboarding is machine-global: a top-level flag, not a per-dir entry.
	if v, _ := cfg["hasCompletedOnboarding"].(bool); !v {
		cfg["hasCompletedOnboarding"] = true
		dirty = true
	}

	// Folder trust is per-project, keyed by the absolute worktree dir.
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	entry, _ := projects[dir].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[dir] = entry
	}
	if v, _ := entry["hasTrustDialogAccepted"].(bool); !v {
		entry["hasTrustDialogAccepted"] = true
		dirty = true
	}

	if !dirty {
		return nil // both grants already present — don't rewrite
	}
	return marshalAtomic(configPath, cfg)
}

// seedProjectMcpApproval sets enableAllProjectMcpServers=true in
// dir/.claude/settings.local.json — the project-local file claude consults
// to decide whether to prompt for the repo's committed .mcp.json servers
// (present or future). Same grant as clicking "Use this and all future MCP
// servers in this project". Creates .claude/ and the file when absent (the
// usual case for a fresh worktree); merges into an existing file otherwise.
func seedProjectMcpApproval(dir string) error {
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", claudeDir, err)
	}
	path := filepath.Join(claudeDir, "settings.local.json")
	settings, err := readJSONObject(path)
	if err != nil {
		return err
	}
	if v, _ := settings["enableAllProjectMcpServers"].(bool); v {
		return nil // already approved — don't rewrite
	}
	settings["enableAllProjectMcpServers"] = true
	return marshalAtomic(path, settings)
}

// readJSONObject reads the JSON object at path into a generic map. A
// missing or empty file yields an empty map, so callers can seed keys and
// write it out; unknown keys survive the round-trip untouched.
func readJSONObject(path string) (map[string]any, error) {
	obj := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) > 0 {
			if err := json.Unmarshal(data, &obj); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		// no file yet — start from empty
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return obj, nil
}

// marshalAtomic writes obj to path as indented JSON via writeAtomic.
func marshalAtomic(path string, obj map[string]any) error {
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return writeAtomic(path, out)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
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
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
