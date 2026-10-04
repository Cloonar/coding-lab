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
// The mechanism selects the picker's OWN trailing "Chat about this" row — a
// downward walk onto it, Enter — then sends the message with the §6 reply
// (paste, Enter). No Escape is involved (compat §7 "Chat about this").

// chatQuestionDialogs are the three answerable question shapes the dock
// offers "Chat about this" on: flat single-select, flat multi-select, and a
// multi-question form.
func chatQuestionDialogs() map[string]provider.Dialog {
	return map[string]provider.Dialog{
		"single-select": {ToolID: "toolu_single", Kind: provider.DialogKindQuestion, Prompt: "Favorite pet?", Answerable: true,
			Options: []provider.DialogOption{{Label: "Dog"}, {Label: "Cat"}, {Label: "Other", IsOther: true}}},
		"multi-select": {ToolID: "toolu_multi", Kind: provider.DialogKindQuestion, Prompt: "Which toppings?", Answerable: true, Multi: true,
			Options: []provider.DialogOption{{Label: "Olives"}, {Label: "Onions"}, {Label: "Other", IsOther: true}}},
		"multi-question": twoQuestionDialog("toolu_form"),
	}
}

// chatRowWalk is how many Down presses reach the "Chat about this" row of
// each chatQuestionDialogs shape (compat §7 row models): past the three
// modeled rows of a single-select picker; past the three modeled rows AND
// the Submit row of a multi-select picker; and, for the form, past the three
// modeled rows of its FIRST question (single-select "Color").
var chatRowWalk = map[string]int{"single-select": 3, "multi-select": 4, "multi-question": 3}

// chatOps is the expected recipe: n single-key Down ops, the Enter that
// selects the row, the paste, the submitting Enter.
func chatOps(n int, text string) []KeyOp {
	ops := downOps(n)
	return append(ops, KeyOp{Named: []string{"Enter"}}, KeyOp{Text: text}, KeyOp{Named: []string{"Enter"}})
}

// chatKeyLog is chatOps as the fake runner logs it.
func chatKeyLog(n int, text string) []tmuxx.KeyEvent {
	var log []tmuxx.KeyEvent
	for range n {
		log = append(log, tmuxx.KeyEvent{Kind: "keys", Keys: "Down"})
	}
	return append(log,
		tmuxx.KeyEvent{Kind: "keys", Keys: "Enter"},
		tmuxx.KeyEvent{Kind: "paste", Text: text},
		tmuxx.KeyEvent{Kind: "keys", Keys: "Enter"})
}

// The builder walks DOWN onto the picker's trailing "Chat about this" row —
// one Down per op, never an Escape — selects it with Enter, then appends the
// reply. The walk length follows the picker shape. The pasted text is the
// validated reply: trimmed, CRLF normalized, newlines kept (it is a composer
// reply, not a single-line picker row).
func TestChatAboutThisKeystrokes_shapes(t *testing.T) {
	for name, d := range chatQuestionDialogs() {
		got, err := ChatAboutThisKeystrokes(d, provider.DialogAnswer{ChatText: "  wait — why those options?\r\nAlso: cost?  "})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := chatOps(chatRowWalk[name], "wait — why those options?\nAlso: cost?")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ops = %+v; want %+v", name, got, want)
		}
		for _, op := range got {
			if len(op.Named) > 1 {
				t.Errorf("%s: op %+v batches keys; want one named key per op", name, op)
			}
			if slices.Contains(op.Named, "Escape") {
				t.Errorf("%s: the recipe sends Escape; want the picker's own row selected instead", name)
			}
		}
	}
}

