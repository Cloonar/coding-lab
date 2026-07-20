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

// landerDoneWords are the verdict words that END a lander run — pass and
// reject — derived from the canonical markers (tracker/verdict.go) through
// the parser itself so the grammar and this reading can never drift. fix-done
// is a FIX run's done-signal (#182) and never ends a lander; `escalate` and
// any future word are likewise not a lander's to finish on.
var landerDoneWords = func() map[string]bool {
	words := map[string]bool{}
	for _, marker := range []string{tracker.VerdictPass, tracker.VerdictReject} {
		w, ok := tracker.ParseVerdict(marker)
		if !ok {
			panic("afk: verdict marker constant does not parse under its own grammar")
		}
		words[w] = true
	}
	return words
}()

// LanderDone derives a lander run's done-signal from polled forge state
// (issue #181 / ADR-0048) — the counterpart of the AFK kinds' PR-presence
// reading, needed because a lander's PR pre-exists the run: presence alone
// would success-reap it on its first tick. Done is the forge-observable state
// the run PRODUCED: the PR merged, or any pass/reject verdict marker on it
// (the spawn rule only launches onto a virgin PR, so such a marker is this
// run's). prPresent/pullState carry tracker.DonePull's reading for the run's
// head branch; verdicts are the marker words VerdictWords extracted from the
// PR's comments. No PR — vanished, or closed-unmerged — is never done: the
// run ends in death/timeout like an AFK run whose PR never lands.
func LanderDone(pullState string, prPresent bool, verdicts []string) bool {
	if !prPresent {
		return false
	}
	if pullState == tracker.PullMerged {
		return true
	}
	for _, w := range verdicts {
		if landerDoneWords[w] {
			return true
		}
	}
	return false
}

// VerdictWords extracts the verdict-marker words from a PR's comment thread,
// in comment order: every body whose FIRST line parses under the marker
// grammar (tracker.ParseVerdict — exact prefix, first line only) contributes
// its word, which may be empty or unknown (the parser judges the grammar, not
// the vocabulary). LanderDone reads the words for the done-signal; the
// autoland poller treats ANY entry as existing verdict state (a non-virgin
// PR, #181's poller never spawns on one).
func VerdictWords(comments []tracker.Comment) []string {
	var words []string
	for _, c := range comments {
		if w, ok := tracker.ParseVerdict(c.Body); ok {
			words = append(words, w)
		}
	}
	return words
}

// LiveReview reports whether any non-dismissed review exists — the poller's
// conservative "no review" gate (issue #181: ANY live review suppresses a
// lander spawn; the ADR-0048 hybrid rejected-state fold is #182's). A
// dismissed review is a superseded verdict, not a live one.
func LiveReview(reviews []tracker.Review) bool {
	for _, r := range reviews {
		if !r.Dismissed {
			return true
		}
	}
	return false
}

// SpawnStage is a spawn candidate's pipeline rank (issue #185) — the ONE
// sort key of the fleet spawn pass. Lower means further down the pipeline
// and launches first: drain the pipeline before filling it. A lander
// finishes work already claimed and PR'd; a fix run repairs work already
// validated-and-rejected; new AFK work fills the pipeline from the top.
// Without the ordering a repo at cap could spend every slot opening new PRs
// while validated-but-unlanded ones queued behind them. Priority is DATA on
// the candidate, not control flow spread across loops — that is what makes
// #182 an addition (a producer emitting StageFix) instead of a third racer.
type SpawnStage int

const (
	StageLander  SpawnStage = iota // validate an open claim PR — furthest down the pipeline
	StageFix                       // repair a rejected claim PR — issue #182's reserved rank; no producer emits it yet
	StageNewWork                   // start a fresh ready issue — fills the pipeline
)

// AutoDecision holds the facts the auto-launch predicate weighs for one repo
// on one spawn pass (v0 afkAutoDecision minus its under-cap term — issue
// #185 moved cap enforcement into the single spawn pass, the cap's sole
// consumer, so no per-repo predicate weighs it any more).
type AutoDecision struct {
	AutoEnabled  bool // the per-repo toggle is on
	AutoInFlight bool // this repo already has an auto run live
	ReadyExists  bool // a CLAIMABLE ready-for-agent issue exists (not raw ready)
	Paused       bool // consecutive_failures >= PauseThreshold
}

// ShouldLaunchAuto decides whether a repo yields a new-work spawn candidate
// this pass (v0 shouldLaunchAuto, cap term relocated to the spawn pass —
// #185): toggle on, no auto run already in flight, a claimable issue
// waiting, not paused. Every remaining term is load-bearing — flipping
// exactly one blocks the launch.
func ShouldLaunchAuto(d AutoDecision) bool {
	return d.AutoEnabled && !d.AutoInFlight && d.ReadyExists && !d.Paused
}

