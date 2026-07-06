package tracker

// Pure decision functions over a Pulls() result. The reaper (M5) and the
// parked-badge UI call these to match a run's head branch against the repo's
// pull requests client-side — no tracker round-trip per candidate branch.
// Both are pure and table-tested (design §4d, §11 TestPullState).

// PullState returns the state of the pull request whose head branch equals
// head, or ("", false) when no matching PR exists. When several PRs collide on
// the same head, open beats merged beats closed — the most "live" state wins,
// so a stale closed PR never masks a re-opened one (design §4d
// open-beats-closed precedence). A matching PR carrying a state outside the
// three-valued vocabulary is ignored (treated as no match); the forge REST
// client and the built-in tracker only ever emit the three, so this is
// defensive.
func PullState(pulls []PullRef, head string) (string, bool) {
	best := ""
	bestRank := 0
	for _, p := range pulls {
		if p.HeadBranch != head {
			continue
		}
		if r := stateRank(p.State); r > bestRank {
			bestRank, best = r, p.State
		}
	}
	if bestRank == 0 {
		return "", false
	}
	return best, true
}

// PRPresent reports whether a head-matching PR counts as an AFK run's
// done-signal: an open or merged PR ⇒ done; a closed-unmerged one (or none at
// all) ⇒ not done, so the run fails on death/timeout instead of being falsely
// reaped as a success. This is the whole point of the three-state vocabulary.
func PRPresent(pulls []PullRef, head string) bool {
	state, ok := PullState(pulls, head)
	return ok && (state == PullOpen || state == PullMerged)
}

// stateRank orders the three-valued PR state for open-beats-merged-beats-closed
// precedence; an unrecognized state ranks 0 and never wins a collision.
func stateRank(state string) int {
	switch state {
	case PullOpen:
		return 3
	case PullMerged:
		return 2
	case PullClosed:
		return 1
	default:
		return 0
	}
}
