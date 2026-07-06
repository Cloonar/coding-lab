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
// worktree so neither the workspace-trust dialog nor the project
// MCP-server prompt blocks an unattended run (SeedTrust), for incogni
// repos additionally disable claude's own attribution output (D15 §9
// measure 1 — merged into the same settings.local.json through the same
// preserve-unknown-keys atomic writer), then keep the seeded files out of
// git status via .git/info/exclude. Called after the worktree exists and
// before claude spawns; any failure aborts the Start (caller rolls back
// the worktree + branch).
func (p *Provider) SeedWorkspace(worktree string, opts provider.SeedOpts) error {
	if err := SeedTrust(p.configPath, worktree); err != nil {
		return err
	}
	if opts.Incogni {
		if err := SeedAttributionOff(worktree); err != nil {
			return err
		}
	}
	return seeder.EnsureExcludes(worktree, gitExcludeEntries)
}

// SeedTrust pre-approves dir so neither the workspace-trust dialog nor the
// project MCP-server prompt appears when claude launches there unattended.
// (Verbatim port of v0 trust.go; exported for the compat trust-key test.)
//
// The two grants go to two different files, because claude reads them from
// two different places:
//
//   - Folder trust (hasTrustDialogAccepted) is per-project state in the
//     global config at configPath (~/.claude.json), keyed by dir.
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
	if err := seedFolderTrust(configPath, dir); err != nil {
		return err
	}
	return seedProjectMcpApproval(dir)
}

// seedFolderTrust sets hasTrustDialogAccepted=true on dir's project entry
// in the global claude config at configPath, so the workspace-trust dialog
// is skipped. Already-true entries are left untouched without a rewrite —
// a needless rewrite risks clobbering a concurrent claude writer.
func seedFolderTrust(configPath, dir string) error {
	cfg, err := readJSONObject(configPath)
	if err != nil {
		return err
	}
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
	if v, _ := entry["hasTrustDialogAccepted"].(bool); v {
		return nil // already trusted — don't rewrite
	}
	entry["hasTrustDialogAccepted"] = true
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
