# Answerable interactive dialogs: capture the pending dialog live via a PreToolUse hook spool

ADR-0016 shipped the embedded chat's dialog render + keystroke-answer path on a
single premise: *"a pending dialog is an unanswered `tool_use` in the
transcript."* That premise is false live. Verified on Claude Code 2.1.198
(2026-07-07): Claude Code does **not** write a pending `AskUserQuestion` /
`ExitPlanMode` `tool_use` to the session JSONL while it is pending — the
transcript file does not change *at all* during the pending window; the
`tool_use` **and** its `tool_result` are flushed together, retroactively (with
their original timestamps), only when the dialog resolves. So
`chat.PendingDialog` never returned true while a question was open, the chat
rendered nothing, the composer stayed unlocked over a focused picker, and the
whole render+answer path (`dialogFromToolUse` → `Dialog` → `DialogKeystrokes` →
`AnswerDialog`) never executed against a live picker in production — it was
snapshot-tested only. This ADR is its first live activation.

The fix feeds the pending dialog from a source that exists *while the question
is open* — a Claude Code **PreToolUse hook** — and takes on the hook contract as
a **fifth pinned Claude Code coupling** alongside the transcript/JSONL and the
send-keys recipes (compat §9). Answering still goes back through the pinned
send-keys recipe into the live TUI picker (hooks cannot inject a selection).

This ADR **amends ADR-0016**:

- **"A pending dialog is an unanswered `tool_use` in the transcript"** becomes
  **"…in the transcript *or* the live PreToolUse spool."** The spool is
  authoritative for a lab-spawned session; the transcript scan stays as a
  **dormant fallback** that lights up automatically if a future Claude Code
  flushes pending `tool_use` (nothing to change when it does).
- **Dialog detection** no longer derives from the transcript tail alone. The
  chat service overlays a spool-derived pending dialog (→ `StateQuestion`) and a
  Notification-derived blocked marker (→ `StateNeedsInput`) onto the
  transcript-derived state for an active run. Composer lock, `ErrDialogPending`,
  and the answer guard all key off this same server-side signal.

The decisions, pinned (settled with the operator 2026-07-07 — do not
re-litigate):

- **Source = the PreToolUse hook.** It fires *before* the picker shows, carrying
  the exact structured `tool_input` + `tool_use_id` + `session_id`. This honours
  ADR-0016's *"native buttons from structured input, never a TUI scrape"*
  literally — the hook payload **is** the structured input, and the same
  `dialogFromToolUse` maps it (one mapper, two sources: transcript and spool).
  The hook is purely observational: it exits 0 with no output, so the local
  operator's TUI picker still shows and the tool is never blocked (only a
  PreToolUse exit code 2 blocks — compat §9).
- **Transport = a spool file.** The hook command atomically writes its stdin
  (temp + rename) to a per-run file under lab's runtime dir. No auth, no new
  endpoint, no dependency on the server being up — the file persists, so a
  pending dialog **survives a lab restart** for free. The tailer already stats
  the transcript every poll; it stats the spool too, so latency matches every
  other chat update.
- **Injection = per-run `--settings <runtime file>`.** At launch, lab writes a
  per-run settings JSON under runtime/ (same dir/lifecycle as the materialized
  credential keys) and appends `--settings <path>` to the spawn argv, *before*
  the trailing prompt positional. `--settings` merges hooks **additively** over
  the repo-shipped `.claude` settings (compat §9), so nothing else is disturbed;
  the worktree stays pristine (nothing the agent can read-modify-commit); the
  `ps`/`tmux` argv stays short. lab knows the runID at spawn, so it bakes the
  exact spool path into the hook command.
- **Scope = `AskUserQuestion` + `ExitPlanMode`** — exactly the two tools
  `dialogFromToolUse` already recognises. A single-question `AskUserQuestion` →
  **fully answerable native buttons** (the goal). A multi-question call → render
  the real content + a deep-link hint. `ExitPlanMode` → render the **actual plan
  text** (which read-through could never show, since the pending `tool_use` is
  not flushed) + a deep-link hint; approve/reject stays TUI-owned per the
  never-scrape rule.
- **The pending dialog reaches the client as a nullable top-level
  `pending_dialog` field** on `GET /runs/{id}/messages`, alongside
  `state:"question"`. The message stream stays **purely transcript-derived** —
  seq numbers remain reparse-stable, no synthetic message to un-say when the
  retro-flush lands at a real seq.
- **Spool lifecycle.** The spool is keyed by runID; the `tool_use_id` is stored
  inside it as the answer-guard token (only one dialog can be pending per
  session — one file, overwritten). **Primary clear = a PostToolUse hook** on
  the same matcher, which deletes the spool the instant the question resolves by
  any route (TUI, chat send-keys, or claude.ai). **Backstop = the tailer**,
  which suppresses (and the run-ended GC deletes) a spool whose `tool_use_id`
  has appeared *resolved* in the retro-flushed transcript — covering Esc-reject,
  process death, or a missed hook. The **answer guard** (under the existing
  per-session lock): the spool exists **and** its `tool_use_id` == the client's
  `tool_id` **and** that id is **not yet** in the transcript → play keystrokes;
  else `ErrDialogChanged` / `ErrNoDialog`.
