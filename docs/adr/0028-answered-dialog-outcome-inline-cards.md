# Answered dialogs stay dialog messages carrying their recorded outcome; the pending dialog renders as an inline card placed by the client

ADR-0016's transcript translation demoted a resolved dialog: an
`AskUserQuestion` / `ExitPlanMode` `tool_use` with a matching `tool_result`
fell out of the dialog branch and emitted a generic tool chip, so history
showed a collapsed chip named after the tool with truncated raw-JSON
input/output — what was asked and what was chosen was unreadable, especially
on a phone. The pending side had its own mobile failure: the picker rendered
inside the sticky bottom composer behind nested `max-height` scrollboxes
(multi-question forms at ~half the viewport, plan markdown capped client-side
and cut server-side at `truncateLimit`), effectively laying over the
conversation. Issue #56 grilled both to consensus.

The wire change is one nullable field: `provider.Dialog` gains `Outcome`. A
dialog message with `Outcome == nil` is pending — exactly the old meaning —
and non-nil is answered and stays in the stream as history. Everything else
(placement, the collapsed composer, full plans) follows client-side, without
touching ADR-0020's server contract.

The decisions, pinned (settled with the operator 2026-07-08 — do not
re-litigate):

- **A resolved dialog tool is a dialog message, never a demoted tool chip.**
  The claudecode fold's first pass now collects `resolutions` (a
  `resolvedTool` per resolved id: the event-level `toolUseResult` /
  `toolDenialKind` — the same compat §5 ground truth the ADR-0020
  verification backstop reads, so this adds a reader, not a new coupling),
  and the `tool_use` branch emits the dialog with `Outcome` stamped by
  `outcomeFor` (`dialogoutcome.go`). The message is not registered in
  `byTool`: its outcome is derived up front, there is no chip to back-patch
  when the `tool_result` event follows. The transcript records a resolution
  for every live answer path — TUI, claude.ai, or lab — so history is
  complete regardless of who answered.
- **The outcome shape is display-only and best-effort.**
  `DialogOutcome{Dismissed, Results, Approved, Feedback}` with
  `QuestionResult{Question, Chosen, OtherText}`, one per question in dialog
  order. Recorded answers resolve against listed option labels by equality:
  single-select free text lands verbatim in `OtherText`; multi-select splits
  the ", "-joined record in toggle order, non-label leftovers re-joining as
  the free text; plan rejection feedback rides after the last
  `"the user said:\n"` marker. Anything resolved without a readable answer —
  denial, interrupt, the 60s unattended timeout's `answers:{}` — degrades to
  `Dismissed`, never to a pending-looking nil. Outcome gates nothing; answer
  desync warnings stay the intent backstop's job (`dialogintent.go`,
  untouched).
- **Only a pending dialog means question.** `deriveState` returns
  `StateQuestion` for a tail dialog only when `Outcome == nil` — a
  retro-flushed answered dialog at the tail means the agent just resumed and
  derives working, exactly as its demoted chip did. `internal/chat`'s
  dormant-fallback `lastDialog` skips answered dialogs, so `AnswerDialog` can
  never be aimed at a resolved picker.
- **Pending placement is the client's; ADR-0020 decision 5 stands.** The
  server serves the pending dialog only through the side-channel
  `pending_dialog` field — never a stream message, so seqs stay
  reparse-stable. The SPA renders it as the interactive card inline in the
  chat stream (at its dialog message's position when one matches by
  `tool_id`, else appended), deduped by `tool_id` so the same dialog never
  renders twice; answered dialogs render as compact, inert Q→A summaries
  (a dismissed marker when no answer landed). No nested scrollboxes — the
  chat pane is the only scroll container. Arrival scroll: only a
  not-yet-seen `tool_id` retargets the follow scroll to the card's top; a
  reader scrolled up into history is never yanked.
- **The composer collapses while a dialog is pending** to a one-line waiting
  note pointing at the card, plus the existing Interrupt — no textarea, since
  free text cannot answer a focused TUI picker (only the explicit Other row
  carries free text). The degraded state (question with no structured dialog)
  keeps its locked note.
- **Plan markdown is never truncated.** `planDialog` passes the full plan as
  `Prompt` (issue #56 decision 4: a cut plan is unreviewable, and plans are
  model-output-bounded, so the payload growth is accepted), pending and
  answered alike — the live spool path reuses the same mapper. Question
  prompts stay exempt from `truncateLimit` too: the prompt doubles as the key
  `toolUseResult.answers` records under, so truncating it would break the
  outcome lookup.

## Status

Accepted. Amends ADR-0016 in two places: the translation premise — the dialog
message kind now means pending or answered, `Outcome` disambiguating — and
its transport tension note, whose "an answered dialog re-parses as a tool
message at the same seq" becomes "re-parses as an ANSWERED dialog message at
the same seq"; the client's latest-window merge-by-seq acceptance carries
over unchanged. Leaves ADR-0020 decision 5 intact: pending dialogs stay OUT
of the message stream server-side, spool-served via `pending_dialog`. No
schema change, no new compat coupling — compat §5's pinned resolution shapes
gained a second reader. Settled via issue #56.

## Considered options

- **Keep the tool-chip demotion and parse the raw JSON client-side.**
  Rejected: it moves a pinned Claude Code coupling — the §5 `toolUseResult`
  shapes — out from behind the provider seam into the SPA, exactly what the
  seam exists to prevent; a future provider would leak its native shapes the
  same way.
- **Derive the displayed answer from the in-memory answer-intent registry
  (`dialogintent.go`).** Rejected: the registry is restart-lossy and records
  only lab-sent answers; the transcript's `toolUseResult` is stamped on every
  live resolution regardless of who answered (TUI, claude.ai, lab) and lives
  as long as the transcript.
- **Emit the pending dialog as a synthetic stream message server-side, so
  placement is uniform.** Rejected: ADR-0020 decision 5 — the stream stays
  purely transcript-derived so seq numbers remain reparse-stable and there is
  nothing to un-say when the retro-flush lands at a real seq. Placement is a
  rendering concern, and the client already holds both inputs.

## Consequences

- `internal/provider`: `Dialog` gains the nullable `Outcome`; new wire types
  `DialogOutcome` and `QuestionResult`; the `MessageDialog` doc reads
  "pending (Outcome nil) or answered".
- `internal/provider/claudecode`: the fold's first pass keeps `resolutions`
  instead of a boolean answered-set; the dialog branch emits answered dialogs
  and skips `byTool`; new `dialogoutcome.go` (`outcomeFor` / `planOutcome` /
  `questionOutcome`) mirrors `dialogintent.go`'s parsing patterns;
  `planDialog` drops its truncate call.
- `internal/compat`: the §5 clause records the outcome derivation as a second
  reader of the pinned resolution shapes; the live-transcript tests re-pin
  the resolutions as dialog-with-`Outcome` assertions instead of chip strings.
- `internal/chat`: `lastDialog` (the dormant fallback answer path) skips
  messages with `Outcome` set.
- SPA + mirror: `web/src/api.ts`'s `Dialog` mirror gains the outcome types;
  `RunChat.tsx` renders the pending card inline (dedupe by `tool_id`,
  top-of-card arrival scroll), the answered Q→A summaries, and the
  waiting-note composer; `base.css` moves options off pill segments onto
  card styling.
