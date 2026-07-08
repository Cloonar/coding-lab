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
		ContextFileName: "CLAUDE.local.md",
		SkillsDir:       ".claude/skills",
		ExcludeEntries:  []string{".claude/", "CLAUDE.local.md"},
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

	fresh := (&Provider{}).SeedMeta()
	if fresh.ExcludeEntries[0] != ".claude/" ||
		fresh.SeededPathPatterns[0] != `^\.claude/skills/` ||
		fresh.ScrubPatterns[0] != `co-authored-by:[[:space:]]*claude` {
		t.Errorf("SeedMeta() shares backing arrays across calls: %#v", fresh)
	}
}