- **Residual blocked states get a badge + a generic card.** A Notification hook
  spools a lightweight per-run marker carrying the `notification_type`
  (`permission_prompt` / `idle_prompt` / `agent_needs_input`). The tailer maps a
  live marker to `StateNeedsInput` so the badge is correct for **all** blocked
  states — including a plain tool-permission prompt (rare under
  `--permission-mode auto`, but real) and the post-decline "stuck on working"
  case — and clears it once the transcript advances past the block. When the
  state is question/needs-input but there is **no** structured `pending_dialog`,
  the chat shows a generic *"Claude needs input — open in claude.ai"* card.
- **Picker geometry re-verified.** The live 2.1.198 `AskUserQuestion` picker has
  **two** synthesized trailing rows — "Type something." (the "Other" row, already
  modelled) **and** "Chat about this" (not modelled), which sits *below* Other.
  Down-navigation to a listed option or Other is unaffected, but the
  normalise-to-top climb must cover the true picker height; it is now `Up ×
  (len(Options) − 1 + trailing synth rows)` (compat §7). Over-climbing is safe —
  the picker clamps at the top.
- **The answer recipe is paced (live bug found).** Live-verified end to end on
  2.1.198: driving the picker with the send-keys ops back-to-back (no gap) over
  the remote-control bridge **intermittently** raced the committing `Enter`
  ahead of the `Down` navigation and selected the wrong option. `AnswerDialog`
  now sleeps a pinned `keyDelay` (250ms) between ops; 0ms was flaky, 150ms+
  reliable (compat §7). This is the first thing found by driving a real picker —
  exactly the "unproven, live-verify" the brief flagged.
- **Neutrality** — inherited from ADR-0016 dec 12 / ADR-0010: nothing in this
  path writes a run outcome or touches the budget clock, claim, or three-strikes
  counter.

## Status

Accepted. Amends ADR-0016 (the pending-dialog premise and dialog detection) and
extends the `AgentProvider` seam with an optional `DialogHooker` capability
following the ADR-0017 pattern (a type assertion at the call site, like
`ConnectingReporter` / `DeepLinker`): claude-code implements it; a provider with
no live-hook surface omits it and keeps transcript-only behavior. No schema
change. Adds compat §9 (the hook contract), re-verifies §7 (picker geometry
against the live "Chat about this" row), and updates §5 (flush-on-resolve). Only
lab-spawned sessions carry the injected hooks; a non-lab session keeps
transcript-only behavior (out of scope, as before).

## Considered options

- **Keep reading the transcript only, and wait for Claude Code to flush pending
  `tool_use`.** Rejected: it is not flushed live on 2.1.198, so the entire
  ADR-0016 answer path would stay dead in production with no timeline for a fix
  we control. The transcript scan is retained as a dormant fallback, not the
  live source.
- **Scrape the TUI pane for the pending question.** Rejected for the same reason
  ADR-0016 rejected it — the pane widget is the most volatile surface Claude has.
  The PreToolUse payload is the *structured* input, which is exactly what the
  never-scrape rule wants; scraping buys nothing and is brittle.
- **A bridge / WebSocket-native answer channel (native plan-approval buttons,
  full fidelity).** Deferred (roadmap-sized). Hooks cannot inject a selection, so
  answering still rides the pinned send-keys recipe into the live picker; the
  hook contract is the minimal live-capture seam that makes single-question
  `AskUserQuestion` answerable today.
- **Inject the hooks via the repo-shipped `.claude/settings.json` (seeded into
  the worktree).** Rejected: it would put a lab-owned, spool-path-bearing file
  inside the worktree, where the agent could read-modify-commit it and where the
  path would be wrong after a restart. Per-run `--settings` under lab's runtime
  dir keeps the worktree pristine and the spool path exact.
- **A new authenticated endpoint the hook POSTs to.** Rejected: it needs the
  server up at the moment the picker opens and adds auth surface. A spool file
  needs neither, and survives a lab restart for free.

## Consequences

- `internal/provider` gains the optional `DialogHooker` capability
  (`HookSettings`, `PendingDialog`, `BlockedState`, `SpoolSig`, `SweepSpools`);
  `internal/provider/claudecode/dialogspool.go` implements it, reusing
  `dialogFromToolUse`. The `providertest` fake implements it too (scriptable).
- `internal/instance/launch.go` writes the per-run settings file and injects
  `--settings` at spawn (rollback unlinks it). `internal/chat` overlays the
  spool signals onto the transcript-derived view (`Read` returns a `View` with a
  side-channel `PendingDialog`), the tailer republishes on **state** change (not
  just file change) and GCs spools for ended runs, and the answer guard keys off
  the spool.
- `internal/httpapi` adds the nullable top-level `pending_dialog` to the
  messages response. The SPA prefers it over the messages-scan (kept as the
  dormant fallback), adds the generic needs-input card, and drives the badge
  from the new state.
- A fifth fragile Claude Code coupling (compat §9 — the hook payload shapes +
  the spool protocol) joins the upgrade checklist. When a Claude Code upgrade
  breaks live dialog capture, §9 says exactly what to re-verify.
