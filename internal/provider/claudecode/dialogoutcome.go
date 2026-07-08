package claudecode

// Answered-dialog outcome derivation (issue #56 decision 3). A dialog tool_use
// with a tool_result used to be demoted to a generic tool chip — history
// showed a collapsed "AskUserQuestion" with truncated raw JSON. Since issue
// #56 it stays a DIALOG message whose Outcome carries the resolved answer,
// derived from the same event-level toolUseResult/toolDenialKind ground truth
// the verification backstop reads (compat §5, all shapes live-verified
// 2026-07-08 on 2.1.198; dialogintent.go's mismatch() holds the authoritative
// parsing patterns this file mirrors). Display-only and best-effort by
// design: Outcome gates nothing — state derivation keys only on Outcome
// PRESENCE, and warning about a desynced answer stays the intent backstop's
// job (a separate concern, untouched).

import (
	"encoding/json"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// planFeedbackMarker precedes the typed rejection feedback inside a plan
// denial string: "Error: The user doesn't want to proceed with this tool use.
// … To tell you how to proceed, the user said:\n<feedback>" (compat §5, live
// 2.1.198). A rejection without typed feedback records the bare denial — no
// marker, Feedback stays empty.
const planFeedbackMarker = "the user said:\n"

// outcomeFor derives an answered dialog's Outcome from its recorded
// resolution. Never nil: the caller only reaches here for a tool_use whose
// tool_result exists (first-pass resolutions map), and a non-nil Outcome IS
// the wire signal that the dialog is answered — an unreadable resolution
// degrades to Dismissed, not to a pending-looking nil.
func outcomeFor(d provider.Dialog, res resolvedTool) *provider.DialogOutcome {
	// A denial is a STRING toolUseResult ("User rejected tool use", "Error:
	// The user doesn't want to proceed…") or the user-rejected stamp — the
	// same disjunction mismatch() uses, since 2.1.198 sets both on a decline
	// and either alone still means "no answer landed".
	denial, hasDenialString := res.denialString()
	isDenial := hasDenialString || res.denialKind == "user-rejected"

	if d.Kind == provider.DialogKindPlan {
		return planOutcome(denial, isDenial, res)
	}
	return questionOutcome(d, isDenial, res)
}

// planOutcome resolves an ExitPlanMode dialog: rejection rides the denial
// string (feedback after the marker), approval is the recorded plan OBJECT
// ({plan,filePath,…} — compat §5), and an empty/absent toolUseResult means
// the resolution left no readable ground truth (an interrupt, an upstream
// shape change) — resolved, but lab cannot say how: Dismissed.
func planOutcome(denial string, isDenial bool, res resolvedTool) *provider.DialogOutcome {
	if isDenial {
		out := &provider.DialogOutcome{}
		// The LAST marker occurrence delimits the feedback: everything before
		// it is claude's fixed rejection boilerplate, and taking the last cut
		// keeps a marker echoed inside earlier boilerplate from swallowing
		// the operator's text. Rows 2–3 without typed text record the bare
		// denial — no marker, empty Feedback (a plain "keep planning").
		if i := strings.LastIndex(denial, planFeedbackMarker); i >= 0 {
			out.Feedback = denial[i+len(planFeedbackMarker):]
		}
		return out
	}
	if len(res.toolUseResult) > 0 {
		return &provider.DialogOutcome{Approved: true}
	}
	return &provider.DialogOutcome{Dismissed: true}
}

// questionOutcome resolves an AskUserQuestion dialog against the recorded
// answers map — the {answers, afkTimeoutMs} object mismatch() reads (compat
// §5). Everything that resolved WITHOUT an answer collapses to Dismissed: a
// denial, an unreadable/absent toolUseResult, and the 60s unattended timeout
// (which records answers:{} plus an afkTimeoutMs stamp, so the len==0 check
// covers it without special-casing the stamp).
func questionOutcome(d provider.Dialog, isDenial bool, res resolvedTool) *provider.DialogOutcome {
	if isDenial {
		return &provider.DialogOutcome{Dismissed: true}
	}
	var rec struct {
		Answers      map[string]string `json:"answers"`
		AfkTimeoutMs int64             `json:"afkTimeoutMs"` // documents the timeout shape; answers:{} is the actual signal
	}
	if len(res.toolUseResult) == 0 || json.Unmarshal(res.toolUseResult, &rec) != nil || len(rec.Answers) == 0 {
		return &provider.DialogOutcome{Dismissed: true}
	}
	// One QuestionResult per question, in DIALOG order — the form's order,
	// never the map's. A single-question dialog is the flat shape: d.Prompt
	// IS the question text (askUserQuestionDialog passes it through
	// untruncated) and therefore the key toolUseResult.answers records under;
	// a multi-question dialog carries its questions in d.Questions.
	out := &provider.DialogOutcome{}
	if len(d.Questions) >= 2 {
		for _, q := range d.Questions {
			out.Results = append(out.Results, questionResult(q.Text, q.Options, q.MultiSelect, rec.Answers))
		}
		return out
	}
	out.Results = append(out.Results, questionResult(d.Prompt, d.Options, d.Multi, rec.Answers))
	return out
}

// questionResult resolves one question's recorded answer for display. An
// absent map entry yields just the question text (no Chosen, no OtherText) —
// the UI renders an unanswered marker for it.
func questionResult(text string, opts []provider.DialogOption, multi bool, answers map[string]string) provider.QuestionResult {
	r := provider.QuestionResult{Question: text}
	recorded, ok := answers[text]
	if !ok {
		return r
	}
	if !multi {
		// Single-select records either a listed label or the Other row's free
		// text VERBATIM (compat §5: "Favorite pet?":"Ferret") — equality
		// against the listed labels is the only way to tell them apart.
		if isListedLabel(opts, recorded) {
			r.Chosen = []string{recorded}
		} else {
			r.OtherText = recorded
		}
		return r
	}
	// Multi-select records the chosen labels joined ", " in TOGGLE order, NOT
	// option order (live 2026-07-08: toggling Apple then Cherry recorded
	// "Cherry, Apple" — see sameAnswer), with an Other free text riding as one
	// more segment. Whole-string equality first: a SINGLE chosen label may
	// itself contain ", " and must not be split. Then split on ", " — label
	// segments become Chosen in recorded order, the leftovers re-join as the
	// free text. A label containing ", " chosen ALONGSIDE other toggles is
	// indistinguishable from its own segments; display-only, so mis-bucketing
	// that pathological case into Chosen-or-OtherText is best-effort by design.
	if isListedLabel(opts, recorded) {
		r.Chosen = []string{recorded}
		return r
	}
	var other []string
	for _, seg := range strings.Split(recorded, ", ") {
		if isListedLabel(opts, seg) {
			r.Chosen = append(r.Chosen, seg)
		} else {
			other = append(other, seg)
		}
	}
	r.OtherText = strings.Join(other, ", ")
	return r
}

// isListedLabel reports whether s equals a listed (non-Other) option label.
// The synthesized Other row is excluded: its label is lab's own UI vocabulary
// (chat_dialog.go), never something claude records — an operator who
// free-types exactly "Other" must still land in OtherText, verbatim.
func isListedLabel(opts []provider.DialogOption, s string) bool {
	for _, o := range opts {
		if !o.IsOther && o.Label == s {
			return true
		}
	}
	return false
}
