package provider

import (
	"fmt"
	"regexp"
)

// CompileScrubPatterns compiles a provider's SeedMeta().ScrubPatterns into
// case-insensitive Go regexps — the Go twin of the incogni pre-push hook's
// `grep -i` match (issue #75 / ADR-0033). The hook interprets each pattern as
// a POSIX BRE; this compiles the same string as RE2, prefixed with "(?i)" so
// matching is case-insensitive like the hook's -i flag. Because one
// declaration now feeds both engines, every pattern MUST stay in the
// BRE∩RE2 common dialect (no RE2-only escapes such as \d/\b/non-greedy
// quantifiers/lookaround, no BRE-only construct whose meaning changes under
// RE2) — claudecode's four patterns are the pinned example of the dialect.
// On a compile failure the returned error names the offending pattern, so a
// declaration that drifts out of the common dialect is easy to trace back to
// its source.
func CompileScrubPatterns(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			return nil, fmt.Errorf("provider: scrub pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}