// LanderSpawnDecision holds the facts the lander-spawn predicate weighs for
// one pull on one spawn pass (issue #181 / ADR-0048) — AutoDecision's
// sibling: landerCandidates gathers the facts, this decides. The repo half
// (enabled/binding/ready/paused) and the pull half (open/claim-branch/
// review/verdict/run) live in one struct so the whole spawn rule is one
// table-tested predicate. Like AutoDecision, the under-cap term is gone
// (issue #185): cap enforcement moved to the single spawn pass.
type LanderSpawnDecision struct {
	AutolandEnabled bool // the per-repo opt-in is on (default off — never by upgrade)
	ForgeBound      bool // tracker_binding is forge — builtin has no PR comments to poll (ADR-0048)
	RepoReady       bool // clone_status is ready
	Paused          bool // consecutive_failures >= PauseThreshold (three-strikes pauses lander spawns too)
	PullOpen        bool // the pull is state open (a merged or closed PR needs no validation)
	ClaimBranch     bool // head matches the repo's claim-branch pattern — human-branch PRs are never touched
	ReviewPresent   bool // any non-dismissed native review exists (conservative "no review"; the hybrid fold is #182's)
	VerdictPresent  bool // any verdict-marker comment exists, ANY word — verdict state means the PR is not virgin
	RunOnBranch     bool // an active run of ANY kind works the branch (the authoring AFK run idling, or a lander already on it)
}

// ShouldSpawnLander decides whether a pull yields a lander spawn candidate
// this pass (cap term relocated to the spawn pass — #185): opted in,
// forge-bound, ready, not paused, and the pull is an open, review-less,
// verdict-less claim PR nobody is working. Every remaining term is
// load-bearing — flipping exactly one blocks the spawn.
func ShouldSpawnLander(d LanderSpawnDecision) bool {
	return d.AutolandEnabled && d.ForgeBound && d.RepoReady && !d.Paused &&
		d.PullOpen && d.ClaimBranch && !d.ReviewPresent && !d.VerdictPresent && !d.RunOnBranch
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

// BranchToken is the rendered-claim-branch placeholder in a seed-prompt
// template (issue #52 / ADR-0027), the sibling of gitx.NToken. Unlike a branch
// PATTERN's <N> — which must appear exactly once and is rendered with
// strings.Replace — a seed-prompt token is interpolated at EVERY occurrence
// (strings.ReplaceAll): a template may name the branch any number of times, or
// none at all (a token-less override is legal), and an unknown token like
// <FOO> passes through as literal text.
const BranchToken = "<BRANCH>"

// SeedPromptTemplate is the built-in AFK seed-prompt template with LITERAL
// gitx.NToken (<N>) and BranchToken (<BRANCH>) placeholders — the one-issue
// instruction set delivered to a just-spawned AFK session (brief §8.4, adapted
// from v0 afkSeedPrompt: read #N, stay on the run branch, implement, verify,
// commit, push, open a PR with `Closes #N`, then stop). The tracker surface is
// labctl ONLY (D10 — tea/gh are gone); the branch is the repo's rendered claim
// branch, never a literal prefix. The incogni variant appends the attribution
// sentence to the commit step (D15 §9 measure 2; reworded from the M7-final
// text by ADR-0033 so the sentence itself spells no attribution-marker token —
// the core-neutrality arch test scans every core literal, prompts included). This
// exact text (non-incogni, tokens un-interpolated) is what the settings/repo
// API serves as the default/effective preview an operator's afk_prompt override
// would replace.
func SeedPromptTemplate(incogni bool) string {
	commit := "5. Commit in Conventional Commits style."
	if incogni {
		commit += " No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links."
	}
	return strings.Join([]string{
		"You are an autonomous AFK run. Resolve exactly one issue, open a pull request, and stop.",
		"",
		"1. Run `labctl issue view " + gitx.NToken + "` and read it fully, including comments.",
		"2. Work only on branch `" + BranchToken + "` in this worktree; never switch branches.",
		"3. Implement the issue completely, following the repository's own conventions (CLAUDE.md / AGENTS.md).",
		"4. Run the project's tests, build, and linters; fix what you break.",
		commit,
		"6. `git push -u origin " + BranchToken + "`.",
		"7. `labctl pr create --title \"…\" --body \"…\"` — the body must include `Closes #" + gitx.NToken + "`.",
		"8. Then stop working. Do not start unrelated work.",
	}, "\n")
}

// SeedPrompt renders the seed prompt delivered to a just-spawned AFK session.
// A non-empty override (issue #52 / ADR-0027: repos.afk_prompt → global
// afk_prompt setting, resolved by instance.ResolveAFKPrompt) is used as the
// template VERBATIM — it REPLACES the whole built-in, and the incogni
// attribution sentence is NOT re-appended (WYSIWYG: incogni stays mechanically
// enforced downstream — the pre-push hook, agentapi sanitizeBody, and the
// provider SeedOpts.Incogni — so the prompt text need not restate it). An empty
// override falls back to SeedPromptTemplate(incogni). Either way tokens are then
// interpolated at ALL occurrences: <N> → n, then <BRANCH> → branch. Interpolating
// <N> first is deliberate — the rendered branch is substituted afterward, so a
// literal <N> the branch could never contain is a non-issue and an override's
// text is treated as WYSIWYG. Unknown tokens pass through untouched.
func SeedPrompt(n int, branch string, incogni bool, override string) string {
	tmpl := override
	if tmpl == "" {
		tmpl = SeedPromptTemplate(incogni)
	}
	tmpl = strings.ReplaceAll(tmpl, gitx.NToken, strconv.Itoa(n))
	tmpl = strings.ReplaceAll(tmpl, BranchToken, branch)
	return tmpl
}
