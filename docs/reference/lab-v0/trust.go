package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SeedTrust pre-approves dir so neither the workspace-trust dialog nor the
// project MCP-server prompt appears when claude launches there unattended.
//
// The two grants go to two different files, because claude reads them from two
// different places:
//
//   - Folder trust (hasTrustDialogAccepted) is per-project state in the global
//     config at configPath (~/.claude.json), keyed by dir.
//   - MCP approval (enableAllProjectMcpServers) is a *project-local* setting in
//     dir/.claude/settings.local.json — NOT the global config. A fresh git
//     worktree inherits the tracked .mcp.json but not that gitignored settings
//     file, where the human's approval lives, so without seeding it an AFK run
//     stalls forever on the "New MCP server found in this project" prompt with
//     no one to clear it. (An earlier fix wrote enableAllProjectMcpServers into
//     the ~/.claude.json project entry instead; claude does not read MCP
//     approval from there, which is why the prompt kept appearing.)
//
// Both writes tolerate a missing file (creating it), preserve unknown keys via
// a generic map round-trip, and write atomically via tmpfile + fsync + rename.
//
// Not safe against a concurrent writer of the same file: a parallel claude
// instance writing at the same instant could clobber our update, in which case
// a dialog appears once and re-resolves on next click. Acceptable for a
// single-user dev microvm.
func SeedTrust(configPath, dir string) error {
	if err := seedFolderTrust(configPath, dir); err != nil {
		return err
	}
	return seedProjectMcpApproval(dir)
}

// seedFolderTrust sets hasTrustDialogAccepted=true on dir's project entry in the
// global claude config at configPath, so the workspace-trust dialog is skipped.
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
// dir/.claude/settings.local.json — the project-local file claude consults to
// decide whether to prompt for the repo's committed .mcp.json servers (present
// or future). Same grant as clicking "Use this and all future MCP servers in
// this project". Creates .claude/ and the file when absent (the usual case for
// a fresh worktree); merges into an existing file otherwise. The file is
// gitignored, so seeding it never surfaces in the AFK run's own git status.
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

// readJSONObject reads the JSON object at path into a generic map. A missing or
// empty file yields an empty map, so callers can seed keys and write it out;
// unknown keys survive the round-trip untouched.
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
