package claudecode

// Reply / AnswerDialog / Interrupt drive the tmux SessionRunner; these assert
// the exact keystroke deliveries (the KeyLog) against a fake, plus the reply
// validation. The dialog recipe itself is snapshot-pinned in internal/compat.

import (
	"context"
	"errors"
	"reflect"
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
// free-text row (row 3), mirroring the question rules.
func TestDialogKeystrokes_planValidation(t *testing.T) {
	d := provider.Dialog{Kind: provider.DialogKindPlan, Answerable: true, Options: []provider.DialogOption{
		{Label: "Approve — auto-accept edits"},
		{Label: "Approve — review each edit"},
		{Label: "Reject — refine the plan"},
		{Label: "Reject with feedback", IsOther: true},
	}}
	if _, err := DialogKeystrokes(d, provider.DialogAnswer{Index: 4}); !errors.Is(err, ErrDialogNotAnswerable) {
		t.Errorf("out of range: err = %v; want ErrDialogNotAnswerable", err)
	}
	if _, err := DialogKeystrokes(d, provider.DialogAnswer{Index: 3}); !errors.Is(err, ErrInvalidReply) {
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
