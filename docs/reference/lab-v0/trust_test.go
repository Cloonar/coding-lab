package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSeedTrust_createsFileWhenMissing(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()

	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	if !trustOf(readCfg(t, cfg), dir) {
		t.Errorf("expected hasTrustDialogAccepted=true for %q", dir)
	}
}

// MCP approval must land in the worktree's project-local .claude/settings.local.json
// — the file claude actually reads — and NOT in the ~/.claude.json project entry,
// where the previous fix wrote it and claude ignored it.
func TestSeedTrust_enablesProjectMcpServersInWorktree(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()

	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	if !mcpApprovedIn(t, dir) {
		t.Errorf("expected enableAllProjectMcpServers=true in %s/.claude/settings.local.json", dir)
	}
	// Regression guard: it must not be written to the global config's project
	// entry, which claude does not consult for MCP approval.
	if mcpOf(readCfg(t, cfg), dir) {
		t.Errorf("MCP approval leaked into ~/.claude.json project entry; claude ignores it there")
	}
}

// A worktree dir already folder-trusted in ~/.claude.json (the field predates
// this fix) must still get MCP approval seeded into its project-local settings
// — the worktree that hung on the prompt looked exactly like this.
func TestSeedTrust_enablesMcpWhenAlreadyFolderTrusted(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()
	writeCfg(t, cfg, map[string]any{
		"projects": map[string]any{
			dir: map[string]any{"hasTrustDialogAccepted": true},
		},
	})

	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	if !mcpApprovedIn(t, dir) {
		t.Errorf("expected enableAllProjectMcpServers=true in worktree settings for already-trusted %q", dir)
	}
}

func TestSeedTrust_preservesUnknownConfigKeys(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	original := map[string]any{
		"unrelatedTop": "keep-me",
		"projects": map[string]any{
			"/some/other/proj": map[string]any{
				"hasTrustDialogAccepted": true,
				"lastSessionId":          "abc",
			},
		},
	}
	writeCfg(t, cfg, original)

	dir := t.TempDir()
	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}

	got := readCfg(t, cfg)
	if v, _ := got["unrelatedTop"].(string); v != "keep-me" {
		t.Errorf("top-level key dropped; got %+v", got)
	}
	other, _ := got["projects"].(map[string]any)["/some/other/proj"].(map[string]any)
	if v, _ := other["lastSessionId"].(string); v != "abc" {
		t.Errorf("nested key dropped; got %+v", other)
	}
	if !trustOf(got, dir) {
		t.Errorf("new project not trusted; got %+v", got)
	}
}

// A worktree that already carries a settings.local.json (e.g. copied in by some
// future step) must keep its other keys when MCP approval is merged in.
func TestSeedTrust_preservesExistingWorktreeSettings(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()
	settingsPath := worktreeSettingsPath(t, dir)
	writeCfg(t, settingsPath, map[string]any{
		"permissions": map[string]any{"allow": []any{"Bash(ls:*)"}},
	})

	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}

	got := readCfg(t, settingsPath)
	if v, _ := got["enableAllProjectMcpServers"].(bool); !v {
		t.Errorf("expected enableAllProjectMcpServers=true; got %+v", got)
	}
	if _, ok := got["permissions"].(map[string]any); !ok {
		t.Errorf("existing permissions key dropped; got %+v", got)
	}
}

func TestSeedTrust_idempotentWhenAlreadySeeded(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()
	// Pre-seed both grants in their real locations, then assert neither file is
	// rewritten — a needless rewrite risks clobbering a concurrent claude writer.
	writeCfg(t, cfg, map[string]any{
		"projects": map[string]any{
			dir: map[string]any{"hasTrustDialogAccepted": true},
		},
	})
	settingsPath := worktreeSettingsPath(t, dir)
	writeCfg(t, settingsPath, map[string]any{"enableAllProjectMcpServers": true})

	cfgBefore := mustModTime(t, cfg)
	settingsBefore := mustModTime(t, settingsPath)
	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	if got := mustModTime(t, cfg); got != cfgBefore {
		t.Errorf("config rewritten despite already trusted; mtime %v -> %v", cfgBefore, got)
	}
	if got := mustModTime(t, settingsPath); got != settingsBefore {
		t.Errorf("settings rewritten despite already approved; mtime %v -> %v", settingsBefore, got)
	}
}

func TestSeedTrust_malformedConfigReturnsError(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(cfg, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedTrust(cfg, t.TempDir()); err == nil {
		t.Errorf("expected error on malformed config JSON; got nil")
	}
}

func TestSeedTrust_malformedWorktreeSettingsReturnsError(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()
	if err := os.WriteFile(worktreeSettingsPath(t, dir), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedTrust(cfg, dir); err == nil {
		t.Errorf("expected error on malformed settings.local.json; got nil")
	}
}

func readCfg(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeCfg(t *testing.T, path string, m map[string]any) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// worktreeSettingsPath returns dir/.claude/settings.local.json, creating .claude/.
func worktreeSettingsPath(t *testing.T, dir string) string {
	t.Helper()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(claudeDir, "settings.local.json")
}

func mustModTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func trustOf(cfg map[string]any, dir string) bool {
	projects, _ := cfg["projects"].(map[string]any)
	entry, _ := projects[dir].(map[string]any)
	v, _ := entry["hasTrustDialogAccepted"].(bool)
	return v
}

func mcpOf(cfg map[string]any, dir string) bool {
	projects, _ := cfg["projects"].(map[string]any)
	entry, _ := projects[dir].(map[string]any)
	v, _ := entry["enableAllProjectMcpServers"].(bool)
	return v
}

// mcpApprovedIn reports whether dir/.claude/settings.local.json grants
// enableAllProjectMcpServers — the project-local flag claude reads.
func mcpApprovedIn(t *testing.T, dir string) bool {
	t.Helper()
	got := readCfg(t, filepath.Join(dir, ".claude", "settings.local.json"))
	v, _ := got["enableAllProjectMcpServers"].(bool)
	return v
}
