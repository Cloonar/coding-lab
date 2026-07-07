package claudecode

// Interactive-dialog detection (issue #7 decision 5). A pending dialog is an
// unanswered tool_use for a known interactive tool. Option buttons are built
// ONLY from the structured tool input — never scraped from the TUI widget —
// so a shape lab cannot render from the input degrades to Answerable=false and
// the UI shows a "open in claude.ai" deep-link hint.

import (
	"encoding/json"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Interactive tool names lab recognises as dialogs (compat.md §7).
const (
	toolAskUserQuestion = "AskUserQuestion"
	toolExitPlanMode    = "ExitPlanMode"
)

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

// askUserQuestionDialog renders a single-question AskUserQuestion into tappable
// options (the pinned answer recipe navigates one picker). A multi-question
// call is not answerable through the keystroke recipe, so it degrades to a
// deep-link hint.
func askUserQuestionDialog(b tBlock) provider.Dialog {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	d := provider.Dialog{ToolID: b.ID, DialogKind: "question"}
	if json.Unmarshal(b.Input, &in) != nil || len(in.Questions) == 0 {
		d.Prompt = "Interactive question"
		return d
	}
	if len(in.Questions) > 1 {
		d.Prompt = "Multiple questions — answer in claude.ai"
		return d // Answerable stays false
	}
	q := in.Questions[0]
	d.Prompt = q.Question
	d.Multi = q.MultiSelect
	for _, o := range q.Options {
		d.Options = append(d.Options, provider.DialogOption{Label: o.Label, Description: o.Description})
	}
	// AskUserQuestion always offers a free-text "Other" row after the listed
	// options (pinned: the tool description guarantees it).
	d.Options = append(d.Options, provider.DialogOption{Label: "Other", IsOther: true})
	d.Answerable = len(q.Options) > 0
	return d
}

// planDialog renders an ExitPlanMode approval. The approve/reject choices are
// TUI-owned (not in the tool input), so per the never-scrape rule the plan text
// is shown but answering degrades to the deep-link hint. It is still a pending
// dialog (Answerable=false), so the composer is locked like any other (issue
// #17 dec 5 — stray free text must not hit the focused plan picker); the
// operator approves in claude.ai or interrupts (Escape).
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
		DialogKind: "plan",
		Prompt:     truncate(prompt, truncateLimit),
		Answerable: false,
	}
}
