# Provider seam v2: generalize the agent-CLI contract so lab's chat works with any agent CLI

ADR-0016 built the embedded chat behind the `AgentProvider` seam and promised
that "a future Codex/Gemini chat is one new implementation and zero refactor."
That promise was aspirational: the universal schema carried only single-question
dialogs, ADR-0016 stated flatly "there is **no** slash-command UI", clearing
context was an accident of Claude's transcript-file rotation, auth and seeding
were claude-shaped, and `internal/httpapi` imported `claudecode` for its typed
errors. This issue makes the promise real. It generalizes the seam so lab's chat
works with **any** agent CLI (Codex CLI, Gemini CLI, …) and proves it
provider-neutral **by construction** — **without implementing a second provider
here** (#2 tracks Codex). Claude Code is brought up to the generalized contract;
the seam is now the written surface a second provider ports against, not a
claude shape a second provider must reverse-engineer.

The proof is structural, not a promise. Every claude-specific assumption is
lifted into one of two places: **provider-declared metadata** (`DisplayName`,
`AuthFlow`, `SeedMeta`, the `Commands` catalog — the "provider declares, lab
renders generically" pattern ADR-0021 established for spawn options) or a
**documented seam obligation** (the clear/epoch duty on `LocateTranscript`, the
reply-while-working and interrupt footnotes). Nothing outside
`internal/provider/claudecode` hardcodes a model, an effort, a provider name, a
login mechanic, a seeded path, or a clear command — enforced by the SPA
grep-guard and by `httpapi` importing no concrete provider. The generic layer
stays pure data-in/data-out; the adapter owns every fragile CLI coupling, each
pinned in its own `internal/compat` section.

This ADR **amends ADR-0016 and ADR-0020**.

**ADR-0016** (embedded chat):

- **The universal schema gains dialog kinds and multi-question forms.**
  `Dialog.Kind` (`question | plan | approval`) and `Dialog.Questions []Question`
  join the schema. A **single**-question dialog keeps the original flat shape
  (`Prompt`/`Options`/`Multi`, `Questions` nil) byte-for-byte — the compat
  snapshots stay valid, existing single-picker behavior is unchanged. A
  **multi**-question form (`len(Questions) >= 2`) carries each question with its
  own options and multi-select flag; the UI collects all answers in one submit
  and `POST /runs/{id}/answer` grows a **positional** per-question `answers[]`
  (`answers[i]` answers `Questions[i]`).
- **Sent slash commands are conversational context, not a breadcrumb to drop.**
  ADR-0016 (hardened by issue #45) dropped a local-command echo entirely. It now
  renders as a **plain user text message** showing the command line (`/clear`,
  `/foo bar baz`), and a non-empty `<local-command-stdout>` renders as a
  follow-up **lifecycle** message. This reverses only the *rendering* half of
  #45; the *state* half stands unchanged — command echoes never touch state
  derivation, which is exactly what keeps a post-`/clear` fresh transcript from
  reading `working` forever.
- **Clearing context is a documented seam duty, not luck.** ADR-0016 depended,
  unstated, on Claude rotating its transcript file on `/clear`. That becomes the
  **clear/epoch obligation** on `LocateTranscript`/`ReadTranscript`: an adapter
  MUST surface a new conversation identity whenever its provider clears context,
  and lab detects the clear only by its **effect** — the located transcript
  changing — never by parsing any provider's clear command. Claude satisfies it
  for free by rotating the file (compat §5); an adapter whose CLI clears in place
  must synthesize a new epoch (e.g. an epoch-qualified path) so the identity
  still rotates and `seq` restarts.
- ADR-0016's "there is no slash-command UI — slash commands are plain text
  through the composer" is **superseded**: there is now a curated command
  catalog behind composer autocomplete (decision 5 below), while execution still
  rides the plain-text reply path.

**ADR-0020** (answerable dialogs via the hook spool):

- **`DialogHooker` becomes the provider-neutral `LiveSignals`** — the same
  contract (spawn-time setup returning a settings payload + argv injection,
  pending-dialog read, blocked-state read, cheap change signature, spool sweep),
  the one method rename `HookSettings`→`Setup`. The capability was never
  claude-specific; only its PreToolUse/PostToolUse/Notification hook
  implementation is, and that stays exactly as ADR-0020 shipped it.
- **The two shapes ADR-0016/0020 left deep-link-degraded are now answerable.**
  Multi-question `AskUserQuestion` (ADR-0020: "render the real content + a
  deep-link hint") and plan approval `ExitPlanMode` (ADR-0020: "approve/reject
  stays TUI-owned per the never-scrape rule") are both answerable in-chat now,
  through **synthesized option rows** and the sequenced picker recipe — and,
  unlike ADR-0016's original single-picker recipe, this time driven end-to-end
  against the live binary before shipping (see Live verification).

The decisions, pinned as shipped (settled with the operator 2026-07-08 — do not
re-litigate):

- **Read-through stands (reaffirms ADR-0016 decision 1).** Every provider runs
  its real TUI under tmux; the adapter maps the provider-native transcript into
  the universal schema on read. There is **no** lab-owned message store — a
  reply lands in native history through the provider's own recording, and an
  ended run stays readable as long as the provider retains the file. The
  generalization changes what the seam *declares*, never this premise.
- **Clear rides the command catalog and is observed by effect.** There is no
  seam-level clear method. Discovery: a `CommandSpec` may carry `Role`
  (`CommandRoleClear` today; claude's `/clear`, a codex `/new`), which the
  frontend binds to a **"New conversation"** affordance. Effect: the clear/epoch
  obligation above. Visibility: the echo-as-user-text rendering above. Together
  these mean lab never learns any provider's clear command — it offers a catalog
  entry, sends it as pasted text, and adopts the rotated transcript when the
  located file changes. **The post-`/clear` stuck-composer regression is
  guarded**: a transcript whose only tail is a command echo derives `idle`, not
  `working`, so the composer offers Send and not a stuck pulsing Interrupt
  (regression-tested; compat §5).
- **Dialog kinds, multi-question forms, and adapter-owned bidirectional
  translation.** The adapter owns **both** directions. *Outbound*: for pickers
  whose rows are not in the structured tool input — the free-text "Other" row,
  the whole `ExitPlanMode` picker — the adapter **synthesizes the option rows as
  write-side pinned constants**, compat-verified per provider version exactly
  like the keystroke recipes; the never-scrape rule is untouched, nothing reads
  the pane. *Inbound*: the adapter alone decides what an answer becomes
  (keystroke sequences for a TUI; RPC for a future provider). The generic layer
  stays pure data; the existing guards (per-session lock, `tool_id` match,
  re-read under lock) stay generic. A **post-resolve verification backstop** is
  mandatory for both shapes: `AnswerDialog` records the intended answer in-memory
  (per `tool_use_id`, bounded at 100, oldest-evicted), and when the
  retro-flushed `tool_result` lands, `ReadTranscript` compares the recorded
  outcome against the intent and emits a lifecycle **warning** on mismatch, so a
  blind-sequence desync is visible in the chat. The backstop is advisory-only —
  intents are in-memory, so a warning vanishes after a lab restart (accepted;
  persisting it would buy a rare warning's permanence with a whole storage
  surface).
- **Conversational state is unchanged and best-effort (reaffirms ADR-0016
  decision 11).** The five-state vocabulary (`idle | working | needs_input |
  question | ended`) is the contract. Every adapter's baseline duty is to
  distinguish `working`/`idle`/`ended` from its transcript; `needs_input` and
  `question` arrive only where a live signal channel exists. No per-provider
  state metadata.
- **A mandatory `Commands` catalog drives composer autocomplete.**
  `Commands(ctx, worktree) ([]CommandSpec, error)` is a new **mandatory** seam
  method — worktree-dependent (project/user commands are discovered relative to
  the run's worktree), so it is served per run as `GET /runs/{id}/commands` and
  cached ~30s. claude-code merges a **pinned builtin table** (compat §10 —
  extracted from the 2.1.198 bundle, descriptions verbatim, primary names only)
  with worktree scans of `.claude/commands/`, the seeded user-invocable skills
  under `.claude/skills/`, and the user-level command dir. The adapter **curates
  the list to chat-safe entries**: `ChatSafe=false` marks a command that would
  strand the TUI in a picker/editor/UI lab cannot see (never-scrape: lab could
  neither render nor answer it) or that fights lab's own management of the
  session; the provider returns every row honestly, the API layer filters. The
  frontend autocompletes when the composer input starts with `/`.
- **`LiveSignals` generalizes the live channel, with honest degradation.** A
  provider **without** a verified structured signal channel simply omits the
  capability (a type assertion at the call site, the ADR-0017 pattern) — never
  a scrape, no half-measure. Auto-approval spawn defaults keep dialogs rare, and
  a residual blocked state degrades to `needs_input` plus the copyable tmux
  attach affordance (ADR-0017). lab keeps transcript-only behavior, and the
  transcript-scan dialog path stays a dormant fallback that lights up
  automatically if a future provider flushes a pending `tool_use`.
- **An auth-flow descriptor generalizes login without generalizing prematurely.**
  The provider declares an `AuthFlow` descriptor surfaced on `GET /providers`:
  `oauth-code` (start returns a URL, the operator pastes a code back — claude
  today), `oauth-redirect` (localhost browser redirect; `Instructions` carries
  operator guidance such as an SSH port-forward), `api-key` (a vault credential
  reference injected at spawn — **schema only** in this issue, no implementation
  behind it), and `external` (managed outside lab; status-only). The routes
  become per-provider — `/providers/{id}/auth/{status|login/start|login/code|
  logout}` — the old claude-only names are gone (the SPA ships with the backend).
  The typed login sentinels (`ErrDialogNotAnswerable`, `ErrInvalidReply`,
  `ErrInvalidCode`, `ErrLoginTimeout`) and the SSE event move to the `provider`
  package (claudecode keeps thin aliases so its `errors.Is` chains hold), so
  `httpapi` imports no concrete provider; the event is the provider-generic
  `provider.auth.changed` carrying a provider id.
- **Seeding rides provider-declared metadata behind one generic seeder.**
  `internal/seeder` no longer hardcodes claude shapes. The provider declares a
  `SeedMeta` (context-file name, skills dir, exclude entries, incogni
  scrub/seeded-path patterns) that the one generic seeder consumes; claude-code
  declares today's values. The split stays coherent — the provider's own
  `SeedWorkspace` seeds its trust/MCP/attribution grants, `SeedMeta` describes
  what lab layers on top of any agent CLI. Byte-identity of the seeded
  claude-code worktree is **pinned from both sides** by goldens (the seeder
  asserts "generic seeder + these values == the pre-#51 bytes" against its own
  inlined literals; `claudecode/seedmeta_test.go` pins the declaration to the
  same literals), so a drifting declaration fails a test rather than silently
  reshaping worktrees.
- **Frontend copy is provider-neutral.** `DisplayName()` drives every
  user-facing string (the auth card title, "<name> needs input", the open
  affordance already comes from `fallback_open`). `ClaudeAuthCard` becomes the
  descriptor-driven `ProviderAuthCard` — it renders flows, never providers. A
  grep-guard test (`providerNeutral.test.ts`) fails on a hardcoded
  "Claude"/"claude.ai" string in a component; `reposvc.DefaultProvider =
  "claude-code"` stays (it is a default, not a leaked string).
- **Contract footnotes, documented as seam obligations (issue decision 10).**
  `Reply` while `working` is legal and best-effort — claude queues mid-turn
  input natively; an adapter whose TUI drops it MUST return a typed error the UI
  can surface, never silently lose the text. `Interrupt` recipes are
  adapter-owned and MUST be hazard-checked against the real TUI (some TUIs quit
  or discard work on double-Esc/Ctrl-C). Each future adapter brings its **own**
  `internal/compat` pin + fixture-driven port, and **may not weaken** claude's.

## Live verification (2026-07-08, Claude Code 2.1.198)

This section is the ADR's strongest evidence and mirrors ADR-0020's
live-activation framing. The generalized dialog recipes were driven **end to end
through the production `Reply`/`AnswerDialog` path** against the real pinned
binary — `TestCompat_Live_askUserQuestionRecipe` and
`TestCompat_Live_exitPlanModeApproval` (`internal/compat/live_recipes_test.go`,
gated on `LAB_COMPAT_LIVE=1`), each spawning a real claude, raising the pinned
dialog shape, playing the answer through the real recipe with production pacing,
then reading the transcript back and asserting the recorded answers match the
intent — the same comparison the backstop makes. Doing so **found and fixed four
real defects that snapshot tests could not have caught** (three in the dialog
recipes, §7; one in the reply path, §6):

1. **Up wraps — it does not clamp.** The pre-#51 normalize-to-top climb (and the
   first cut of the new recipes) assumed `Up` clamps at the top row; it does
   not. One `Up` from the top option jumps to the **last** option, and on the
   review screen from "Submit answers" to "Cancel" — so `[Up][Enter]` on the
   review screen silently landed on **Cancel and discarded the whole form** (the
   model then re-asked). Fix: **no climb** — every freshly presented picker
   already opens on row 0, lab always answers a fresh picker in one shot, so the
   recipes walk purely **downward**; the review step is a bare `[Enter]`.
2. **Enter on the empty free-text row DECLINES the whole dialog.** The shipped
   "Other" recipe sent `Enter` *before* the text. But `Enter` on the empty "Type
   something." row is recorded as `toolUseResult:"User rejected tool use"` —
   it declines. Fix: **type-first** — `[Down×idx][PasteText][Enter]`; the text
   fills the row inline, then `Enter` submits it. The same type-first path drives
   the plan feedback row.
3. **Enter on a multi-select option row toggles — it never commits.** The
   shipped multi-select recipe confirmed with a bare `Enter` after the `Space`
   walk, but `Enter` on an option row toggles exactly like `Space` (observed: it
   flipped the last selection **off**) and never commits. Commit is a dedicated
   **unnumbered "Submit" row** below the options. Fix: walk onto Submit, `Enter`
   there.
4. **Reply's Enter raced a multi-line paste and was dropped.** A ~600-char
   7-line reply pasted with an immediate `Enter` sat **unsubmitted** in the
   composer indefinitely; the `Enter` raced the composer's paste processing.
   Fix: pace the submit `Enter` by `keyDelay` (250ms) just like the dialog ops —
   short replies never hit the window because it scales with paste size.

Two non-bug live findings shaped the design rather than fixing a bug:

- **A multi-select answer's recorded label order is not option-index order.**
  Live, toggling Apple then Cherry recorded `"Cherry, Apple"`. The backstop
  therefore compares the comma-joined labels **as a set** (split, sort, compare),
  or a correct answer would false-warn.
- **The plan picker's row-0 TUI label varies with session state** under the very
  same spawn flag ("Yes, and use auto mode" → "Yes, auto-accept edits" — the
  bundle builds the row set from the permission mode + auto-mode state). Lab
  therefore surfaces its **own stable semantic labels** — `Approve — auto-accept
  edits`, `Approve — review each edit`, `Reject — refine the plan`, `Reject with
  feedback` — pinned **by index** (rows 0–1 approve, rows 2–3 reject), never a
  mirror of the drifting TUI text; the recipe and the backstop both couple only
  to the index. Separately, an undriven picker **self-resolves after a 60s
  `afkTimeout`** with `answers:{}` — the backstop classifies "timed out although
  lab sent an answer" as a warning, and no recorded intent as silent.

The live JSONL transcripts are captured as compat fixtures
(`testdata/transcript-{askuserquestion,exitplanmode,multiselect-timeout}-live-2.1.198.jsonl`).
These tests exist so the next Claude Code upgrade re-verifies the §7 recipes with
one command instead of a by-hand TUI session.

## What this does not do

- **No Codex or Gemini adapter.** This issue ships the generalized seam and one
  adapter (claude-code) brought up to it. #2 tracks the Codex implementation;
  Gemini transcript durability, Codex `notify` payload richness, and
  pasted-slash execution on their TUIs are their own verification spikes.
- **No protocol-mode / push adapter.** Answering still rides the pinned
  send-keys recipe into the live picker; a bridge/WebSocket-native answer channel
  stays deferred (ADR-0020).
- **No lab-owned message store.** Read-through is reaffirmed (ADR-0016) — the
  transcript is the provider's durable record; lab persists only
  `runs.transcript_path`.
- **No TUI pane scraping.** The never-scrape rule is universal. Synthesized
  option rows and the command builtins are **write-side** pinned constants, like
  the keystroke recipes — nothing reads the pane. (The live tests watch the pane
  with `capture-pane` only to know *when* the picker is up; that is the
  verification harness observing its own probe, not a production read.)
- **No `api-key` auth implementation, no per-provider state metadata, no
  new schema migration.** The `api-key` flow is a descriptor kind only; the
  five-state vocabulary is unchanged; the schema change is the additive
  `Dialog.Questions` / `answers[]`, no migration.

## Status

Accepted. Amends ADR-0016 (the universal schema gains dialog kinds +
multi-question forms; command echoes render as user text + lifecycle stdout; the
clear/epoch obligation becomes a documented seam duty; the no-slash-command-UI
stance is superseded) and ADR-0020 (`DialogHooker` → `LiveSignals`;
multi-question and plan approval are now answerable). Extends the `AgentProvider`
seam (ADR-0008) with four mandatory methods (`DisplayName`, `AuthFlow`,
`SeedMeta`, `Commands`) and follows ADR-0017's optional-capability pattern for
`LiveSignals` and ADR-0021's "provider declares, lab renders generically"
pattern for `AuthFlow`/`SeedMeta`. `internal/compat` gains §10 (the builtin
command catalog) and live-re-verifies §5/§6/§7 against 2.1.198 with the new
`live_recipes_test.go`. No database migration. Unblocks #2 (Codex): a new
provider implements the seam, declares its metadata, brings its own compat pin,
and gets the chat, auth, seeding, and command surfaces with zero refactor of the
generic core.

## Considered options

- **Implement a second provider now to prove the seam.** Rejected for this
  issue: a real Codex adapter carries its own live-verification spikes (headless
  remote-connections, transcript durability) that would dwarf the seam work and
  gate it on OpenAI's behavior. Proving the seam **by construction** — every
  claude assumption lifted into declared metadata or a written obligation, the
  grep-guard and the no-`claudecode`-import rule enforcing it — is the cheaper,
  stronger proof, and #2 becomes a pure add.
- **Scrape the pane for the plan picker and multi-question rows.** Rejected for
  the same reason ADR-0016/0020 rejected it — the pane widget is the most
  volatile surface. Synthesizing the rows as pinned constants (verified live,
  index-coupled, backstop-guarded) keeps the never-scrape rule intact.
- **A dedicated seam clear method.** Rejected: clearing is a normal command
  (`/clear`, `/new`), so it rides the command catalog and the plain-text reply
  path; the only thing lab must *know* is the **effect** (a new transcript
  identity), captured as a seam obligation rather than a method every provider
  must special-case.
- **Mirror the TUI's own plan-approval labels.** Rejected on the live evidence:
  row 0's label drifts with session state under the same spawn flag, so mirroring
  would desync the SPA's buttons run to run. Stable lab-owned semantic labels,
  coupled to the index, do not.
- **Fully generalize auth (implement all four kinds).** Deferred: only claude's
  `oauth-code` has a real backend today. The descriptor is designed for all four
  so the SPA card is data-driven now, but `api-key`/`oauth-redirect`/`external`
  are schema-only until a provider needs them — the ADR-0021 discipline of
  declaring the seam without over-building it.

## Consequences

- The seam is now **the contract a second provider ports against**: implement
  `AgentProvider` (with `DisplayName`/`AuthFlow`/`SeedMeta`/`Commands`),
  optionally advertise `DeepLinker`/`ConnectingReporter`/`LiveSignals`, declare
  metadata, and the chat, auth, seeding, and command surfaces follow with no
  generic-core change. Each future adapter brings its **own** `internal/compat`
  pin and fixture-driven port and **may not weaken claude's** — the existing
  pins are a floor.
- `internal/provider` grows the dialog kinds + `Question`/`QuestionAnswer`, the
  `CommandSpec` catalog, the `AuthFlow`/`SeedMeta` descriptors, the generic auth
  sentinels + `provider.auth.changed`, and `LiveSignals` (renamed). The
  `providertest` fakes implement every new surface.
- `internal/seeder` is one generic seeder driven by `SeedMeta`;
  `internal/httpapi` imports no concrete provider; the SPA's copy is
  descriptor-driven and grep-guarded. `internal/compat` §10 joins the upgrade
  checklist alongside §5–§9, and the live recipe tests make an upgrade's dialog
  re-verification a single command.
- The backstop is advisory-only and in-memory: a mismatch warning is
  deterministic per parse while the intent exists, but disappears after a lab
  restart (later seqs shift down one). Accepted — the alternative is a storage
  surface for a rare warning.
