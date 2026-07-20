package afk

import (
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// t0 anchors the Classify rows: v0's table passed (age, budget) with a 45min
// test budget; the persisted-clock port maps them to (now = t0+age,
// deadline = t0+budget) — `now >= deadline` ⇔ `age >= budget`.
var t0 = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

// TestClassify transcribes the complete v0 TestClassifyAFKRun table (port
// spec §2.4; test budget 45m). Priority order is load-bearing: PR beats
// everything (death and over-budget included), death beats timeout, and the
// budget boundary is inclusive (>=, not >).
func TestClassify(t *testing.T) {
	const budget = 45 * time.Minute
	deadline := t0.Add(budget)
	tests := []struct {
		name         string
		prPresent    bool
		sessionAlive bool
		age          time.Duration
		want         Outcome
	}{
		{"alive under budget no PR is in progress", false, true, 10 * time.Minute, OutcomeRunning},
		{"PR present is success", true, true, 1 * time.Minute, OutcomeSuccess},
		{"PR present beats death", true, false, 1 * time.Minute, OutcomeSuccess},
		{"PR present beats timeout", true, true, 90 * time.Minute, OutcomeSuccess},
		{"dead without PR is death", false, false, 1 * time.Minute, OutcomeDeath},
		{"alive over budget is timeout", false, true, 46 * time.Minute, OutcomeTimeout},
		{"boundary age == budget is timeout", false, true, 45 * time.Minute, OutcomeTimeout},
		{"dead over budget is death not timeout", false, false, 90 * time.Minute, OutcomeDeath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.prPresent, tt.sessionAlive, t0.Add(tt.age), deadline); got != tt.want {
				t.Errorf("Classify(%v, %v, t0+%s, t0+45m) = %v, want %v",
					tt.prPresent, tt.sessionAlive, tt.age, got, tt.want)
			}
		})
	}
}

func TestOutcomeStrings(t *testing.T) {
	tests := []struct {
		o          Outcome
		str        string
		runOutcome string
	}{
		{OutcomeRunning, "in-progress", ""},
		{OutcomeSuccess, "success", "success"},
		{OutcomeDeath, "failure (death)", "death"},
		{OutcomeTimeout, "failure (timeout)", "timeout"},
		{OutcomeEscalated, "escalated", "escalated"},
	}
	for _, tt := range tests {
		if got := tt.o.String(); got != tt.str {
			t.Errorf("%d.String() = %q, want %q", tt.o, got, tt.str)
		}
		if got := tt.o.RunOutcome(); got != tt.runOutcome {
			t.Errorf("%d.RunOutcome() = %q, want %q", tt.o, got, tt.runOutcome)
		}
	}
}

// TestSpawnStageOrder pins the pipeline ranks as a table so #182 cannot
// accidentally reorder them: lower launches first (drain before fill), and
// the fix rank sits strictly between lander and new work.
func TestSpawnStageOrder(t *testing.T) {
	tests := []struct {
		name          string
		lower, higher SpawnStage
	}{
		{"lander outranks fix", StageLander, StageFix},
		{"fix outranks new work", StageFix, StageNewWork},
		{"lander outranks new work", StageLander, StageNewWork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.lower >= tt.higher {
				t.Errorf("stage order broken: %d must sort before %d", tt.lower, tt.higher)
			}
		})
	}
}

// TestShouldLaunchAuto transcribes the v0 table (port spec §2.6) minus its
// cap row — cap enforcement moved to the single spawn pass (#185), so the
// predicate no longer weighs it: base is all-go; flipping exactly one term
// blocks the launch.
func TestShouldLaunchAuto(t *testing.T) {
	base := AutoDecision{AutoEnabled: true, AutoInFlight: false, ReadyExists: true, Paused: false}
	tests := []struct {
		name   string
		mutate func(*AutoDecision)
		want   bool
	}{
		{"all conditions go", func(*AutoDecision) {}, true},
		{"toggle off vetoes", func(d *AutoDecision) { d.AutoEnabled = false }, false},
		{"auto already in flight vetoes", func(d *AutoDecision) { d.AutoInFlight = true }, false},
		{"no ready issue vetoes", func(d *AutoDecision) { d.ReadyExists = false }, false},
		{"paused vetoes", func(d *AutoDecision) { d.Paused = true }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base
			tt.mutate(&d)
			if got := ShouldLaunchAuto(d); got != tt.want {
				t.Errorf("ShouldLaunchAuto(%+v) = %v, want %v", d, got, tt.want)
			}
		})
	}
}

