package tracker

// Pure decision functions over a Pulls() result. The reaper (M5) and the
// parked-badge UI call these to match a run's head branch against the repo's
// pull requests client-side — no tracker round-trip per candidate branch; the
// issue list (issue #88) calls IssuePulls to join the same one read onto its
// rows. All are pure and table-tested (design §4d, §11 TestPullState).

import "git.cloonar.com/Cloonar/coding-lab/internal/gitx"

// PullState returns the state of the pull request whose head branch equals
// head, or ("", false) when no matching PR exists. When several PRs collide on
// the same head, open beats merged beats closed — the most "live" state wins,
// so a stale closed PR never masks a re-opened one (design §4d
// open-beats-closed precedence). A matching PR carrying a state outside the
// three-valued vocabulary is ignored (treated as no match); the forge REST
// client and the built-in tracker only ever emit the three, so this is
// defensive.
func PullState(pulls []PullRef, head string) (string, bool) {
	best, rank := bestPull(pulls, head)
	if rank == 0 {
		return "", false
	}
	return best.State, true
}

// bestPull is the one collision loop both PullState and DonePull project from
// — the highest-ranked head-matching pull plus its rank (0 = no recognizable
// match) — so their notion of "the winning pull" can never diverge.
func bestPull(pulls []PullRef, head string) (PullRef, int) {
	var best PullRef
	bestRank := 0
	for _, p := range pulls {
		if p.HeadBranch != head {
			continue
		}
		if r := stateRank(p.State); r > bestRank {
			bestRank, best = r, p
		}
	}
	return best, bestRank
}

// DonePull returns the head-matching pull that constitutes an AFK run's
// done-signal — an open or merged PR/CR — or (PullRef{}, false) when none does.
// Open beats merged on a same-head collision (stateRank), so a run whose branch
// carries both a live re-opened PR and a stale merged one reports the live one;
// a closed-unmerged pull (or an unrecognized state) never matches, so the run
// fails on death/timeout instead of being falsely reaped as a success. The
// whole three-state vocabulary exists for exactly this reading.
func DonePull(pulls []PullRef, head string) (PullRef, bool) {
	// The done floor is merged: a winning closed (rank 1) or unrecognized
	// (rank 0) pull never matches, so a stale closed PR cannot mask a run.
	best, rank := bestPull(pulls, head)
	if rank < stateRank(PullMerged) {
		return PullRef{}, false
	}
	return best, true
}

// PRPresent reports whether a head-matching PR counts as an AFK run's
// done-signal — the boolean projection of DonePull, so "is there a done-signal"
// and "which pull is it" can never diverge.
func PRPresent(pulls []PullRef, head string) bool {
	_, ok := DonePull(pulls, head)
	return ok
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

// IssuePulls joins a Pulls() result onto issue numbers: for every issue some
// OPEN pull resolves, the pull that resolves it — the issue list's `pull`
// field (issue #88), which lets the New run page offer to land an issue's
// pull straight from the row. It is the one read the list spends on pulls,
// joined client-side like PullState, never a per-issue tracker round-trip.
//
// A pull resolves issue N by either of two signals, and may resolve several
// issues at once:
//
//   - its head branch is the repo's claim branch for N — gitx.MatchBranch
//     against branchPattern (the repo's afk_branch_pattern), the same
//     strict-inverse mapping the autoland poller reads a PR's issue through,
//     so a branch that is not an exact rendering (leading zeros, a stray
//     suffix) claims nothing;
//   - N is in its Closes — the body's real closing directives, which is how
//     a pull on a non-claim branch (a human's feature/x saying "Closes #47"),
//     or one closing several issues, still resolves them.
//
// Only PullOpen counts: a merged pull has nothing left to land, and a
// closed-unmerged one is "no PR" here exactly as it is to the reaper — the
// operator's answer to an issue whose pull was abandoned is a new run, not a
// stale Land. When several open pulls resolve one issue the highest Number
// wins: numbers are allocated monotonically on every backend, so it is the
// newest attempt, and the join never depends on Pulls' list order. An issue no
// open pull resolves is absent from the map; the result is never nil.
func IssuePulls(pulls []PullRef, branchPattern string) map[int]PullRef {
	out := make(map[int]PullRef)
	for _, p := range pulls {
		if p.State != PullOpen {
			continue
		}
		attach := func(n int) {
			if cur, ok := out[n]; !ok || p.Number > cur.Number {
				out[n] = p
			}
		}
		if n, ok := gitx.MatchBranch(branchPattern, p.HeadBranch); ok {
			attach(n)
		}
		for _, n := range p.Closes {
			attach(n)
		}
	}
	return out
}
