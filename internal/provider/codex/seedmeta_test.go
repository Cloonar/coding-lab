package codex

// SeedMeta byte-identity pin (issue #51 decision 8, mirroring claudecode's
// golden). The generic seeder (internal/seeder) produces byte-identical
// worktree contents and pre-push hooks ONLY as long as codex keeps declaring
// today's shapes; this test pins the REAL declaration so a drift fails a
// test rather than silently reshaping seeded worktrees. If you change any
// value here, you are changing what lab seeds into every codex worktree.

import (
	"reflect"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

func TestSeedMeta_pinnedGolden(t *testing.T) {
	want := provider.SeedMeta{
		ContextFileName:      "AGENTS.local.md",
		SkillsDir:            ".codex/skills",
		NativeSkillDiscovery: false,
		ExcludeEntries:       []string{".codex/", "AGENTS.local.md"},
		SeededPathPatterns:   []string{`^\.codex/skills/`, `^AGENTS\.local\.md$`},
		ScrubPatterns: []string{
			`co-authored-by:[[:space:]]*codex`,
			`co-authored-by:.*<[^>]*@openai\.com>`,
			`generated with.*codex`,
		},
	}
	got := (&Provider{}).SeedMeta()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("codex.SeedMeta() drifted from the pinned golden.\n got: %#v\nwant: %#v", got, want)
	}
}

// SeedMeta returns defensive copies: a caller mutating the returned slices
// must not corrupt the next caller's view (the declaration is a shared
// package var).
func TestSeedMeta_returnsClones(t *testing.T) {
	m := (&Provider{}).SeedMeta()
	m.ExcludeEntries[0] = "mutated"
	m.SeededPathPatterns[0] = "mutated"
	m.ScrubPatterns[0] = "mutated"

	fresh := (&Provider{}).SeedMeta()
	if fresh.ExcludeEntries[0] != ".codex/" ||
		fresh.SeededPathPatterns[0] != `^\.codex/skills/` ||
		fresh.ScrubPatterns[0] != `co-authored-by:[[:space:]]*codex` {
		t.Errorf("SeedMeta() shares backing arrays across calls: %#v", fresh)
	}
}

// BRE↔RE2 semantic equivalence pin (issue #75 / ADR-0033). codex's three
// declared ScrubPatterns feed TWO enforcement points — the pre-push hook's
// `grep -i` (BRE) and the agent-API body sanitizer's compiled `(?i)` Go
// regexp (RE2) — so this locks that the REAL declaration (a) compiles
// cleanly via provider.CompileScrubPatterns and (b) each compiled pattern
// matches the canonical DEFENSIVE attribution line it exists to catch
// (codex 0.133 writes none at the source — these guard against a future
// version turning attribution on), and (c) near-miss clean lines match NONE.
func TestSeedMeta_scrubPatternsCompileAndMatchCanonicalSamples(t *testing.T) {
	m := (&Provider{}).SeedMeta()
	res, err := provider.CompileScrubPatterns(m.ScrubPatterns)
	if err != nil {
		t.Fatalf("CompileScrubPatterns(codex's declared ScrubPatterns): %v", err)
	}
	if len(res) != len(m.ScrubPatterns) {
		t.Fatalf("CompileScrubPatterns returned %d regexps; want %d", len(res), len(m.ScrubPatterns))
	}

	// One canonical sample per declared pattern, in declaration order.
	samples := []string{
		"Co-authored-by: Codex <noreply@openai.com>",
		"Co-authored-by: ChatGPT Codex <bot@openai.com>",
		"Generated with Codex",
	}
	for i, sample := range samples {
		if !res[i].MatchString(sample) {
			t.Errorf("pattern %d (%q) did not match its canonical sample %q", i, m.ScrubPatterns[i], sample)
		}
	}

	// Lines that must slip past every pattern: a human co-author with no
	// openai.com email, and unrelated prose naming the domain.
	nonMatches := []string{
		"Co-authored-by: Alice <alice@example.com>",
		"The openai.com docs describe the responses API.",
	}
	for _, sample := range nonMatches {
		for i, re := range res {
			if re.MatchString(sample) {
				t.Errorf("pattern %d (%q) unexpectedly matched non-marker line %q", i, m.ScrubPatterns[i], sample)
			}
		}
	}
}