// TestLanderDone pins the lander's done-signal derivation (issue #181 /
// ADR-0048, last-word rework #182): merged ends the run with no comment
// read; on an open PR only the LAST verdict word being pass or reject ends
// it — fix-done is a FIX run's signal, escalate an ESCALATE run's, a bare
// marker or unknown word ends nothing — so a round-2 lander spawned at
// fix-done is NOT instant-reaped by round 1's stale reject; and no PR
// (vanished or closed-unmerged, DonePull's floor) is never done.
func TestLanderDone(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		prPresent bool
		verdicts  []string
		want      bool
	}{
		{"merged is done", tracker.PullMerged, true, nil, true},
		{"open with pass verdict is done", tracker.PullOpen, true, []string{"pass"}, true},
		{"open with reject verdict is done", tracker.PullOpen, true, []string{"reject"}, true},
		{"open with fix-done only is NOT done", tracker.PullOpen, true, []string{"fix-done"}, false},
		{"open with no comments is not done", tracker.PullOpen, true, nil, false},
		{"open with escalate/bare words is not done", tracker.PullOpen, true, []string{"escalate", ""}, false},
		{"pass after fix-done is done", tracker.PullOpen, true, []string{"fix-done", "pass"}, true},
		{"round-2 spawn state is NOT done (stale reject never instant-reaps)", tracker.PullOpen, true, []string{"reject", "fix-done"}, false},
		{"round-2 verdict after stale round 1 is done", tracker.PullOpen, true, []string{"reject", "fix-done", "pass"}, true},
		{"trailing unknown word does not mask the last verdict", tracker.PullOpen, true, []string{"pass", "bogus"}, true},
		{"no PR is not done", "", false, []string{"pass"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LanderDone(tt.state, tt.prPresent, tt.verdicts); got != tt.want {
				t.Errorf("LanderDone(%q, %v, %v) = %v, want %v", tt.state, tt.prPresent, tt.verdicts, got, tt.want)
			}
		})
	}
}

// TestLastVerdictWord pins the marker-state fold (issue #182): the fold
// walks backward to the newest KNOWN word — unknown and empty words are
// skipped (their markers still make a PR non-virgin, but that reading is
// VerdictWords' length, never the fold's) — and "" means no known word.
func TestLastVerdictWord(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		want  string
	}{
		{"nil folds to empty", nil, ""},
		{"unknown words only fold to empty", []string{"", "bogus"}, ""},
		{"single known word", []string{"pass"}, "pass"},
		{"last known word wins", []string{"reject", "fix-done"}, "fix-done"},
		{"trailing unknown words are skipped", []string{"reject", "bogus", ""}, "reject"},
		{"escalate is a known word", []string{"reject", "escalate"}, "escalate"},
		{"full round narrative folds to the newest", []string{"reject", "fix-done", "pass"}, "pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LastVerdictWord(tt.words); got != tt.want {
				t.Errorf("LastVerdictWord(%v) = %q, want %q", tt.words, got, tt.want)
			}
		})
	}
}

// TestFixDone pins the fix run's done-signal (issue #182): merged is moot
// and moot is done; else only fix-done as the LAST word — the state a fix
// run actually spawns from (an earlier fix-done superseded by a reject) is
// never done at spawn, which is what makes the signal sound; and no PR is
// never done.
func TestFixDone(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		prPresent bool
		verdicts  []string
		want      bool
	}{
		{"merged is done", tracker.PullMerged, true, nil, true},
		{"rerequest landed is done", tracker.PullOpen, true, []string{"reject", "fix-done"}, true},
		{"spawn state (last word reject) is not done", tracker.PullOpen, true, []string{"fix-done", "reject"}, false},
		{"virgin thread is not done", tracker.PullOpen, true, nil, false},
		{"trailing unknown word does not mask fix-done", tracker.PullOpen, true, []string{"reject", "fix-done", "bogus"}, true},
		{"pass as the last word is not a fix run's done", tracker.PullOpen, true, []string{"pass"}, false},
		{"no PR is never done", "", false, []string{"fix-done"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FixDone(tt.state, tt.prPresent, tt.verdicts); got != tt.want {
				t.Errorf("FixDone(%q, %v, %v) = %v, want %v", tt.state, tt.prPresent, tt.verdicts, got, tt.want)
			}
		})
	}
}

