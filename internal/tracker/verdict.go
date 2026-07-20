package tracker

// The autoland verdict-marker grammar: a PR comment's FIRST line reading
// exactly `[autoland] verdict: <word>`, findings/CONCERNS prose following
// below. ADR-0048 is the grammar's canonical DOC home (it pins the exact
// contract and the parse rule); this file is the grammar's single CODE home,
// so the agentapi verdict verbs that COMPOSE a marker comment and the #181
// poller that READS one share one definition instead of two copies drifting
// apart.

import "strings"

// VerdictMarkerPrefix opens every verdict-marker comment. VerdictReject,
// VerdictPass, VerdictFixDone, and VerdictEscalate are the full first-line
// markers the verdict verbs compose (handlePRReject/Approve/Rerequest/
// Escalate, ADR-0048). VerdictEscalate is the escalate-mode lander's terminal
// marker (issue #182): ADR-0048 reserved the word, #182 ships it.
const (
	VerdictMarkerPrefix = "[autoland] verdict:"

	VerdictReject   = VerdictMarkerPrefix + " reject"
	VerdictPass     = VerdictMarkerPrefix + " pass"
	VerdictFixDone  = VerdictMarkerPrefix + " fix-done"
	VerdictEscalate = VerdictMarkerPrefix + " escalate"
)

// ParseVerdict parses body's verdict marker, if it has one. The FIRST line
// (everything before the first "\n") must open with VerdictMarkerPrefix by an
// EXACT match — no leading whitespace tolerated, so a marker indented by even
// one space is inert, and a keyword quoted mid-line or reappearing on a
// second line never triggers (ADR-0048's parse rule: first line only, exact
// prefix match). On a match it returns the line's remainder after the
// prefix, trimmed, and ok=true; word may be empty (a bare prefix with
// nothing following) or a word this package does not recognize — ParseVerdict
// parses the GRAMMAR only, it does not validate against the known verdict
// words, so a future word parses under the same rule with zero change here
// (this is how `escalate`, #182, arrived needing no change to this func).
// Anywhere else word is "" and ok is false.
func ParseVerdict(body string) (word string, ok bool) {
	first, _, _ := strings.Cut(body, "\n")
	if !strings.HasPrefix(first, VerdictMarkerPrefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(first, VerdictMarkerPrefix)), true
}
