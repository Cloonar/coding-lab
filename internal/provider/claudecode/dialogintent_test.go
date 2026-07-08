package claudecode

// Post-resolve verification backstop tests (issue #51 decision 3). Fixture-
// driven: every recorded-resolution line below is the live 2.1.198 shape
// captured 2026-07-08 (the full transcripts live in internal/compat/testdata/
// transcript-*-live-2.1.198.jsonl; these are the load-bearing lines with the
// bulky usage/uuid fields elided — field names and value shapes verbatim).
// The flow under test is the production one: AnswerDialog records the intent
// against a fake runner, ReadTranscript (the intent-aware parse) verifies the
// resolution and emits — or stays silent.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// twoQuestionDialog mirrors the live 2-question form (askuserquestion-live
// fixture): a single-select Color question and a multiSelect Fruits question,
// each with the synthesized Other row appended.
func twoQuestionDialog(toolID string) provider.Dialog {
	return provider.Dialog{
		ToolID: toolID, Kind: provider.DialogKindQuestion, Prompt: "2 questions", Answerable: true,
		Questions: []provider.Question{
			{Header: "Color", Text: "Which color do you prefer?", Options: []provider.DialogOption{
				{Label: "Red", Description: "warm"}, {Label: "Blue", Description: "cool"}, {Label: "Other", IsOther: true},
			}},
			{Header: "Fruits", Text: "Which fruits do you like?", MultiSelect: true, Options: []provider.DialogOption{
				{Label: "Apple"}, {Label: "Banana"}, {Label: "Cherry"}, {Label: "Other", IsOther: true},
			}},
		},
	}
}

func planTestDialog(toolID string) provider.Dialog {
	return provider.Dialog{
		ToolID: toolID, Kind: provider.DialogKindPlan, Prompt: "# Plan", Answerable: true,
		Options: planPickerOptions(),
	}
}

// Live-shape resolution lines (2.1.198, 2026-07-08), parametrized by tool id.
const (
	// toolUseResult OBJECT with the answers map — the answered shape.
	answeredLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Your questions have been answered: \"Which color do you prefer?\"=\"Red\", \"Which fruits do you like?\"=\"Apple, Cherry\". You can now continue with these answers in mind.","tool_use_id":"TOOLID"}]},"timestamp":"2026-07-08T15:37:01.165Z","toolUseResult":{"questions":[],"answers":{"Which color do you prefer?":"Red","Which fruits do you like?":"Apple, Cherry"},"annotations":{}}}`
	// answers:{} + afkTimeoutMs — the 60s unattended-timeout shape.
	timeoutLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"No response after 60s — the user may be away from keyboard. Proceed using your best judgment based on the context so far; you can re-ask this question later if it's still relevant.","tool_use_id":"TOOLID"}]},"timestamp":"2026-07-08T15:47:47.200Z","toolUseResult":{"questions":[],"answers":{},"annotations":{},"afkTimeoutMs":60000}}`
	// toolUseResult STRING + toolDenialKind — the declined shape.
	declinedLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed.","is_error":true,"tool_use_id":"TOOLID"}]},"timestamp":"2026-07-08T15:38:32.213Z","toolUseResult":"User rejected tool use","toolDenialKind":"user-rejected"}`
	// Plan approval — toolUseResult OBJECT carrying the plan.
	planApprovedLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"User has approved your plan. You can now start coding.","tool_use_id":"TOOLID"}]},"timestamp":"2026-07-08T15:45:28.192Z","toolUseResult":{"plan":"# Plan","isAgent":false,"filePath":"/tmp/plan.md","planWasEdited":false}}`
	// Plan rejection with feedback — toolUseResult STRING ending in the feedback.
	planRejectedLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"The user doesn't want to proceed with this tool use. To tell you how to proceed, the user said:\nAlso add the current date to the note","is_error":true,"tool_use_id":"TOOLID"}]},"timestamp":"2026-07-08T15:42:48.832Z","toolUseResult":"Error: The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). To tell you how to proceed, the user said:\nAlso add the current date to the note","toolDenialKind":"user-rejected"}`
)

// resolvedTranscript renders a minimal transcript: the retro-flushed tool_use
// + the given resolution line, both keyed to toolID (compat §5: the pair is
// flushed together when the dialog resolves).
func resolvedTranscript(toolName, toolID, resolutionLine string) string {
	use := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + toolID + `","name":"` + toolName + `","input":{}}]},"timestamp":"2026-07-08T15:34:33.444Z"}`
	return use + "\n" + strings.ReplaceAll(resolutionLine, "TOOLID", toolID) + "\n"
}