// TestEscalateDelivered pins the escalate run's done-signal (issue #182):
// merged first — a human merged mid-hand-off, an ordinary success, even with
// the marker already posted — else the escalate marker as the LAST word means
// delivered viaMarker (the reaper's outcome 'escalated'); anything else is
// still running.
func TestEscalateDelivered(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		prPresent bool
		verdicts  []string
		wantDone  bool
		wantVia   bool
	}{
		{"merged is success", tracker.PullMerged, true, nil, true, false},
		{"merged beats the marker", tracker.PullMerged, true, []string{"reject", "escalate"}, true, false},
		{"escalate marker delivered", tracker.PullOpen, true, []string{"reject", "escalate"}, true, true},
		{"spawn state (last word reject) is not delivered", tracker.PullOpen, true, []string{"reject"}, false, false},
		{"trailing unknown word does not mask escalate", tracker.PullOpen, true, []string{"escalate", "bogus"}, true, true},
		{"no PR is never delivered", "", false, []string{"escalate"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, via := EscalateDelivered(tt.state, tt.prPresent, tt.verdicts)
			if done != tt.wantDone || via != tt.wantVia {
				t.Errorf("EscalateDelivered(%q, %v, %v) = (%v, %v), want (%v, %v)",
					tt.state, tt.prPresent, tt.verdicts, done, via, tt.wantDone, tt.wantVia)
			}
		})
	}
}

// TestVerdictWords pins the comment→words extraction: first-line exact-prefix
// markers register (their word may be empty or unknown — grammar only), while
// mid-line quotes, second-line markers, and indented markers stay inert
// (ADR-0048's parse rule, via tracker.ParseVerdict).
func TestVerdictWords(t *testing.T) {
	comments := []tracker.Comment{
		{Body: tracker.VerdictPass + "\n\nCONCERNS: none"},
		{Body: "prose quoting [autoland] verdict: reject mid-line"},
		{Body: "prose first\n" + tracker.VerdictReject},
		{Body: tracker.VerdictFixDone},
		{Body: tracker.VerdictMarkerPrefix},
		{Body: " " + tracker.VerdictPass},
	}
	got := VerdictWords(comments)
	want := []string{"pass", "fix-done", ""}
	if len(got) != len(want) {
		t.Fatalf("VerdictWords = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("VerdictWords[%d] = %q, want %q (comment order preserved)", i, got[i], want[i])
		}
	}
	if words := VerdictWords(nil); words != nil {
		t.Errorf("VerdictWords(nil) = %v, want nil", words)
	}
}

// TestLiveReview pins the conservative "no review" gate: any non-dismissed
// review suppresses, a dismissed one is a superseded verdict and does not.
func TestLiveReview(t *testing.T) {
	if LiveReview(nil) {
		t.Error("LiveReview(nil) = true, want false")
	}
	if LiveReview([]tracker.Review{{Reviewer: "h", State: tracker.ReviewApproved, Dismissed: true}}) {
		t.Error("a dismissed review reported live")
	}
	if !LiveReview([]tracker.Review{{Reviewer: "h", State: tracker.ReviewCommented}}) {
		t.Error("a non-dismissed review (any state) not reported live")
	}
	if !LiveReview([]tracker.Review{
		{Reviewer: "a", State: tracker.ReviewApproved, Dismissed: true},
		{Reviewer: "b", State: tracker.ReviewChangesRequested},
	}) {
		t.Error("a live review hidden behind a dismissed one")
	}
}

// TestHumanRejected pins the human half of ADR-0048's hybrid rejected-state
// (issue #182): per-reviewer fold, positional recency (Reviews() is
// oldest-first, no timestamps), only approved/changes-requested rows carry a
// verdict, dismissed rows are cleared verdicts, and one outstanding rejection
// among any number of approvers is enough.
func TestHumanRejected(t *testing.T) {
	rev := func(who, state string, dismissed bool) tracker.Review {
		return tracker.Review{Reviewer: who, State: state, Dismissed: dismissed}
	}
	tests := []struct {
		name    string
		reviews []tracker.Review
		want    bool
	}{
		{"no reviews", nil, false},
		{"changes requested binds", []tracker.Review{rev("alice", tracker.ReviewChangesRequested, false)}, true},
		{"approval alone does not", []tracker.Review{rev("alice", tracker.ReviewApproved, false)}, false},
		{"newer approval clears the same reviewer's rejection",
			[]tracker.Review{rev("alice", tracker.ReviewChangesRequested, false), rev("alice", tracker.ReviewApproved, false)}, false},
		{"newer rejection overrides the same reviewer's approval",
			[]tracker.Review{rev("alice", tracker.ReviewApproved, false), rev("alice", tracker.ReviewChangesRequested, false)}, true},
		{"a comment neither sets nor clears a standing rejection",
			[]tracker.Review{rev("alice", tracker.ReviewChangesRequested, false), rev("alice", tracker.ReviewCommented, false)}, true},
		{"a re-request row neither sets nor clears",
			[]tracker.Review{rev("alice", tracker.ReviewChangesRequested, false), rev("alice", tracker.ReviewRequested, false)}, true},
		{"a dismissed rejection does not bind", []tracker.Review{rev("alice", tracker.ReviewChangesRequested, true)}, false},
		{"one rejecting reviewer among approvers",
			[]tracker.Review{rev("alice", tracker.ReviewApproved, false), rev("bob", tracker.ReviewChangesRequested, false)}, true},
		{"a non-verdict-only thread does not bind", []tracker.Review{rev("alice", tracker.ReviewCommented, false)}, false},
		{"anonymous rows are skipped", []tracker.Review{rev("", tracker.ReviewChangesRequested, false)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HumanRejected(tt.reviews); got != tt.want {
				t.Errorf("HumanRejected(%v) = %v, want %v", tt.reviews, got, tt.want)
			}
		})
	}
}

