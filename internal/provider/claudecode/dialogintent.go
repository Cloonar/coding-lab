package claudecode

// Post-resolve verification backstop (issue #51 decision 3). The dialog
// keystroke recipes are BLIND sequences — nothing reads the pane back — so a
// desync (a synth-row drift, a picker redesign, a paste race) would silently
// record an answer the operator never chose. The backstop closes that gap
// after the fact: AnswerDialog records the INTENDED answer in-memory, keyed by
// tool_use_id; when the dialog resolves and Claude Code retro-flushes the
// tool_use + tool_result pair into the transcript (compat §5), ReadChat
// compares the recorded outcome (the top-level toolUseResult /
// toolDenialKind, live-verified shapes 2026-07-08 on 2.1.198 — compat §5)
// against the intent and emits a LIFECYCLE message with Error=true right
// after the tool result on a mismatch, so the desync is visible in the chat.
//
// Determinism and the restart caveat: while an intent record exists, every
// parse emits the same warning at the same position (seq-stable across
// re-reads); a MATCHED intent is cleared on first verification and never
// emits at all. The registry is in-memory only — after a lab restart the
// intent is gone and a previously emitted warning DISAPPEARS from subsequent
// parses (later messages shift down one seq). Accepted: the backstop is
// advisory-only, and persisting intents would buy a rare warning's permanence
// with a whole new storage surface.
//
// Forensic logging (issue #165 item 1): a live investigation (2026-07-16)
// failed to reproduce the suspected paste/Enter desync — 24/24 production-
// recipe trials landed cleanly through tmux's serialized pane-input queue,
// even at 0ms inter-op gap, a 90k-char paste, and heavy load. So this lands
// as PASSIVE INSTRUMENTATION ONLY (zero behavior change: same warnings, same
// seq stability, same return values everywhere): on first detection of a
// mismatched intent, verify logs the intent, the exact keystroke recipe
// (ops), and a pane snapshot taken at that moment — ONCE per intent, never
// on a re-parse's repeat warning — so the next real production occurrence
// hands us the mechanism instead of another unreproduced report.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// maxDialogIntents caps the registry: answers a run never resolves (a session
// killed mid-dialog, a mismatch the operator ignores) must not accumulate
// forever. Oldest-first eviction; 100 is far beyond any plausible number of
// concurrently unresolved dialogs (one per live session).
const maxDialogIntents = 100

// dialogIntent is what lab meant a dialog answer to be, in the vocabulary the
// transcript records resolutions in (so verification is a direct compare).
type dialogIntent struct {
	kind string // provider.DialogKindQuestion | provider.DialogKindPlan

	// answers (question kind) maps each question's text to the answer string
	// claude records for it in toolUseResult.answers: the chosen option label,
	// multi-select labels comma+space-joined in row order — with a filled
	// free-text row riding as one more segment (live 2026-07-09: "Onions, no
	// anchovies") — or the free text verbatim for an Other answer (live
	// 2026-07-08: `"answers":{"Which fruits do you like?":"Apple, Cherry",
	// "Favorite pet?":"Ferret"}`).
	answers map[string]string

	// approve/feedback (plan kind): rows 0–1 of the pinned picker approve,
	// rows 2–3 reject; feedback carries row 3's typed text (recorded inside
	// the denial string after "the user said:\n" — live 2026-07-08).
	approve  bool
	feedback string

	// Forensics for the one-shot mismatch log (issue #165 item 1). session is
	// the tmux session AnswerDialog played the recipe into (the pane-capture
	// target); ops is the exact recipe, kept verbatim rather than
	// re-derived — verify must log what actually got sent, not what a fresh
	// DialogKeystrokes call would build from the same inputs today. logged
	// flags that the forensic log has already fired for this toolID; it does
	// NOT affect the operator-facing warning, which still re-emits on every
	// re-parse per the determinism note above.
	session string
	ops     []KeyOp
	logged  bool
}

// intentRegistry is the bounded, concurrency-safe intent store on the
// Provider. ReadChat runs from both the chat tailer and HTTP reads, so
// verify must be safe under concurrent parses of the same transcript; the
// worst race outcome is one duplicate-suppressed warning, never corruption.
type intentRegistry struct {
	mu    sync.Mutex
	byID  map[string]dialogIntent
	order []string // insertion order, for oldest-first cap eviction

	// log and capture wire the forensic backstop (issue #165 item 1) into the
	// registry. Both optional: nil silently disables that half of
	// instrumentation, so the zero-value registry (every bare test
	// construction, and any future caller that never sets them) keeps
	// verifying and warning exactly as before — only the log line is gated.
	// capture is called OUTSIDE r.mu (see verify) with whatever bounded
	// context it closes over; the registry itself never builds one.
	log     *slog.Logger
	capture func(sessionName string) (string, error)
}

