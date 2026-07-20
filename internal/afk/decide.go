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
// still running (the only non-terminal value), or one of the terminal
// outcomes the reaper acts on.
type Outcome int

const (
	OutcomeRunning   Outcome = iota // alive, no PR, under budget — leave it
	OutcomeSuccess                  // an open/merged PR/CR on the run's branch exists
	OutcomeDeath                    // session gone, no done-signal
	OutcomeTimeout                  // alive, no done-signal, at/over budget
	OutcomeEscalated                // an escalate run's terminal marker landed (#182) — promoted from success by classifyAndClaim, never by Classify
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
	case OutcomeEscalated:
		return "escalated"
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
	case OutcomeEscalated:
		return store.RunOutcomeEscalated
	default:
		return ""
	}
}

// failureReason is the runs.failure_reason a terminal outcome records
// (successes record none — escalated included: the hand-off is the escalate
// run doing its job, not a failure).
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
//
// Deliberately kind-blind: it never returns OutcomeEscalated. An escalate run
// whose done-signal arrived via its marker classifies as a plain success here
// and classifyAndClaim promotes it (#182) — the table stays the ONE
// classification rule for every kind.
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

// verdictWord derives a marker constant's word through the parser itself so
// the grammar and every reading built on it can never drift. The panic can
// only fire if a tracker marker constant stops parsing under its own grammar
// — a build defect, never a runtime state.
func verdictWord(marker string) string {
	w, ok := tracker.ParseVerdict(marker)
	if !ok {
		panic("afk: verdict marker constant does not parse under its own grammar")
	}
	return w
}

// The verdict-word vocabulary, derived (never retyped) from the canonical
// markers (tracker/verdict.go) so this package spells no marker word of its
// own.
var (
	verdictWordPass     = verdictWord(tracker.VerdictPass)
	verdictWordReject   = verdictWord(tracker.VerdictReject)
	verdictWordFixDone  = verdictWord(tracker.VerdictFixDone)
	verdictWordEscalate = verdictWord(tracker.VerdictEscalate)
)

// landerDoneWords are the verdict words that END a lander run — pass and
// reject. fix-done is a FIX run's done-signal (FixDone) and never ends a
// lander; escalate is an ESCALATE run's (EscalateDelivered).
var landerDoneWords = map[string]bool{verdictWordPass: true, verdictWordReject: true}

// knownVerdictWords is the complete verdict vocabulary LastVerdictWord folds
// over — every word some run kind's done-signal reads (ADR-0048's grammar:
// pass, reject, fix-done, escalate). A word outside this set is unknown:
// skipped by the fold, though its marker comment still makes the PR
// non-virgin (that reading is VerdictWords' length, never the fold's).
var knownVerdictWords = map[string]bool{
	verdictWordPass:     true,
	verdictWordReject:   true,
	verdictWordFixDone:  true,
	verdictWordEscalate: true,
}

// LastVerdictWord folds a PR's verdict-word sequence (VerdictWords, comment
// order) down to the newest KNOWN word — the marker state every #182
// done-signal and the autoland decision read. Recency is positional: comments
// arrive oldest-first and carry the only ordering there is, so the last known
// word is the newest marker. Unknown and empty words are skipped — a bare or
// future marker must not mask the round state below it — and "" means no
// known word exists. The reading is sound without timestamps because the
// active-run gate (DecideAutoland case 2) admits one marker writer per branch
// at a time: the last word is always the newest round's.
func LastVerdictWord(words []string) string {
	for i := len(words) - 1; i >= 0; i-- {
		if knownVerdictWords[words[i]] {
			return words[i]
		}
	}
	return ""
}

// LanderDone derives a lander run's done-signal from polled forge state
// (issue #181 / ADR-0048) — the counterpart of the AFK kinds' PR-presence
// reading, needed because a lander's PR pre-exists the run: presence alone
// would success-reap it on its first tick. Done is the forge-observable state
// the run PRODUCED: the PR merged, or the LAST verdict word being pass or
// reject. Last-word semantics replace #181's "any pass/reject word" reading
// because #182's re-validation rounds (DecideAutoland case 4) spawn landers
// onto PRs already carrying round 1's stale markers — the any-word reading
// would instant-reap a round-2 lander on round 1's reject before it did a
// thing. The last word at done time is safely this run's OWN: the active-run
// gate serializes marker writers per branch, so nothing else moves the last
// word while a lander is live. Round 1 (a virgin PR at spawn) is unchanged —
// every word on the PR is the run's own under either reading.
// prPresent/pullState carry tracker.DonePull's reading for the run's head
// branch; verdicts are the marker words VerdictWords extracted from the PR's
// comments. No PR — vanished, or closed-unmerged — is never done: the run
// ends in death/timeout like an AFK run whose PR never lands.
func LanderDone(pullState string, prPresent bool, verdicts []string) bool {
	if !prPresent {
		return false
	}
	if pullState == tracker.PullMerged {
		return true
	}
	return landerDoneWords[LastVerdictWord(verdicts)]
}

