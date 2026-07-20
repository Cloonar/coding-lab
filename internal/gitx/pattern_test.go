package gitx

import "testing"

func TestValidatePattern(t *testing.T) {
	tests := []struct {
		pattern string
		wantOK  bool
	}{
		// Accepts: both §4a table anchors plus grammar-legal shapes.
		{"afk/<N>", true},
		{"issue-<N>", true},
		{"agent/issue-<N>", true},
		{"a.b/c-<N>", true},
		{"v<N>-wip", true},
		{"<N>", true},

		// Rejects.
		{"", false},            // no <N>
		{"afk/", false},        // no <N>
		{"afk", false},         // no <N>
		{"afk/<n>", false},     // token is case-sensitive (and <,> unsafe)
		{"afk/<N>/<N>", false}, // two <N>
		{"afk/<N><N>", false},  // two <N>
		{"afk ~<N>", false},    // space
		{"afk~<N>", false},     // ~
		{"afk?<N>", false},     // outside [A-Za-z0-9._/-]
		{"/afk/<N>", false},    // leading /
		{"<N>/", false},        // trailing /
		{"a..b/<N>", false},    // ..
		{"..<N>", false},       // ..
	}
	for _, tt := range tests {
		err := ValidatePattern(tt.pattern)
		if got := err == nil; got != tt.wantOK {
			t.Errorf("ValidatePattern(%q) = %v, want ok=%v", tt.pattern, err, tt.wantOK)
		}
	}
}

func TestValidateManualPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		wantOK bool
	}{
		// Accepts: both defaults end with '/' — trailing '/' must be legal.
		{"lab/", true},
		{"wip/", true},
		{"lab", true},
		{"dev.x/", true},
		{"a/b/", true},

		// Rejects.
		{"", false},       // empty prefix would classify every branch as manual
		{"/lab/", false},  // leading /
		{"a..b/", false},  // ..
		{"wip ~", false},  // space
		{"wip~", false},   // ~
		{"<N>", false},    // literal prefix; <,> outside the safe set anyway
		{"wip:x/", false}, // outside [A-Za-z0-9._/-]
	}
	for _, tt := range tests {
		err := ValidateManualPrefix(tt.prefix)
		if got := err == nil; got != tt.wantOK {
			t.Errorf("ValidateManualPrefix(%q) = %v, want ok=%v", tt.prefix, err, tt.wantOK)
		}
	}
}

func TestValidatePatternPair(t *testing.T) {
	tests := []struct {
		afk    string
		manual string
		wantOK bool
	}{
		// The two default pairs (design §3a).
		{"afk/<N>", "lab/", true},
		{"issue-<N>", "wip/", true},
		// Cross-combinations of the defaults are fine too.
		{"afk/<N>", "wip/", true},
		{"issue-<N>", "lab/", true},

		// §4a's canonical reject: manual prefix covers the afk namespace.
		{"wip/<N>", "wip/", false},
		{"afk/<N>", "afk/", false},
		{"issue-<N>", "issue-", false},
		{"issue-<N>", "iss", false},  // shorter prefix still covers issue-*
		{"afk/<N>", "a", false},      // covers afk/* as well
		{"afk/<N>", "afk/1", false},  // matches afk/1, afk/12, …
		{"afk/<N>", "afk/12", false}, // matches afk/12, afk/123, …
		{"<N>", "9", false},          // bare-number pattern vs digit prefix

		// Non-overlaps that look close.
		{"afk/<N>", "afk/0", true},  // no rendered afk branch starts afk/0 (N ≥ 1, no leading zeros)
		{"afk/<N>", "afk/x-", true}, // 'x' can never appear where digits go
		{"afk/<N>", "afkx/", true},  // afk/* never starts with afkx/
		{"<N>", "lab/", true},       // rendered forms are pure digits
		{"v<N>-wip", "lab/", true},

		// Suffix-aware overlap: rendered "v2x1" starts with manual "v2x".
		{"v<N>x1", "v2x", false},
		{"v<N>x", "v2y", true}, // 'y' never continues a rendering

		// Digit-leading suffix: N may stop before the manual prefix's digit
		// run ends. RenderBranch("a<N>2X", 1) = "a12X" is simultaneously a
		// manual branch under "a12X" — the pair must be rejected.
		{"a<N>2X", "a12X", false},
		{"a<N>2X", "a122X", false}, // N=12 renders "a122X"
		{"a<N>3X", "a12X", true},   // near miss: no N makes "a…3X" start with "a12X"
		{"a<N>2X", "a2X", true},    // N must contribute at least one digit before "2X"
		{"a<N>2X", "a02X", true},   // leading-zero N is never rendered

		// Pair validation also runs the individual validations.
		{"afk/", "lab/", false},
		{"afk/<N>", "", false},
	}
	for _, tt := range tests {
		err := ValidatePatternPair(tt.afk, tt.manual)
		if got := err == nil; got != tt.wantOK {
			t.Errorf("ValidatePatternPair(%q, %q) = %v, want ok=%v", tt.afk, tt.manual, err, tt.wantOK)
		}
	}
}

func TestRenderBranch(t *testing.T) {
	tests := []struct {
		pattern string
		n       int
		want    string
	}{
		{"afk/<N>", 7, "afk/7"},
		{"afk/<N>", 1000, "afk/1000"},
		{"issue-<N>", 42, "issue-42"},
		{"v<N>-wip", 3, "v3-wip"},
		{"<N>", 1, "1"},
	}
	for _, tt := range tests {
		if got := RenderBranch(tt.pattern, tt.n); got != tt.want {
			t.Errorf("RenderBranch(%q, %d) = %q, want %q", tt.pattern, tt.n, got, tt.want)
		}
	}
}

