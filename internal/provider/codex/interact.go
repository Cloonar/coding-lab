package codex

// Send-keys recipes for the chat surface, live-verified against the codex
// 0.133.0 TUI (issue #87, 2026-07-10): the mid-session reply (bracketed
// paste + paced Enter) and the single-Escape interrupt. There are no
// answerable dialogs in v1 — codex runs under the never-ask posture
// (--ask-for-approval never) and has no structured question tool — so
// AnswerDialog is a hard ErrDialogNotAnswerable.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// maxReplyLen caps a free-text reply before it reaches the paste buffer
// (claudecode's pinned ceiling).
const maxReplyLen = 100_000

// Reply implements provider.AgentProvider: deliver a free-text reply to the
// live session. The text is bracketed-pasted — LIVE-VERIFIED on 0.133.0:
// bracketed paste lands multi-line text WITHOUT submitting — then, after the
// keyDelay settling gap, a single Enter submits.
//
// Mid-turn replies are legal: codex queues-with-steer, so lab never gates a
// reply on conversational state and there is no typed drop error.
//
// LIVE HAZARD (0.133.0): slash-command text and its Enter MUST remain
// separate paced bursts — delivered in one burst the command merges with
// subsequent input. This recipe already satisfies that (paste, delay,
// Enter); never collapse the three steps.
func (p *Provider) Reply(ctx context.Context, sessionName, text string) error {
	clean, err := validateReply(text)
	if err != nil {
		return fmt.Errorf("%w: %w", provider.ErrInvalidReply, err)
	}
	if err := p.runner.PasteText(ctx, sessionName, clean); err != nil {
		return fmt.Errorf("paste reply: %w", err)
	}
	if p.keyDelay > 0 && !sleepOrDone(ctx, p.keyDelay) {
		return ctx.Err()
	}
	if err := p.runner.SendNamedKeys(ctx, sessionName, "Enter"); err != nil {
		return fmt.Errorf("submit reply: %w", err)
	}
	return nil
}

// Interrupt implements provider.AgentProvider: exactly ONE Escape, the
// chat's stop-generating affordance. Hazards, live-checked on 0.133.0
// (issue #51 decision 10's bar):
//
//   - A single Esc mid-turn interrupts cleanly — it lands in the rollout as
//     turn_aborted reason=interrupted.
//   - Esc on an IDLE composer arms backtrack (a second Esc opens an overlay;
//     q recovers) — lab's chat layer only offers interrupt while the state
//     is working, so a stray double-Esc backtrack cannot arise from the UI.
//   - NEVER Ctrl-C: codex quits immediately, no confirmation — a chat
//     interrupt must never be able to kill the session.
func (p *Provider) Interrupt(ctx context.Context, sessionName string) error {
	if err := p.runner.SendNamedKeys(ctx, sessionName, "Escape"); err != nil {
		return fmt.Errorf("interrupt: %w", err)
	}
	return nil
}

// AnswerDialog implements provider.AgentProvider: no answerable dialogs
// exist in v1 — the never-ask spawn posture keeps approval prompts out and
// codex has no structured question tool — so every dialog is unanswerable
// from lab (the parser never emits MessageDialog either).
func (p *Provider) AnswerDialog(context.Context, string, provider.Dialog, provider.DialogAnswer) error {
	return provider.ErrDialogNotAnswerable
}

// validateReply trims and sanity-checks free text before it reaches the
// paste buffer (claudecode's pinned rules). CRLF (and stray CR) normalize to
// LF first — an API client sending Windows line endings means newlines, not
// control characters. Newlines and tabs are legitimate message content
// (bracketed paste inserts them literally); other control characters are
// rejected so a stray escape can't break out of the composer.
func validateReply(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", errors.New("empty reply")
	}
	if len(text) > maxReplyLen {
		return "", errors.New("reply too long")
	}
	for _, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return "", errors.New("reply contains a control character")
		}
	}
	return text, nil
}
