package afk

// The engine's pure decision functions — ported verbatim from lab-v0 afk.go
// (the afk-engine port-spec §2 tables are the contract), clock-injected and
// free of tmux/tracker/git/store so every one is table-testable in isolation.
// The one D12 delta visible here: the budget clock is the persisted
// runs.budget_deadline (a time), not an in-memory age, so Classify compares
// now against the deadline — `now >= deadline` is exactly v0's inclusive
// `age >= budget` boundary.

import (
	"strconv"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// PauseThreshold is how many consecutive failed runs (death or timeout)
// pause a repo's automatic loop: at/over it the scheduler launches no
// further auto runs and a manual AFK start is refused (409), until a success
// reap or a human POST reset zeroes the counter (ADR-0007: never an
// automatic un-pause). The comparison is `consecutive_failures >=
// PauseThreshold` everywhere.
const PauseThreshold = 3

// Outcome is the classification of an AFK run on a reaper tick:
// still running (the only non-terminal value), or one of the three terminal
// outcomes the reaper acts on.
type Outcome int

const (
	OutcomeRunning Outcome = iota // alive, no PR, under budget — leave it
	OutcomeSuccess                // an open/merged PR/CR on the run's branch exists
	OutcomeDeath                  // session gone, no done-signal
	OutcomeTimeout                // alive, no done-signal, at/over budget
)

// String renders the v0 log vocabulary ("afk reap <repo> #<n>: <outcome>").
func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeDeath:
		return "failure (death)"
	case OutcomeTimeout:
		return "failure (timeout)"
	default:
		return "in-progress"
	}
}

// RunOutcome maps a terminal Outcome onto the runs.outcome vocabulary
// (design §3a); OutcomeRunning has no row value and maps to "".
func (o Outcome) RunOutcome() string {
	switch o {
	case OutcomeSuccess:
		return store.RunOutcomeSuccess
	case OutcomeDeath:
		return store.RunOutcomeDeath
	case OutcomeTimeout:
		return store.RunOutcomeTimeout
	default:
		return ""
	}
}

// failureReason is the runs.failure_reason a terminal outcome records
// (successes record none).
func (o Outcome) failureReason() string {
	switch o {
	case OutcomeDeath:
		return "session died before its done-signal"
	case OutcomeTimeout:
		return "budget exhausted before its done-signal"
	default:
		return ""
	}
}

// Classify is the unit-tested core of the reaper: it decides a run's outcome
// from these inputs and nothing else — no tmux, tracker, or wall clock. The
// priority order is load-bearing (v0 classifyAFKRun, ported verbatim):
//
//	prPresent              → success   (the done-signal beats everything,
//	                                     death and over-budget included)
//	!sessionAlive          → death     (death beats timeout)
//	now >= budgetDeadline  → timeout   (inclusive boundary — v0's age >= budget)
//	otherwise              → running
func Classify(prPresent, sessionAlive bool, now, budgetDeadline time.Time) Outcome {
	switch {
	case prPresent:
		return OutcomeSuccess
	case !sessionAlive:
		return OutcomeDeath
	case !now.Before(budgetDeadline):
		return OutcomeTimeout
	default:
		return OutcomeRunning
	}
}

// AutoDecision holds the facts the auto-launch predicate weighs for one repo
// on one scheduler tick (v0 afkAutoDecision, verbatim).
type AutoDecision struct {
	AutoEnabled  bool // the per-repo toggle is on
	UnderCap     bool // lab is below its live-instance cap
	AutoInFlight bool // this repo already has an auto run live
	ReadyExists  bool // a CLAIMABLE ready-for-agent issue exists (not raw ready)
	Paused       bool // consecutive_failures >= PauseThreshold
}

// ShouldLaunchAuto decides whether the scheduler launches an auto AFK run
// for a repo this tick (v0 shouldLaunchAuto, verbatim): toggle on, under the
// cap, no auto run already in flight, a claimable issue waiting, not paused.
// Every term is load-bearing — flipping exactly one blocks the launch.
func ShouldLaunchAuto(d AutoDecision) bool {
	return d.AutoEnabled && d.UnderCap && !d.AutoInFlight && d.ReadyExists && !d.Paused
}