// record stores the intent behind a validated dialog answer, plus the
// forensics (sessionName, ops) a later mismatch log needs. Best-effort by
// design: an answer shape record cannot derive an expectation from is skipped
// (no intent → no warning), never an error — the recipe already validated.
func (r *intentRegistry) record(sessionName string, d provider.Dialog, a provider.DialogAnswer, ops []KeyOp) {
	in, ok := intentFor(d, a)
	if !ok || d.ToolID == "" {
		return
	}
	in.session = sessionName
	in.ops = ops
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = map[string]dialogIntent{}
	}
	if _, exists := r.byID[d.ToolID]; !exists {
		r.order = append(r.order, d.ToolID)
	}
	r.byID[d.ToolID] = in
	for len(r.order) > maxDialogIntents {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.byID, oldest)
	}
}

// verify compares the recorded resolution of toolID against its intent.
// ok=false: no intent recorded (lab did not answer this dialog — e.g. the
// operator answered in the TUI or claude.ai, or lab restarted). warn=="" with
// ok=true: the outcome matches the intent, which is now cleared (verified
// intents emit nothing, ever). A non-empty warn is the operator-facing
// mismatch text; the intent stays recorded so every re-parse emits the same
// warning deterministically (seq stability — see the package comment).
//
// On a mismatch's FIRST detection (logged still false), it also fires the
// forensic log (issue #165 item 1) — but only after releasing r.mu: the log
// call's pane capture shells out to tmux, and holding a mutex across that
// exec would serialize every concurrent ReadChat behind one slow capture.
// The logged flip itself happens under the lock (the intent stays in the
// map either way, so a lost race there just means the rare double-log this
// package comment already accepts as worst-effort).
func (r *intentRegistry) verify(toolID string, res resolvedTool) (warn string, ok bool) {
	r.mu.Lock()
	in, exists := r.byID[toolID]
	if !exists {
		r.mu.Unlock()
		return "", false
	}
	warn = mismatch(in, res)
	if warn == "" {
		delete(r.byID, toolID)
		for i, id := range r.order {
			if id == toolID {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
		r.mu.Unlock()
		return warn, true
	}
	fireLog := r.log != nil && !in.logged
	if fireLog {
		in.logged = true
		r.byID[toolID] = in
	}
	log, capture := r.log, r.capture
	r.mu.Unlock()
	if fireLog {
		logMismatch(log, capture, toolID, in, warn)
	}
	return warn, true
}

// intentFor derives the expected recorded outcome from a dialog + the answer
// lab is about to play. It mirrors DialogKeystrokes' normalization (ascending
// multi-select walk order, trimmed free text) so the expectation is exactly
// what a correctly landed answer records.
func intentFor(d provider.Dialog, a provider.DialogAnswer) (dialogIntent, bool) {
	switch d.Kind {
	case provider.DialogKindPlan:
		if len(a.Answers) == 1 {
			a = provider.DialogAnswer{Index: a.Answers[0].Index, OtherText: a.Answers[0].OtherText}
		}
		if a.Index < 0 || a.Index >= len(d.Options) {
			return dialogIntent{}, false
		}
		in := dialogIntent{kind: provider.DialogKindPlan, approve: a.Index <= 1}
		if d.Options[a.Index].IsOther {
			if text, err := validateOtherText(a.OtherText); err == nil {
				in.feedback = text
			}
		}
		return in, true
	case provider.DialogKindQuestion:
		answers := map[string]string{}
		if len(d.Questions) >= 2 {
			if len(a.Answers) != len(d.Questions) {
				return dialogIntent{}, false
			}
			for i, q := range d.Questions {
				v, ok := expectedAnswer(q.Options, q.MultiSelect, a.Answers[i])
				if !ok {
					return dialogIntent{}, false
				}
				answers[q.Text] = v
			}
			return dialogIntent{kind: provider.DialogKindQuestion, answers: answers}, true
		}
		qa := provider.QuestionAnswer{Index: a.Index, Selected: a.Selected, OtherText: a.OtherText}
		if len(a.Answers) == 1 {
			qa = a.Answers[0]
		}
		v, ok := expectedAnswer(d.Options, d.Multi, qa)
		if !ok {
			return dialogIntent{}, false
		}
		// The flat Prompt IS the question text for a single-question
		// AskUserQuestion (askUserQuestionDialog passes it through untruncated),
		// which is the key toolUseResult.answers records under.
		answers[d.Prompt] = v
		return dialogIntent{kind: provider.DialogKindQuestion, answers: answers}, true
	default:
		return dialogIntent{}, false
	}
}

// sameAnswer compares a recorded answer string against the intended one,
// order-insensitively across the comma+space label join. A multi-select
// question records its chosen labels joined by ", ", but the recorded ORDER is
// NOT the option-index order — LIVE 2026-07-08, toggling Apple then Cherry
// recorded "Cherry, Apple" — so an ordered compare would false-warn on a
// correct answer. Both sides are split on ", ", sorted, and compared as
// multisets. A single-select answer is one segment, unaffected; a free-text
// answer that itself contains ", " sorts identically on both sides (it is the
// same string), so it is never a false warning either.
func sameAnswer(got, want string) bool {
	gs, ws := strings.Split(got, ", "), strings.Split(want, ", ")
	if len(gs) != len(ws) {
		return false
	}
	slices.Sort(gs)
	slices.Sort(ws)
	return slices.Equal(gs, ws)
}

// expectedAnswer renders one question's expected recorded answer string.
func expectedAnswer(opts []provider.DialogOption, multi bool, a provider.QuestionAnswer) (string, bool) {
	if multi {
		sel, err := normSelected(opts, a.Selected, a.OtherText != "")
		if err != nil {
			return "", false
		}
		labels := make([]string, 0, len(sel)+1)
		for _, idx := range sel {
			labels = append(labels, opts[idx].Label)
		}
		if a.OtherText != "" {
			// The free-text row's fill records as one more ", "-joined segment
			// (live 2026-07-09: "Onions, no anchovies"); sameAnswer compares
			// segment multisets, so its recorded position never matters.
			text, err := validateOtherText(a.OtherText)
			if err != nil {
				return "", false
			}
			labels = append(labels, text)
		}
		return strings.Join(labels, ", "), true
	}
	if a.Index < 0 || a.Index >= len(opts) {
		return "", false
	}
	if opts[a.Index].IsOther {
		text, err := validateOtherText(a.OtherText)
		if err != nil {
			return "", false
		}
		return text, true
	}
	return opts[a.Index].Label, true
}

// resolvedTool is the transcript's recorded resolution of one dialog tool:
// the top-level fields of the user event carrying its tool_result block
// (tItem.ToolUseResult / tItem.ToolDenialKind — compat §5 pins the shapes).
type resolvedTool struct {
	toolUseResult json.RawMessage
	denialKind    string
}

// denialString returns the toolUseResult as a string when the resolution is a
// denial ("User rejected tool use", "Error: The user doesn't want to
// proceed…the user said:\n<feedback>"); ok=false when it is an object (a
// recorded answer / an approved plan) or absent.
func (r resolvedTool) denialString() (string, bool) {
	b := r.toolUseResult
	if len(b) == 0 || b[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(b, &s) != nil {
		return "", false
	}
	return s, true
}

// mismatch compares an intent against the recorded resolution and returns the
// operator-facing warning text, or "" for a match. An absent/undecodable
// toolUseResult verifies as a silent match: the ground-truth key is stamped on
// every live 2.1.198 resolution (compat §5), so its absence means an upstream
// shape change lab cannot judge — the backstop is advisory and must not cry
// wolf on evidence it cannot read.
func mismatch(in dialogIntent, res resolvedTool) string {
	const prefix = "Dialog answer may not have landed: "
	denial, isDenial := res.denialString()
	isDenial = isDenial || res.denialKind == "user-rejected"

	if in.kind == provider.DialogKindPlan {
		switch {
		case in.approve && isDenial:
			return prefix + "lab answered approve, but the transcript recorded a rejected plan"
		case !in.approve && !isDenial && len(res.toolUseResult) > 0:
			return prefix + "lab answered reject, but the transcript recorded an approved plan"
		case !in.approve && isDenial && in.feedback != "" && !strings.Contains(denial, in.feedback):
			return prefix + fmt.Sprintf("expected the rejection feedback %q, recorded %q",
				truncate(in.feedback, 80), truncate(denial, 160))
		default:
			return ""
		}
	}

	// Question kind.
	if isDenial {
		return prefix + "lab sent an answer, but the transcript recorded a decline (user-rejected)"
	}
	var rec struct {
		Answers      map[string]string `json:"answers"`
		AfkTimeoutMs int64             `json:"afkTimeoutMs"`
	}
	if len(res.toolUseResult) == 0 || json.Unmarshal(res.toolUseResult, &rec) != nil {
		return "" // no readable ground truth — see the doc comment
	}
	// The 60s unattended timeout resolves the picker by itself with answers:{}
	// and an afkTimeoutMs stamp (live 2026-07-08): lab DID send keystrokes
	// (there is a recorded intent), so the answer did not land in time.
	if rec.AfkTimeoutMs > 0 {
		return prefix + fmt.Sprintf("the picker timed out unanswered (no response after %ds) although lab sent an answer",
			rec.AfkTimeoutMs/1000)
	}
	for q, want := range in.answers {
		got, ok := rec.Answers[q]
		if !ok {
			return prefix + fmt.Sprintf("no answer was recorded for %q (expected %q)",
				truncate(q, 80), truncate(want, 80))
		}
		if !sameAnswer(got, want) {
			return prefix + fmt.Sprintf("for %q expected %q, recorded %q",
				truncate(q, 80), truncate(want, 80), truncate(got, 80))
		}
	}
	if len(rec.Answers) != len(in.answers) {
		return prefix + fmt.Sprintf("%d answers recorded, %d intended", len(rec.Answers), len(in.answers))
	}
	return ""
}

// Forensic log (issue #165 item 1). logMismatch renders and emits the
// one-shot Warn line: the intent (what lab meant), the ops (what lab
// actually sent), and — when capture is wired — a pane snapshot at
// detection time. capture runs here, already outside r.mu (see verify).

// paneSnapshotMaxLines and paneSnapshotMaxBytes bound the captured pane
// text carried in the log line: CapturePane's `-S -` pulls the WHOLE
// scrollback, and a mismatch is a tail-of-pane question (what does the
// picker look like right now), not a full-session one — an unbounded
// snapshot would bloat every log line for no diagnostic gain.
const (
	paneSnapshotMaxLines = 40
	paneSnapshotMaxBytes = 4096
)

// logMismatch fires the forensic Warn line for a first-detected mismatch.
// log is never nil here (verify gates fireLog on it); capture may be —
// skipped entirely rather than logging a synthetic "capture unavailable"
// (issue #165 item 1 design: nil capture silently turns off that half of
// instrumentation).
func logMismatch(log *slog.Logger, capture func(sessionName string) (string, error), toolID string, in dialogIntent, warn string) {
	attrs := []any{
		"tool_id", toolID,
		"session", in.session,
		"warn", warn,
		"intent", renderIntent(in),
		"ops", renderOps(in.ops),
	}
	if capture != nil {
		if pane, err := capture(in.session); err != nil {
			attrs = append(attrs, "pane_error", err.Error())
		} else {
			attrs = append(attrs, "pane", truncatePane(pane))
		}
	}
	log.Warn("dialog answer mismatch", attrs...)
}

// renderIntent renders what lab MEANT the answer to be, in one readable
// line — the counterpart to warn, which already carries what the transcript
// actually recorded. Question-kind answers are sorted for deterministic log
// output (map iteration order is not).
func renderIntent(in dialogIntent) string {
	if in.kind == provider.DialogKindPlan {
		switch {
		case in.approve:
			return "plan approve"
		case in.feedback != "":
			return fmt.Sprintf("plan reject feedback=%q", in.feedback)
		default:
			return "plan reject"
		}
	}
	parts := make([]string, 0, len(in.answers))
	for q, a := range in.answers {
		parts = append(parts, fmt.Sprintf("%q=%q", q, a))
	}
	slices.Sort(parts)
	return "question " + strings.Join(parts, ", ")
}

// renderOps renders a keystroke recipe COMPACTLY for the log: named keys
// verbatim, a paste op as Paste(<n>ch) — never the pasted text itself. The
// expected text already rides the intent (renderIntent); the recipe line
// exists to answer "what did lab actually send", not to re-carry the
// content, so it stays one short, greppable string (e.g. "Down Down
// Paste(17ch) Enter").
func renderOps(ops []KeyOp) string {
	parts := make([]string, 0, len(ops))
	for _, op := range ops {
		if op.Text != "" {
			parts = append(parts, fmt.Sprintf("Paste(%dch)", len(op.Text)))
			continue
		}
		parts = append(parts, strings.Join(op.Named, "+"))
	}
	return strings.Join(parts, " ")
}

// truncatePane trims a captured pane to its tail — the last
// paneSnapshotMaxLines lines, then the last paneSnapshotMaxBytes bytes of
// that — prefixing a marker when either bound cut something. The mismatch
// is diagnosed from the pane's state AT DETECTION TIME, which capture
// already pins; only the tail is ever relevant to what a picker currently
// shows.
func truncatePane(pane string) string {
	lines := strings.Split(pane, "\n")
	truncated := len(lines) > paneSnapshotMaxLines
	if truncated {
		lines = lines[len(lines)-paneSnapshotMaxLines:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > paneSnapshotMaxBytes {
		truncated = true
		out = out[len(out)-paneSnapshotMaxBytes:]
	}
	if truncated {
		out = "…(truncated)…\n" + out
	}
	return out
}
