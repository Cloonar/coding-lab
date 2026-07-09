package tracker

// Pure aggregate function over a Checks() result. The agent-readable CI
// status surface (labctl pr checks) calls this client-side — never a forge's
// own combined-status verdict — because Forgejo reports "pending" for a
// commit carrying zero registered statuses (there is no CI to be pending on),
// which would spin a waiting agent forever if lab trusted it instead of
// computing the aggregate itself. ChecksState is pure and table-tested
// (checks_test.go).

// ChecksState aggregates a Checks() result into a single word. ChecksNone iff
// checks is empty (see the package comment above for why that must be
// computed here, never read off the forge). Otherwise precedence is
// failure > pending > success: one red row is actionable immediately even
// while sibling jobs are still running — a DELIBERATE departure from a forge
// UI's combined badge, which typically stays "pending" until every job
// finishes. The consumer here is an agent deciding whether to act, not a
// human glancing at a status dot, and a red job rarely turns green on its
// own, so surfacing failure early is the useful answer. A row whose State
// falls outside the three-word vocabulary (CheckPending|CheckSuccess|
// CheckFailure) also ranks as failure — the OPPOSITE default from pulls.go's
// stateRank, which ignores an unrecognized PR state defensively (a state it
// can't rank just loses ties). Here silence is the wrong default: a backend
// emitting a state this package does not know is a vocabulary bug, and a
// vocabulary bug must be a loud failure signal, never a silently-green check
// an agent trusts and merges on.
func ChecksState(checks []Check) string {
	if len(checks) == 0 {
		return ChecksNone
	}
	sawPending := false
	for _, c := range checks {
		switch c.State {
		case CheckFailure:
			return CheckFailure
		case CheckPending:
			sawPending = true
		case CheckSuccess:
			// A success row alone never ends the scan — a later failure still wins.
		default:
			return CheckFailure // unrecognized state: loud, never silently green.
		}
	}
	if sawPending {
		return CheckPending
	}
	return CheckSuccess
}
