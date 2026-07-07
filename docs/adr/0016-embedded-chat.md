# Embedded chat: view and reply to an instance's conversation inside lab's UI

ADR-0005 pinned the interaction model as deliberate scaffolding: lab manages
lifecycle, the operator drives sessions in the claude.ai app through captured
**deep links**, and the API/SSE surface exists so the "embedded remote
interface (chat/terminal inside lab's UI)" it names as roadmap needs no
redesign when it lands. This is that interface — the chat half of it.

The embedded **Chat** is a phone-first view inside lab's UI that renders an
instance's conversation and lets the operator reply directly, for manual
instances and AFK runs alike. It **complements** the deep link (which stays as
the escape hatch for anything the chat can't do), never replaces it. Claude
Code is the only provider implementation, but the message schema and every
mechanism sit behind the `AgentProvider` seam, so a future Codex/Gemini chat is
one new implementation and zero refactor.

The decisions, pinned:

- **Read is read-through, not replicated.** The chat reads the provider-native
  **Transcript** — for Claude Code, the live JSONL under
  `~/.claude/projects/<cwd-slug>/<sessionId>.jsonl`, located by worktree-cwd
  match exactly as the deep link is (the same registry file even, now read for
  its `sessionId`). The provider maps it behind the seam into a small
  **universal schema** — `text | tool | dialog | lifecycle`. There is **no
  message table**: the sole persisted state is one nullable column,
  `runs.transcript_path`, captured async by cwd-match (the `deep_link_url`
  pattern) so ended runs stay readable while the provider retains the file; a
  retired file degrades to a graceful "transcript no longer available" state.
- **Reply is `tmux send-keys`.** The argv-only stance for the *initial* prompt
  stands (the cold-start TUI race, ADR-0008); mid-session replies are exempt.
  Free text is delivered as a bracketed paste plus Enter, so multi-line content
  inserts lines instead of submitting turn-by-turn; a mid-turn agent queues it
  in its own TUI (the UI shows a subtle "queued" hint). The composer is
  disabled for ended instances.
- **Interactive dialogs render native buttons from structured input, never a
  TUI scrape.** A pending dialog is an unanswered `tool_use` in the transcript.
  For `AskUserQuestion`, the option buttons come from the tool input and are
  answered by a pinned keystroke recipe (normalise the picker to the top,
  `Down`×N, `Enter`; `Space` toggles multi-select; "Other" selects, types,
  Enter). While a dialog is pending the free-text composer is locked so stray
  text cannot hit a focused picker. Shapes lab cannot drive from the input
  (multi-question `AskUserQuestion`, plan approval whose choices are TUI-owned,
  unknown tools) degrade to an "open in claude.ai" deep-link hint. Answers the
  operator gives in claude.ai flow back through the transcript — no divergence
  handling.
- **Interrupt is an explicit Escape** behind a confirm tap, distinct from a run
  Stop. There is no slash-command UI — slash commands are plain text through
  the composer.
- **Transport reuses the existing bus.** A per-live-instance **tailer** polls
  the transcript and publishes a debounced `run.messages.changed`
  `{type, repoID, runID}` (added to the ADR-0005 canonical event list); the
  chat view refetches `GET /api/v1/runs/{id}/messages?after=<cursor>` — an
  append-only cursor (the message sequence), latest window first, older on
  scroll-up. SSE payloads stay envelope-small (ADR-0005). The tailer keeps its
  set in sync with the active runs off the same `run.changed` it already sees
  (plus a periodic resync, since the bus may drop events for a slow
  subscriber), so no wiring into instance/afk/reconcile is needed.
  A known tension: seq numbers are reparse-stable, but two mutations happen
  *behind* the cursor — a tool chip's status/output back-patches when its
  result lands, and an answered dialog re-parses as a tool message at the same
  seq. An `after=` fetch alone never re-delivers those, so the client pairs the
  cursor tail with a latest-window refetch and merges by seq; a mutation older
  than the latest window is only caught by the client's window accumulation.
  Accepted as inherent to read-through + append-only (revisit with a
  `changed-since` param if it ever bites in practice).
- **Signaling comes from the tailer.** It derives a per-instance conversational
  state — *working / needs input / question pending / idle* — served on the
  instance list and shown as live badges on Dashboard rows and the chat header.
  Web Push stays a separate roadmap item that later plugs into these states.
- **Intervention is neutral.** Replying to or interrupting an AFK run has no
  effect on its budget clock, claim, or the three-strikes counter — structural,
  because nothing in the chat path writes a run outcome or ends a session
  (reference ADR-0010's neutral Stop).
- **Four new fragile couplings, pinned like the existing ones.** The transcript
  location + JSONL schema, the reply send-keys recipe, the dialog keystroke
  recipes, and the Escape interrupt each get a section in
  `internal/compat/compat.md` (§5–§8) with provenance, live-verified against the
  pinned Claude Code version like couplings §1–§4.

## Status

Accepted. Realises the embedded remote interface ADR-0005 left as roadmap and
consumes its API/SSE surface unchanged (one new event, no diff protocol).
Extends the `AgentProvider` seam (ADR-0008) with the chat surface. Reaffirms
ADR-0010's intervention neutrality. Migration 0003 adds
`runs.transcript_path` — the one schema change. A future Codex/Gemini provider
must implement the chat seam methods when it lands.

## Considered options

- **Replace the deep link with the embedded chat.** Rejected: the chat must
  reach feature parity (approvals of every shape, image input, raw terminal)
  before claude.ai can be dropped; making it a complement lets the new surface
  mature incrementally while nothing existing changes, and keeps one less
  fragile coupling from becoming load-bearing prematurely.
- **A message table (persist the conversation in lab's DB).** Rejected: the
  transcript is already the provider's durable, complete record; mirroring it
  into a table doubles the storage, invites drift, and buys nothing the
  read-through plus one `transcript_path` column doesn't. Ended runs stay
  readable as long as the provider retains the file — the same lifetime the
  deep link already has.
- **Scrape the TUI pane for dialog options and render those.** Rejected: the
  pane widget is the most volatile surface claude has; building buttons from
  the structured tool input is stable and testable, and the shapes without
  structured options degrade to the deep link rather than to a brittle scrape.
- **A WebSocket / embedded raw terminal (xterm.js) now.** Deferred (ADR-0005
  pre-authorises a socket for the terminal without disturbing this surface): a
  chat-first, refetch-on-event view covers the phone-first reply/approve/
  interrupt need at envelope-small transport cost; the raw terminal is a
  separate roadmap item.
- **Answer multi-question `AskUserQuestion` and plan approval through
  keystrokes.** Deferred: the single-picker recipe is deterministic and safe;
  multi-picker sequencing and the TUI-owned plan-approval widget are not
  capturable from structured input today, so they degrade to the deep-link hint
  until captured live.

## Consequences

- `internal/provider` gains the universal chat schema and five seam methods
  (`LocateTranscript`, `ReadTranscript`, `Reply`, `AnswerDialog`, `Interrupt`);
  `internal/tmuxx` gains `SendNamedKeys` and `PasteText` (the old literal-text
  `SendKeys` could not send `Escape`/arrows or a bracketed paste).
- `internal/chat` is the new brain: the read/act facade plus the tailer. It is
  a bus subscriber, so its lifecycle is self-managing.
- The instance list grows a `state` field; Dashboard rows and the chat header
  render it as a live badge. Web Push (roadmap) plugs into the same states.
- The four compat couplings join the upgrade checklist: when a Claude Code
  upgrade breaks the chat, `compat.md` §5–§8 say exactly what to re-verify.
