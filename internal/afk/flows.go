package afk

// Flows (issue #247 / ADR-0062): the built-in, lab-vendored catalog of
// routing instruction blocks a Schedule appends to its freeform prompt at
// spawn. A flow is PROSE, never machinery — it routes the OUTPUT of a
// scheduled run's investigation into the tracker (file, or update, fully
// specified issues and stop), so the pipeline that already owns selection,
// claiming, validation and landing does the actual work. Nothing here
// spawns, labels, or lands anything; the catalog only briefs the run.
//
// The texts live in lab's source rather than in a repo (the skills-bundle
// spirit): `labctl` verbs and the triage label strings are lab's own
// vocabulary and stay correct as lab evolves, and a scheduled run's brief is
// versioned with the binary that spawned it.
//
// Composition order is the CATALOG's, never the operator's selection order
// (ADR-0062): two Schedules that select the same flows must brief their runs
// identically, so selection order and duplicates are dropped on the floor.

import "strings"

// ScheduleToken is the Schedule-name placeholder in a flow's instruction
// text — the sibling of gitx.NToken and BranchToken, and interpolated the
// same way: literal ReplaceAll at EVERY occurrence, across the composed
// prompt as a whole (the operator's freeform prompt included), with unknown
// tokens passing through as literal text. A flow names its Schedule to make
// dedup mechanical: the issues a Schedule files carry its name in their
// titles, so the next firing can recognize its own prior work in a plain
// `labctl issue list`.
const ScheduleToken = "<SCHEDULE>"

// Flow is one catalog entry: a stable Key (what a Schedule row stores and
// the API validates against), a Label and one-sentence Description for the
// operator-facing multiselect, and the Instructions block appended to the
// Schedule's freeform prompt at spawn. A Flow is a value — FlowCatalog and
// FlowByKey hand out copies, so no caller can reach the catalog's strings.
type Flow struct {
	Key          string
	Label        string
	Description  string
	Instructions string
}

// autolanderInstructions is the autolander flow's block (ADR-0062): file
// fully-specified `ready-for-agent` issues and stop, leaving the normal AFK
// pipeline to claim them and — where Autoland is enabled — land them. The
// pins the text bakes in, in order: dedup FIRST (a Schedule fires forever;
// the second firing must recognize the first's issues instead of refiling
// them), `labctl label create` before any filing (applying a label the
// repository does not define is an error, and create is idempotent — the
// EscalateSeedPromptTemplate precedent), the schedule marker on every new
// title, a specification an agent can act on with no way to ask a question,
// and an explicit no-implementation fence: a scheduled run has no claim, no
// branch of its own to push, and no done-signal, so a pull request from it
// is a dead end (ADR-0062's rejected do-work-and-PR flow).
func autolanderInstructions() string {
	return strings.Join([]string{
		"Route what you found into the tracker for the AFK pipeline — you investigate and file; you never implement.",
		"",
		"1. Run `labctl issue list` FIRST. Issues this Schedule filed before end their titles with `[schedule: " + ScheduleToken + "]` — read the open ones before you file anything.",
		"2. If an open issue already covers a finding, update it instead of filing a duplicate: `labctl issue edit <n> --body \"…\"` for the current specification, then `labctl issue comment <n> \"…\"` for what changed since it was filed.",
		"3. Run `labctl label create --name ready-for-agent` before filing — idempotent and safe unconditionally, and applying a label the repository does not define is an error.",
		"4. File every finding no open issue covers yet: `labctl issue create --title \"… [schedule: " + ScheduleToken + "]\" --body \"…\" --labels ready-for-agent`. One issue per independent piece of work, and every new title ends with ` [schedule: " + ScheduleToken + "]`.",
		"5. Specify each body well enough that an agent can implement it with no other context and no way to ask you a question: the goal, the files and areas in scope, what \"done\" looks like, and how to verify it (the tests, build, or commands to run). Quote the evidence you found — paths, versions, command output.",
		"6. Skip findings that are not worth acting on. A thin or vague issue burns a whole agent run.",
		"7. Implement nothing yourself: no code changes, no branches, no commits, no pull requests. Filing and updating issues is this run's entire output — the AFK pipeline picks them up automatically, and where Autoland is enabled it lands them.",
		"8. Then stop working. Do not start unrelated work.",
	}, "\n")
}

