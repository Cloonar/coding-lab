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

// TestShouldLaunchAuto transcribes the complete v0 table (port spec §2.6):
// base is all-go; flipping exactly one term blocks the launch.
func TestShouldLaunchAuto(t *testing.T) {
	base := AutoDecision{AutoEnabled: true, UnderCap: true, AutoInFlight: false, ReadyExists: true, Paused: false}
	tests := []struct {
		name   string
		mutate func(*AutoDecision)
		want   bool
	}{
		{"all conditions go", func(*AutoDecision) {}, true},
		{"toggle off vetoes", func(d *AutoDecision) { d.AutoEnabled = false }, false},
		{"at cap vetoes", func(d *AutoDecision) { d.UnderCap = false }, false},
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
	rejects := []string{"afk-x", "afk-feature", "afk-0", "afk--1", "afk-1x", "afk-auto-", "afk-auto-0", "afk-auto-x", "", "20260706-1200", "lab-login", "auto-7"}
	for _, label := range rejects {
		if _, _, ok := ParseLabel(label); ok {
			t.Errorf("ParseLabel(%q) accepted, want reject", label)
		}
	}
}

// TestSeedPrompt pins the §8.4 template: every ADR-0007 contract element,
// labctl-only vocabulary (never tea/gh), the repo's rendered branch (never a
// literal afk/), and the incogni sentence only on incogni repos.
func TestSeedPrompt(t *testing.T) {
	p := SeedPrompt(63, "issue-63", false)
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

	inc := SeedPrompt(7, "afk/7", true)
	if !strings.Contains(inc, "No AI attribution, no Co-Authored-By, no generated-with footers anywhere.") {
		t.Errorf("incogni seed prompt missing the attribution sentence:\n%s", inc)
	}
	if !strings.Contains(inc, "5. Commit in Conventional Commits style. No AI attribution") {
		t.Errorf("incogni sentence not attached to the commit step:\n%s", inc)
	}
}
