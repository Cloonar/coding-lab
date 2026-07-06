package claudecode

// Incogni measure 1 tests (D15 §9): the attribution-off keys — verified
// against Claude Code 2.1.198, compat.md §4 — land in the worktree's
// .claude/settings.local.json exactly when SeedOpts.Incogni is set, merge
// non-destructively, and never rewrite an already-seeded file.

import (
	"os"
	"path/filepath"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// attributionOffIn reads dir's settings.local.json and reports whether the
// full attribution-off key set is present.
func attributionOffIn(t *testing.T, dir string) bool {
	t.Helper()
	return attributionOff(readCfg(t, filepath.Join(dir, ".claude", "settings.local.json")))
}

func TestSeedWorkspace_incogniSeedsAttributionOff(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	wt := newFakeWorktree(t)

	if err := p.SeedWorkspace(wt, provider.SeedOpts{Incogni: true}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}

	got := readCfg(t, filepath.Join(wt, ".claude", "settings.local.json"))
	attr, _ := got["attribution"].(map[string]any)
	if attr == nil {
		t.Fatalf("no attribution object seeded; settings = %+v", got)
	}
	if v, ok := attr["commit"].(string); !ok || v != "" {
		t.Errorf("attribution.commit = %v, want \"\" (hides the Co-Authored-By trailer)", attr["commit"])
	}
	if v, ok := attr["pr"].(string); !ok || v != "" {
		t.Errorf("attribution.pr = %v, want \"\" (hides the generated-with PR footer)", attr["pr"])
	}
	if v, ok := attr["sessionUrl"].(bool); !ok || v {
		t.Errorf("attribution.sessionUrl = %v, want false (lab sessions are --remote-control; true leaks the Claude-Session trailer)", attr["sessionUrl"])
	}
	if v, ok := got["includeCoAuthoredBy"].(bool); !ok || v {
		t.Errorf("includeCoAuthoredBy = %v, want false (deprecated key kept as defense in depth)", got["includeCoAuthoredBy"])
	}
	// The trust step's MCP approval shares the file and must coexist.
	if v, _ := got["enableAllProjectMcpServers"].(bool); !v {
		t.Errorf("enableAllProjectMcpServers lost when attribution keys merged; got %+v", got)
	}
}

// A non-incogni seed must not write any attribution key — the operator's
// own attribution settings (or claude's defaults) stay in force.
func TestSeedWorkspace_nonIncogniLeavesAttributionAlone(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	wt := newFakeWorktree(t)

	if err := p.SeedWorkspace(wt, provider.SeedOpts{}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}

	got := readCfg(t, filepath.Join(wt, ".claude", "settings.local.json"))
	if _, present := got["attribution"]; present {
		t.Errorf("attribution seeded on a non-incogni repo; got %+v", got)
	}
	if _, present := got["includeCoAuthoredBy"]; present {
		t.Errorf("includeCoAuthoredBy seeded on a non-incogni repo; got %+v", got)
	}
}

// Merging into an existing settings.local.json preserves unknown keys at
// every level — top-level AND inside an existing attribution object — while
// overriding the three attribution fields (an operator's custom attribution
// text must not survive on an incogni repo).
func TestSeedAttributionOff_mergePreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	settingsPath := worktreeSettingsPath(t, dir)
	writeCfg(t, settingsPath, map[string]any{
		"permissions": map[string]any{"allow": []any{"Bash(ls:*)"}},
		"attribution": map[string]any{
			"commit":     "Co-Authored-By: Claude <noreply@anthropic.com>",
			"customKey":  "keep-me",
			"sessionUrl": true,
		},
	})

	if err := SeedAttributionOff(dir); err != nil {
		t.Fatalf("SeedAttributionOff: %v", err)
	}

	got := readCfg(t, settingsPath)
	if _, ok := got["permissions"].(map[string]any); !ok {
		t.Errorf("top-level permissions key dropped; got %+v", got)
	}
	attr, _ := got["attribution"].(map[string]any)
	if v, _ := attr["customKey"].(string); v != "keep-me" {
		t.Errorf("unknown attribution key dropped; got %+v", attr)
	}
	if v, _ := attr["commit"].(string); v != "" {
		t.Errorf("attribution.commit = %q, want \"\" (custom attribution text must be overridden)", v)
	}
	if v, ok := attr["sessionUrl"].(bool); !ok || v {
		t.Errorf("attribution.sessionUrl = %v, want false", attr["sessionUrl"])
	}
	if !attributionOffIn(t, dir) {
		t.Errorf("full attribution-off key set not present after merge; got %+v", got)
	}
}

// An already-fully-seeded file is not rewritten (the SeedTrust
// don't-clobber-a-concurrent-writer rule).
func TestSeedAttributionOff_idempotentWhenAlreadySeeded(t *testing.T) {
	dir := t.TempDir()
	settingsPath := worktreeSettingsPath(t, dir)
	writeCfg(t, settingsPath, map[string]any{
		"includeCoAuthoredBy": false,
		"attribution":         map[string]any{"commit": "", "pr": "", "sessionUrl": false},
	})

	before := mustModTime(t, settingsPath)
	if err := SeedAttributionOff(dir); err != nil {
		t.Fatalf("SeedAttributionOff: %v", err)
	}
	if got := mustModTime(t, settingsPath); got != before {
		t.Errorf("settings rewritten despite attribution already off; mtime %v -> %v", before, got)
	}
}

func TestSeedAttributionOff_malformedSettingsReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(worktreeSettingsPath(t, dir), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedAttributionOff(dir); err == nil {
		t.Error("expected error on malformed settings.local.json; got nil")
	}
}
