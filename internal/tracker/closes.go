package tracker

// The shared closing-directive grammar (design §3a cr_closes, M6 pinned
// contract). ONE implementation of the GitHub/Forgejo closing-keyword match
// lives here and every consumer goes through it: the agent API validates and
// injects `Closes #<N>` on PR/CR bodies with ContainsCloses, and the built-in
// tracker's CreatePull persists ParseCloses(body) as the new CR's cr_closes
// rows — the issues the operator's merge later auto-closes.

import (
	"regexp"
	"sort"
	"strconv"
)

// closesDirectiveRe matches a GitHub/Forgejo issue-closing directive: one of
// the closing keywords (close/closes/closed, fix/fixes/fixed,
// resolve/resolves/resolved), case-insensitive and word-bounded on the left,
// an optional colon, whitespace, then #<number> bounded on the right (so
// "discloses" is not "closes", and "#7abc"/"#70" are not #7). Capture group 1
// is the referenced issue number.
var closesDirectiveRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#(\d+)\b`)

// ParseCloses extracts every issue number body references through a real
// closing directive, deduplicated and sorted ascending — the form
// store.CreateCR persists as cr_closes rows. Non-positive numbers and tokens
// too large for an int are ignored. The result is never nil; an empty slice
// means "no closing directives".
func ParseCloses(body string) []int {
	seen := make(map[int]bool)
	out := make([]int, 0)
	for _, m := range closesDirectiveRe.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// ContainsCloses reports whether body already carries a REAL closing
// directive for issue n — any of the closing keywords (not just "closes"),
// word- and number-bounded so a bare substring ("discloses #7"), an adjacent
// number ("Closes #70"), or a trailing token ("Closes #7abc") never falsely
// satisfy issue 7. Forgejo's own closing-keyword matcher is bounded the same
// way, so an unbounded match here would let a fake directive suppress the
// agent API's injected trailer and drop the real auto-close.
func ContainsCloses(body string, n int) bool {
	for _, m := range closesDirectiveRe.FindAllStringSubmatch(body, -1) {
		// Parse, don't string-compare: ParseCloses runs Atoi, so "Fixes #07"
		// closes issue 7 — a digit-string compare would miss it and make the
		// two halves of the same grammar disagree on leading zeros.
		if got, err := strconv.Atoi(m[1]); err == nil && got == n {
			return true
		}
	}
	return false
}
