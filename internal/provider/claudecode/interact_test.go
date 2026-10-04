package claudecode

// Reply / AnswerDialog / Interrupt drive the tmux SessionRunner; these assert
// the exact keystroke deliveries (the KeyLog) against a fake, plus the reply
// validation. The dialog recipe itself is snapshot-pinned in internal/compat.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

const chatSession = "proj~dom-1"

func armedRunner(t *testing.T) (*Provider, *tmuxx.Fake) {
	t.Helper()
	f := tmuxx.NewFake()
	f.AddLive(chatSession)
	p, _ := testProvider(t, f)
	return p, f
}

func TestReply_pasteThenEnter(t *testing.T) {
	p, f := armedRunner(t)
	if err := p.Reply(context.Background(), chatSession, "keep going\nsecond line"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	log := f.KeyLog(chatSession)
	want := []tmuxx.KeyEvent{
		{Kind: "paste", Text: "keep going\nsecond line"},
		{Kind: "keys", Keys: "Enter"},
	}
	if len(log) != len(want) || log[0] != want[0] || log[1] != want[1] {
		t.Errorf("reply key log = %+v; want %+v", log, want)
	}
}

func TestReply_rejectsEmptyAndControl(t *testing.T) {
	p, f := armedRunner(t)
	for _, bad := range []string{"   ", "has\x1bescape"} {
		if err := p.Reply(context.Background(), chatSession, bad); !errors.Is(err, ErrInvalidReply) {
			t.Errorf("Reply(%q) err = %v; want ErrInvalidReply", bad, err)
		}
	}
	if n := len(f.KeyLog(chatSession)); n != 0 {
		t.Errorf("rejected replies still sent %d keystrokes; want 0", n)
	}
}

func TestInterrupt_sendsEscape(t *testing.T) {
	p, f := armedRunner(t)
	if err := p.Interrupt(context.Background(), chatSession); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	log := f.KeyLog(chatSession)
	if len(log) != 1 || log[0].Kind != "keys" || log[0].Keys != "Escape" {
		t.Errorf("interrupt key log = %+v; want a single Escape", log)
	}
}

func TestAnswerDialog_playsRecipe(t *testing.T) {
	p, f := armedRunner(t)
	d := provider.Dialog{Answerable: true, Options: []provider.DialogOption{
		{Label: "a"}, {Label: "b"}, {Label: "Other", IsOther: true},
	}}
	if err := p.AnswerDialog(context.Background(), chatSession, d, provider.DialogAnswer{Index: 1}); err != nil {
		t.Fatalf("AnswerDialog: %v", err)
	}
	// Down / Enter — a pure downward walk from the picker's top row. No climb:
	// Up wraps on 2.1.198 and the picker opens on row 0, so a climb would land
	// on the wrong row (compat §7, live 2026-07-08).
	var keys []string
	for _, e := range f.KeyLog(chatSession) {
		keys = append(keys, e.Keys)
	}
	if got := strings.Join(keys, "|"); got != "Down|Enter" {
		t.Errorf("answer key log = %q; want \"Down|Enter\"", got)
	}
}

// With a non-zero keyDelay, AnswerDialog paces its ops and honours context
// cancellation between them — it plays the first op, then stops at the first
// paced gap once the context is done (compat §7 pacing).
func TestAnswerDialog_pacedRespectsCancel(t *testing.T) {
	p, f := armedRunner(t)
	p.keyDelay = time.Hour // make the first inter-op gap effectively block
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the first gap returns immediately
	d := provider.Dialog{Answerable: true, Options: []provider.DialogOption{{Label: "a"}, {Label: "b"}, {Label: "Other", IsOther: true}}}
	if err := p.AnswerDialog(ctx, chatSession, d, provider.DialogAnswer{Index: 1}); err == nil {
		t.Fatal("AnswerDialog with a cancelled context = nil; want context error")
	}
	// Only the first op (the normalize Up batch) was played before the gap.
	if log := f.KeyLog(chatSession); len(log) != 1 {
		t.Errorf("played %d ops before the cancelled gap; want 1 (stopped at the pace)", len(log))
	}
}

// The pre-first-key settle (compat §7, 2.1.280: an Enter that lands as the
// picker mounts is dropped) sits BEFORE any op and honours cancellation: a
// cancelled context plays nothing at all — not even the first key.
func TestAnswerDialog_settleRespectsCancel(t *testing.T) {
	p, f := armedRunner(t)
	p.settleDelay = time.Hour // make the settle effectively block
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := provider.Dialog{Answerable: true, Options: []provider.DialogOption{{Label: "a"}, {Label: "b"}, {Label: "Other", IsOther: true}}}
	if err := p.AnswerDialog(ctx, chatSession, d, provider.DialogAnswer{Index: 0}); err == nil {
		t.Fatal("AnswerDialog with a cancelled context = nil; want context error")
	}
	if log := f.KeyLog(chatSession); len(log) != 0 {
		t.Errorf("played %d ops despite the cancelled settle; want 0 (nothing before the settle)", len(log))
	}
}

func TestAnswerDialog_refusesUnanswerable(t *testing.T) {
	p, _ := armedRunner(t)
	d := provider.Dialog{Answerable: false, Kind: provider.DialogKindPlan}
	if err := p.AnswerDialog(context.Background(), chatSession, d, provider.DialogAnswer{}); !errors.Is(err, ErrDialogNotAnswerable) {
		t.Errorf("AnswerDialog(plan) err = %v; want ErrDialogNotAnswerable", err)
	}
}

func TestReply_normalizesCRLF(t *testing.T) {
	p, f := armedRunner(t)
	if err := p.Reply(context.Background(), chatSession, "line one\r\nline two\rline three"); err != nil {
		t.Fatalf("Reply(CRLF): %v", err)
	}
	log := f.KeyLog(chatSession)
	if len(log) == 0 || log[0].Text != "line one\nline two\nline three" {
		t.Errorf("pasted text = %+v; want CR/CRLF normalized to LF", log)
	}
}

// Multi-question answers are POSITIONAL (issue #51 decision 3: answers[i]
// answers Questions[i]) with strict per-question encoding — single-select in
// Index (Selected empty; OtherText exactly when the chosen row IsOther),
// multi-select in Selected plus optional OtherText for the free-text row
// (whose index never appears in Selected). A payload in the OLD divergent
// SPA encoding (question index in Index, the single-select choice in
// Selected) must be REJECTED at the door rather than silently misplayed:
// keystrokes built from a misread answer are exactly the desync the
// verification backstop exists for, and a 4xx beats a post-hoc warning.
func TestDialogKeystrokes_multiQuestionEncodingValidation(t *testing.T) {
	d := provider.Dialog{
		Kind: provider.DialogKindQuestion, Prompt: "2 questions", Answerable: true,
		Questions: []provider.Question{
			{Text: "Color?", Options: []provider.DialogOption{{Label: "Red"}, {Label: "Blue"}, {Label: "Other", IsOther: true}}},
			{Text: "Fruits?", MultiSelect: true, Options: []provider.DialogOption{{Label: "Apple"}, {Label: "Banana"}, {Label: "Other", IsOther: true}}},
		},
	}
	cases := map[string]struct {
		answers []provider.QuestionAnswer
		want    error
	}{
		"answer count below questions": {[]provider.QuestionAnswer{{Index: 0}}, ErrInvalidReply},
		"answer count above questions": {[]provider.QuestionAnswer{{Index: 0}, {Selected: []int{0}}, {Index: 1}}, ErrInvalidReply},
		// The old divergent encoding: Index carried the QUESTION index and
		// Selected carried the single-select choice.
		"old encoding: selected on single-select": {[]provider.QuestionAnswer{{Index: 0, Selected: []int{1}}, {Index: 1, Selected: []int{0}}}, ErrInvalidReply},
		"multi-select with no answer at all":      {[]provider.QuestionAnswer{{Index: 0}, {}}, ErrInvalidReply},
		"multi-select toggling the Other row":     {[]provider.QuestionAnswer{{Index: 0}, {Selected: []int{2}}}, ErrInvalidReply},
		"multi-select multi-line other text":      {[]provider.QuestionAnswer{{Index: 0}, {Selected: []int{0}, OtherText: "a\nb"}}, ErrInvalidReply},
		"single-select index out of range":        {[]provider.QuestionAnswer{{Index: 9}, {Selected: []int{0}}}, ErrDialogNotAnswerable},
		"other row without text":                  {[]provider.QuestionAnswer{{Index: 2}, {Selected: []int{0}}}, ErrInvalidReply},
		"other text on a non-Other row":           {[]provider.QuestionAnswer{{Index: 0, OtherText: "stray"}, {Selected: []int{0}}}, ErrInvalidReply},
	}
	for name, c := range cases {
		if _, err := DialogKeystrokes(d, provider.DialogAnswer{Answers: c.answers}); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v; want %v", name, err, c.want)
		}
	}
	// The valid positional encodings pass: single-select Other text, a plain
	// multi-select toggle set, and a multi-select with the free-text row
	// riding other_text alongside a toggle (captured 2026-07-09, compat §7).
	if _, err := DialogKeystrokes(d, provider.DialogAnswer{Answers: []provider.QuestionAnswer{
		{Index: 2, OtherText: "Teal"}, {Selected: []int{0, 1}},
	}}); err != nil {
		t.Errorf("valid positional answers rejected: %v", err)
	}
	if _, err := DialogKeystrokes(d, provider.DialogAnswer{Answers: []provider.QuestionAnswer{
		{Index: 0}, {Selected: []int{0}, OtherText: "dragonfruit"},
	}}); err != nil {
		t.Errorf("multi-select with other text rejected: %v", err)
	}
}

