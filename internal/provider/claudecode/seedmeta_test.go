package claudecode

// SeedMeta byte-identity pin (issue #51 decision 8). The generic seeder
// (internal/seeder) produces byte-identical worktree contents and pre-push
// hooks ONLY as long as claude-code keeps declaring today's shapes. The seeder
// tests prove "generic seeder + these golden values == today's bytes" against
// their OWN inlined literals (they must not import claudecode); this test is
// the other half of the contract — it pins the REAL declaration to the same
// literals, so a drift in either place fails a test. If you change any value
// here, you are changing what lab seeds into every claude-code worktree.

import (
	"reflect"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

func TestSeedMeta_pinnedGolden(t *testing.T) {
	want := provider.SeedMeta{
		ContextFileName:      "CLAUDE.local.md",
		SkillsDir:            ".claude/skills",
		NativeSkillDiscovery: true,
		ExcludeEntries:       []string{".claude/", "CLAUDE.local.md"},
		SeededPathPatterns: []string{
			`^\.claude/skills/`,
			`^\.claude/settings\.local\.json$`,
			`^CLAUDE\.local\.md$`,
		},
		ScrubPatterns: []string{
			`co-authored-by:[[:space:]]*claude`,
			`co-authored-by:.*<[^>]*@anthropic\.com>`,
			`generated with.*claude`,
			`claude-session:`,
		},
		// The host a claude-code run reaches DIRECTLY, folded into a
		// gateway-wired run's NO_PROXY (issue #24 / ADR-0067). Pinned here
		// because this literal must live in the adapter and nowhere else —
		// ADR-0033's neutrality guard fails the build if core names it.
		DirectAPIHosts: []string{"api.anthropic.com"},
	}
	got := (&Provider{}).SeedMeta()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claudecode.SeedMeta() drifted from the pinned golden.\n got: %#v\nwant: %#v", got, want)
	}
}

// SeedMeta returns defensive copies: a caller mutating the returned slices must
// not corrupt the next caller's view (the declaration is a shared package var).
func TestSeedMeta_returnsClones(t *testing.T) {
	m := (&Provider{}).SeedMeta()
	m.ExcludeEntries[0] = "mutated"
	m.SeededPathPatterns[0] = "mutated"
	m.ScrubPatterns[0] = "mutated"
	m.DirectAPIHosts[0] = "mutated"

	fresh := (&Provider{}).SeedMeta()
	if fresh.ExcludeEntries[0] != ".claude/" ||
		fresh.SeededPathPatterns[0] != `^\.claude/skills/` ||
		fresh.ScrubPatterns[0] != `co-authored-by:[[:space:]]*claude` ||
		fresh.DirectAPIHosts[0] != "api.anthropic.com" {
		t.Errorf("SeedMeta() shares backing arrays across calls: %#v", fresh)
	}
}

// BRE↔RE2 semantic equivalence pin (issue #75 / ADR-0033). claudecode's four
// declared ScrubPatterns now feed TWO enforcement points — the pre-push
// hook's `grep -i` (BRE) and the agent-API body sanitizer's compiled `(?i)`
// Go regexp (RE2) — so this locks that the REAL declaration (a) compiles
// cleanly via provider.CompileScrubPatterns and (b) each compiled pattern
// still matches the canonical attribution line it exists to catch, and (c)
// an unrelated co-author line and an unrelated "generated with" line match
// NONE of the four. A change to any pattern that breaks either side fails
// here, not silently at one of the two enforcement points.
func TestSeedMeta_scrubPatternsCompileAndMatchCanonicalSamples(t *testing.T) {
	m := (&Provider{}).SeedMeta()
	res, err := provider.CompileScrubPatterns(m.ScrubPatterns)
	if err != nil {
		t.Fatalf("CompileScrubPatterns(claudecode's declared ScrubPatterns): %v", err)
	}
	if len(res) != len(m.ScrubPatterns) {
		t.Fatalf("CompileScrubPatterns returned %d regexps; want %d", len(res), len(m.ScrubPatterns))
	}

	// One canonical sample per declared pattern, in declaration order.
	samples := []string{
		"Co-Authored-By: Claude <noreply@anthropic.com>",
		"Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>",
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)",
		"Claude-Session: https://claude.ai/code/session_x",
	}
	for i, sample := range samples {
		if !res[i].MatchString(sample) {
			t.Errorf("pattern %d (%q) did not match its canonical sample %q", i, m.ScrubPatterns[i], sample)
		}
	}

	// Lines that must slip past every pattern: a human co-author with no
	// anthropic.com email, and an unrelated "generated with" trailer.
	nonMatches := []string{
		"Co-Authored-By: Alice <alice@example.com>",
		"Docs generated with pandoc.",
	}
	for _, sample := range nonMatches {
		for i, re := range res {
			if re.MatchString(sample) {
				t.Errorf("pattern %d (%q) unexpectedly matched non-marker line %q", i, m.ScrubPatterns[i], sample)
			}
		}
	}
}
