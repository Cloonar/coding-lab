package afk

import (
	"regexp"
	"strings"
	"testing"
)

// TestFlowCatalogShape pins the catalog itself (issue #247 / ADR-0062):
// exactly two flows, in the order [autolander, human-triage] — the order IS
// composition order, so a reorder here silently rewrites every existing
// Schedule's composed prompt — with unique, non-empty keys and populated
// operator-facing fields (the multiselect renders Label and Description).
func TestFlowCatalogShape(t *testing.T) {
	got := FlowCatalog()
	wantKeys := []string{"autolander", "human-triage"}
	if len(got) != len(wantKeys) {
		t.Fatalf("FlowCatalog() has %d flows, want %d: %+v", len(got), len(wantKeys), got)
	}
	for i, want := range wantKeys {
		if got[i].Key != want {
			t.Errorf("FlowCatalog()[%d].Key = %q, want %q", i, got[i].Key, want)
		}
	}
	seen := map[string]bool{}
	for i, f := range got {
		if f.Key == "" {
			t.Errorf("FlowCatalog()[%d] has an empty Key", i)
		}
		if seen[f.Key] {
			t.Errorf("FlowCatalog()[%d] repeats key %q", i, f.Key)
		}
		seen[f.Key] = true
		if f.Label == "" || f.Description == "" || f.Instructions == "" {
			t.Errorf("FlowCatalog()[%d] (%s) has an empty Label/Description/Instructions: %+v", i, f.Key, f)
		}
	}
	if got[0].Label != "Autolander" {
		t.Errorf("autolander Label = %q, want Autolander", got[0].Label)
	}
	if got[1].Label != "Human triage" {
		t.Errorf("human-triage Label = %q, want Human triage", got[1].Label)
	}
}

// TestFlowInstructionsContract pins what every flow's text must actually
// say — the pins are load-bearing prose, not decoration: the dedup pass
// (`labctl issue list` plus the `[schedule: <SCHEDULE>]` title marker the
// next firing recognizes its own issues by), the idempotent
// `labctl label create` before any filing (applying an undefined label is an
// error), the filing verb with the right label, the implement-nothing fence,
// and the house closer.
func TestFlowInstructionsContract(t *testing.T) {
	tests := []struct {
		key      string
		label    string
		wantSubs []string
	}{
		{
			key:   "autolander",
			label: "ready-for-agent",
			wantSubs: []string{
				"`labctl issue list`",
				"`labctl issue edit <n> --body \"…\"`",
				"`labctl issue comment <n> \"…\"`",
				"`labctl label create --name ready-for-agent`",
				"labctl issue create",
				"--labels ready-for-agent",
				"[schedule: " + ScheduleToken + "]",
				"One issue per independent piece of work",
				"Implement nothing yourself",
				"no pull requests",
				"AFK pipeline",
			},
		},
		{
			key:   "human-triage",
			label: "needs-triage",
			wantSubs: []string{
				"`labctl issue list`",
				"`labctl issue edit <n> --body \"…\"`",
				"`labctl issue comment <n> \"…\"`",
				"`labctl label create --name needs-triage`",
				"labctl issue create",
				"--labels needs-triage",
				"[schedule: " + ScheduleToken + "]",
				"One issue per independent piece of work",
				"Implement nothing yourself",
				"no pull requests",
				"a human triages them",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			f, ok := FlowByKey(tc.key)
			if !ok {
				t.Fatalf("FlowByKey(%q) missing", tc.key)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(f.Instructions, sub) {
					t.Errorf("%s instructions missing %q:\n%s", tc.key, sub, f.Instructions)
				}
			}
			// The other flow's label must not leak into this one.
			for _, other := range tests {
				if other.label != tc.label && strings.Contains(f.Instructions, other.label) {
					t.Errorf("%s instructions mention the wrong label %q", tc.key, other.label)
				}
			}
			lines := strings.Split(f.Instructions, "\n")
			if !strings.Contains(lines[0], "you never implement") {
				t.Errorf("%s instructions do not open with a role statement: %q", tc.key, lines[0])
			}
			const closer = "Then stop working. Do not start unrelated work."
			if !strings.HasSuffix(f.Instructions, closer) {
				t.Errorf("%s instructions do not end with %q:\n%s", tc.key, closer, f.Instructions)
			}
		})
	}
}

// TestFlowByKey covers the lookup seam the API validates stored keys with:
// a hit returns the catalog entry, a miss returns the zero Flow and false
// (never a partial value a caller could mistake for a flow).
func TestFlowByKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"autolander", "autolander", true},
		{"human triage", "human-triage", true},
		{"unknown", "land-my-own-pr", false},
		{"empty", "", false},
		{"case sensitive", "Autolander", false},
		{"label not a key", "Human triage", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FlowByKey(tc.key)
			if ok != tc.want {
				t.Fatalf("FlowByKey(%q) ok = %v, want %v", tc.key, ok, tc.want)
			}
			if ok && got.Key != tc.key {
				t.Errorf("FlowByKey(%q).Key = %q", tc.key, got.Key)
			}
			if !ok && got != (Flow{}) {
				t.Errorf("FlowByKey(%q) miss returned %+v, want the zero Flow", tc.key, got)
			}
		})
	}
}