// A flat single-question dialog accepts a one-element Answers slice with
// answers[0] semantics (API symmetry with the multi-question encoding); more
// than one element cannot address it.
func TestDialogKeystrokes_flatAcceptsSingleAnswersElement(t *testing.T) {
	d := provider.Dialog{Answerable: true, Options: []provider.DialogOption{
		{Label: "a"}, {Label: "b"}, {Label: "Other", IsOther: true},
	}}
	flat, err := DialogKeystrokes(d, provider.DialogAnswer{Index: 1})
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	viaAnswers, err := DialogKeystrokes(d, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 1}}})
	if err != nil {
		t.Fatalf("answers[0]: %v", err)
	}
	if !reflect.DeepEqual(flat, viaAnswers) {
		t.Errorf("answers[0] recipe %v != flat recipe %v", viaAnswers, flat)
	}
	if _, err := DialogKeystrokes(d, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 0}, {Index: 1}}}); !errors.Is(err, ErrInvalidReply) {
		t.Errorf("two answers on a flat dialog: err = %v; want ErrInvalidReply", err)
	}
}

// The plan picker rejects out-of-range rows and requires feedback text on the
// free-text row (row 2 of the three-row picker), mirroring the question rules.
func TestDialogKeystrokes_planValidation(t *testing.T) {
	d := provider.Dialog{Kind: provider.DialogKindPlan, Answerable: true, Options: []provider.DialogOption{
		{Label: "Approve — auto-accept edits"},
		{Label: "Approve — review each edit"},
		{Label: "Reject with feedback", IsOther: true},
	}}
	if _, err := DialogKeystrokes(d, provider.DialogAnswer{Index: 3}); !errors.Is(err, ErrDialogNotAnswerable) {
		t.Errorf("out of range: err = %v; want ErrDialogNotAnswerable", err)
	}
	if _, err := DialogKeystrokes(d, provider.DialogAnswer{Index: 2}); !errors.Is(err, ErrInvalidReply) {
		t.Errorf("feedback row without text: err = %v; want ErrInvalidReply", err)
	}
}

