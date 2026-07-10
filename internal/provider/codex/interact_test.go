package codex

// Reply / Interrupt drive the tmux SessionRunner; these assert the exact
// keystroke deliveries (the KeyLog) against the fake, plus the reply
// validation and the no-dialogs stance.

import (
	"context"
	"errors"
	"testing"

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

// The reply recipe: one bracketed paste (multi-line text lands WITHOUT
// submitting — live 0.133.0), then a SEPARATE Enter. Two bursts, never one:
// merged delivery is the live-verified slash-command hazard.
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

// Slash-command text rides the same two-burst recipe (paste, then Enter) —
// the command must never merge with subsequent input.
func TestReply_slashCommandStaysTwoBursts(t *testing.T) {
	p, f := armedRunner(t)
	if err := p.Reply(context.Background(), chatSession, "/new"); err != nil {
		t.Fatalf("Reply(/new): %v", err)
	}
	log := f.KeyLog(chatSession)
	if len(log) != 2 || log[0].Kind != "paste" || log[0].Text != "/new" ||
		log[1].Kind != "keys" || log[1].Keys != "Enter" {
		t.Errorf("slash-command key log = %+v; want separate paste + Enter", log)
	}
}

func TestReply_rejectsEmptyAndControl(t *testing.T) {
	p, f := armedRunner(t)
	for _, bad := range []string{"   ", "has\x1bescape"} {
		if err := p.Reply(context.Background(), chatSession, bad); !errors.Is(err, provider.ErrInvalidReply) {
			t.Errorf("Reply(%q) err = %v; want ErrInvalidReply", bad, err)
		}
	}
	if n := len(f.KeyLog(chatSession)); n != 0 {
		t.Errorf("rejected replies still sent %d keystrokes; want 0", n)
	}
}

// Exactly ONE Escape: a single Esc mid-turn interrupts cleanly (rollout
// records turn_aborted reason=interrupted); a second Esc would arm
// backtrack, and Ctrl-C would QUIT codex outright — neither may ever be
// sent.
func TestInterrupt_singleEscape(t *testing.T) {
	p, f := armedRunner(t)
	if err := p.Interrupt(context.Background(), chatSession); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	log := f.KeyLog(chatSession)
	if len(log) != 1 || log[0].Kind != "keys" || log[0].Keys != "Escape" {
		t.Errorf("interrupt key log = %+v; want a single Escape", log)
	}
}

// No answerable dialogs exist in v1 (never-ask posture, no structured
// question tool): AnswerDialog always refuses, and no keys ever play.
func TestAnswerDialog_neverAnswerable(t *testing.T) {
	p, f := armedRunner(t)
	d := provider.Dialog{Answerable: true, Options: []provider.DialogOption{{Label: "yes"}}}
	err := p.AnswerDialog(context.Background(), chatSession, d, provider.DialogAnswer{Index: 0})
	if !errors.Is(err, provider.ErrDialogNotAnswerable) {
		t.Errorf("AnswerDialog err = %v; want ErrDialogNotAnswerable", err)
	}
	if n := len(f.KeyLog(chatSession)); n != 0 {
		t.Errorf("AnswerDialog sent %d keystrokes; want 0", n)
	}
}