// render is the interpolation the composition performs, applied to one
// block so a test can spell its expectation without restating the prose.
func render(s, scheduleName string) string {
	return strings.ReplaceAll(s, ScheduleToken, scheduleName)
}

// TestComposeSchedulePrompt pins the composition contract (ADR-0062): prompt
// first, then the selected blocks in CATALOG order regardless of selection
// order or repeats, unknown keys skipped defensively, "\n\n" between blocks,
// and ScheduleToken interpolated across the whole result — the operator's
// own prose included.
func TestComposeSchedulePrompt(t *testing.T) {
	auto, _ := FlowByKey("autolander")
	human, _ := FlowByKey("human-triage")

	tests := []struct {
		name   string
		prompt string
		keys   []string
		sched  string
		want   string
	}{
		{
			name:   "zero flows is the prompt alone",
			prompt: "Check for dependency updates.",
			keys:   nil,
			sched:  "Weekly deps",
			want:   "Check for dependency updates.",
		},
		{
			name:   "empty selection slice is the prompt alone",
			prompt: "Check for dependency updates.",
			keys:   []string{},
			sched:  "Weekly deps",
			want:   "Check for dependency updates.",
		},
		{
			name:   "prompt then one block",
			prompt: "Check for dependency updates.",
			keys:   []string{"autolander"},
			sched:  "Weekly deps",
			want:   "Check for dependency updates.\n\n" + render(auto.Instructions, "Weekly deps"),
		},
		{
			name:   "selection order ignored, catalog order wins",
			prompt: "Audit.",
			keys:   []string{"human-triage", "autolander"},
			sched:  "Nightly audit",
			want: "Audit.\n\n" + render(auto.Instructions, "Nightly audit") +
				"\n\n" + render(human.Instructions, "Nightly audit"),
		},
		{
			name:   "duplicates collapse to one block",
			prompt: "Audit.",
			keys:   []string{"autolander", "autolander", "autolander"},
			sched:  "Nightly audit",
			want:   "Audit.\n\n" + render(auto.Instructions, "Nightly audit"),
		},
		{
			name:   "unknown key skipped, known one kept",
			prompt: "Audit.",
			keys:   []string{"land-my-own-pr", "human-triage", ""},
			sched:  "Nightly audit",
			want:   "Audit.\n\n" + render(human.Instructions, "Nightly audit"),
		},
		{
			name:   "only unknown keys is the prompt alone",
			prompt: "Audit.",
			keys:   []string{"land-my-own-pr"},
			sched:  "Nightly audit",
			want:   "Audit.",
		},
		{
			name:   "empty prompt is the blocks alone",
			prompt: "",
			keys:   []string{"autolander", "human-triage"},
			sched:  "S",
			want:   render(auto.Instructions, "S") + "\n\n" + render(human.Instructions, "S"),
		},
		{
			name:   "both empty composes to nothing",
			prompt: "",
			keys:   nil,
			sched:  "S",
			want:   "",
		},
		{
			name:   "token in the operator prompt is interpolated too",
			prompt: "You are the " + ScheduleToken + " run.",
			keys:   nil,
			sched:  "Weekly deps",
			want:   "You are the Weekly deps run.",
		},
		{
			name:   "schedule name with regex-special characters is literal",
			prompt: ScheduleToken,
			keys:   []string{"autolander"},
			sched:  `v1.2 (a|b)$ [x] \d`,
			want: render(ScheduleToken, `v1.2 (a|b)$ [x] \d`) + "\n\n" +
				render(auto.Instructions, `v1.2 (a|b)$ [x] \d`),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComposeSchedulePrompt(tc.prompt, tc.keys, tc.sched)
			if got != tc.want {
				t.Errorf("ComposeSchedulePrompt(%q, %v, %q) =\n%s\n\nwant:\n%s", tc.prompt, tc.keys, tc.sched, got, tc.want)
			}
			if strings.Contains(got, ScheduleToken) {
				t.Errorf("ComposeSchedulePrompt left %s uninterpolated:\n%s", ScheduleToken, got)
			}
		})
	}
}

// TestComposeSchedulePromptBlockOrder states the catalog-order pin directly
// rather than through an equality expectation: selecting human-triage first
// still puts the autolander block ahead of it.
func TestComposeSchedulePromptBlockOrder(t *testing.T) {
	auto, _ := FlowByKey("autolander")
	human, _ := FlowByKey("human-triage")
	got := ComposeSchedulePrompt("Audit.", []string{"human-triage", "autolander"}, "S")

	autoAt := strings.Index(got, render(strings.SplitN(auto.Instructions, "\n", 2)[0], "S"))
	humanAt := strings.Index(got, render(strings.SplitN(human.Instructions, "\n", 2)[0], "S"))
	if autoAt < 0 || humanAt < 0 {
		t.Fatalf("composed prompt is missing a block (autolander at %d, human-triage at %d):\n%s", autoAt, humanAt, got)
	}
	if autoAt > humanAt {
		t.Errorf("autolander block at %d comes after human-triage at %d; catalog order must win:\n%s", autoAt, humanAt, got)
	}
}