func TestAnswerDialog_multiSelectValidation(t *testing.T) {
	p, f := armedRunner(t)
	d := provider.Dialog{Answerable: true, Multi: true, Options: []provider.DialogOption{
		{Label: "a"}, {Label: "b"}, {Label: "Other", IsOther: true},
	}}
	cases := map[string]struct {
		sel   []int
		other string
		want  error
	}{
		// A zero-selection Enter would confirm nothing as if it answered —
		// unless the free-text row answers alone (valid, covered below).
		"empty": {nil, "", ErrInvalidReply},
		// A dropped index would confirm a selection the operator never made.
		"out of range": {[]int{0, 7}, "", ErrInvalidReply},
		// The free-text row's toggle IS its text: it rides other_text, never
		// Selected (Space on the row would type a literal space — compat §7).
		"other row toggled by index": {[]int{0, 2}, "", ErrInvalidReply},
		// The row is a one-line field; a newline records as literal \r.
		"multi-line other text": {[]int{0}, "two\nlines", ErrInvalidReply},
	}
	for name, c := range cases {
		if err := p.AnswerDialog(context.Background(), chatSession, d, provider.DialogAnswer{Selected: c.sel, OtherText: c.other}); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v; want %v", name, err, c.want)
		}
	}
	if n := len(f.KeyLog(chatSession)); n != 0 {
		t.Errorf("rejected answers still sent %d keystrokes; want 0", n)
	}
	// Free text alone (nothing toggled) is a valid multi-select answer.
	if err := p.AnswerDialog(context.Background(), chatSession, d, provider.DialogAnswer{OtherText: "kale"}); err != nil {
		t.Errorf("other-only multi-select answer rejected: %v", err)
	}
}