// TestDecideAutoland transcribes the #182 decision table — priority order
// exactly as pinned: escalated beats everything, an active run beats
// spawning, the virgin lander keeps #181's conservative any-review gate, the
// fix-done re-validation lander outranks rejected-state (the instant-reap
// trap: a fix run spawned at fix-done would be born done), rejected-state
// spawns a fix run under the bound and escalates at it, and everything else
// is nothing.
func TestDecideAutoland(t *testing.T) {
	tests := []struct {
		name        string
		st          PullVerdictState
		wantAction  AutolandAction
		wantApprove bool
	}{
		{"virgin PR spawns a lander",
			PullVerdictState{MaxFixAttempts: 2}, ActionLander, false},
		{"escalated beats everything",
			PullVerdictState{Words: []string{"reject"}, LiveReview: true, HumanRejected: true, Escalated: true, MaxFixAttempts: 2}, ActionNone, false},
		{"escalated blocks even a virgin lander (marker deleted, run row remains)",
			PullVerdictState{Escalated: true, MaxFixAttempts: 2}, ActionNone, false},
		{"active run on the branch beats spawning",
			PullVerdictState{RunOnBranch: true, MaxFixAttempts: 2}, ActionNone, false},
		{"active run blocks a fix spawn too",
			PullVerdictState{Words: []string{"reject"}, RunOnBranch: true, MaxFixAttempts: 2}, ActionNone, false},
		{"live review on a virgin PR suppresses the lander",
			PullVerdictState{LiveReview: true, MaxFixAttempts: 2}, ActionNone, false},
		{"human rejection on a virgin PR spawns a fix run",
			PullVerdictState{LiveReview: true, HumanRejected: true, MaxFixAttempts: 2}, ActionFix, false},
		{"fix-done spawns the re-validation lander",
			PullVerdictState{Words: []string{"reject", "fix-done"}, MaxFixAttempts: 2}, ActionLander, false},
		{"fix-done under a human rejection is an approve-only lander, never a fix run (the instant-reap trap)",
			PullVerdictState{Words: []string{"reject", "fix-done"}, LiveReview: true, HumanRejected: true, MaxFixAttempts: 2}, ActionLander, true},
		{"reject under the bound spawns a fix run",
			PullVerdictState{Words: []string{"reject"}, FixSpawns: 1, MaxFixAttempts: 2}, ActionFix, false},
		{"reject at the bound escalates",
			PullVerdictState{Words: []string{"reject"}, FixSpawns: 2, MaxFixAttempts: 2}, ActionEscalate, false},
		{"human rejection at the bound escalates",
			PullVerdictState{Words: []string{"pass"}, LiveReview: true, HumanRejected: true, FixSpawns: 2, MaxFixAttempts: 2}, ActionEscalate, false},
		{"reject at BOTH bounds is nothing — the hand-off is bounded too, or a dead escalate run respawns forever",
			PullVerdictState{Words: []string{"reject"}, FixSpawns: 2, MaxFixAttempts: 2, EscalateSpawns: MaxEscalateAttempts}, ActionNone, false},
		{"reject at the fix bound with the last escalate attempt left still escalates",
			PullVerdictState{Words: []string{"reject"}, FixSpawns: 2, MaxFixAttempts: 2, EscalateSpawns: MaxEscalateAttempts - 1}, ActionEscalate, false},
		{"max_fix_attempts 0 escalates on the first rejection, still under its own bound",
			PullVerdictState{Words: []string{"reject"}, FixSpawns: 0, MaxFixAttempts: 0}, ActionEscalate, false},
		{"human rejection after a pass spawns a fix run",
			PullVerdictState{Words: []string{"pass"}, LiveReview: true, HumanRejected: true, MaxFixAttempts: 2}, ActionFix, false},
		{"pass with no rejection is nothing",
			PullVerdictState{Words: []string{"pass"}, MaxFixAttempts: 2}, ActionNone, false},
		{"unknown words only is nothing (non-virgin, no known state)",
			PullVerdictState{Words: []string{"bogus", ""}, MaxFixAttempts: 2}, ActionNone, false},
		{"trailing unknown word does not mask fix-done",
			PullVerdictState{Words: []string{"reject", "fix-done", "bogus"}, MaxFixAttempts: 2}, ActionLander, false},
		{"a zero-attempt bound escalates on the first rejection",
			PullVerdictState{Words: []string{"reject"}, MaxFixAttempts: 0}, ActionEscalate, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, approve := DecideAutoland(tt.st)
			if action != tt.wantAction || approve != tt.wantApprove {
				t.Errorf("DecideAutoland(%+v) = (%v, %v), want (%v, %v)",
					tt.st, action, approve, tt.wantAction, tt.wantApprove)
			}
		})
	}
}