func TestParseBranch(t *testing.T) {
	tests := []struct {
		pattern string
		ref     string
		wantN   int
		wantOK  bool
	}{
		// afk/<N> rows.
		{"afk/<N>", "afk/7", 7, true},
		{"afk/<N>", "afk/1", 1, true},
		{"afk/<N>", "afk/1000", 1000, true},
		{"afk/<N>", "afk/0", 0, false},   // issue numbers start at 1
		{"afk/<N>", "afk/007", 0, false}, // no leading zeros
		{"afk/<N>", "afk/01", 0, false},  // no leading zeros
		{"afk/<N>", "afk/+7", 0, false},  // Atoi would take it; exact re-render rejects
		{"afk/<N>", "afk/-1", 0, false},
		{"afk/<N>", "afk/7x", 0, false}, // trailing junk
		{"afk/<N>", "afk/x7", 0, false},
		{"afk/<N>", "afk/1.5", 0, false},
		{"afk/<N>", "afk/", 0, false}, // no digits
		{"afk/<N>", "afk", 0, false},
		{"afk/<N>", "lab/7", 0, false},                    // other namespace
		{"afk/<N>", "afk/99999999999999999999", 0, false}, // overflows int

		// issue-<N> rows (§4a: table tests must cover it alongside afk/<N>).
		{"issue-<N>", "issue-42", 42, true},
		{"issue-<N>", "issue-1", 1, true},
		{"issue-<N>", "issue-0", 0, false},
		{"issue-<N>", "issue-042", 0, false},
		{"issue-<N>", "issue-", 0, false},
		{"issue-<N>", "issue-42-fix", 0, false},
		{"issue-<N>", "issues-42", 0, false},
		{"issue-<N>", "afk/42", 0, false},

		// Suffix-bearing pattern: strict on both sides of <N>.
		{"v<N>-wip", "v3-wip", 3, true},
		{"v<N>-wip", "v3-wipx", 0, false},
		{"v<N>-wip", "v3", 0, false},
		{"v<N>-wip", "v-wip", 0, false},

		// Ref shorter than prefix+suffix must not panic.
		{"aa<N>aa", "aaa", 0, false},

		// Pattern without <N> (never validated in) parses nothing.
		{"afk/", "afk/7", 0, false},
	}
	for _, tt := range tests {
		n, ok := ParseBranch(tt.pattern, tt.ref)
		if n != tt.wantN || ok != tt.wantOK {
			t.Errorf("ParseBranch(%q, %q) = (%d, %v), want (%d, %v)",
				tt.pattern, tt.ref, n, ok, tt.wantN, tt.wantOK)
		}
	}
}

// TestMatchBranch pins the Autoland-facing reverse of RenderBranch (issue
// #181): a PR head branch matches iff it is an exact rendering of the claim
// pattern — the same strict-inverse contract as ParseBranch, so the two
// oracles can never disagree on what is a claim.
func TestMatchBranch(t *testing.T) {
	tests := []struct {
		pattern string
		branch  string
		wantN   int
		wantOK  bool
	}{
		// Match rows.
		{"afk/<N>", "afk/7", 7, true},
		{"afk/<N>", "afk/181", 181, true}, // multi-digit
		{"issue-<N>", "issue-42", 42, true},

		// No-match rows.
		{"afk/<N>", "lab/7", 0, false},
		{"afk/<N>", "afk/7x", 0, false},
		{"afk/<N>", "afk/007", 0, false}, // leading zeros are not a rendering
		{"afk/<N>", "afk/0", 0, false},   // n starts at 1
		{"afk/<N>", "main", 0, false},

		// Prefix-only pattern.
		{"afk/<N>", "afk/", 0, false},

		// Suffix patterns: strict on both sides of <N>.
		{"v<N>-wip", "v3-wip", 3, true},
		{"v<N>-wip", "v3", 0, false},
		{"v<N>-wip", "v3-wipx", 0, false},
	}
	for _, tt := range tests {
		n, ok := MatchBranch(tt.pattern, tt.branch)
		if n != tt.wantN || ok != tt.wantOK {
			t.Errorf("MatchBranch(%q, %q) = (%d, %v), want (%d, %v)",
				tt.pattern, tt.branch, n, ok, tt.wantN, tt.wantOK)
		}
	}
}

func TestParseBranchRoundTrip(t *testing.T) {
	for _, pattern := range []string{"afk/<N>", "issue-<N>", "v<N>-wip", "<N>"} {
		for _, n := range []int{1, 2, 7, 62, 1000} {
			ref := RenderBranch(pattern, n)
			got, ok := ParseBranch(pattern, ref)
			if !ok || got != n {
				t.Errorf("ParseBranch(%q, RenderBranch(%q, %d)=%q) = (%d, %v), want (%d, true)",
					pattern, pattern, n, ref, got, ok, n)
			}
		}
	}
}

func TestPatternPrefix(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{"afk/<N>", "afk/"},
		{"issue-<N>", "issue-"},
		{"v<N>-wip", "v"},
		{"<N>", ""},
	}
	for _, tt := range tests {
		if got := PatternPrefix(tt.pattern); got != tt.want {
			t.Errorf("PatternPrefix(%q) = %q, want %q", tt.pattern, got, tt.want)
		}
	}
}