// --- "Chat about this" (issue #58) ------------------------------------------
//
// The action selects the picker's OWN trailing "Chat about this" row — a
// downward walk onto it, Enter — and nothing else: no Escape, no text, no
// extra wait. On a multi-question form the operator's earlier answers are
// played first, exactly as given, so the row is chosen on the question the
// operator chose it on (compat §7 "Chat about this").

var chatOther = provider.DialogOption{Label: "Other", IsOther: true}

// chatSingle / chatMulti are the two flat question shapes: three modeled rows
// each (two options + the free-text row).
func chatSingle() provider.Dialog {
	return provider.Dialog{ToolID: "toolu_single", Kind: provider.DialogKindQuestion, Prompt: "Favorite pet?", Answerable: true,
		Options: []provider.DialogOption{{Label: "Dog"}, {Label: "Cat"}, chatOther}}
}

func chatMulti() provider.Dialog {
	return provider.Dialog{ToolID: "toolu_multi", Kind: provider.DialogKindQuestion, Prompt: "Which toppings?", Answerable: true, Multi: true,
		Options: []provider.DialogOption{{Label: "Olives"}, {Label: "Onions"}, chatOther}}
}

// keys renders named keys as single-key ops ("Down" → one op each).
func keys(names ...string) []KeyOp {
	out := make([]KeyOp, 0, len(names))
	for _, n := range names {
		out = append(out, KeyOp{Named: []string{n}})
	}
	return out
}

// Flat dialogs: the walk passes every modeled row (and, on a multi-select
// picker, the Submit row), then Enter. That is the entire recipe — it ends on
// the selecting Enter, with no paste and no review step.
func TestDialogKeystrokes_chatAboutThis_flat(t *testing.T) {
	cases := map[string]struct {
		d    provider.Dialog
		a    provider.DialogAnswer
		want []KeyOp
	}{
		"single-select":                {chatSingle(), provider.DialogAnswer{Chat: true}, keys("Down", "Down", "Down", "Enter")},
		"multi-select":                 {chatMulti(), provider.DialogAnswer{Chat: true}, keys("Down", "Down", "Down", "Down", "Enter")},
		"single-select via answers[0]": {chatSingle(), provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Chat: true}}}, keys("Down", "Down", "Down", "Enter")},
	}
	for name, c := range cases {
		got, err := DialogKeystrokes(c.d, c.a)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ops = %+v; want %+v", name, got, c.want)
		}
		for _, op := range got {
			if op.Text != "" {
				t.Errorf("%s: the recipe pastes %q; want no text at all", name, op.Text)
			}
			if slices.Contains(op.Named, "Escape") {
				t.Errorf("%s: the recipe sends Escape; want the picker's own row selected", name)
			}
		}
	}
}