// TestRejectionContext pins the fix run's work order rendering: the LATEST
// reject comment's prose with the marker line stripped, then each reviewer
// whose latest live verdict is changes-requested with that review's body —
// superseded, dismissed, and approving rows excluded, empty sections skipped
// cleanly, "" when nothing renders.
func TestRejectionContext(t *testing.T) {
	comments := []tracker.Comment{
		{Body: "ordinary thread prose"},
		{Body: tracker.VerdictReject + "\n\nround-1 findings"},
		{Body: tracker.VerdictFixDone},
		{Body: tracker.VerdictReject + "\n\nround-2 findings:\n- test X fails"},
	}
	reviews := []tracker.Review{
		{Reviewer: "alice", State: tracker.ReviewChangesRequested, Body: "please split this function"},
		{Reviewer: "bob", State: tracker.ReviewApproved, Body: "lgtm"},
	}
	got := RejectionContext(comments, reviews)
	want := "Lander rejection:\nround-2 findings:\n- test X fails\n\n" +
		"Review by alice (changes requested):\nplease split this function"
	if got != want {
		t.Errorf("RejectionContext =\n%q\nwant\n%q", got, want)
	}

	// Per-reviewer recency: a newer approval clears that reviewer's section;
	// a newer rejection's body supersedes the older one's.
	if got := RejectionContext(nil, []tracker.Review{
		{Reviewer: "alice", State: tracker.ReviewChangesRequested, Body: "old findings"},
		{Reviewer: "alice", State: tracker.ReviewApproved},
	}); got != "" {
		t.Errorf("cleared rejection still rendered: %q", got)
	}
	if got := RejectionContext(nil, []tracker.Review{
		{Reviewer: "alice", State: tracker.ReviewChangesRequested, Body: "first-pass findings"},
		{Reviewer: "alice", State: tracker.ReviewChangesRequested, Body: "second-pass findings"},
	}); got != "Review by alice (changes requested):\nsecond-pass findings" {
		t.Errorf("latest rejection body not used: %q", got)
	}

	// Dismissed rows and empty bodies render no section; a bare reject marker
	// (no prose below the marker line) renders none either.
	if got := RejectionContext(
		[]tracker.Comment{{Body: tracker.VerdictReject}},
		[]tracker.Review{
			{Reviewer: "alice", State: tracker.ReviewChangesRequested, Body: "stale", Dismissed: true},
			{Reviewer: "bob", State: tracker.ReviewChangesRequested, Body: "   "},
		},
	); got != "" {
		t.Errorf("empty sections not skipped: %q", got)
	}
	if got := RejectionContext(nil, nil); got != "" {
		t.Errorf("RejectionContext(nil, nil) = %q, want empty", got)
	}

	// A bare LATEST reject supersedes an earlier round's findings — they are
	// already worked, never re-served as a stale work order.
	if got := RejectionContext([]tracker.Comment{
		{Body: tracker.VerdictReject + "\n\nround-1 findings"},
		{Body: tracker.VerdictReject},
	}, nil); got != "" {
		t.Errorf("stale round-1 findings re-served: %q", got)
	}

	// A rejection cleared by a later fix-done/pass renders NO lander section
	// even though its comment survives in the thread: a fix run spawned off a
	// HUMAN rejection after the lander round passed must get only the live
	// human findings, never the already-worked lander round's.
	if got := RejectionContext([]tracker.Comment{
		{Body: tracker.VerdictReject + "\n\nround-1 findings"},
		{Body: tracker.VerdictFixDone},
		{Body: tracker.VerdictPass},
	}, []tracker.Review{
		{Reviewer: "alice", State: tracker.ReviewChangesRequested, Body: "ship without the debug flag"},
	}); got != "Review by alice (changes requested):\nship without the debug flag" {
		t.Errorf("cleared lander rejection re-served beside the live human findings: %q", got)
	}
}

