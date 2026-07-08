package claudecode

// Send-keys recipes for the chat surface (issue #7 decisions 4–6, dialog
// shapes generalized by issue #51 decision 3). Each is a fragile Claude Code
// TUI coupling pinned in internal/compat: the mid-session reply (§6), the
// dialog answer keystrokes (§7), and the Escape interrupt (§8). The recipes
// are built as pure []KeyOp sequences so the compat test can snapshot them;
// AnswerDialog just plays the sequence through the runner (and records the
// intended answer for the post-resolve verification backstop, dialogintent.go).

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

// ErrDialogNotAnswerable / ErrInvalidReply are thin aliases of the
// provider-generic sentinels (issue #51 decision 7 moved them to the seam so
// httpapi needs no claudecode import); package-internal wrap sites and
// errors.Is chains keep working unchanged. See the provider package for the
// contract docs.
var (
	ErrDialogNotAnswerable = provider.ErrDialogNotAnswerable
	ErrInvalidReply        = provider.ErrInvalidReply
)

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
//
// The submit is paced by p.keyDelay exactly like the dialog recipes: an Enter
// sent in the same instant as a multi-line paste races the composer's paste
// processing and is dropped — the text sits unsubmitted (LIVE-OBSERVED
// 2026-07-08 on 2.1.198: a ~600-char 7-line paste followed by an immediate
// Enter never sent; the same paste with a settling gap submits reliably,
// compat §6). Short replies never noticed because the race window scales with
// the paste size.
func (p *Provider) Reply(ctx context.Context, sessionName, text string) error {
	clean, err := validateReply(text)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidReply, err)
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
// recipe for the operator's selection into the live session's picker. A pinned
// gap (p.keyDelay, compat §7) separates each op: over the remote-control bridge
// a committing Enter sent immediately after a Down navigation intermittently
// races ahead of it and selects the wrong row (live-verified 2.1.198) — pacing
// the ops makes the recipe deterministic.
//
// Once the recipe validates, the intended answer is recorded in the intent
// registry (issue #51 decision 3, dialogintent.go) so ReadTranscript can
// verify the retro-flushed tool_result against it — recorded BEFORE the keys
// play, deliberately: a send that dies mid-recipe leaves the picker in an
// unknown state, which is exactly when the backstop warning earns its keep.
func (p *Provider) AnswerDialog(ctx context.Context, sessionName string, dialog provider.Dialog, answer provider.DialogAnswer) error {
	ops, err := DialogKeystrokes(dialog, answer)
	if err != nil {
		return err
	}
	p.intents.record(dialog, answer)
	for i, op := range ops {
		if i > 0 && p.keyDelay > 0 {
			if !sleepOrDone(ctx, p.keyDelay) {
				return ctx.Err()
			}
		}
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

// No normalize-to-top climb (LIVE-VERIFIED 2026-07-08, 2.1.198 — compat §7).
// The pre-#51 recipes opened every picker with an Up×N climb to "normalise the
// cursor to the top row", on the assumption that Up CLAMPS at the top. Driving
// the real pickers disproved both halves: (1) Up WRAPS — one Up from the top
// option jumps to the LAST option (and on the review screen, from "Submit
// answers" to "Cancel"), so a climb actively lands the cursor on the wrong row
// and, on the review screen, would silently CANCEL the whole form; (2) every
// freshly-presented picker already starts on its top row (row 0), and lab
// always answers a fresh picker in one AnswerDialog shot, so there is nothing
// to normalise. The recipes therefore navigate purely DOWNWARD from the known
// top-of-picker start. If a future version does NOT open at the top, the §5
// backstop catches the resulting mismatch.

// DialogKeystrokes builds the send-keys recipe that answers d with answer
// (compat.md §7). Every picker starts on its top row (see the no-climb note
// above), so each recipe is a pure downward walk. Exported for the compat
// snapshot test.
//
// Shapes (all live-verified 2026-07-08, 2.1.198 — compat §7 carries the row
// models and the three recipe bugs this rewrite fixed):
//
//   - flat single-select question: [Down×idx][Enter]; an IsOther row takes
//     [Down×idx][PasteText][Enter] — type FIRST (Enter on the empty free-text
//     row declines the whole dialog).
//   - flat multi-select question: [Down-walk with Space toggles][Down onto the
//     Submit row][Enter], then the review screen [Enter] (Enter on an option
//     row toggles, it never commits; the ✔ Submit tab model adds a review
//     screen even for a single multi-select question).
//   - multi-question form: the per-question recipes concatenated in question
//     order (committing a question auto-advances the form; no transition keys),
//     then the review screen [Enter].
//   - plan approval: [Down×idx][Enter] over the four pinned rows; row 3
//     (IsOther) takes the type-first feedback path like Other.
//
// Answer encoding (issue #51 decision 3, per-question strictness): a
// multi-question dialog requires exactly len(Questions) positional Answers —
// answers[i] answers Questions[i]; per question, single-select carries the
// chosen option in Index (Selected must be empty; OtherText exactly when the
// row IsOther) and multi-select carries the toggled options in Selected
// (Index unused, OtherText forbidden, the Other row untoggleable). A flat
// dialog accepts the flat fields as today, or a single Answers element with
// answers[0] semantics. Violations return the provider sentinels so the API's
// 400/409 mapping stays meaningful — a misencoded answer must fail at the
// door, never play misplaced keystrokes.
func DialogKeystrokes(d provider.Dialog, answer provider.DialogAnswer) ([]KeyOp, error) {
	if !d.Answerable {
		return nil, ErrDialogNotAnswerable
	}
	if len(d.Questions) >= 2 {
		return multiQuestionKeystrokes(d, answer)
	}
	if len(answer.Answers) > 1 {
		return nil, fmt.Errorf("%w: %d answers for a single-question dialog", ErrInvalidReply, len(answer.Answers))
	}
	if len(answer.Answers) == 1 {
		qa := answer.Answers[0]
		answer = provider.DialogAnswer{Index: qa.Index, Selected: qa.Selected, OtherText: qa.OtherText}
	}
	if len(d.Options) == 0 {
		return nil, ErrDialogNotAnswerable
	}
	if d.Kind == provider.DialogKindPlan {
		// The plan picker has no ✔ Submit tab and no review screen: Enter on the
		// chosen row resolves it directly (live 2026-07-08).
		return singleSelectOps(d.Options, answer.Index, answer.OtherText)
	}
	if d.Multi {
		ops, err := multiSelectOps(d.Options, answer.Selected)
		if err != nil {
			return nil, err
		}
		// A single multiSelect question still carries the ✔ Submit tab, so the
		// form ends on the review screen (live 2026-07-08) — commit it.
		return append(ops, reviewOps()...), nil
	}
	return singleSelectOps(d.Options, answer.Index, answer.OtherText)
}

// multiQuestionKeystrokes sequences a multi-question form: the per-question
// recipes in question order — committing a question (option Enter / Submit-row
// Enter) auto-advances the form to the next picker, so no transition keys are
// needed (live 2026-07-08; keyDelay pacing still applies between every op) —
// then the review screen. Validation per the positional answer encoding (see
// DialogKeystrokes); the whole form is validated before any key plays.
func multiQuestionKeystrokes(d provider.Dialog, answer provider.DialogAnswer) ([]KeyOp, error) {
	if len(answer.Answers) != len(d.Questions) {
		return nil, fmt.Errorf("%w: %d answers for %d questions", ErrInvalidReply, len(answer.Answers), len(d.Questions))
	}
	var ops []KeyOp
	for i, q := range d.Questions {
		qops, err := questionOps(q, answer.Answers[i])
		if err != nil {
			return nil, fmt.Errorf("question %d: %w", i, err)
		}
		ops = append(ops, qops...)
	}
	return append(ops, reviewOps()...), nil
}

// questionOps builds one question's picker recipe, enforcing the per-question
// answer encoding strictly: a payload in the wrong encoding (e.g. the old
// divergent SPA shape that put the option choice in Selected) must be rejected
// rather than silently misplayed — a blind keystroke sequence built from a
// misread answer is exactly the desync the verification backstop exists for,
// and 4xx at the door beats a warning after the fact.
func questionOps(q provider.Question, a provider.QuestionAnswer) ([]KeyOp, error) {
	if q.MultiSelect {
		if a.OtherText != "" {
			return nil, fmt.Errorf("%w: other text has no meaning for a multi-select question", ErrInvalidReply)
		}
		return multiSelectOps(q.Options, a.Selected)
	}
	if len(a.Selected) > 0 {
		return nil, fmt.Errorf("%w: selected indices have no meaning for a single-select question (use index)", ErrInvalidReply)
	}
	return singleSelectOps(q.Options, a.Index, a.OtherText)
}

// singleSelectOps is the one single-select picker recipe, shared by flat
// questions, multi-question forms, and the plan picker: from the top-of-picker
// start, Down to the row, commit.
//
//	[Down × idx] [Enter]
//
// An IsOther row (the question free-text row; the plan "Tell Claude what to
// change" feedback row) takes the TYPE-FIRST path instead:
//
//	[Down × idx] [PasteText] [Enter]
//
// LIVE BUG FIX (2026-07-08, 2.1.198): the pre-#51 recipe sent Enter to
// "select" the Other row before typing — but Enter on the EMPTY free-text row
// DECLINES the whole dialog (recorded toolUseResult "User rejected tool use").
// The row takes inline type-first fill: the pasted text fills the row label,
// then Enter submits it. (The pre-#51 normalize-to-top climb is also gone —
// see the no-climb note above DialogKeystrokes.)
func singleSelectOps(opts []provider.DialogOption, idx int, otherText string) ([]KeyOp, error) {
	rows := len(opts)
	if idx < 0 || idx >= rows {
		return nil, fmt.Errorf("%w: option index %d out of range [0,%d)", ErrDialogNotAnswerable, idx, rows)
	}
	var ops []KeyOp
	if down := repeat("Down", idx); len(down) > 0 {
		ops = append(ops, KeyOp{Named: down})
	}
	if opts[idx].IsOther {
		clean, err := validateReply(otherText)
		if err != nil {
			return nil, fmt.Errorf("%w: other text: %w", ErrInvalidReply, err)
		}
		return append(ops, KeyOp{Text: clean}, KeyOp{Named: []string{"Enter"}}), nil
	}
	if otherText != "" {
		return nil, fmt.Errorf("%w: other text is only valid for the free-text option", ErrInvalidReply)
	}
	return append(ops, KeyOp{Named: []string{"Enter"}}), nil
}

// multiSelectOps is the multi-select picker recipe: from the top-of-picker
// start, walk down toggling each selected row with Space, then commit via the
// UNNUMBERED Submit row Claude Code appends below the options.
//
//	[Down/Space walk] [Down × (rows − last)] [Enter]
//
// The Submit row's navigation index is rows (options 0..rows−1, Submit next),
// so the final Down batch (rows − last-selected) is never empty — the recipe
// can never end on a bare option-row Enter.
//
// LIVE BUG FIX (2026-07-08, 2.1.198): the pre-#51 recipe confirmed with a
// bare Enter on the last toggled row — but Enter on an option row TOGGLES
// exactly like Space (observed: it flipped the last selection OFF) and never
// commits. Commit = navigate onto Submit, Enter there. (The pre-#51 climb is
// gone — Up wraps, so a climb would land in the wrong place; see the no-climb
// note above DialogKeystrokes.)
func multiSelectOps(opts []provider.DialogOption, sel []int) ([]KeyOp, error) {
	sel, err := normSelected(opts, sel)
	if err != nil {
		return nil, err
	}
	rows := len(opts)
	var ops []KeyOp
	cur := 0
	for _, idx := range sel {
		if down := repeat("Down", idx-cur); len(down) > 0 {
			ops = append(ops, KeyOp{Named: down})
		}
		ops = append(ops, KeyOp{Named: []string{"Space"}}) // toggle this option on
		cur = idx
	}
	ops = append(ops, KeyOp{Named: repeat("Down", rows-cur)}) // onto the Submit row
	return append(ops, KeyOp{Named: []string{"Enter"}}), nil  // commit
}

// reviewOps commits the "Review your answers" screen that ends every form
// carrying the ✔ Submit tab — a multi-question form, or a single-question
// multiSelect (live 2026-07-08; the all-single-select multi-question case is
// derived from the tab model rather than separately driven — compat §7 — and
// the verification backstop covers drift). Two rows ("Submit answers",
// "Cancel"), cursor DEFAULTS to "Submit answers" (row 0): a bare Enter submits.
//
// LIVE BUG FIX (2026-07-08, 2.1.198): the first cut of this recipe sent
// [Up][Enter] to "normalise" to the top — but Up WRAPS from "Submit answers"
// to "Cancel", so [Up][Enter] silently CANCELLED the whole form (discarding
// every answer; the model then re-asked). The cursor is already on "Submit
// answers", so Enter alone commits.
func reviewOps() []KeyOp {
	return []KeyOp{{Named: []string{"Enter"}}}
}

// normSelected validates and normalizes a multi-select answer against a
// question's options: every index must be a real, non-Other option (a silently
// dropped index would confirm a selection the operator never made, and Space
// on the free-text Other row is an uncaptured TUI path — degrade that to the
// deep link). The result is ascending and de-duplicated so the walk only ever
// moves down.
func normSelected(opts []provider.DialogOption, sel []int) ([]int, error) {
	if len(sel) == 0 {
		return nil, fmt.Errorf("%w: no options selected", ErrInvalidReply)
	}
	rows := len(opts)
	seen := map[int]bool{}
	var out []int
	for _, i := range sel {
		if i < 0 || i >= rows {
			return nil, fmt.Errorf("%w: option index %d out of range [0,%d)", ErrInvalidReply, i, rows)
		}
		if opts[i].IsOther {
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