// A multi-question form is entered EXACTLY as the operator did it: the
// answers to the questions before the chat, each with its ordinary recipe
// (committing auto-advances), then the walk onto "Chat about this" on the
// question they chose it on — by that question's own geometry — and nothing
// after: no later question, no review screen.
func TestDialogKeystrokes_chatAboutThis_multiQuestion(t *testing.T) {
	// Color (single-select: Red, Blue, Other) then Fruits (multi-select:
	// Apple, Banana, Cherry, Other).
	form := twoQuestionDialog("toolu_form")
	three := provider.Dialog{ToolID: "toolu_3q", Kind: provider.DialogKindQuestion, Prompt: "3 questions", Answerable: true,
		Questions: []provider.Question{
			{Header: "Toppings", Text: "Which toppings?", MultiSelect: true, Options: []provider.DialogOption{{Label: "Olives"}, {Label: "Onions"}, chatOther}},
			{Header: "Size", Text: "Which size?", Options: []provider.DialogOption{{Label: "S"}, {Label: "M"}, {Label: "L"}, chatOther}},
			{Header: "Crust", Text: "Which crust?", Options: []provider.DialogOption{{Label: "Thin"}, chatOther}},
		}}
	cases := map[string]struct {
		d    provider.Dialog
		a    provider.DialogAnswer
		want []KeyOp
	}{
		// Chat on the first question: no earlier answers, Color's row 3.
		"chat on the first question": {form,
			provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Chat: true}}},
			keys("Down", "Down", "Down", "Enter")},
		// The maintainer's example: second option on the first question
		// ([Down][Enter] → auto-advance), chat about the second — Fruits is
		// multi-select with four modeled rows, so the row sits past Submit: 5.
		"second option, then chat about the second question": {form,
			provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 1}, {Chat: true}}},
			keys("Down", "Enter" /* Blue */, "Down", "Down", "Down", "Down", "Down", "Enter" /* Chat about this */)},
		// A multi-select answer first (Space on Olives, 3 Downs onto Submit,
		// Enter), a single-select pick ([Down][Down][Enter] → L), then chat on
		// the third question: Thin, Other | Chat about this → row 2.
		"two answers, then chat about the third question": {three,
			provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Selected: []int{0}}, {Index: 2}, {Chat: true}}},
			keys("Space", "Down", "Down", "Down", "Enter", "Down", "Down", "Enter", "Down", "Down", "Enter")},
	}
	for name, c := range cases {
		got, err := DialogKeystrokes(c.d, c.a)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ops = %+v; want %+v", name, got, c.want)
		}
	}

	// A free-text answer before the chat is entered too (type-first row).
	got, err := DialogKeystrokes(form, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 2, OtherText: "Teal"}, {Chat: true}}})
	if err != nil {
		t.Fatalf("free text then chat: %v", err)
	}
	want := append(keys("Down", "Down"), KeyOp{Text: "Teal"})
	want = append(want, keys("Enter", "Down", "Down", "Down", "Down", "Down", "Enter")...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("free text then chat: ops = %+v; want %+v", got, want)
	}
}

// Every refusal happens at the door with the provider sentinel the API maps
// (ErrInvalidReply → 400, ErrDialogNotAnswerable → 409).
func TestDialogKeystrokes_chatAboutThis_rejects(t *testing.T) {
	single := chatSingle()
	form := twoQuestionDialog("toolu_form")
	unanswerable := single
	unanswerable.Answerable = false
	rowless := single
	rowless.Options = nil
	chat := provider.QuestionAnswer{Chat: true}
	cases := map[string]struct {
		d    provider.Dialog
		a    provider.DialogAnswer
		want error
	}{
		"flat: with other_text":             {single, provider.DialogAnswer{Chat: true, OtherText: "Ferret"}, ErrInvalidReply},
		"flat: with selected":               {single, provider.DialogAnswer{Chat: true, Selected: []int{0}}, ErrInvalidReply},
		"flat: with index":                  {single, provider.DialogAnswer{Chat: true, Index: 1}, ErrInvalidReply},
		"flat: chat beside answers":         {single, provider.DialogAnswer{Chat: true, Answers: []provider.QuestionAnswer{{Index: 0}}}, ErrInvalidReply},
		"flat: plan dialog":                 {planTestDialog("toolu_plan"), provider.DialogAnswer{Chat: true}, ErrDialogNotAnswerable},
		"flat: approval kind":               {provider.Dialog{Kind: provider.DialogKindApproval, Answerable: true, Options: []provider.DialogOption{{Label: "yes"}}}, provider.DialogAnswer{Chat: true}, ErrDialogNotAnswerable},
		"flat: non-answerable dialog":       {unanswerable, provider.DialogAnswer{Chat: true}, ErrDialogNotAnswerable},
		"flat: no modeled rows":             {rowless, provider.DialogAnswer{Chat: true}, ErrDialogNotAnswerable},
		"form: top-level chat":              {form, provider.DialogAnswer{Chat: true}, ErrInvalidReply},
		"form: top-level chat and answers":  {form, provider.DialogAnswer{Chat: true, Answers: []provider.QuestionAnswer{{Index: 0}, chat}}, ErrInvalidReply},
		"form: an answer after the chat":    {form, provider.DialogAnswer{Answers: []provider.QuestionAnswer{chat, {Selected: []int{0}}}}, ErrInvalidReply},
		"form: more entries than questions": {form, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 0}, {Selected: []int{0}}, chat}}, ErrInvalidReply},
		"form: chat entry with an option":   {form, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Chat: true, Index: 1}}}, ErrInvalidReply},
		"form: chat entry with text":        {form, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Chat: true, OtherText: "x"}}}, ErrInvalidReply},
		"form: bad earlier answer":          {form, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Selected: []int{0}}, chat}}, ErrInvalidReply},
		"form: short answers without chat":  {form, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 0}}}, ErrInvalidReply},
	}
	for name, c := range cases {
		if ops, err := DialogKeystrokes(c.d, c.a); !errors.Is(err, c.want) {
			t.Errorf("%s: ops = %+v, err = %v; want %v", name, ops, err, c.want)
		}
	}
}

