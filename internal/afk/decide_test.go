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