// TestEscalationHistory pins the round narrative rendering: every
// verdict-marker comment in order with its FULL body (marker line included —
// which round said what is the point), every review with reviewer, state,
// and dismissal flag (dismissed rows are history too), non-marker thread
// prose excluded, and "" on empty inputs.
func TestEscalationHistory(t *testing.T) {
	comments := []tracker.Comment{
		{Body: "plain thread prose"},
		{Body: tracker.VerdictReject + "\n\nround-1 findings"},
		{Body: tracker.VerdictFixDone},
		{Body: tracker.VerdictReject + "\n\nround-2 findings"},
	}
	reviews := []tracker.Review{
		{Reviewer: "alice", State: tracker.ReviewChangesRequested, Body: "split this"},
		{Reviewer: "alice", State: tracker.ReviewApproved, Dismissed: true},
	}
	got := EscalationHistory(comments, reviews)
	want := "Verdict comments (oldest first):\n\n" +
		tracker.VerdictReject + "\n\nround-1 findings\n\n" +
		tracker.VerdictFixDone + "\n\n" +
		tracker.VerdictReject + "\n\nround-2 findings\n\n" +
		"Reviews (oldest first):\n\n" +
		"Review by alice (changes_requested):\nsplit this\n\n" +
		"Review by alice (approved, dismissed):"
	if got != want {
		t.Errorf("EscalationHistory =\n%q\nwant\n%q", got, want)
	}

	// One-sided inputs render their one section, with no dangling separator.
	if got := EscalationHistory(comments[:2], nil); got != "Verdict comments (oldest first):\n\n"+tracker.VerdictReject+"\n\nround-1 findings" {
		t.Errorf("comments-only history = %q", got)
	}
	if got := EscalationHistory(nil, reviews[:1]); got != "Reviews (oldest first):\n\nReview by alice (changes_requested):\nsplit this" {
		t.Errorf("reviews-only history = %q", got)
	}
	if got := EscalationHistory(nil, nil); got != "" {
		t.Errorf("EscalationHistory(nil, nil) = %q, want empty", got)
	}
}

func issues(ns ...int) []tracker.Issue {
	out := make([]tracker.Issue, 0, len(ns))
	for _, n := range ns {
		out = append(out, tracker.Issue{Number: n})
	}
	return out
}

// TestClaimableIssues transcribes the complete v0 table (port spec §2.3;
// ready = [7,8,9]) plus the pickLowestIssue assertion from the same test.
func TestClaimableIssues(t *testing.T) {
	ready := issues(7, 8, 9)
	tests := []struct {
		name    string
		claimed map[int]bool
		want    []int
	}{
		{"nil claimed keeps all", nil, []int{7, 8, 9}},
		{"empty claimed keeps all", map[int]bool{}, []int{7, 8, 9}},
		{"middle claim drained around", map[int]bool{8: true}, []int{7, 9}},
		{"all claimed is empty", map[int]bool{7: true, 8: true, 9: true}, []int{}},
		{"unrelated claim ignored", map[int]bool{42: true}, []int{7, 8, 9}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClaimableIssues(ready, tt.claimed)
			if got == nil {
				t.Fatal("ClaimableIssues returned nil, want a non-nil slice")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ClaimableIssues = %v, want numbers %v", got, tt.want)
			}
			for i := range got {
				if got[i].Number != tt.want[i] {
					t.Errorf("ClaimableIssues[%d] = #%d, want #%d (order preserved)", i, got[i].Number, tt.want[i])
				}
			}
		})
	}

	// pickLowestIssue over a drained queue: parked lowest skipped, next
	// lowest taken (the v0 TestClaimableIssues final assertion).
	got, ok := PickLowestIssue(ClaimableIssues(ready, map[int]bool{7: true}))
	if !ok || got.Number != 8 {
		t.Errorf("PickLowestIssue(claimable minus 7) = (#%d, %v), want (#8, true)", got.Number, ok)
	}
}

func TestPickLowestIssue(t *testing.T) {
	if _, ok := PickLowestIssue(nil); ok {
		t.Error("PickLowestIssue(empty) reported ok")
	}
	// The minimum is computed, never trusted from list order.
	got, ok := PickLowestIssue(issues(12, 7, 9))
	if !ok || got.Number != 7 {
		t.Errorf("PickLowestIssue([12,7,9]) = (#%d, %v), want (#7, true)", got.Number, ok)
	}
}