// PickLowestIssue returns the issue with the minimum Number from the list;
// ok=false on an empty list. Deliberately does NOT trust the tracker's list
// order — the minimum is computed so the claim is deterministic. First-seen
// wins on (impossible-in-practice) duplicates (`<`, not `<=`).
func PickLowestIssue(issues []tracker.Issue) (tracker.Issue, bool) {
	if len(issues) == 0 {
		return tracker.Issue{}, false
	}
	lowest := issues[0]
	for _, is := range issues[1:] {
		if is.Number < lowest.Number {
			lowest = is
		}
	}
	return lowest, true
}

// ClaimableIssues filters a ready queue down to the issues not already
// claimed — those without a local claim branch (ADR-0013: the branch IS the
// claim). Preserves input order; returns an empty (non-nil, len 0) slice
// when all are claimed. This is what makes selection drain *around*
// parked/in-flight issues with zero tracker writes.
func ClaimableIssues(ready []tracker.Issue, claimed map[int]bool) []tracker.Issue {
	out := make([]tracker.Issue, 0, len(ready))
	for _, is := range ready {
		if !claimed[is.Number] {
			out = append(out, is)
		}
	}
	return out
}

// ClaimedIssues is the claim oracle over a branch listing: every branch that
// is an exact rendering of the repo's afk_branch_pattern registers its issue
// number as claimed (gitx.ParseBranch's strict inverse — a stray branch
// under the prefix never registers). Nothing here assumes the literal
// "afk/": the pattern is per-repo config (D15.4).
func ClaimedIssues(branches []string, pattern string) map[int]bool {
	claimed := make(map[int]bool, len(branches))
	for _, b := range branches {
		if n, ok := gitx.ParseBranch(pattern, b); ok {
			claimed[n] = true
		}
	}
	return claimed
}

// AFK label grammar (v0 instance.go §2.8, verbatim): manual AFK runs are
// labelled afk-<N>, auto runs afk-auto-<N>. Both use only [a-z0-9-] so they
// survive label sanitization and never contain the "~" session separator.
const (
	labelPrefix = "afk-"
	autoMarker  = "auto-"
)

// Label renders an AFK run's instance label from its issue number and kind.
func Label(n int, auto bool) string {
	if auto {
		return labelPrefix + autoMarker + strconv.Itoa(n)
	}
	return labelPrefix + strconv.Itoa(n)
}

// ParseLabel is the strict inverse of Label: cut the "afk-" prefix (else not
// AFK), optionally cut "auto-" (an auto run), then require a positive
// integer. Rejects afk-0, afk--1, afk-1x, afk-x, and every non-AFK label —
// a user label starting with "afk-" never registers as an AFK run.
func ParseLabel(label string) (n int, auto, ok bool) {
	rest, found := strings.CutPrefix(label, labelPrefix)
	if !found {
		return 0, false, false
	}
	if r, isAuto := strings.CutPrefix(rest, autoMarker); isAuto {
		auto = true
		rest = r
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false, false
	}
	return n, auto, true
}

// SeedPrompt is the one-issue instruction set delivered to a just-spawned
// AFK session (brief §8.4, adapted from v0 afkSeedPrompt — every ADR-0007
// contract element kept: read #N, stay on the run branch, implement, verify,
// commit, push, open a PR with `Closes #N`, then stop). The tracker surface
// is labctl ONLY (D10 — tea/gh are gone); branch is the repo's rendered
// claim branch, never a literal prefix. The incogni sentence is appended to
// the commit step only for incogni repos (M7 refines the wording; the
// conditional exists now).
func SeedPrompt(n int, branch string, incogni bool) string {
	num := strconv.Itoa(n)
	commit := "5. Commit in Conventional Commits style."
	if incogni {
		commit += " No AI attribution, no Co-Authored-By, no generated-with footers anywhere."
	}
	return strings.Join([]string{
		"You are an autonomous AFK run. Resolve exactly one issue, open a pull request, and stop.",
		"",
		"1. Run `labctl issue view " + num + "` and read it fully, including comments.",
		"2. Work only on branch `" + branch + "` in this worktree; never switch branches.",
		"3. Implement the issue completely, following the repository's own conventions (CLAUDE.md / AGENTS.md).",
		"4. Run the project's tests, build, and linters; fix what you break.",
		commit,
		"6. `git push -u origin " + branch + "`.",
		"7. `labctl pr create --title \"…\" --body \"…\"` — the body must include `Closes #" + num + "`.",
		"8. Then stop working. Do not start unrelated work.",
	}, "\n")
}
