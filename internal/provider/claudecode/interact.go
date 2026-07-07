package claudecode

// Send-keys recipes for the chat surface (issue #7 decisions 4–6). Each is a
// fragile Claude Code TUI coupling pinned in internal/compat: the mid-session
// reply (§6), the dialog answer keystrokes (§7), and the Escape interrupt
// (§8). The recipes are built as pure []KeyOp sequences so the compat test can
// snapshot them; AnswerDialog just plays the sequence through the runner.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// maxReplyLen caps a free-text reply before it reaches the paste buffer — a
// generous ceiling that still stops a runaway paste (v0 login-code cap, scaled
// up for prose).
const maxReplyLen = 100_000

// ErrDialogNotAnswerable is returned by AnswerDialog for a dialog lab cannot
// drive from structured input (multi-question, plan approval, unknown). The UI
// shows the claude.ai deep-link hint for these instead of option buttons.
var ErrDialogNotAnswerable = errors.New("dialog is not answerable from lab; open it in claude.ai")

// ErrInvalidReply wraps a rejected reply (empty after trim, oversize, or a
// forbidden control character). The API layer maps it to 400.
var ErrInvalidReply = errors.New("invalid reply")

// KeyOp is one step of a send-keys recipe: either a set of tmux key names sent
// in one call (Named), or literal text delivered as a bracketed paste (Text).
type KeyOp struct {
	Named []string
	Text  string
}

// Reply implements provider.AgentProvider: deliver a free-text reply to the
// live session. The text is bracketed-pasted (embedded newlines insert lines,
// not submissions — the argv-only stance is for the initial prompt only), then
// a single Enter submits. A mid-turn agent queues it in its own TUI.
func (p *Provider) Reply(ctx context.Context, sessionName, text string) error {
	clean, err := validateReply(text)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidReply, err)
	}
	if err := p.runner.PasteText(ctx, sessionName, clean); err != nil {
		return fmt.Errorf("paste reply: %w", err)
	}
	if err := p.runner.SendNamedKeys(ctx, sessionName, "Enter"); err != nil {
		return fmt.Errorf("submit reply: %w", err)
	}
	return nil
}

// Interrupt implements provider.AgentProvider: send the interrupt keystroke
// (Escape), the chat's stop-generating affordance. Distinct from a run Stop —
// it does not touch the session, the budget clock, the claim, or the
// three-strikes counter (issue #7 decision 12).
func (p *Provider) Interrupt(ctx context.Context, sessionName string) error {
	if err := p.runner.SendNamedKeys(ctx, sessionName, "Escape"); err != nil {
		return fmt.Errorf("interrupt: %w", err)
	}
	return nil
}

// AnswerDialog implements provider.AgentProvider: play the pinned keystroke
// recipe for the operator's selection into the live session's picker.
func (p *Provider) AnswerDialog(ctx context.Context, sessionName string, dialog provider.Dialog, answer provider.DialogAnswer) error {
	ops, err := DialogKeystrokes(dialog, answer)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Text != "" {
			if err := p.runner.PasteText(ctx, sessionName, op.Text); err != nil {
				return fmt.Errorf("answer dialog (paste): %w", err)
			}
			continue
		}
		if err := p.runner.SendNamedKeys(ctx, sessionName, op.Named...); err != nil {
			return fmt.Errorf("answer dialog (keys): %w", err)
		}
	}
	return nil
}

// DialogKeystrokes builds the send-keys recipe that answers d with answer
// (compat.md §7). The picker is normalised to its top row (Up × rows−1) before
// navigating down, so the cursor's start position never matters. Exported for
// the compat snapshot test.
func DialogKeystrokes(d provider.Dialog, answer provider.DialogAnswer) ([]KeyOp, error) {
	if !d.Answerable {
		return nil, ErrDialogNotAnswerable
	}
	rows := len(d.Options)
	if rows == 0 {
		return nil, ErrDialogNotAnswerable
	}

	var ops []KeyOp
	if up := repeat("Up", rows-1); len(up) > 0 {
		ops = append(ops, KeyOp{Named: up}) // normalise cursor to the top option
	}

	if d.Multi {
		sel, err := normSelected(d, answer.Selected)
		if err != nil {
			return nil, err
		}
		cur := 0
		for _, idx := range sel {
			if down := repeat("Down", idx-cur); len(down) > 0 {
				ops = append(ops, KeyOp{Named: down})
			}
			ops = append(ops, KeyOp{Named: []string{"Space"}}) // toggle this option on
			cur = idx
		}
		return append(ops, KeyOp{Named: []string{"Enter"}}), nil // confirm the multi-select
	}

	if answer.Index < 0 || answer.Index >= rows {
		return nil, fmt.Errorf("%w: option index %d out of range [0,%d)", ErrDialogNotAnswerable, answer.Index, rows)
	}
	if down := repeat("Down", answer.Index); len(down) > 0 {
		ops = append(ops, KeyOp{Named: down})
	}
	ops = append(ops, KeyOp{Named: []string{"Enter"}}) // select the row

	if d.Options[answer.Index].IsOther {
		clean, err := validateReply(answer.OtherText)
		if err != nil {
			return nil, fmt.Errorf("%w: other text: %w", ErrInvalidReply, err)
		}
		ops = append(ops, KeyOp{Text: clean}, KeyOp{Named: []string{"Enter"}}) // type the free text, submit
	}
	return ops, nil
}

// normSelected validates and normalizes a multi-select answer: every index
// must be a real, non-Other option (a silently dropped index would confirm a
// selection the operator never made, and Space on the free-text Other row is
// an uncaptured TUI path — degrade that to the deep link). The result is
// ascending and de-duplicated so the walk only ever moves down.
func normSelected(d provider.Dialog, sel []int) ([]int, error) {
	if len(sel) == 0 {
		return nil, fmt.Errorf("%w: no options selected", ErrInvalidReply)
	}
	rows := len(d.Options)
	seen := map[int]bool{}
	var out []int
	for _, i := range sel {
		if i < 0 || i >= rows {
			return nil, fmt.Errorf("%w: option index %d out of range [0,%d)", ErrInvalidReply, i, rows)
		}
		if d.Options[i].IsOther {
			return nil, fmt.Errorf("%w: the Other option cannot be toggled in a multi-select", ErrDialogNotAnswerable)
		}
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	// insertion sort — the set is tiny (a handful of options)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func repeat(key string, n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = key
	}
	return out
}

// validateReply trims and sanity-checks free text before it reaches the paste
// buffer. CRLF (and stray CR) normalize to LF first — an API client sending
// Windows line endings means newlines, not control characters. Newlines and
// tabs are legitimate message content (bracketed paste inserts them
// literally); other control characters are rejected so a stray escape can't
// break out of the composer.
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
