package claudecode

// Interactive-dialog detection (issue #7 decision 5, generalized by issue #51
// decision 3). A pending dialog is an unanswered tool_use for a known
// interactive tool. Option buttons are built ONLY from the structured tool
// input — never scraped from the TUI widget — plus write-side PINNED
// constants for rows the TUI synthesizes beyond the input (the free-text
// "Other" row, the whole ExitPlanMode picker): a pinned constant is a
// keystroke-recipe-style coupling (compat §7), verified against the live
// 2.1.198 picker, and nothing ever reads the pane.

import (
	"encoding/json"
	"fmt"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Interactive tool names lab recognises as dialogs (compat.md §7).
const (
	toolAskUserQuestion = "AskUserQuestion"
	toolExitPlanMode    = "ExitPlanMode"
)

// otherOptionLabel is lab's STABLE label for the synthesized free-text row
// appended to every AskUserQuestion question. The 2.1.198 TUI renders that
// row as "Type something." (live, 2026-07-08) — lab deliberately labels it
// "Other" instead: the label is lab's own UI vocabulary (the SPA renders a
// free-text input on the IsOther row, not a TUI transcription), and the
// keystroke recipe navigates by INDEX, so only the row's existence and
// position are couplings (compat §7), never its on-screen text.
const otherOptionLabel = "Other"

// dialogFromToolUse maps a tool_use block to a Dialog, reporting false when the
// tool is not an interactive dialog (an ordinary tool chip).
func dialogFromToolUse(b tBlock) (provider.Dialog, bool) {
	switch b.Name {
	case toolAskUserQuestion:
		return askUserQuestionDialog(b), true
	case toolExitPlanMode:
		return planDialog(b), true
	default:
		return provider.Dialog{}, false
	}
}

// askUserQuestionDialog renders an AskUserQuestion into tappable options. A
// SINGLE question keeps the original flat shape byte-for-byte (issue #51
// decision 3: zero churn, the compat snapshots stay valid); a MULTI-question
// call (N ≥ 2) carries each question in Questions — full form, one submit —
// and is answerable through the sequenced picker recipe since issue #51
// (before which it degraded to a deep-link hint). Every question gets the
// synthesized free-text Other row appended: the tool always offers free text
// (single-select: type-first inline fill; multi-select: a toggleable checkbox
// lab refuses to toggle — see normSelected), and the IsOther row IS the
// "free text allowed" signal in both shapes.
func askUserQuestionDialog(b tBlock) provider.Dialog {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	d := provider.Dialog{ToolID: b.ID, Kind: provider.DialogKindQuestion}
	if json.Unmarshal(b.Input, &in) != nil || len(in.Questions) == 0 {
		d.Prompt = "Interactive question"
		return d
	}
	if len(in.Questions) == 1 {
		q := in.Questions[0]
		d.Prompt = q.Question
		d.Multi = q.MultiSelect
		for _, o := range q.Options {
			d.Options = append(d.Options, provider.DialogOption{Label: o.Label, Description: o.Description})
		}
		d.Options = append(d.Options, provider.DialogOption{Label: otherOptionLabel, IsOther: true})
		d.Answerable = len(q.Options) > 0
		return d
	}
	// Multi-question: Prompt degrades to a short summary (the SPA renders the
	// real form from Questions); the flat Options/Multi stay empty.
	d.Prompt = fmt.Sprintf("%d questions", len(in.Questions))
	answerable := true
	for _, q := range in.Questions {
		opts := make([]provider.DialogOption, 0, len(q.Options)+1)
		for _, o := range q.Options {
			opts = append(opts, provider.DialogOption{Label: o.Label, Description: o.Description})
		}
		opts = append(opts, provider.DialogOption{Label: otherOptionLabel, IsOther: true})
		d.Questions = append(d.Questions, provider.Question{
			Header: q.Header, Text: q.Question, Options: opts, MultiSelect: q.MultiSelect,
		})
		if len(q.Options) == 0 {
			answerable = false // mirror the flat rule: no listed options → degrade
		}
	}
	d.Answerable = answerable
	return d
}

// planPickerOptions are the four rows of the ExitPlanMode approval picker,
// pinned 1:1 by INDEX — LIVE-VERIFIED 2026-07-08 on 2.1.198 under lab's exact
// spawn shape (`claude --remote-control <name> --permission-mode auto`), the
// ADR-0020 process (compat §7). The rows are TUI-owned (nothing in the tool
// input carries them), so like the keystroke recipes they are write-side
// pinned constants; the never-scrape rule is untouched.
//
// The recipe navigates by index, and the APPROVE/REJECT semantic per index is
// stable: rows 0–1 approve (0 auto-accepts edits, 1 reviews each edit), rows
// 2–3 reject (2 refines the plan, 3 is the free-text feedback row — IsOther,
// type-first inline fill like the question Other row — which rejects with the
// typed feedback). The LABELS below are lab's OWN operator-facing wording, NOT
// a mirror of the TUI text: two live runs under the SAME spawn flag showed
// row 0's TUI label vary with session state ("Yes, and use auto mode" →
// "Yes, auto-accept edits" — the shipped JS builds the row set from the
// permission mode + auto-mode state: option values yes-accept-edits,
// yes-auto-clear-context, yes-resume-auto-mode, no, …). Mirroring that drifting
// text would desync the SPA's rendered buttons from run to run; lab's stable
// semantic labels don't, and the recipe couples only to the index. The §5
// backstop (approve-vs-denial, never label text) catches a genuine index-order
// change.
func planPickerOptions() []provider.DialogOption {
	return []provider.DialogOption{
		{Label: "Approve — auto-accept edits"},
		{Label: "Approve — review each edit"},
		{Label: "Reject — refine the plan"},
		{Label: "Reject with feedback", IsOther: true},
	}
}

// planDialog renders an ExitPlanMode approval: Kind=plan, the (truncated) plan
// markdown as the Prompt, and the pinned approve/reject rows as Options —
// ANSWERABLE since issue #51 decision 3 (it degraded to a deep-link hint while
// the picker rows were uncaptured; they are live-pinned now, see
// planPickerOptions). Single-select; answered via the plan keystroke recipe
// (compat §7). The full plan also lands on disk (input planFilePath, ignored —
// the transcript carries the same markdown).
func planDialog(b tBlock) provider.Dialog {
	var in struct {
		Plan string `json:"plan"`
	}
	_ = json.Unmarshal(b.Input, &in)
	prompt := in.Plan
	if prompt == "" {
		prompt = "Plan ready for review"
	}
	return provider.Dialog{
		ToolID:     b.ID,
		Kind:       provider.DialogKindPlan,
		Prompt:     truncate(prompt, truncateLimit),
		Options:    planPickerOptions(),
		Answerable: true,
	}
}
