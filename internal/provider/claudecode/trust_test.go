package claudecode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// The eight SeedTrust tests below are the complete transcribed v0
// contract (trust_test.go).

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

// MCP approval must land in the worktree's project-local
// .claude/settings.local.json — the file claude actually reads — and NOT
// in the ~/.claude.json project entry, where the previous fix wrote it
// and claude ignored it.
func TestSeedTrust_enablesProjectMcpServersInWorktree(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()

	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	if !mcpApprovedIn(t, dir) {
		t.Errorf("expected enableAllProjectMcpServers=true in %s/.claude/settings.local.json", dir)
	}
	// Regression guard: it must not be written to the global config's
	// project entry, which claude does not consult for MCP approval.
	if mcpOf(readCfg(t, cfg), dir) {
		t.Errorf("MCP approval leaked into ~/.claude.json project entry; claude ignores it there")
	}
}

// A worktree dir already folder-trusted in ~/.claude.json (the field
// predates this fix) must still get MCP approval seeded into its
// project-local settings — the worktree that hung on the prompt looked
// exactly like this.
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

// A worktree that already carries a settings.local.json must keep its
// other keys when MCP approval is merged in.
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
	// Pre-seed all grants in their real locations, then assert neither
	// file is rewritten — a needless rewrite risks clobbering a
	// concurrent claude writer. The global config carries BOTH the
	// machine-global onboarding flag and the per-dir trust entry, so both
	// must be present or seedGlobalConfig legitimately rewrites to add the
	// missing one.
	writeCfg(t, cfg, map[string]any{
		"hasCompletedOnboarding": true,
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

// --- onboarding grant (new; post-dates the transcribed v0 contract) ---

// A fresh install: `claude auth login` performs only the OAuth exchange and
// does NOT complete onboarding, so SeedTrust must set the machine-global
// hasCompletedOnboarding flag — otherwise the first --remote-control spawn
// blocks on the theme picker with no one to answer it (compat §4a; live
// 2.1.198).
func TestSeedTrust_completesOnboarding(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()

	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	if !onboardedOf(readCfg(t, cfg)) {
		t.Errorf("expected top-level hasCompletedOnboarding=true in %q", cfg)
	}
}

// Onboarding is machine-global, not gated on the per-dir trust entry: a
// worktree already folder-trusted from a prior spawn — on a host whose
// onboarding flag is still unset (a fresh install, or a claude migration that
// reset it) — must STILL get onboarding seeded. Guards against ever
// short-circuiting the onboarding write on an already-trusted dir.
func TestSeedTrust_completesOnboardingWhenAlreadyFolderTrusted(t *testing.T) {
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
	if !onboardedOf(readCfg(t, cfg)) {
		t.Errorf("onboarding not seeded when the dir was already folder-trusted; got %+v", readCfg(t, cfg))
	}
}

// --- SeedWorkspace: trust + MCP + .git/info/exclude -------------------

// newFakeWorktree returns a dir with a plain .git directory — enough for
// gitExcludePath to resolve without a real git repo.
func newFakeWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSeedWorkspace_seedsTrustAndExclude(t *testing.T) {
	run := newFakeRunner()
	p, _ := testProvider(t, run)
	wt := newFakeWorktree(t)

	if err := p.SeedWorkspace(wt, provider.SeedOpts{}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}
	if !trustOf(readCfg(t, p.configPath), wt) {
		t.Errorf("worktree not folder-trusted in %s", p.configPath)
	}
	if !mcpApprovedIn(t, wt) {
		t.Errorf("MCP approval missing in worktree settings")
	}
	got := readExclude(t, wt)
	if !strings.Contains(got, ".claude/\n") {
		t.Errorf("exclude = %q; want a .claude/ line", got)
	}
}

func TestSeedWorkspace_excludeDedupAndPreserve(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	wt := newFakeWorktree(t)
	// Pre-existing hand-written exclude WITHOUT a trailing newline: the
	// append must not glue onto the last line.
	excl := filepath.Join(wt, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excl, []byte("node_modules/"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // second run must be a no-op (dedup)
		if err := p.SeedWorkspace(wt, provider.SeedOpts{}); err != nil {
			t.Fatalf("SeedWorkspace #%d: %v", i+1, err)
		}
	}
	got := readExclude(t, wt)
	if want := "node_modules/\n.claude/\n"; got != want {
		t.Errorf("exclude = %q; want %q (preserved + deduped)", got, want)
	}
}

// Against a REAL linked worktree the exclude entry must land in the
// shared git dir (via the commondir pointer) and actually hide .claude/
// from git status — the property the whole teardown/dirty machinery
// relies on.
func TestSeedWorkspace_realLinkedWorktreeHidesClaudeDir(t *testing.T) {
	testutil.RequireTool(t, "git")
	p, _ := testProvider(t, newFakeRunner())
	root := t.TempDir()
	env := append(os.Environ(), testutil.HermeticGitEnv(root)...)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	repo := filepath.Join(root, "repo")
	git("init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("-C", repo, "add", "f.txt")
	git("-C", repo, "commit", "-m", "init")
	wt := filepath.Join(root, "wt")
	git("-C", repo, "worktree", "add", "-b", "lab/x-1", wt)

	if err := p.SeedWorkspace(wt, provider.SeedOpts{}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}

	cmd := exec.Command("git", "-C", wt, "status", "--porcelain")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("git status after SeedWorkspace = %q; want clean (.claude/ excluded)", s)
	}
}

func TestSeedWorkspace_missingGitFailsLoud(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if err := p.SeedWorkspace(t.TempDir(), provider.SeedOpts{}); err == nil {
		t.Error("SeedWorkspace on a dir with no .git succeeded; want an error")
	}
}

// --- helpers (transcribed from v0 trust_test.go) ----------------------

func readExclude(t *testing.T, worktree string) string {
	t.Helper()
	path, err := seeder.GitExcludePath(worktree)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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

// worktreeSettingsPath returns dir/.claude/settings.local.json, creating
// .claude/.
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

// onboardedOf reports the machine-global top-level hasCompletedOnboarding
// flag — the sole gate on claude's first-run onboarding wizard.
func onboardedOf(cfg map[string]any) bool {
	v, _ := cfg["hasCompletedOnboarding"].(bool)
	return v
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
