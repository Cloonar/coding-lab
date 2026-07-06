package claudecode

// Incogni measure 1 (brief D15 §9): seed the worktree's project-local
// .claude/settings.local.json with attribution disabled, so claude itself
// never writes AI markers into commits or PR bodies. The keys are verified
// against the installed Claude Code 2.1.198 — see internal/compat/compat.md
// §4 for the schema extraction and provenance:
//
//   - `attribution.commit` (string): commit-trailer attribution text; ""
//     hides it (default: `Co-Authored-By: <model> <noreply@anthropic.com>`).
//   - `attribution.pr` (string): PR-body attribution text; "" hides it
//     (default: `🤖 Generated with [Claude Code](…)`).
//   - `attribution.sessionUrl` (bool): whether remote-control/web sessions
//     append the claude.ai session link (`Claude-Session` trailer + PR-body
//     link; default true). Load-bearing for lab: every lab session IS a
//     --remote-control session, so without sessionUrl=false a claude.ai
//     link leaks into every commit even with commit/pr blanked.
//   - `includeCoAuthoredBy` (bool): deprecated on 2.1.198 in favor of
//     `attribution`, still honored when `attribution` sets neither
//     commit nor pr. Seeded false as defense in depth (and for older
//     claudes that predate the attribution object).

import (
	"fmt"
	"os"
	"path/filepath"
)

// SeedAttributionOff merges the attribution-disabling keys into
// dir/.claude/settings.local.json — the same project-local file (and the
// same preserve-unknown-keys atomic writer) as the trust step's MCP
// approval. Unknown keys survive at every level, including inside an
// existing `attribution` object; an already-fully-seeded file is not
// rewritten (a needless rewrite risks clobbering a concurrent claude
// writer, the SeedTrust rule).
func SeedAttributionOff(dir string) error {
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", claudeDir, err)
	}
	path := filepath.Join(claudeDir, "settings.local.json")
	settings, err := readJSONObject(path)
	if err != nil {
		return err
	}
	if attributionOff(settings) {
		return nil // already fully seeded — don't rewrite
	}
	attr, _ := settings["attribution"].(map[string]any)
	if attr == nil {
		attr = map[string]any{}
	}
	attr["commit"] = ""
	attr["pr"] = ""
	attr["sessionUrl"] = false
	settings["attribution"] = attr
	settings["includeCoAuthoredBy"] = false
	return marshalAtomic(path, settings)
}

// attributionOff reports whether settings already carries every
// attribution-disabling key SeedAttributionOff would write.
func attributionOff(settings map[string]any) bool {
	inc, ok := settings["includeCoAuthoredBy"].(bool)
	if !ok || inc {
		return false
	}
	attr, _ := settings["attribution"].(map[string]any)
	if attr == nil {
		return false
	}
	commit, ok := attr["commit"].(string)
	if !ok || commit != "" {
		return false
	}
	pr, ok := attr["pr"].(string)
	if !ok || pr != "" {
		return false
	}
	sessionURL, ok := attr["sessionUrl"].(bool)
	return ok && !sessionURL
}
