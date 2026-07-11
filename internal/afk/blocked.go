package afk

// The AFK dependency-gate grammar (issue #136): before the scheduler claims a
// ready-for-agent issue it reads the issue body's "## Blocked by" section and
// skips the issue while any referenced issue is still open. These are the pure
// functions behind that gate — no tracker, store, git, or log — so every rule
// below is table-testable in isolation, mirroring the section-scoped,
// tolerant-but-bounded style of tracker.ParseCloses (internal/tracker/closes.go).
//
// The contract, pinned by #136's decisions:
//   - A ref blocks IFF that issue exists and is currently open; a closed
//     blocker unblocks regardless of HOW it closed (merged, wontfix, no PR).
//   - A ref absent from the open set — a closed issue, a dangling/deleted ref,
//     or a plain typo — never blocks: on data ambiguity we fail toward progress.
//   - Only the "Blocked by" section counts; a "#12" anywhere else in the body
//     is not a blocker. Issues predating the convention carry no such section
//     and are therefore unblocked.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// blockedByHeadingRe matches a "Blocked by" ATX heading on its own line: 1–6
// leading hashes (tolerant of a missing space after them, so "###Blocked by"
// counts), the words "blocked by" case-insensitively, an optional trailing
// colon, and only whitespace to end of line. A heading carrying trailing text
// ("## Blocked by #5") is deliberately NOT matched — the marker must stand
// alone, exactly as the to-issues template renders it.
var blockedByHeadingRe = regexp.MustCompile(`(?i)^#{1,6}[ \t]*blocked[ \t]+by[ \t]*:?[ \t]*$`)

// atxHeadingRe matches any ATX heading line — 1–6 hashes followed by
// whitespace or end of line (the CommonMark rule) — the boundary that ends a
// "Blocked by" section. "#5 …" is NOT a heading (no space after the hash), so a
// bare local ref on its own line stays INSIDE the section rather than closing
// it; only a real sibling heading (of any level, higher or lower) terminates.
var atxHeadingRe = regexp.MustCompile(`^#{1,6}(?:[ \t]|$)`)

// blockedByRefRe matches a local issue reference "#<n>" inside a section:
// number-bounded on the right exactly like tracker.closesDirectiveRe (so
// "#7abc" and "#70" are never #7), and NOT preceded by a word character or "/"
// — so the "#12" buried in a cross-repo ref (owner/repo#12) or a URL fragment
// (…/page#12) never registers as a LOCAL blocker. RE2 has no lookbehind, so the
// left boundary is a consumed leading class (start-of-text or a non-word,
// non-slash byte — a newline qualifies, so a ref at line start counts). Capture
// group 1 is the number.
var blockedByRefRe = regexp.MustCompile(`(?:^|[^\w/])#(\d+)\b`)

// foreignRefRe matches the two shapes of cross-repo dependency this per-repo
// gate cannot evaluate: a full issue URL (https://…/issues/<n>) and an
// owner/repo#<n> cross-repo ref. It is best-effort by design — the caller only
// LOGS these so a genuine off-repo dependency is visible rather than silently
// scheduled — so catching the two canonical shapes matters more than precision.
var foreignRefRe = regexp.MustCompile(`(?i)https?://\S+/issues/\d+\b|[\w.-]+/[\w.-]+#\d+\b`)

// blockedBySection returns the concatenated text of every "Blocked by" section
// in body — the region from the line after each matching heading to the next
// ATX heading of ANY level (higher or lower) or end of body. Multiple headings
// (a degenerate but legal shape) union their sections in document order, which
// preserves ForeignBlockedBy's order-of-appearance guarantee. No heading yields
// the empty string, i.e. no refs. Trailing "\r" is stripped per line so CRLF
// bodies detect their headings identically to LF ones.
func blockedBySection(body string) string {
	var b strings.Builder
	inSection := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case blockedByHeadingRe.MatchString(line):
			inSection = true // (re)enter; the heading line itself is never content
		case inSection && atxHeadingRe.MatchString(line):
			inSection = false // a sibling heading of any level closes the section
		case inSection:
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ParseBlockedBy returns every local issue number the body's "Blocked by"
// section references, deduplicated and sorted ascending — the candidate
// blockers PartitionBlocked weighs against the open set. Refs are section-
// scoped: a "#12" anywhere else in the body (before OR after the section) is
// never a blocker. Non-positive numbers and tokens too large for an int are
// dropped, mirroring tracker.ParseCloses. The result is never nil; an empty
// slice means "no local blockers" — which is also what a body with no such
// section, or the to-issues template's "None - can start immediately"
// placeholder, yields (neither carries a bounded local #<n>).
func ParseBlockedBy(body string) []int {
	section := blockedBySection(body)
	seen := make(map[int]bool)
	out := make([]int, 0)
	for _, m := range blockedByRefRe.FindAllStringSubmatch(section, -1) {
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

// ForeignBlockedBy returns the cross-repo and full-URL issue references inside
// the body's "Blocked by" section — the two shapes (owner/repo#<n> and
// https://…/issues/<n>) this per-repo gate cannot resolve to a local open/closed
// state. Tokens are returned verbatim, in order of appearance, deduplicated, and
// never nil. The caller only logs them, so a genuine off-repo dependency
// surfaces instead of being silently scheduled as unblocked; this is best-effort
// logging fodder, so the two canonical shapes matter more than precision.
func ForeignBlockedBy(body string) []string {
	section := blockedBySection(body)
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, tok := range foreignRefRe.FindAllString(section, -1) {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// BlockedIssue pairs a ready issue the gate is holding back with the subset of
// its blockers that are actually open — the refs that, once closed, could
// release it. OpenBlockers is sorted ascending and feeds the scheduler's
// log line "skipped #87 (blocked by #74)".
type BlockedIssue struct {
	Issue        tracker.Issue
	OpenBlockers []int
}

// PartitionBlocked splits an already-claim-filtered ready queue against the set
// of currently-open issue numbers. An issue is blocked IFF at least one of its
// ParseBlockedBy refs is present in open — multiple refs hold it until ALL of
// them resolve. A ref absent from open (a closed issue, a dangling/deleted ref,
// or a typo) never blocks: on data ambiguity the gate fails toward progress.
//
// An open blocker that is already claimed/in-flight still blocks, because a
// claimed issue is still OPEN — unmerged work is not done work; this falls out
// naturally, since open membership is all that is weighed. Cycles (A blocked-by
// B, B blocked-by A, or a self-reference as a 1-cycle) simply never schedule —
// a triage data bug, so there is deliberately no cycle detection here.
//
// Input order is preserved in both outputs, and unblocked is non-nil even when
// empty (like ClaimableIssues) so the caller can range it unconditionally.
// BlockedIssue.OpenBlockers holds only the open refs, sorted ascending (it
// inherits ParseBlockedBy's ordering).
func PartitionBlocked(ready []tracker.Issue, open map[int]bool) (unblocked []tracker.Issue, blocked []BlockedIssue) {
	unblocked = make([]tracker.Issue, 0, len(ready))
	for _, is := range ready {
		var openBlockers []int
		for _, ref := range ParseBlockedBy(is.Body) {
			if open[ref] {
				openBlockers = append(openBlockers, ref)
			}
		}
		if len(openBlockers) == 0 {
			unblocked = append(unblocked, is)
			continue
		}
		blocked = append(blocked, BlockedIssue{Issue: is, OpenBlockers: openBlockers})
	}
	return unblocked, blocked
}