// humanTriageInstructions is the human-triage flow's block (ADR-0062): the
// autolander flow's discipline aimed at a person instead of an agent — the
// same dedup-first, label-create-first, marker-on-every-title, implement-
// nothing contract, with `needs-triage` as the label so the issues enter the
// human triage state machine (docs/agents/triage-labels.md) and a specification
// bar written for a triager: enough to decide on, with uncertainty stated
// plainly rather than guessed away.
func humanTriageInstructions() string {
	return strings.Join([]string{
		"Route what you found into the tracker for a human triager — you investigate and file; you never implement.",
		"",
		"1. Run `labctl issue list` FIRST. Issues this Schedule filed before end their titles with `[schedule: " + ScheduleToken + "]` — read the open ones before you file anything.",
		"2. If an open issue already covers a finding, update it instead of filing a duplicate: `labctl issue edit <n> --body \"…\"` for the current picture, then `labctl issue comment <n> \"…\"` for what changed since it was filed.",
		"3. Run `labctl label create --name needs-triage` before filing — idempotent and safe unconditionally, and applying a label the repository does not define is an error.",
		"4. File every finding no open issue covers yet: `labctl issue create --title \"… [schedule: " + ScheduleToken + "]\" --body \"…\" --labels needs-triage`. One issue per independent piece of work, and every new title ends with ` [schedule: " + ScheduleToken + "]`.",
		"5. Specify each body well enough that a human can triage it without asking you anything: the goal, the files and areas in scope, what \"done\" looks like, how to verify it, and the evidence you found — paths, versions, command output. State plainly what you are unsure about instead of guessing.",
		"6. Skip findings that are not worth acting on. Every issue you file spends a human's attention.",
		"7. Implement nothing yourself: no code changes, no branches, no commits, no pull requests. Filing and updating issues is this run's entire output — a human triages them from there and decides what happens next.",
		"8. Then stop working. Do not start unrelated work.",
	}, "\n")
}

// builtinFlows is the catalog in its fixed, load-bearing order (ADR-0062):
// autolander, then human-triage. Order here IS composition order, so an
// insertion reorders every existing Schedule's composed prompt — append new
// flows at the end unless the reordering is the intent. Unexported and never
// handed out by reference: FlowCatalog copies, FlowByKey returns a value.
var builtinFlows = []Flow{
	{
		Key:          "autolander",
		Label:        "Autolander",
		Description:  "Files each finding as a fully specified issue labeled ready-for-agent, for the AFK pipeline to claim and land.",
		Instructions: autolanderInstructions(),
	},
	{
		Key:          "human-triage",
		Label:        "Human triage",
		Description:  "Files each finding as an issue labeled needs-triage, for a human to triage before any agent claims it.",
		Instructions: humanTriageInstructions(),
	},
}

// FlowCatalog returns the built-in flows in catalog order — the list the
// settings UI renders as a multiselect and the API validates a Schedule's
// stored keys against. The returned slice is a fresh copy: a caller may sort,
// filter, or rewrite it without disturbing the catalog every later
// composition reads.
func FlowCatalog() []Flow {
	out := make([]Flow, len(builtinFlows))
	copy(out, builtinFlows)
	return out
}

// FlowByKey looks up one catalog entry by its stored key, reporting whether
// it exists — the write-time validation seam (an unknown key is a 400 at the
// API, never a silent drop at spawn).
func FlowByKey(key string) (Flow, bool) {
	for _, f := range builtinFlows {
		if f.Key == key {
			return f, true
		}
	}
	return Flow{}, false
}

// ComposeSchedulePrompt renders the initial prompt of a scheduled run
// (ADR-0062): the Schedule's freeform prompt first, then the selected flows'
// instruction blocks in CATALOG order, joined by blank lines, with
// ScheduleToken interpolated to scheduleName across the whole result.
//
// The selection is a SET, not a sequence: keys are matched by walking the
// catalog, so the caller's order is ignored and a repeated key contributes
// its block exactly once. Composition must be deterministic and
// catalog-owned, or two Schedules selecting the same flows would brief their
// runs differently. Unknown keys are skipped defensively rather than
// erroring — validation belongs at the API layer, and a flow that a lab
// upgrade retired must degrade a firing to the remaining brief, never break
// it (the skip-layer discipline ADR-0030 applies to stale knob overrides).
//
// Degenerate shapes fall out of the same walk: zero flows compose to the
// prompt alone, an empty prompt to the flow blocks alone, and both empty to
// "". Interpolation runs last and over EVERYTHING, the operator's prompt
// included, so a Schedule's own prose may name itself with the token too.
func ComposeSchedulePrompt(prompt string, keys []string, scheduleName string) string {
	selected := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		selected[k] = struct{}{}
	}
	blocks := make([]string, 0, 1+len(builtinFlows))
	if prompt != "" {
		blocks = append(blocks, prompt)
	}
	for _, f := range builtinFlows {
		if _, ok := selected[f.Key]; ok {
			blocks = append(blocks, f.Instructions)
		}
	}
	return strings.ReplaceAll(strings.Join(blocks, "\n\n"), ScheduleToken, scheduleName)
}
