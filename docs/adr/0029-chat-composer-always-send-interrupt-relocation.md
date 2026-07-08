# Chat composer always-Send: reverse the working-state morph, relocate Interrupt to the header

`deriveState` (`internal/provider/claudecode/chat.go`) guesses the
chat's conversational state from the last transcript block, with no
liveness signal of its own: a stalled tail — a trailing `tool_result`,
an `assistant:thinking`, even a bare `user:text` — makes `working` stick
forever, because the tailer only recomputes on file change. Under ADR-0022's
morph, a false `working` pinned the composer to a no-op Interrupt with no
Send at all, and a mirrored gate on `canClear` blocked New conversation
too — a hard operator lockout with no way out short of the transcript
itself moving (issue #38). Issue #61 deliberately does **not** fix the
wrong state; that deeper liveness fix is issue #62's umbrella. Instead it
decouples the composer's primary actions from a signal that cannot be trusted.

This ADR records that decoupling. It is a **pure frontend/CSS** diff _within_
the existing design language — no colour/type/radius/shadow redesign,
and **no** backend/API/SSE/message-schema/provider/migration change —
the same class of change as ADR-0019 and ADR-0022. It **supersedes ADR-0022
wholesale**: the morph it pinned (Send↔Interrupt swapping on `working`)
is reversed end to end. ADR-0022's own two supersessions of ADR-0016 —
dropping the queued-hint copy and dropping the confirm-tap interrupt —
are **not** reopened; both carry forward in force, now attached to the
controls this ADR relocates.

The decisions, pinned (settled with the maintainer 2026-07-09 via /grill-me
— do not re-litigate):

- **Always-Send — the composer button never morphs.** In `idle`,
  `needs_input`, and `working` alike the composer's primary button is always
  the `send` paper-plane, never a `square`/interrupt glyph. Enabled iff the
  textarea is non-empty and no send is already in flight; a click always
  POSTs `/reply` immediately through the existing `replyRun` path. **Send
  never reads `working`** — this supersedes ADR-0022's morphing-button
  decision outright.

- **No lab-managed queue.** A genuinely mid-turn reply is queued by Claude
  Code's own TUI, exactly as ADR-0016 originally shipped: the backend `Reply`
  (tmux bracketed-paste + `Enter`) is **unchanged** and guards only an ended
  run and a pending dialog (`internal/chat/chat.go`'s `Reply`). There is
  no lab-side queue UI and no optimistic echo — the reply becomes visible
  when the transcript reflects it, not before.

- **Cmd/Ctrl+Enter always sends** in `idle`/`needs_input`/`working` alike;
  **bare Enter stays a newline** in every state (the composer is phone-first,
  ADR-0005). The slash-command popover's own keys are unchanged.

- **The working-only cue is deleted.** "The agent is working — tap to
  interrupt." described the morph's square; with no morph left to describe,
  the line is removed rather than repurposed.

- **Interrupt relocates to the chat header**, gated on **`live`** (the run
  row's own outcome signal — active means the session is alive, so an
  interrupt can land; on an idle prompt it is a harmless no-op — never on
  `working`, the untrusted signal this ADR stops trusting): desktop
  (≥640px) gets an inline icon-button immediately **left of Stop**; mobile
  (<640px) gets a `•••` menu item **above Stop**. One-tap, no confirm,
  accent colour; accessible name **"Interrupt"**, title **"Interrupt the
  current turn (keeps the session)"**; it calls the same `interruptRun`
  (the tmux `Escape`) that ADR-0022 wired.

- **Interrupt and Stop sit adjacent now, so they stop sharing a glyph.**
  Interrupt becomes a new `pause` glyph — Lucide's outline, two rounded
  bars, vendored into `web/src/components/Icon.tsx` — accent, one-tap. Stop
  keeps its unchanged `square`, danger-red, two-step verbal confirm ("Stop and
  end this instance"). ADR-0019's "icon-only never applies to a destructive
  confirmation" still holds: the destructive control keeps its confirm,
  the non-destructive one stays one-tap.

- **`canClear` is un-gated from `working`.** New conversation follows the
  composer's own lock: available whenever the composer is not `composerLocked`
  (ended, transcript-gone, dialog-pending, or a degraded question) — the
  old `working` exclusion existed only to mirror ADR-0022's send-block,
  and it dies with that block. This kills the lockout's second half.

- **The two locked-state escape hatches stay**, adopting the `pause` glyph
  for visual consistency with the header: the dialog-pending waiting note
  and the degraded-question locked state both keep their `InterruptButton`,
  unchanged in behaviour otherwise.

- **The state badge still shows a possibly-wrong "working."** Accepted as a
  cosmetic gap: a stuck `working` badge no longer traps anyone downstream of
  it, and the honest fix — teaching `deriveState` a real liveness signal
  — is issue #62's job, not this ADR's.

**Scope guard.** This ADR reaches only the `idle` / `needs_input` /
`working` composer states. The dialog-pending and question free-text
locks are **unchanged**: the authoritative dialog spool and the backend
`ErrDialogPending` still lock the composer while a dialog is pending,
and free text still cannot answer a focused TUI picker (only the dialog's
own Other row can).

## Status

Accepted. Supersedes ADR-0022 — which morphed Send↔Interrupt on
`working` — while keeping ADR-0022's own supersessions of ADR-0016 in
force: the queued-hint removal and the one-tap-no-confirm interrupt carry
forward unchanged, now attached to the relocated header control.

(Numbering note: issue #61's text asked for "ADR-0028"; that number was
already taken by the answered-dialog-cards ADR, which landed first —
this record is ADR-0029.)

A pure SPA diff: `web/src/routes/RunChat.tsx`, `web/src/components/Icon.tsx`,
`web/src/base.css`, and `web/src/routes/RunChat.test.tsx`, plus this ADR
and the three compat.md edits below. **No** Go, API, SSE, message-schema,
provider, or migration change — a future Codex/Gemini provider inherits
the reversed behaviour for free, same as ADR-0022 did.

## Considered options

- **Keep the ADR-0022 morph and fix `deriveState` first.** Rejected:
  the liveness fix is a materially deeper backend change (issue #62, the
  umbrella for teaching `deriveState` a real liveness signal), and every
  false `working` between now and then is a lockout. Even a *correct*
  `working` needlessly blocks the TUI-supported mid-turn queueing ADR-0016
  originally shipped — the morph's cost was never only its bugs.
- **A lab-managed queue with an optimistic echo.** Rejected: lab cannot
  honestly show the TUI's own queue state — the same "a promise lab can't
  show being kept" reasoning that killed ADR-0016's "queued" hint applies
  just as hard to a lab-drawn optimistic bubble for a reply the TUI has
  not actually accepted yet.
- **Keep Interrupt in the composer, alongside Send, as a second button.**
  Rejected: two adjacent primary actions in the input row invite mis-taps,
  and keeping Interrupt composer-side re-introduces a control whose placement
  implies it is gated on `working`; the header, next to Stop, groups the
  two turn/run-level interventions where they read as a pair instead of
  input-row clutter.
- **Gate the relocated Interrupt on `working` instead of `live`.** Rejected:
  `working` is the untrusted signal this whole ADR exists to stop trusting;
  `live` derives from the run row's own outcome, which cannot false-stick
  the way a stalled transcript tail can.

## Consequences

- `web/src/routes/RunChat.test.tsx` is rewritten: the morph assertions are
  replaced with always-Send (idle/needs_input/working all render `send`,
  enabled purely on non-empty text plus no in-flight send), Cmd/Ctrl+Enter
  sending while `working`, a `live`-gated Interrupt rendering distinctly
  in the header icon-button (desktop) and the `•••` menu (mobile)
  — never merged with Stop — and New conversation un-gated from `working`.
- `internal/compat/compat.md` §6 (Reply send-keys) and §8 (Interrupt
  keystroke) are rewritten for the new surfacing; the §5 state-neutrality
  bullet's present tense is corrected so a false `working` is documented
  as reaching only the state badge.
- The pulse keyframe and the working-cue CSS (the "tap to interrupt" line
  and its `@media (prefers-reduced-motion: reduce)`-gated pulse) die along
  with the morph they animated.
- A false `working` can no longer block Send, Clear, or Interrupt — the
  residual harm is purely the cosmetic state badge; issue #62's honest fix
  is still owed, but nothing downstream of the badge locks up anymore.