// FixDone derives a fix run's done-signal (issue #182 / ADR-0048): the PR
// merged — a human merged mid-run, the repair is moot and moot is done — or
// the LAST verdict word is fix-done, meaning the run's `labctl pr rerequest`
// landed. Sound at spawn by the decision table's ordering: a fix run spawns
// only from rejected-state, and rejected-state requires the last word NOT be
// fix-done (case 4, the re-validation lander, outranks case 5 exactly so a
// stale round-N fix-done is consumed before round N+1's fix run exists — see
// DecideAutoland), so this signal is guaranteed false at launch and can never
// instant-reap. Same argument as LanderDone for whose word the last word is:
// the active-run gate makes it this run's own.
func FixDone(pullState string, prPresent bool, verdicts []string) bool {
	if !prPresent {
		return false
	}
	if pullState == tracker.PullMerged {
		return true
	}
	return LastVerdictWord(verdicts) == verdictWordFixDone
}

// EscalateDelivered derives an escalate run's done-signal (issue #182 /
// ADR-0048). Merged first, mirroring LanderDone: a human merged the PR
// mid-hand-off, so the run ends an ordinary success — (true, false). Else the
// LAST verdict word being escalate means the run's terminal `labctl pr
// escalate` marker landed — (true, true), which the reaper maps to outcome
// 'escalated' (store.RunOutcomeEscalated), not 'success': the escalated
// outcome is the poller's permanent-terminality gate, so the distinction must
// survive here. Not delivered → (false, false). Sound at spawn: an escalate
// candidate exists only in rejected-state, and any escalate word on the PR
// would have made it Escalated (DecideAutoland case 1) — the last word at
// spawn is never escalate.
func EscalateDelivered(pullState string, prPresent bool, verdicts []string) (done, viaMarker bool) {
	if !prPresent {
		return false, false
	}
	if pullState == tracker.PullMerged {
		return true, false
	}
	if LastVerdictWord(verdicts) == verdictWordEscalate {
		return true, true
	}
	return false, false
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
// lander spawn, a rule #182 scopes to the VIRGIN case — DecideAutoland case
// 3; the ADR-0048 hybrid rejected-state fold is HumanRejected). A dismissed
// review is a superseded verdict, not a live one.
func LiveReview(reviews []tracker.Review) bool {
	for _, r := range reviews {
		if !r.Dismissed {
			return true
		}
	}
	return false
}

// HumanRejected reports whether any reviewer's LATEST verdict-bearing,
// non-dismissed native review requests changes — the human half of ADR-0048's
// hybrid rejected-state. The fold is per reviewer and mirrors forgejo's
// changesRequestedReviewers (the rerequest ping's fold — mirrored over the
// normalized []tracker.Review, never imported) so the two readings of
// "outstanding human rejection" cannot disagree: tracker.Review carries no
// timestamp, but Reviews() returns oldest-first, so positional order IS
// recency — the last verdict seen per reviewer is their latest. Only
// ReviewApproved and ReviewChangesRequested rows carry a verdict (the forge's
// own rule: nothing else is a verdict event), so a later comment or
// re-request row neither sets nor clears a reviewer's standing verdict — a
// reviewer who requested changes and then replied in the thread is STILL
// requesting changes. A dismissed row is skipped (the forge cleared it), and
// so is a row without a Reviewer login — the per-reviewer fold has no key for
// it (defensive; Reviews() always fills the login). A human rejection is
// cleared ONLY by that human's newer approval or a dismissal — never by a fix
// run's fix-done marker (the ADR-0048 ownership rule; this fold reads markers
// not at all).
func HumanRejected(reviews []tracker.Review) bool {
	latest := make(map[string]string, len(reviews))
	for _, r := range reviews {
		if r.Reviewer == "" || r.Dismissed {
			continue
		}
		if r.State != tracker.ReviewApproved && r.State != tracker.ReviewChangesRequested {
			continue // non-verdict row: commented/review_requested/unknown
		}
		latest[r.Reviewer] = r.State
	}
	for _, state := range latest {
		if state == tracker.ReviewChangesRequested {
			return true
		}
	}
	return false
}

// PullVerdictState is DecideAutoland's folded per-PR input — every field a
// pure fact the poller already gathered from the PR's comment thread, its
// native reviews, and the runs store. The repo-half preconditions (enabled,
// forge-bound, ready, unpaused) and the pull-half ones (open, claim branch)
// stay the caller's, unchanged from #181.
type PullVerdictState struct {
	Words          []string // VerdictWords in comment order (oldest first)
	LiveReview     bool     // any non-dismissed native review — the conservative virgin gate (LiveReview)
	HumanRejected  bool     // some reviewer's latest live verdict requests changes (HumanRejected)
	Escalated      bool     // escalate word present (ANY position) OR an escalated-outcome run exists — the caller ORs both sources, because a human can delete the marker comment but never the run row
	RunOnBranch    bool     // an active run of ANY kind works the branch
	FixSpawns      int      // fix launches EVER attempted for this branch (autoland_attempts) — intents, never rejection verdicts and never surviving runs rows: a fix run that dies on the launch pad still burns an attempt (ADR-0048)
	MaxFixAttempts int      // the repo's per-branch fix-attempt bound (repos.max_fix_attempts)
	EscalateSpawns int      // escalate launches EVER attempted for this branch (autoland_attempts), bounded by MaxEscalateAttempts
}

// MaxEscalateAttempts bounds escalate launches per branch (issue #182).
//
// The escalate arm needs a bound of its own, and it is not optional. Its
// terminality is written by the reaper only once the escalate run POSTS its
// marker; an escalate run that dies or times out first leaves no 'escalated'
// row, so the next pass re-derives the identical rejected-at-bound state and
// escalates again. Nothing else brakes that loop: reapRun excludes escalate
// from the consecutive-failure counter in both directions, and the candidate
// sorts at StageFix — AHEAD of new AFK work under drain-before-fill — so a
// single wedged branch would starve its repo's new-work pipeline indefinitely.
//
// Three, because escalation is one comment, one label flip and one marker: if
// three separate runs cannot land that, the failure is systemic and retrying
// is not what fixes it. Past the bound the poller goes quiet on the PR (and
// says so at error level) rather than spinning — the PR keeps its rejected
// state and its issue keeps ready-for-agent, so a human running the land-pr
// skill still has every path in.
const MaxEscalateAttempts = 3

// AutolandAction is what the per-PR decision yields: nothing, or exactly one
// spawn candidate. The action names the KIND; its pipeline rank in the spawn
// pass (issue #185) is noted per value — fix and escalate both drain at
// StageFix, because escalation concludes the fix pipeline and must not queue
// behind new work.
type AutolandAction int

const (
	ActionNone     AutolandAction = iota // nothing to spawn for this PR this tick
	ActionLander                         // validate: virgin round 1, or re-validation after fix-done (StageLander)
	ActionFix                            // repair: rejected-state under the attempt bound (StageFix)
	ActionEscalate                       // hand off: rejected-state at the bound (StageFix rank — concludes the fix pipeline)
)

// DecideAutoland is the per-PR autoland decision (issue #182 / ADR-0048),
// pure over PullVerdictState — the fix-forward loop's whole spawn rule in one
// table-tested function. Priority order, every case load-bearing:
//
//  1. Escalated → nothing, forever. Terminal by either source (marker or
//     escalated-outcome run); re-entry is exactly one path — a human running
//     the interactive land-pr skill.
//  2. An active run on the branch (any kind) → nothing this tick. This gate
//     is what serializes marker writers per branch; every last-word reading
//     in the done-signals leans on it.
//  3. Virgin (no verdict markers at all AND no live native review) → lander:
//     #181's round 1, unchanged — any live review suppresses here, the
//     conservative virgin gate.
//  4. Last word fix-done → lander: the re-validation round. An outstanding
//     human changes-requested does NOT suppress it — the round must run to
//     move the marker state — but forces approveOnly (never merge over a
//     live human rejection, ADR-0048's ownership rule: only that human's
//     newer review clears it). This deliberately refines #181's "any live
//     review suppresses" rule, which #181 itself scoped to the virgin case.
//  5. Rejected-state — last word reject, OR a live human changes-requested —
//     → fix while FixSpawns < MaxFixAttempts, else escalate.
//  6. Anything else (pass with no live rejection, unknown words only) →
//     nothing.
//
// Why case 4 outranks 5: if a human rejects while the PR sits at fix-done
// awaiting re-validation, spawning a fix run would instant-reap — its
// done-signal, fix-done as the last word, is already true at spawn. The
// re-validation lander runs first, moves the last word to pass/reject, and
// THEN case 5 fires with a sound done-signal. This ordering is what makes
// marker order sufficient without comment timestamps. approveOnly is
// meaningful only with ActionLander and false everywhere else.
func DecideAutoland(st PullVerdictState) (action AutolandAction, approveOnly bool) {
	if st.Escalated {
		return ActionNone, false
	}
	if st.RunOnBranch {
		return ActionNone, false
	}
	if len(st.Words) == 0 && !st.LiveReview {
		return ActionLander, false
	}
	last := LastVerdictWord(st.Words)
	if last == verdictWordFixDone {
		return ActionLander, st.HumanRejected
	}
	if last == verdictWordReject || st.HumanRejected {
		if st.FixSpawns < st.MaxFixAttempts {
			return ActionFix, false
		}
		// At the fix bound: hand off — but the hand-off is itself bounded,
		// because a dead escalate run writes no terminality (MaxEscalateAttempts).
		if st.EscalateSpawns < MaxEscalateAttempts {
			return ActionEscalate, false
		}
		return ActionNone, false
	}
	return ActionNone, false
}

// RejectionContext renders a fix run's work order — the text FixSeedPrompt
// appends verbatim below its separator: the LATEST reject-marker comment's
// findings (its body below the marker line — the marker is transport, the
// prose is the content), headed "Lander rejection:"; then every outstanding
// human rejection — each reviewer whose latest live verdict is
// changes-requested, the HumanRejected fold — as "Review by <login> (changes
// requested):" plus that review's body, reviewers in first-seen order. The
// lander section renders only while the rejection is the LIVE marker state —
// the last known word is reject: a fix run can also spawn off a HUMAN
// rejection after a later fix-done/pass cleared the lander's (case 5 with
// last == pass), and every reject comment in the thread is then
// already-worked history that would send this run chasing repaired ground.
// Within a live rejection, only the latest reject comment renders, for the
// same staleness reason. A section with no prose is skipped cleanly (a
// bare marker or an empty review body carries no work order), and "" when
// nothing renders — defensive: the decision table only spawns a fix run from
// rejected-state, but the seed must not carry an empty heading if state
// moved between decision and launch.
func RejectionContext(comments []tracker.Comment, reviews []tracker.Review) string {
	var sections []string

	// The lander half exists only while the last known word is reject; the
	// latest reject marker then wins outright — even a bare one supersedes an
	// earlier round's findings (they are already worked).
	findings := ""
	if words := VerdictWords(comments); LastVerdictWord(words) == verdictWordReject {
		for _, c := range comments {
			if w, ok := tracker.ParseVerdict(c.Body); ok && w == verdictWordReject {
				_, rest, _ := strings.Cut(c.Body, "\n")
				findings = strings.TrimSpace(rest)
			}
		}
	}
	if findings != "" {
		sections = append(sections, "Lander rejection:\n"+findings)
	}

	// Per-reviewer fold, HumanRejected's rules, keeping the winning row so
	// its body can be quoted; first-seen reviewer order like forgejo's fold.
	latest := make(map[string]tracker.Review, len(reviews))
	order := make([]string, 0, len(reviews))
	for _, r := range reviews {
		if r.Reviewer == "" || r.Dismissed {
			continue
		}
		if r.State != tracker.ReviewApproved && r.State != tracker.ReviewChangesRequested {
			continue
		}
		if _, seen := latest[r.Reviewer]; !seen {
			order = append(order, r.Reviewer)
		}
		latest[r.Reviewer] = r
	}
	for _, login := range order {
		r := latest[login]
		if r.State != tracker.ReviewChangesRequested {
			continue
		}
		if body := strings.TrimSpace(r.Body); body != "" {
			sections = append(sections, "Review by "+login+" (changes requested):\n"+body)
		}
	}
	return strings.Join(sections, "\n\n")
}

// EscalationHistory renders the full round narrative an escalate run digests
// from — the text EscalateSeedPrompt appends verbatim below its separator:
// every verdict-marker comment in comment order, full body INCLUDING the
// marker line (unlike RejectionContext this is history, not a work order —
// which round said what is exactly the point), then every review with its
// reviewer, state, and dismissal flag (a dismissed rejection is part of the
// story even though it no longer binds; the digest explains rounds, so
// nothing is folded away). Non-marker comments stay out — thread prose is
// not round state. "" when there is nothing to tell — defensive; an
// escalate run only spawns after rejected rounds exist.
func EscalationHistory(comments []tracker.Comment, reviews []tracker.Review) string {
	var sections []string

	var markers []string
	for _, c := range comments {
		if _, ok := tracker.ParseVerdict(c.Body); ok {
			markers = append(markers, strings.TrimSpace(c.Body))
		}
	}
	if len(markers) > 0 {
		sections = append(sections, "Verdict comments (oldest first):\n\n"+strings.Join(markers, "\n\n"))
	}

	var revs []string
	for _, r := range reviews {
		head := "Review by " + r.Reviewer + " (" + r.State
		if r.Dismissed {
			head += ", dismissed"
		}
		head += "):"
		if body := strings.TrimSpace(r.Body); body != "" {
			head += "\n" + body
		}
		revs = append(revs, head)
	}
	if len(revs) > 0 {
		sections = append(sections, "Reviews (oldest first):\n\n"+strings.Join(revs, "\n\n"))
	}
	return strings.Join(sections, "\n\n")
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
	StageFix                       // repair a rejected claim PR — fix runs AND the escalate runs that conclude the fix pipeline (issue #182)
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