// The walk length is the picker's row count, not a constant: it grows with the
// option list, and a multi-question form uses its FIRST question's geometry —
// here a multi-select one, so the walk also passes the Submit row.
func TestChatAboutThisKeystrokes_walkFollowsTheRowModel(t *testing.T) {
	other := provider.DialogOption{Label: "Other", IsOther: true}
	five := provider.Dialog{Kind: provider.DialogKindQuestion, Prompt: "Pick one", Answerable: true,
		Options: []provider.DialogOption{{Label: "A"}, {Label: "B"}, {Label: "C"}, {Label: "D"}, other}}
	multiFirst := provider.Dialog{Kind: provider.DialogKindQuestion, Prompt: "2 questions", Answerable: true,
		Questions: []provider.Question{
			{Header: "Fruits", Text: "Which fruits?", MultiSelect: true, Options: []provider.DialogOption{{Label: "Apple"}, {Label: "Cherry"}, other}},
			{Header: "Color", Text: "Which color?", Options: []provider.DialogOption{{Label: "Red"}, other}},
		}}
	for name, c := range map[string]struct {
		d    provider.Dialog
		walk int
	}{
		"five modeled rows":            {five, 5},
		"form opening on multi-select": {multiFirst, 4},
	} {
		got, err := ChatAboutThisKeystrokes(c.d, provider.DialogAnswer{ChatText: "hi"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := chatOps(c.walk, "hi"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ops = %+v; want %+v", name, got, want)
		}
	}
}

// Every refusal happens at the door with the provider sentinel the API maps
// (ErrInvalidReply → 400, ErrDialogNotAnswerable → 409).
func TestChatAboutThisKeystrokes_rejects(t *testing.T) {
	single := chatQuestionDialogs()["single-select"]
	form := chatQuestionDialogs()["multi-question"]
	plan := planTestDialog("toolu_plan")
	unanswerable := single
	unanswerable.Answerable = false
	rowless := single
	rowless.Options = nil
	cases := map[string]struct {
		d    provider.Dialog
		a    provider.DialogAnswer
		want error
	}{
		"empty text":              {single, provider.DialogAnswer{ChatText: ""}, ErrInvalidReply},
		"whitespace text":         {single, provider.DialogAnswer{ChatText: " \n\t "}, ErrInvalidReply},
		"control character":       {single, provider.DialogAnswer{ChatText: "hi\x1b[2J"}, ErrInvalidReply},
		"oversize text":           {single, provider.DialogAnswer{ChatText: strings.Repeat("x", maxReplyLen+1)}, ErrInvalidReply},
		"with other_text":         {single, provider.DialogAnswer{ChatText: "hi", OtherText: "Ferret"}, ErrInvalidReply},
		"with selected":           {single, provider.DialogAnswer{ChatText: "hi", Selected: []int{0}}, ErrInvalidReply},
		"with answers":            {form, provider.DialogAnswer{ChatText: "hi", Answers: []provider.QuestionAnswer{{Index: 0}, {Selected: []int{0}}}}, ErrInvalidReply},
		"plan dialog":             {plan, provider.DialogAnswer{ChatText: "hi"}, ErrDialogNotAnswerable},
		"non-answerable dialog":   {unanswerable, provider.DialogAnswer{ChatText: "hi"}, ErrDialogNotAnswerable},
		"no modeled rows":         {rowless, provider.DialogAnswer{ChatText: "hi"}, ErrDialogNotAnswerable},
		"unknown kind (approval)": {provider.Dialog{Kind: provider.DialogKindApproval, Answerable: true, Options: []provider.DialogOption{{Label: "yes"}}}, provider.DialogAnswer{ChatText: "hi"}, ErrDialogNotAnswerable},
	}
	for name, c := range cases {
		if ops, err := ChatAboutThisKeystrokes(c.d, c.a); !errors.Is(err, c.want) {
			t.Errorf("%s: ops = %+v, err = %v; want %v", name, ops, err, c.want)
		}
	}
}

// AnswerDialog with chat_text plays the sequence through the runner, in
// order: the Down walk onto the row, Enter, the paste, Enter — and records a
// "no option picked" backstop intent, never a pick.
func TestAnswerDialog_chatAboutThis_playsRowWalkPasteEnter(t *testing.T) {
	for name, d := range chatQuestionDialogs() {
		p, f := armedRunner(t)
		if err := p.AnswerDialog(context.Background(), chatSession, d, provider.DialogAnswer{ChatText: "let's discuss first"}); err != nil {
			t.Fatalf("%s: AnswerDialog: %v", name, err)
		}
		want := chatKeyLog(chatRowWalk[name], "let's discuss first")
		if got := f.KeyLog(chatSession); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: key log = %+v; want %+v", name, got, want)
		}
		in, ok := p.intents.byID[d.ToolID]
		if !ok || !in.chat || len(in.answers) != 0 {
			t.Errorf("%s: intent = %+v (recorded %v); want a chat intent with no expected answers", name, in, ok)
		}
	}
}

// A refused chat answer plays nothing at all — not one key of the walk
// (which on its own would leave the picker's cursor off the top row).
func TestAnswerDialog_chatAboutThis_rejectedPlaysNothing(t *testing.T) {
	p, f := armedRunner(t)
	single := chatQuestionDialogs()["single-select"]
	for _, c := range []struct {
		d provider.Dialog
		a provider.DialogAnswer
	}{
		{single, provider.DialogAnswer{ChatText: "   "}},
		{single, provider.DialogAnswer{ChatText: "bell\a"}},
		{single, provider.DialogAnswer{ChatText: "hi", OtherText: "Ferret"}},
		{single, provider.DialogAnswer{ChatText: "hi", Selected: []int{1}}},
		{planTestDialog("toolu_plan"), provider.DialogAnswer{ChatText: "hi"}},
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

// The chat settle sits between the Enter that selects the row and the paste,
// and honours context cancellation: a request cancelled there has selected the
// row but pasted nothing, so no text ever reaches a picker that may not have
// closed yet.
func TestAnswerDialog_chatAboutThis_settleRespectsCancel(t *testing.T) {
	p, f := armedRunner(t)
	p.chatSettleDelay = time.Hour // make the chat settle effectively block
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := chatQuestionDialogs()["single-select"]
	if err := p.AnswerDialog(ctx, chatSession, d, provider.DialogAnswer{ChatText: "hold on"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AnswerDialog with a cancelled context = %v; want context.Canceled", err)
	}
	want := []tmuxx.KeyEvent{
		{Kind: "keys", Keys: "Down"}, {Kind: "keys", Keys: "Down"}, {Kind: "keys", Keys: "Down"},
		{Kind: "keys", Keys: "Enter"},
	}
	if got := f.KeyLog(chatSession); !reflect.DeepEqual(got, want) {
		t.Errorf("key log = %+v; want the walk and the selecting Enter only (stopped at the chat settle, before the paste)", got)
	}
}

// The walk is paced like every picker walk: keyDelay sits before each key
// after the first, so a request cancelled there stops after ONE Down — the
// picker never sees a burst.
func TestAnswerDialog_chatAboutThis_walkIsPaced(t *testing.T) {
	p, f := armedRunner(t)
	p.keyDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := chatQuestionDialogs()["multi-select"]
	if err := p.AnswerDialog(ctx, chatSession, d, provider.DialogAnswer{ChatText: "hold on"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AnswerDialog with a cancelled context = %v; want context.Canceled", err)
	}
	want := []tmuxx.KeyEvent{{Kind: "keys", Keys: "Down"}}
	if got := f.KeyLog(chatSession); !reflect.DeepEqual(got, want) {
		t.Errorf("key log = %+v; want exactly one Down (stopped at the first keyDelay)", got)
	}
}

// The chat path keeps the pre-first-key settle every dialog recipe gets: a
// request cancelled there plays nothing, not even the first Down.
func TestAnswerDialog_chatAboutThis_preFirstKeySettleRespectsCancel(t *testing.T) {
	p, f := armedRunner(t)
	p.settleDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := chatQuestionDialogs()["multi-question"]
	if err := p.AnswerDialog(ctx, chatSession, d, provider.DialogAnswer{ChatText: "hold on"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AnswerDialog with a cancelled context = %v; want context.Canceled", err)
	}
	if log := f.KeyLog(chatSession); len(log) != 0 {
		t.Errorf("played %+v despite the cancelled pre-first-key settle; want nothing", log)
	}
}
