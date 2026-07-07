package claudecode

// Reply / AnswerDialog / Interrupt drive the tmux SessionRunner; these assert
// the exact keystroke deliveries (the KeyLog) against a fake, plus the reply
// validation. The dialog recipe itself is snapshot-pinned in internal/compat.

import (
	"context"
	"errors"
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
	// Up Up Up / Down / Enter — normalize-then-navigate. The climb is
	// len(Options)=3 (2 modeled options + Other) plus the un-modeled trailing
	// "Chat about this" row (ADR-0020 / compat §7): 3−1+1 = 3 Ups.
	var keys []string
	for _, e := range f.KeyLog(chatSession) {
		keys = append(keys, e.Keys)
	}
	if got := strings.Join(keys, "|"); got != "Up Up Up|Down|Enter" {
		t.Errorf("answer key log = %q; want \"Up Up Up|Down|Enter\"", got)
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
	d := provider.Dialog{Answerable: false, DialogKind: "plan"}
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

func TestAnswerDialog_multiSelectValidation(t *testing.T) {
	p, f := armedRunner(t)
	d := provider.Dialog{Answerable: true, Multi: true, Options: []provider.DialogOption{
		{Label: "a"}, {Label: "b"}, {Label: "Other", IsOther: true},
	}}
	cases := map[string]struct {
		sel  []int
		want error
	}{
		// A zero-selection Enter would confirm nothing as if it answered.
		"empty": {nil, ErrInvalidReply},
		// A dropped index would confirm a selection the operator never made.
		"out of range": {[]int{0, 7}, ErrInvalidReply},
		// Space on the free-text Other row is an uncaptured TUI path.
		"other row": {[]int{0, 2}, ErrDialogNotAnswerable},
	}
	for name, c := range cases {
		if err := p.AnswerDialog(context.Background(), chatSession, d, provider.DialogAnswer{Selected: c.sel}); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v; want %v", name, err, c.want)
		}
	}
	if n := len(f.KeyLog(chatSession)); n != 0 {
		t.Errorf("rejected answers still sent %d keystrokes; want 0", n)
	}
}