// answerThenRead plays answer into dialog (recording the intent) and reads the
// transcript back through the intent-aware ReadTranscript.
func answerThenRead(t *testing.T, dialog provider.Dialog, answer provider.DialogAnswer, transcript string) provider.Chat {
	t.Helper()
	p, f := armedRunner(t)
	if err := p.AnswerDialog(context.Background(), chatSession, dialog, answer); err != nil {
		t.Fatalf("AnswerDialog: %v", err)
	}
	if len(f.KeyLog(chatSession)) == 0 {
		t.Fatal("AnswerDialog sent no keystrokes")
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	chat, err := p.ReadTranscript(path)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	return chat
}

// backstopWarnings returns the lifecycle-error messages of a chat (the
// backstop's emission shape) — API-error lifecycles are also Error=true, but
// none of these fixtures carry one.
func backstopWarnings(chat provider.Chat) []provider.Message {
	var out []provider.Message
	for _, m := range chat.Messages {
		if m.Kind == provider.MessageLifecycle && m.Error {
			out = append(out, m)
		}
	}
	return out
}

func TestBackstop_answeredAsIntended_silent(t *testing.T) {
	chat := answerThenRead(t, twoQuestionDialog("toolu_2q"),
		provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 0}, {Selected: []int{0, 2}}}},
		resolvedTranscript(toolAskUserQuestion, "toolu_2q", answeredLine))
	if w := backstopWarnings(chat); len(w) != 0 {
		t.Errorf("matching answers emitted warnings: %+v", w)
	}
}

func TestBackstop_wrongLabelRecorded_warns(t *testing.T) {
	// lab intended Blue for the Color question; the transcript recorded Red.
	chat := answerThenRead(t, twoQuestionDialog("toolu_2q"),
		provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 1}, {Selected: []int{0, 2}}}},
		resolvedTranscript(toolAskUserQuestion, "toolu_2q", answeredLine))
	w := backstopWarnings(chat)
	if len(w) != 1 {
		t.Fatalf("wrong recorded label: %d warnings; want 1 (%+v)", len(w), chat.Messages)
	}
	for _, want := range []string{"Dialog answer may not have landed", `"Blue"`, `"Red"`} {
		if !strings.Contains(w[0].Text, want) {
			t.Errorf("warning %q lacks %q", w[0].Text, want)
		}
	}
	// Emitted immediately after the tool result, and state-neutral: the tail
	// warning must not flip the tool_result-derived working state.
	if last := chat.Messages[len(chat.Messages)-1]; last.Seq != w[0].Seq {
		t.Errorf("warning not adjacent to the tool result: %+v", chat.Messages)
	}
	if chat.State != provider.StateWorking {
		t.Errorf("state = %q; want %q (the advisory warning must not drive state)", chat.State, provider.StateWorking)
	}
}

func TestBackstop_afkTimeoutAfterAnswer_warns(t *testing.T) {
	d := provider.Dialog{
		ToolID: "toolu_to", Kind: provider.DialogKindQuestion, Prompt: "Which toppings?",
		Multi: true, Answerable: true,
		Options: []provider.DialogOption{{Label: "Olives"}, {Label: "Onions"}, {Label: "Other", IsOther: true}},
	}
	chat := answerThenRead(t, d, provider.DialogAnswer{Selected: []int{0}},
		resolvedTranscript(toolAskUserQuestion, "toolu_to", timeoutLine))
	w := backstopWarnings(chat)
	if len(w) != 1 || !strings.Contains(w[0].Text, "timed out") || !strings.Contains(w[0].Text, "60s") {
		t.Fatalf("afkTimeout after an answer: warnings = %+v; want one timeout warning", w)
	}
}

func TestBackstop_userRejectedAfterAnswer_warns(t *testing.T) {
	d := provider.Dialog{
		ToolID: "toolu_rej", Kind: provider.DialogKindQuestion, Prompt: "Favorite pet?", Answerable: true,
		Options: []provider.DialogOption{{Label: "Dog"}, {Label: "Cat"}, {Label: "Other", IsOther: true}},
	}
	chat := answerThenRead(t, d, provider.DialogAnswer{Index: 0},
		resolvedTranscript(toolAskUserQuestion, "toolu_rej", declinedLine))
	w := backstopWarnings(chat)
	if len(w) != 1 || !strings.Contains(w[0].Text, "decline") {
		t.Fatalf("user-rejected after an answer: warnings = %+v; want one decline warning", w)
	}
}

func TestBackstop_planApprovedAsIntended_silent(t *testing.T) {
	chat := answerThenRead(t, planTestDialog("toolu_plan"), provider.DialogAnswer{Index: 0},
		resolvedTranscript(toolExitPlanMode, "toolu_plan", planApprovedLine))
	if w := backstopWarnings(chat); len(w) != 0 {
		t.Errorf("approved-as-intended plan emitted warnings: %+v", w)
	}
}

