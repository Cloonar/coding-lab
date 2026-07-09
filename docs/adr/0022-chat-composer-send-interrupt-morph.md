# Chat composer send/interrupt morph: disable Send + one-tap interrupt while working

ADR-0016 shipped the embedded chat with a composer that stayed *sendable* while
the agent worked — a mid-turn reply was delivered as a bracketed paste and the
agent's own TUI queued it, with the UI showing a subtle "queued" hint — and put
Interrupt behind a two-step confirm tap, distinct from the run Stop. ADR-0019
then made the composer Send an icon-only paper-plane. In practice the queue
affordance misleads (the reply vanishes into the TUI with no in-lab echo, and
"queued" reads as a promise lab can't show being kept) and the confirm tap adds
friction to the one intervention an operator reaches for most — interrupting a
turn that has gone the wrong way. The Claude app models this better: while the
agent is working, Send is replaced by a single interrupt control.

This ADR records that change. It is a **pure frontend/CSS** diff _within_ the
existing design language — no colour/type/radius/shadow redesign, and **no**
backend/API/SSE/message-schema/provider/migration change — the same class of
change as ADR-0019. It **evolves ADR-0016**, superseding two of its pinned
bullets (the queue hint and the confirm-tap interrupt); the rest of ADR-0016
stands.

The decisions, pinned (settled with the maintainer 2026-07-07 via
/frontend-design + /grill-me — do not re-litigate):

- **One morphing composer button replaces the [Send + separate Interrupt]
  pair.** Same slot; the glyph, action, and accessible name swap on the
  conversational state. **idle / needs_input** → a `send` paper-plane,
  accessible name **"Send"**, disabled when the input is empty or a send is in
  flight, click replies through the existing `replyRun` flow (unchanged).
  **working** → a `square`, accessible name **"Interrupt"**, always enabled, one
  tap calls `interruptRun` (the tmux `Escape`), showing a busy "…" while the
  request is in flight.

- **Strict swap — queueing is dropped from the UI (supersedes ADR-0016).** There
  is no Send affordance while the agent works and the "your reply will be queued"
  hint is removed. The textarea stays **editable** while working
  (compose-ahead), but nothing can be sent until the agent returns to
  idle/needs_input. The backend Reply (tmux bracketed-paste + Enter) is
  **unchanged** — it is simply no longer reachable from the UI mid-turn. This
  supersedes ADR-0016's "a mid-turn agent queues it … the UI shows a subtle
  'queued' hint."

- **One-tap interrupt — no confirm (supersedes ADR-0016).** The working-state
  square interrupts immediately. Interrupt is **non-destructive** — the agent
  survives, idles, and is re-promptable — and it is the primary intervention
  affordance, so a confirm tap is pure friction. This supersedes ADR-0016's
  "Interrupt is an explicit Escape behind a confirm tap."

- **Consistency — the same one-tap square everywhere it appears in the
  composer.** The two-step InterruptButton is replaced by the identical one-tap
  square in all three composer sites: the working-state morph, the
  question-without-structured-dialog locked state, and the DialogPanel escape
  hatch. Accessible name **"Interrupt"** in every case.

- **Lexicon and separation from the header Stop.** The accessible name is
  **"Interrupt"**, never "Abort/Cancel/Stop" (CONTEXT.md forbids *cancel/abort/
  kill* on the Neutral Stop concept, and ADR-0016 already names this action
  "Interrupt"; the maintainer said "abort" colloquially, but the UI stays
  "Interrupt"). The square glyph is shared with the header Stop but the two stay
  distinct: header **Stop** = danger-red, two-step verbal confirm, destructive
  teardown (ADR-0019, unchanged); composer **Interrupt** = accent, one-tap,
  non-destructive. ADR-0019's "icon-only never applies to a destructive
  confirmation" still holds — the destructive one (Stop) keeps its verbal
  confirm; the non-destructive one (Interrupt) is icon-only and one-tap.

- **An honest working cue.** The removed "queued" hint is replaced by a calm
  line — "The agent is working — tap to interrupt." — that describes what the
  square does rather than promising a queued send.

- **Composer polish, all within the design language.** (a) **Cmd/Ctrl+Enter
  sends** in the idle/needs_input state only — **never bare Enter**, so a phone's
  return key inserts a newline instead of firing a send [overturned on
  fine-pointer devices by ADR-0031, 2026-07-09 — bare Enter now sends there;
  touch devices are unaffected]; the shortcut is inert
  while working (no keyboard interrupt — the square is the only interrupt). (b)
  The textarea **auto-grows** from one row to a capped height then scrolls,
  replacing the fixed `rows={1}` + `resize: vertical`. (c) The working-state
  square carries a **subtle pulse**, gated by
  `@media (prefers-reduced-motion: reduce)`.

## Status

Superseded by ADR-0029 (issue #61): the working-state morph and its
send-block are reversed — Send is always available and Interrupt lives
in the chat header. ADR-0022's supersessions of ADR-0016 (no queued
hint; one-tap interrupt) remain in force via ADR-0029.

[This ADR's bare-Enter-stays-a-newline clause, and the corresponding
rejected option below, are additionally superseded/overturned by
ADR-0031 (issue #70, 2026-07-09): bare Enter now sends on fine-pointer
devices. Touch devices keep the original return-as-newline behaviour.]

Accepted. A pure SPA diff: `web/src/routes/RunChat.tsx`, `web/src/base.css`, and
`web/src/routes/RunChat.test.tsx`, plus this ADR and the two compat.md notes
below. **No** Go, API, SSE, message-schema, provider, or migration change. A
future Codex/Gemini provider inherits the composer behaviour for free — it is
provider-agnostic.

## Considered options

- **Keep the queue-while-working composer (ADR-0016 status quo).** Rejected: the
  reply disappears into the agent's TUI with no in-lab echo, and the "queued"
  copy reads as a promise lab cannot visibly keep. Compose-ahead (an editable
  textarea whose send is gated until the agent idles) gives the same "type now,
  send soon" without the misleading affordance.
- **Keep the two-step confirm on Interrupt.** Rejected: interrupt is
  non-destructive and re-promptable, and it is the affordance an operator
  reaches for most when a turn goes wrong — a confirm tap is friction on the hot
  path. The confirm stays only on the genuinely destructive header Stop.
- **Bare Enter sends (with Shift+Enter for a newline).** Rejected: the composer
  is phone-first (ADR-0005) and a mobile keyboard's return key must insert a
  newline, not submit. Cmd/Ctrl+Enter is the explicit send; bare Enter is always
  a newline. [Rejected then, overturned for fine-pointer devices by ADR-0031,
  2026-07-09 — phones and other touch-only devices keep this rejection in
  force; the reversal applies only where a fine pointer is present.]
- **Style the composer Interrupt like the header Stop (danger-red).** Rejected:
  danger colour signals destructive teardown; a non-destructive interrupt in the
  accent colour, occupying the same slot Send just vacated, makes the morph read
  as one control changing mode rather than two different buttons.
- **A JS-measured max-rows textarea vs CSS-only.** The auto-grow sets an exact
  inline height from `scrollHeight` (reset to one row first) and lets CSS
  `max-height` + `overflow-y: auto` do the cap and scroll — the smallest change
  that grows-then-scrolls. jsdom performs no layout (`scrollHeight` is 0), so the
  grow is an inert no-op under test and the unit suite still asserts behaviour by
  role/accessible name.

## Consequences

- `internal/compat/compat.md` is kept accurate: §6 (Reply send-keys) notes the
  recipe is **retained at the backend but no longer surfaced in the UI
  mid-turn**, and §8 (Interrupt keystroke) drops "behind a confirm tap" for the
  one-tap UI. The keystroke recipes themselves are unchanged.
- Component tests select the composer controls by accessible name — `Send` and
  `Interrupt` (ADR-0019's pattern for icon-only controls) — and assert the morph,
  the one-tap interrupt (no confirm), the Cmd/Ctrl+Enter vs bare-Enter split
  [bare-Enter side overturned on fine-pointer devices by ADR-0031], and
  the absence of any "queued" copy.
- The pulse adds one keyframe behind a `prefers-reduced-motion` guard, matching
  the existing reduced-motion discipline (`pulse-fade`, `progress-slide`); the
  button is fully usable with motion disabled.
