# Composer autocomplete gets tiered ranking; Tab completes, Enter always sends, click sends unless hinted — partially reversing ADR-0031's popover-Enter-accept precedence

The slash-command popover's matcher is a plain case-insensitive substring test
across a command's `name`, `description`, and `arg_hint`, results stay in
catalog order (builtins pinned, then project skills alphabetical), and the
highlighted row is always index 0. Typing `/triage` therefore highlights
`setup-matt-pocock-skills` instead of `triage` — its *description* happens to
contain the word "triage" and it sorts ahead of `triage` in catalog order —
and accepting the highlight inserts the wrong skill. Discovery-via-description
is a real, wanted behaviour (a user typing a topic word should still surface a
relevant skill), the bug is only that it can outrank an exact or prefix match
on the command's own name.

Separately, ADR-0031 pinned "Slash-command popover precedence is unchanged":
while the popover is open, Enter (like Tab) accepts the highlighted command
and the send gate is only reached once the popover is closed. That was a
deliberate carry-forward at the time, but in practice it makes sending any
slash command a two-step gesture — Enter inserts `/name `, and a *second*
Enter is needed to send, even for a zero-argument command like `/clear`. Both
complaints were raised and resolved in the same grill session (issue #122,
2026-07-10).

This ADR is a **pure frontend diff** — no colour/type/radius/shadow change, no
backend/API/SSE/message-schema/provider/migration change, the same class of
change as ADR-0019/0022/0029/0031. It **partially reverses** ADR-0031's
"Slash-command popover precedence is unchanged" clause (itself inherited from
issue #70): Enter no longer accepts the highlighted command under any
circumstance. Every other ADR-0031 decision — the fine-pointer
`matchMedia`-gated bare-Enter-sends rule, the IME guard (`isComposing` /
`keyCode === 229`), Shift+Enter and Alt+Enter staying an explicit newline,
Cmd/Ctrl+Enter sending everywhere regardless of pointer type or agent state,
and the empty-composer no-send-but-`preventDefault` rule — stands exactly as
pinned. `isComposerSend` itself is untouched; this ADR only removes the
popover-acceptance branch that used to run *ahead of* it.

The decisions, pinned (settled with the maintainer 2026-07-10 via /grill-me —
do not re-litigate):

- **Tiered ranking in `acMatches`.** Matches are ranked into four tiers,
  evaluated in order, with catalog order preserved as a stable sort *within*
  a tier and the highlight always staying at index 0 of the ranked list:
  1. exact name match, 2. name prefix match, 3. name substring match,
  4. description/arg_hint substring match. Query `/triage` now highlights
  `triage` (tier 1) with `setup-matt-pocock-skills` (tier 4) still present
  below it — discovery via description still works, it just never outranks a
  match on the command's own name.
- **Tab is the only completion gesture.** It inserts `/name ` for the
  highlighted suggestion, keeps focus in the input, and dismisses the popover
  until the next keystroke re-opens it. Nothing else completes a suggestion.
- **Enter always sends the input as-is.** With the popover open or closed,
  Enter falls straight through to the existing `isComposerSend` rules (bare
  Enter on a fine pointer, Cmd/Ctrl+Enter everywhere, IME guard, Shift/Alt+Enter
  passthrough) and never accepts or auto-completes the highlighted row. This
  is the deliberate reversal: issue #70 / ADR-0031 had popover-Enter-accept
  run ahead of the send gate; that branch is now removed from `onKeyDown`
  entirely.
- **Click on a suggestion sends immediately when it has no `arg_hint`**; when
  an `arg_hint` is present, click instead completes to `/name ` and focuses
  the input so the argument can be typed. `arg_hint` presence is the one
  "this command expects more text" signal the popover has, and it now drives
  both the ranking-independent completion choice and the click behaviour.
- **ArrowUp/Down navigate, Escape dismisses — unchanged.** Neither gesture
  sends or completes; they only move or clear the highlight.
- **No data-model, backend, or `SKILL.md` frontmatter changes.** No new
  `CommandSpec` field, no backend change, no vendored-skill frontmatter edit —
  `arg_hint` is read as it already exists today. Hints can be added per-skill
  later as pain arises, but that is a separate, future decision.

## Status

Accepted. Partially reverses ADR-0031 (and, through it, the issue #70
decision ADR-0031 had carried forward): Enter no longer accepts a highlighted
slash-command suggestion under any circumstance, popover open or not. Every
other ADR-0031 decision — the fine-pointer bare-Enter gate, the IME guard,
Shift/Alt+Enter newline, Cmd/Ctrl+Enter sending everywhere, and the
empty-composer guard — remains in force, unamended. A pure SPA diff:
`web/src/routes/RunChat.tsx` (`acMatches`, `onKeyDown`, `acceptCommand`, and
the popover row click handler) and `web/src/routes/RunChat.test.tsx`. **No**
Go, API, SSE, message-schema, provider, or migration change.

## Considered options

- **Fuzzy matching (e.g. subsequence or edit-distance scoring) instead of
  tiered substring ranking.** Rejected: out of scope for issue #122 — the
  reported bug is that a *worse* substring match outranks a *better* one, not
  that substring matching itself is too strict; tiering plain substring
  results already fixes the reported case with far less surface area than a
  fuzzy-scoring engine.
- **A new `CommandSpec`/frontmatter field such as `sendable` to declare
  whether a command takes arguments.** Rejected: `arg_hint`'s presence already
  encodes exactly that signal today, and adding a second field for the same
  fact would be a data-model change this ADR explicitly keeps out of scope.
- **Keep popover-Enter-accept precedence (ADR-0031/#70's status quo) and only
  fix ranking.** Rejected: ranking alone doesn't address the second complaint
  — sending any slash command, even a correctly-highlighted zero-argument one
  like `/clear`, would still take two Enters.
- **Extend the same autocomplete/gesture model to the NewRun spawn
  composer.** Rejected as out of scope: that composer has no autocomplete
  today, and adding one would first need a catalog source to draw from before
  a run even exists — a separate piece of work.

## Consequences

- `RunChat.tsx`'s `acMatches` is rewritten to rank into the four tiers above
  instead of a single substring test, with catalog order used only as the
  within-tier tiebreaker.
- `RunChat.tsx`'s `onKeyDown` drops the popover Enter/Tab-accept branch
  entirely; Tab gets its own handler that completes-only (no send), and Enter
  falls straight through to `isComposerSend` unconditionally. ArrowUp/Down and
  Escape are unchanged.
- `acceptCommand` is split into a complete-only path (used by Tab and by
  click-with-`arg_hint`) and a send path (used by click-without-`arg_hint`);
  neither path is reachable from Enter any more.
- The popover row click handler now checks `arg_hint` to decide whether to
  send immediately or complete-and-focus.
- `RunChat.test.tsx`'s "Slash-command autocomplete" block is updated: the old
  Enter-accepts test and the #70 Enter-precedence test are rewritten for the
  new contract (Enter sends the raw input, complete or partial, popover open
  or not), and new tests cover tiered ranking (exact/prefix beats a
  description match), Tab-completes-without-sending, click-sends when there is
  no `arg_hint`, and click-completes-and-focuses when there is one.
- Accepted consequence: partial input like `/tri` followed by Enter sends the
  literal text `/tri` — completing first with Tab is the user's own
  responsibility, the same way a typo isn't auto-corrected before sending.
- Accepted consequence: clicking `/clear` executes it in a single click, while
  the Enter path still requires the full command text `/clear` to be present
  in the input — typing (or completing to) the whole word is itself the
  confirmation step that a one-click affordance skips.
- Accepted consequence: clicking `/land-pr` sends it bare today (it declares
  no `arg_hint`); the skill itself prompts for the PR number rather than the
  composer collecting it first.