func TestBackstop_planRejectedWithFeedbackAsIntended_silent(t *testing.T) {
	chat := answerThenRead(t, planTestDialog("toolu_plan"),
		provider.DialogAnswer{Index: 3, OtherText: "Also add the current date to the note"},
		resolvedTranscript(toolExitPlanMode, "toolu_plan", planRejectedLine))
	if w := backstopWarnings(chat); len(w) != 0 {
		t.Errorf("rejected-with-feedback-as-intended plan emitted warnings: %+v", w)
	}
}

func TestBackstop_planOutcomeFlips_warn(t *testing.T) {
	// Approve intended, rejection recorded.
	chat := answerThenRead(t, planTestDialog("toolu_p1"), provider.DialogAnswer{Index: 1},
		resolvedTranscript(toolExitPlanMode, "toolu_p1", planRejectedLine))
	if w := backstopWarnings(chat); len(w) != 1 || !strings.Contains(w[0].Text, "rejected plan") {
		t.Errorf("approve→rejected: warnings = %+v; want one", w)
	}
	// Reject intended, approval recorded.
	chat = answerThenRead(t, planTestDialog("toolu_p2"),
		provider.DialogAnswer{Index: 3, OtherText: "change it"},
		resolvedTranscript(toolExitPlanMode, "toolu_p2", planApprovedLine))
	if w := backstopWarnings(chat); len(w) != 1 || !strings.Contains(w[0].Text, "approved plan") {
		t.Errorf("reject→approved: warnings = %+v; want one", w)
	}
}

// A mismatch warning is deterministic across parses while the intent exists
// (seq-stable re-reads), and a matched intent is cleared — the warning-free
// parse stays warning-free. The restart caveat (intents are in-memory, so a
// warning disappears after a lab restart) is documented on ReadTranscript and
// in compat §5; a fresh Provider parsing the same bytes is that case.
func TestBackstop_deterministicPerParse_andRestartCaveat(t *testing.T) {
	p, _ := armedRunner(t)
	d := twoQuestionDialog("toolu_det")
	if err := p.AnswerDialog(context.Background(), chatSession, d,
		provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 1}, {Selected: []int{0, 2}}}}); err != nil {
		t.Fatalf("AnswerDialog: %v", err)
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(resolvedTranscript(toolAskUserQuestion, "toolu_det", answeredLine)), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := p.ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(backstopWarnings(first)) != 1 || len(backstopWarnings(second)) != 1 {
		t.Fatalf("mismatch warning not deterministic: first %d, second %d",
			len(backstopWarnings(first)), len(backstopWarnings(second)))
	}
	if first.Cursor != second.Cursor {
		t.Errorf("cursor drifted across re-parses: %d vs %d", first.Cursor, second.Cursor)
	}
	// A restarted lab (fresh Provider, empty registry) parses the same bytes
	// without the warning — the documented advisory-only caveat.
	fresh, _ := armedRunner(t)
	chat, err := fresh.ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(backstopWarnings(chat)) != 0 {
		t.Errorf("fresh provider (restart case) still warns: %+v", chat.Messages)
	}
}

// The pure ParseTranscript stays verification-free (compat fixtures parse
// identically no matter what was answered), and the registry stays bounded.
func TestBackstop_pureParseUnaffected_andBounded(t *testing.T) {
	p, _ := armedRunner(t)
	d := planTestDialog("toolu_x")
	_ = p.AnswerDialog(context.Background(), chatSession, d, provider.DialogAnswer{Index: 0})
	chat, err := ParseTranscript(strings.NewReader(resolvedTranscript(toolExitPlanMode, "toolu_x", planRejectedLine)))
	if err != nil {
		t.Fatal(err)
	}
	if len(backstopWarnings(chat)) != 0 {
		t.Errorf("pure ParseTranscript emitted backstop warnings: %+v", chat.Messages)
	}

	// Cap: recording far past maxDialogIntents keeps the registry bounded and
	// evicts oldest-first.
	for i := 0; i < maxDialogIntents+20; i++ {
		di := planTestDialog("toolu_cap_" + strconv.Itoa(i))
		p.intents.record(di, provider.DialogAnswer{Index: 0})
	}
	if n := len(p.intents.byID); n != maxDialogIntents {
		t.Errorf("registry size = %d; want the %d cap", n, maxDialogIntents)
	}
	if _, ok := p.intents.byID["toolu_cap_0"]; ok {
		t.Error("oldest intent survived past the cap")
	}
}