// TestComposeSchedulePromptInterpolatesEveryOccurrence pins ReplaceAll, not
// Replace: a flow names its Schedule several times (the dedup marker appears
// in the list step, the edit step, and the filing step) and every one of them
// must carry the name.
func TestComposeSchedulePromptInterpolatesEveryOccurrence(t *testing.T) {
	for _, f := range FlowCatalog() {
		occurrences := strings.Count(f.Instructions, ScheduleToken)
		if occurrences < 2 {
			t.Errorf("%s instructions name the Schedule %d time(s); the dedup marker should appear repeatedly", f.Key, occurrences)
		}
		got := ComposeSchedulePrompt("", []string{f.Key}, "Weekly deps")
		if n := strings.Count(got, "Weekly deps"); n != occurrences {
			t.Errorf("%s composed with %d name occurrences, want %d:\n%s", f.Key, n, occurrences, got)
		}
	}
}

// TestFlowCatalogMutationSafety proves FlowCatalog hands out a copy: a caller
// that rewrites or truncates the returned slice must not disturb the catalog
// every later composition reads.
func TestFlowCatalogMutationSafety(t *testing.T) {
	before := FlowCatalog()
	baseline := ComposeSchedulePrompt("p", []string{"autolander", "human-triage"}, "S")

	mine := FlowCatalog()
	mine[0] = Flow{Key: "clobbered", Label: "x", Description: "x", Instructions: "x"}
	mine[1].Instructions = "wiped"
	mine = append(mine[:0], mine[1:]...)
	if len(mine) != len(before)-1 {
		t.Fatalf("caller-side truncation did not happen: len %d, want %d", len(mine), len(before)-1)
	}

	after := FlowCatalog()
	if len(after) != len(before) {
		t.Fatalf("FlowCatalog() length changed after caller mutation: %d, want %d", len(after), len(before))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("FlowCatalog()[%d] mutated: %+v, want %+v", i, after[i], before[i])
		}
	}
	if got, ok := FlowByKey("autolander"); !ok || got.Instructions == "x" {
		t.Errorf("FlowByKey(autolander) sees mutated state: ok=%v %+v", ok, got)
	}
	if got := ComposeSchedulePrompt("p", []string{"autolander", "human-triage"}, "S"); got != baseline {
		t.Errorf("composition changed after caller mutation:\n%s\n\nwant:\n%s", got, baseline)
	}
}

// bannedWords are CONTEXT.md's globally banned words, spelled by
// concatenation on purpose: a literal scan of this repo's sources reads test
// files too, and a guard that must name what it forbids would otherwise trip
// itself. Keep them assembled.
var bannedWords = []string{"t" + "ask", "j" + "ob"}

// TestFlowTextsAvoidBannedVocabulary is the self-guard: no flow's key, label,
// description, or instruction text may use the globally banned vocabulary
// (CONTEXT.md's avoid-list — the object is a Schedule, a firing spawns a
// scheduled run, and what an agent does is work). Matched case-insensitively
// on word boundaries, with a sanity case so the guard can never go blind.
func TestFlowTextsAvoidBannedVocabulary(t *testing.T) {
	banned := regexp.MustCompile(`(?i)\b(` + strings.Join(bannedWords, "|") + `)s?\b`)

	for _, w := range bannedWords {
		for _, probe := range []string{w, strings.ToUpper(w), "a " + w + "s here", "the " + w + "."} {
			if !banned.MatchString(probe) {
				t.Fatalf("guard is blind: pattern does not match %q", probe)
			}
		}
	}
	// ...and is not so broad that a mere substring trips it (assembled, same
	// reason as bannedWords).
	if banned.MatchString("a " + bannedWords[1] + "bing sub" + bannedWords[0] + "ed string") {
		t.Fatal("guard is too broad: it matched a string with no banned word in it")
	}

	for _, f := range FlowCatalog() {
		for _, field := range []struct{ name, text string }{
			{"Key", f.Key},
			{"Label", f.Label},
			{"Description", f.Description},
			{"Instructions", f.Instructions},
		} {
			if hit := banned.FindString(field.text); hit != "" {
				t.Errorf("flow %s %s uses banned vocabulary %q:\n%s", f.Key, field.name, hit, field.text)
			}
		}
	}
	if hit := banned.FindString(ComposeSchedulePrompt("", []string{"autolander", "human-triage"}, "S")); hit != "" {
		t.Errorf("composed prompt uses banned vocabulary %q", hit)
	}
}