// AnswerDialog plays the chat recipe through the runner like any answer —
// keys only, never a paste — and records a chat intent naming the questions
// that must stay unanswered.
func TestAnswerDialog_chatAboutThis_playsTheOperatorsPath(t *testing.T) {
	p, f := armedRunner(t)
	form := twoQuestionDialog("toolu_form")
	answer := provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 1}, {Chat: true}}}
	if err := p.AnswerDialog(context.Background(), chatSession, form, answer); err != nil {
		t.Fatalf("AnswerDialog: %v", err)
	}
	var want []tmuxx.KeyEvent
	for _, k := range []string{"Down", "Enter", "Down", "Down", "Down", "Down", "Down", "Enter"} {
		want = append(want, tmuxx.KeyEvent{Kind: "keys", Keys: k})
	}
	if got := f.KeyLog(chatSession); !reflect.DeepEqual(got, want) {
		t.Errorf("key log = %+v; want %+v", got, want)
	}
	in, ok := p.intents.byID["toolu_form"]
	if !ok || !in.chat || !reflect.DeepEqual(in.unanswered, []string{"Which fruits do you like?"}) || len(in.answers) != 0 {
		t.Errorf("intent = %+v (recorded %v); want a chat intent for the Fruits question", in, ok)
	}

	// Flat: the single question itself must stay unanswered.
	p, f = armedRunner(t)
	if err := p.AnswerDialog(context.Background(), chatSession, chatSingle(), provider.DialogAnswer{Chat: true}); err != nil {
		t.Fatalf("AnswerDialog(flat): %v", err)
	}
	if got := len(f.KeyLog(chatSession)); got != 4 {
		t.Errorf("flat chat played %d key events; want 4 (three Downs and the Enter)", got)
	}
	if in := p.intents.byID["toolu_single"]; !in.chat || !reflect.DeepEqual(in.unanswered, []string{"Favorite pet?"}) {
		t.Errorf("flat intent = %+v; want a chat intent for the question itself", in)
	}
}

// A refused chat answer plays nothing at all — not one key of the walk
// (which on its own would leave the picker's cursor off the top row) — and
// records no intent.
func TestAnswerDialog_chatAboutThis_rejectedPlaysNothing(t *testing.T) {
	p, f := armedRunner(t)
	form := twoQuestionDialog("toolu_form")
	for _, c := range []struct {
		d provider.Dialog
		a provider.DialogAnswer
	}{
		{chatSingle(), provider.DialogAnswer{Chat: true, OtherText: "Ferret"}},
		{chatSingle(), provider.DialogAnswer{Chat: true, Selected: []int{1}}},
		{form, provider.DialogAnswer{Chat: true}},
		{form, provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Chat: true}, {Selected: []int{0}}}}},
		{planTestDialog("toolu_plan"), provider.DialogAnswer{Chat: true}},
	} {
		if err := p.AnswerDialog(context.Background(), chatSession, c.d, c.a); err == nil {
			t.Errorf("AnswerDialog(%+v) = nil; want a refusal", c.a)
		}
	}
	if log := f.KeyLog(chatSession); len(log) != 0 {
		t.Errorf("refused chat answers still played %+v; want nothing", log)
	}
	if n := len(p.intents.byID); n != 0 {
		t.Errorf("refused chat answers recorded %d intents; want none", n)
	}
}

// The chat recipe is paced like every recipe: keyDelay sits before each key
// after the first, so a request cancelled there stops after ONE key — the
// picker never sees a burst.
func TestAnswerDialog_chatAboutThis_isPaced(t *testing.T) {
	p, f := armedRunner(t)
	p.keyDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.AnswerDialog(ctx, chatSession, chatMulti(), provider.DialogAnswer{Chat: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AnswerDialog with a cancelled context = %v; want context.Canceled", err)
	}
	want := []tmuxx.KeyEvent{{Kind: "keys", Keys: "Down"}}
	if got := f.KeyLog(chatSession); !reflect.DeepEqual(got, want) {
		t.Errorf("key log = %+v; want exactly one Down (stopped at the first keyDelay)", got)
	}
}