// TestClaimedIssues covers the claim oracle across branch patterns — the v0
// TestParseAFKBranch reject rows under "afk/<N>" plus the design §4a
// requirement that "issue-<N>" rows behave identically (nothing assumes the
// literal "afk/").
func TestClaimedIssues(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		branches []string
		want     map[int]bool
	}{
		{
			"afk pattern accepts exact renderings only",
			"afk/<N>",
			[]string{"afk/7", "afk/63", "afk/", "afk/7/x", "afk/x", "feature/7", "main", "", "afk/007", "afk/-3", "afk/0"},
			map[int]bool{7: true, 63: true},
		},
		{
			"issue pattern behaves identically",
			"issue-<N>",
			[]string{"issue-7", "issue-63", "issue-", "issue-7x", "issue-007", "issue-0", "afk/7", "main"},
			map[int]bool{7: true, 63: true},
		},
		{
			"no branches no claims",
			"afk/<N>",
			nil,
			map[int]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClaimedIssues(tt.branches, tt.pattern)
			if len(got) != len(tt.want) {
				t.Fatalf("ClaimedIssues = %v, want %v", got, tt.want)
			}
			for n := range tt.want {
				if !got[n] {
					t.Errorf("issue #%d not claimed; got %v", n, got)
				}
			}
		})
	}
}

// TestLabelRoundTrip pins the AFK label grammar (port spec §2.8): both kinds
// render and parse, and every reject row stays rejected — a user label
// starting with "afk-" must never register as an AFK run.
func TestLabelRoundTrip(t *testing.T) {
	if got := Label(7, false); got != "afk-7" {
		t.Errorf("Label(7, manual) = %q, want afk-7", got)
	}
	if got := Label(7, true); got != "afk-auto-7" {
		t.Errorf("Label(7, auto) = %q, want afk-auto-7", got)
	}
	for _, n := range []int{1, 7, 88, 12345} {
		for _, auto := range []bool{false, true} {
			gotN, gotAuto, ok := ParseLabel(Label(n, auto))
			if !ok || gotN != n || gotAuto != auto {
				t.Errorf("ParseLabel(Label(%d, %v)) = (%d, %v, %v), want round-trip", n, auto, gotN, gotAuto, ok)
			}
		}
	}
	rejects := []string{"afk-x", "afk-feature", "afk-0", "afk--1", "afk-1x", "afk-auto-", "afk-auto-0", "afk-auto-x", "", "20260706-1200", "lab-login", "auto-7", "lander-7"}
	for _, label := range rejects {
		if _, _, ok := ParseLabel(label); ok {
			t.Errorf("ParseLabel(%q) accepted, want reject", label)
		}
	}
}

// wantSeedDefaultNonIncogni is the byte-exact built-in seed prompt for
// SeedPrompt(63, "issue-63", false, "") — the pinned §8.4 template with <N> and
// <BRANCH> interpolated. Any drift here is a contract change (compat snapshot).
const wantSeedDefaultNonIncogni = "You are an autonomous AFK run. Resolve exactly one issue, open a pull request, and stop.\n" +
	"\n" +
	"1. Run `labctl issue view 63` and read it fully, including comments.\n" +
	"2. Work only on branch `issue-63` in this worktree; never switch branches.\n" +
	"3. Implement the issue completely, following the repository's own conventions (CLAUDE.md / AGENTS.md).\n" +
	"4. Run the project's tests, build, and linters; fix what you break.\n" +
	"5. Commit in Conventional Commits style.\n" +
	"6. `git push -u origin issue-63`.\n" +
	"7. `labctl pr create --title \"…\" --body \"…\"` — the body must include `Closes #63`.\n" +
	"8. Then stop working. Do not start unrelated work."

// wantSeedDefaultIncogni is the byte-exact built-in for SeedPrompt(7, "afk/7",
// true, "") — identical but for the branch/number and the attribution sentence
// appended to the commit step.
const wantSeedDefaultIncogni = "You are an autonomous AFK run. Resolve exactly one issue, open a pull request, and stop.\n" +
	"\n" +
	"1. Run `labctl issue view 7` and read it fully, including comments.\n" +
	"2. Work only on branch `afk/7` in this worktree; never switch branches.\n" +
	"3. Implement the issue completely, following the repository's own conventions (CLAUDE.md / AGENTS.md).\n" +
	"4. Run the project's tests, build, and linters; fix what you break.\n" +
	"5. Commit in Conventional Commits style. No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links.\n" +
	"6. `git push -u origin afk/7`.\n" +
	"7. `labctl pr create --title \"…\" --body \"…\"` — the body must include `Closes #7`.\n" +
	"8. Then stop working. Do not start unrelated work."

