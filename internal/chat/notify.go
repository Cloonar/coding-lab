package chat

// The notify gate is the tailer's edge-trigger for a "your run wants you" Web
// Push (issue #99). It lives at the tail loop's state-transition edge, NOT as a
// bus subscriber: the event bus drops events for a slow subscriber by design
// (tailer.go), so a lossy subscriber could silently miss the one transition
// that matters. The tailer already re-derives conversational state every tick
// with at-least-once delivery, so folding the trigger in here is the lossless
// seam. The push side is reached through an injected nil-safe closure so this
// package never imports internal/push (the import-boundary reasoning of
// reconcile's AFKRunEnded seam).

import (
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// Notification mirrors push.Payload field-for-field so cmd/lab can map it 1:1
// without chat importing push (same import-boundary reasoning as reconcile's
// AFKRunEnded seam, ADR-0038 pattern).
type Notification struct {
	Title string
	Body  string
	Tag   string
	Route string
}

const (
	// defaultNotifyDebounce is the flap-debounce window: a run must SETTLE in a
	// notify state for this long before its single push fires, so a transient
	// needs_input→working blip never notifies (issue #99 asks ~2–3s).
	defaultNotifyDebounce = 2 * time.Second
	// notifyBodyLimit caps the notification body in RUNES (not bytes): a push
	// body past this length is truncated by the OS anyway, so lab does the cut
	// rune-safely itself and appends an ellipsis.
	notifyBodyLimit = 150
)

// isNotifyState reports whether s is a conversational state that warrants a
// push: the run settled awaiting the operator (needs_input) or is blocked on an
// interactive dialog (question). Provider-neutral strings (issue #99).
func isNotifyState(s string) bool {
	return s == provider.StateNeedsInput || s == provider.StateQuestion
}

// notifyGate is one tailer goroutine's edge-trigger + flap-debounce state.
// Not concurrency-safe; owned by a single tail loop.
type notifyGate struct {
	now      func() time.Time
	debounce time.Duration
	run      store.Run
	pending  bool
	deadline time.Time
	payload  Notification
}

// newNotifyGate builds a gate for run, clocked by now (tests inject a fake) and
// debounced by debounce.
func newNotifyGate(now func() time.Time, debounce time.Duration, run store.Run) *notifyGate {
	return &notifyGate{now: now, debounce: debounce, run: run}
}

// observe folds a fresh chat read into the gate, given the PREVIOUS state the
// tail loop held before this tick (the empty first-tick prev "" counts as a
// non-notify state — exactly the deliberate re-adoption re-fire from issue #99).
// Three edges:
//   - ENTERING the notify set (prev outside, chat inside): arm a new episode —
//     pending, deadline = now + debounce, snapshot the payload.
//   - STAYING inside the notify set while pending (needs_input→question, or an
//     identity re-read carrying a fresher dialog): refresh the payload so the
//     newest question replaces the body, but DO NOT touch the deadline — a newer
//     body must not extend the debounce.
//   - LEAVING the notify set: disarm, arming the next episode.
//
// "Exactly once per episode" needs no latch flag: once due() consumes the
// pending send, ENTERING can't recur while the run stays inside the set (prev
// is inside too — lastState never resets mid-run), and STAYING refreshes
// nothing once pending is consumed. Only LEAVING re-arms.
func (g *notifyGate) observe(prev string, chat provider.Chat) {
	switch {
	case isNotifyState(chat.State) && !isNotifyState(prev):
		g.pending = true
		g.deadline = g.now().Add(g.debounce)
		g.payload = g.buildPayload(chat)
	case isNotifyState(chat.State):
		if g.pending {
			g.payload = g.buildPayload(chat)
		}
	default:
		g.pending = false
	}
}

// due reports a ready notification and consumes it: when the gate is pending
// and the debounce deadline has elapsed in real time, it hands back the
// snapshot exactly once. Called on EVERY tick — quiet no-change ticks and
// post-read-error ticks alike — because the deadline elapses while nothing
// else about the run changes (issue #99).
func (g *notifyGate) due() (Notification, bool) {
	if g.pending && !g.now().Before(g.deadline) {
		g.pending = false
		return g.payload, true
	}
	return Notification{}, false
}

// buildPayload snapshots a Notification from the run and the fresh chat. Title
// leads with the run's display name — the title overlay when set, else the
// session name (issue #111, the same fallback the SPA renders); Tag and Route
// are pure run identity, no store lookup; Route is a PWA-internal path the
// service worker navigates to, never a forge URL. Body follows the issue #99
// precedence and is truncated rune-safely.
func (g *notifyGate) buildPayload(chat provider.Chat) Notification {
	return Notification{
		Title: g.run.DisplayName() + " needs input",
		Body:  truncateRunes(notifyBody(chat), notifyBodyLimit),
		Tag:   g.run.ID,
		Route: "/runs/" + g.run.ID,
	}
}

// notifyBody picks the notification body by the issue #99 precedence:
//  1. the live PendingDialog prompt, when non-blank;
//  2. else the newest VISIBLE assistant text (backward scan, mirroring
//     lastDialog's style in chat.go) — thinking blocks are hidden-by-default
//     chain-of-thought and must never reach a lock screen, and the text-kind
//     pin keeps any future non-text kind that carries a Role out too;
//  3. else the literal fallback.
func notifyBody(chat provider.Chat) string {
	if chat.PendingDialog != nil {
		if p := strings.TrimSpace(chat.PendingDialog.Prompt); p != "" {
			return p
		}
	}
	for i := len(chat.Messages) - 1; i >= 0; i-- {
		m := chat.Messages[i]
		if m.Kind != provider.MessageText || m.Role != "assistant" || m.Thinking {
			continue
		}
		if t := strings.TrimSpace(m.Text); t != "" {
			return t
		}
	}
	return "needs your input"
}

// truncateRunes caps s to limit runes (rune-safe, not bytes): an over-limit
// string keeps its first limit-1 runes and gains a single '…', so the result is
// exactly limit runes.
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}