// TestSeedPrompt pins the §8.4 built-in template: the byte-exact output for both
// incogni values (BYTE-IDENTICAL to the pre-#52 SeedPrompt — the override arg
// left ""), plus every ADR-0007 contract element, labctl-only vocabulary (never
// tea/gh), the repo's rendered branch (never a literal afk/), and the incogni
// sentence only on incogni repos.
func TestSeedPrompt(t *testing.T) {
	p := SeedPrompt(63, "issue-63", false, "")
	if p != wantSeedDefaultNonIncogni {
		t.Errorf("non-incogni seed prompt drifted:\n got %q\nwant %q", p, wantSeedDefaultNonIncogni)
	}
	for _, want := range []string{
		"Resolve exactly one issue",
		"`labctl issue view 63`",
		"including comments",
		"branch `issue-63`",
		"never switch branches",
		"tests, build, and linters",
		"Conventional Commits",
		"`git push -u origin issue-63`",
		"labctl pr create",
		"`Closes #63`",
		"8. Then stop working. Do not start unrelated work.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("seed prompt missing %q:\n%s", want, p)
		}
	}
	for _, banned := range []string{"tea ", "`tea", "gh ", "`gh", "afk/"} {
		if strings.Contains(p, banned) {
			t.Errorf("seed prompt for an issue-<N> repo contains banned vocabulary %q:\n%s", banned, p)
		}
	}
	if strings.Contains(p, "Co-Authored-By") {
		t.Error("non-incogni seed prompt carries the incogni sentence")
	}

	inc := SeedPrompt(7, "afk/7", true, "")
	if inc != wantSeedDefaultIncogni {
		t.Errorf("incogni seed prompt drifted:\n got %q\nwant %q", inc, wantSeedDefaultIncogni)
	}
	if !strings.Contains(inc, "No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links.") {
		t.Errorf("incogni seed prompt missing the attribution sentence:\n%s", inc)
	}
	if !strings.Contains(inc, "5. Commit in Conventional Commits style. No AI attribution") {
		t.Errorf("incogni sentence not attached to the commit step:\n%s", inc)
	}
}

// TestSeedPromptTemplate pins that the built-in template carries the LITERAL
// tokens (issue #52 / ADR-0027) — the un-interpolated text the settings/repo API
// serves as the default/effective preview.
func TestSeedPromptTemplate(t *testing.T) {
	for _, incogni := range []bool{false, true} {
		tmpl := SeedPromptTemplate(incogni)
		if !strings.Contains(tmpl, "<N>") {
			t.Errorf("template (incogni=%v) missing literal <N>:\n%s", incogni, tmpl)
		}
		if !strings.Contains(tmpl, "<BRANCH>") {
			t.Errorf("template (incogni=%v) missing literal <BRANCH>:\n%s", incogni, tmpl)
		}
	}
	// The incogni variant is the only one carrying the attribution sentence.
	if strings.Contains(SeedPromptTemplate(false), "No AI attribution") {
		t.Error("non-incogni template carries the incogni sentence")
	}
	if !strings.Contains(SeedPromptTemplate(true), "No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links.") {
		t.Error("incogni template missing the attribution sentence")
	}
}

// TestSeedPromptOverride pins the #52/ADR-0027 override semantics: a non-empty
// override REPLACES the built-in verbatim, tokens interpolate at EVERY occurrence,
// a token-less override passes through unchanged, unknown tokens stay literal, and
// an override on an incogni repo does NOT gain the attribution sentence (WYSIWYG).
func TestSeedPromptOverride(t *testing.T) {
	// Override wins over the built-in; both tokens replaced.
	got := SeedPrompt(42, "afk/42", false, "Fix <N> on <BRANCH>.")
	if want := "Fix 42 on afk/42."; got != want {
		t.Errorf("override = %q, want %q", got, want)
	}
	if strings.Contains(got, "labctl") {
		t.Errorf("override did not replace the built-in: %q", got)
	}

	// Every occurrence of each token is replaced (ReplaceAll, not a single sub).
	got = SeedPrompt(9, "issue-9", false, "<N> <N> <BRANCH> <BRANCH> <N>")
	if want := "9 9 issue-9 issue-9 9"; got != want {
		t.Errorf("multi-occurrence override = %q, want %q", got, want)
	}

	// A token-less override passes through verbatim.
	got = SeedPrompt(1, "afk/1", false, "just do the thing")
	if want := "just do the thing"; got != want {
		t.Errorf("token-less override = %q, want %q", got, want)
	}

	// An unknown token stays literal; the known ones still interpolate.
	got = SeedPrompt(5, "afk/5", false, "<FOO> issue <N> on <BRANCH>")
	if want := "<FOO> issue 5 on afk/5"; got != want {
		t.Errorf("unknown-token override = %q, want %q", got, want)
	}

	// An override on an incogni repo does NOT gain the attribution sentence:
	// incogni is enforced downstream, so the override text is WYSIWYG.
	got = SeedPrompt(3, "issue-3", true, "handle <N>")
	if want := "handle 3"; got != want {
		t.Errorf("incogni override = %q, want %q", got, want)
	}
	if strings.Contains(got, "No AI attribution") {
		t.Errorf("incogni override gained the attribution sentence: %q", got)
	}
}
