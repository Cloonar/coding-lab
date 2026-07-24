# Codex CLI compatibility pins

Pinned version: **codex-cli 0.133.0** — live probes on the lab host,
2026-07-10 (issue #87's Tier-2 spike sweep, all eleven live-spike questions,
recorded in the issue's Amendment 2; fixture captures re-run the same day
while writing this record). One pin rides newer: the §1 model-catalog
probe SCHEMA is additionally live-verified against codex-cli **0.144.1**
(2026-07-13, issue #156 / ADR-0043) — the catalog *values* are no longer
pinned at all; they are probed from the binary at boot. Every recipe and
every other pin remains 0.133.0. This is the committed Tier-2 compat record
ADR-0036 requires for a new adapter (`docs/agents/provider-authoring.md`):
the four mandatory spikes — transcript, reply/interrupt recipes,
context-file discovery, incogni attribution ground truth — plus the wider
couplings the sweep pinned. The implementation of every coupling lives in
`internal/provider/codex` (ADR-0037 pins the decisions; this document pins
the couplings); the probe tests in this package exercise the same exported
entry points against captured fixtures in `testdata/`.

Codex has **no deep link** (no per-session web surface — OpenAI pairing
requires the desktop app), **no LiveSignals** in v1 (hooks are stable in
0.133 and recorded in §8 as the follow-up substrate), and **no answerable
dialogs** (never-ask spawn posture, no structured question tool) — honest
degradation per ADR-0026/0017, so this record has no registry, hook-spool,
or dialog-recipe sections; §5–§7 carry the chat surface that does exist.

Provenance legend:

- **live** — observed on the installed codex-cli 0.133.0 during the
  2026-07-10 spikes (real TUI over tmux, real `codex exec` runs, real
  rollouts; the staged raw artifacts behind the fixtures came from those
  probes).
- **fixture** — pinned by a captured fixture asserted by tests; re-verify
  live when a Codex upgrade misbehaves.
- **cli extraction** — read out of the installed binary's own
  machine-readable output (`codex debug models`, `codex features list`) —
  stronger than a fixture (it is the shipped binary's own contract) but not
  observed end to end in a live flow.

## 1. Spawn argv — live (0.133.0)

```
{codex} --ask-for-approval never --sandbox danger-full-access
        -c project_doc_fallback_filenames=["AGENTS.local.md"]
        [--model M] -c model_reasoning_effort=E [prompt]
```

- **Never-ask, full-access posture** (`--ask-for-approval never --sandbox
  danger-full-access`): no approval prompt can ever block an unattended
  run. Full access is not convenience — codex's `workspace-write` sandbox
  has **three live-verified traps** that each break a lab session: (1)
  network off by default (breaks `labctl` and `git push`); (2) writes
  confined to cwd (a linked worktree's commits write to the shared git dir
  *outside* cwd); (3) **`.git` is mounted read-only even inside cwd** —
  observed live: `git commit` failed with code 128 ("couldn't write
  .git/index"), `touch .git/…` exited 1, in a repo wholly inside the
  workspace (the failing turn is preserved verbatim in
  `testdata/rollout-patch-0.133.0.jsonl`). A sandbox that cannot commit is
  a broken session, not a safety layer; isolation is the host's job
  (service user + worktree). live.
- Each `-c` value is a **single argv element, no shell quoting** — tmux
  receives argv verbatim (the fallback-filenames JSON array included).
  live.
- **Effort is ALWAYS emitted**: `codex exec` defaults the reasoning effort
  to **none** (its banner echoes `reasoning effort: none`), while the TUI
  defaults to medium — so an empty `spec.Effort` injects `medium` instead
  of omitting the flag. live.
- **NO `-c commit_attribution=""`**: unknown config key on 0.133 —
  silently ignored normally, **hard error under `--strict-config`** — and
  attribution is absent at source anyway (§9). The original issue-#87 spec
  carried it; the live sweep removed it. live (refuted).
- The rumored `-m model[effort]` bracket syntax is **refuted**: the string
  passes verbatim as the slug and 400s at request time (there is no
  client-side catalog validation). live (refuted).
- A non-empty `InitialPrompt` rides as the single trailing positional
  (official `codex [options] [prompt]` behavior); an empty prompt appends
  nothing. **Never deliver the prompt via stdin**: `codex exec` announces
  "Reading additional input from stdin..." when stdin is a pipe and blocks
  on it. live.
- `spec.SessionName` is unused — codex has no `--remote-control`
  equivalent (no deep link, §preamble).
- Model/effort catalogs are **PROBED at provider construction**, not
  pinned (issue #156 / ADR-0043, superseding the ADR-0037 pinned list):
  `New` runs `codex debug models` exactly once, synchronously, via
  `exec.CommandContext` on the configured CodexBin — hard timeout 10s
  (`defaultProbeTimeout`, one budget shared with the best-effort
  `codex --version` capture), stdout capped at 8 MiB (`probeOutputCap`;
  the real 0.144.1 output is ~174 KB). Process-lifetime cache: no TTL, no
  background refresh — the binary is nix-pinned, a new binary implies a
  restart. cli extraction.
- **The fragile coupling is now the probe SCHEMA, not the catalog
  values.** Pinned fields (all else — `base_instructions` et al. —
  ignored): `slug`, `display_name` (→ Label; defensively the slug when
  empty), `visibility` (only `"list"`
  entries are served — this filter replaces the pinned-era hardcoded
  `codex-auto-review` slug filter; that entry is `"hide"`), `priority`
  (ascending order; FIRST entry = spawn default — on 0.144.1 the bare
  default becomes `gpt-5.6-terra`), `default_reasoning_level` (the model's
  DefaultEffort, sanitized to "" when not a member of its own list), and
  `supported_reasoning_levels[].effort` (the model's OWN efforts, binary
  order). The union effort catalog (`Efforts()`, the repo/global defaults
  pickers) is first-seen order across the sorted listed models. cli
  extraction (schema live-verified on 0.144.1, 2026-07-13).
- **Per-model efforts are load-bearing — codex does NOT clamp**: an
  unsupported model+effort combo passes through the CLI and 400s at the
  API (live-verified on 0.144.1: `gpt-5.5` + `ultra` → 400). Explicit
  spawn effort therefore validates against the RESOLVED model's list
  (`internal/instance` — 400 at lab's boundary instead of a session that
  fails on its first turn); stored defaults skip-layer as usual. live
  (0.144.1).
- **Effort labels are lab-side** — codex reports no display names for
  reasoning levels: pinned map low → Low, medium → Medium, high → High,
  xhigh → "Extra high", with a title-case fallback for unknown values
  (max → "Max", ultra → "Ultra").
- **On ANY probe failure** — binary missing, timeout, nonzero exit,
  bad/oversize JSON, zero listed models, a listed model with an empty
  slug or empty effort list, duplicate slugs — the compiled-in
  **0.133.0 catalog is served as
  the fallback** (`fallbackModels`/`fallbackEfforts` in source: `gpt-5.5`
  + `gpt-5.4-mini`, efforts low/medium/high/xhigh — no `minimal` tier —
  default medium) and ONE loud structured Warn carries the probe error.
  No status surface, no metric; the boot log records the catalog source
  (probe vs fallback) and the binary version (best-effort
  `codex --version`).
- Fixtures: `testdata/models-0.133.0.json` (this package,
  operator-relevant fields only; the multi-KB `base_instructions` blobs
  are deliberately not committed) pins the FALLBACK catalog; the trimmed
  0.144.1 capture at `internal/provider/codex/testdata/models-0.144.1.json`
  (same trimming convention) pins the probe schema. 0.133-era observations
  kept for the record: `gpt-5.6-terra`/`-luna` existed server-side but
  0.133 rejects them ("requires a newer version of Codex"); `gpt-5.6-sol`
  and `gpt-5.1-codex` are rejected outright.

Pinned by `TestCompat_SpawnArgvSnapshot`,
`TestCompat_SpawnArgvSeedPromptSnapshot`,
`TestCompat_SpawnArgvEffortAlwaysExplicit`, and
`TestCompat_ModelCatalog_matchesDebugModelsFixture` (which now pins the
FALLBACK catalog — a zero-value `Provider` that never probed — against
`models-0.133.0.json`); plus the codex package's `TestSpawnArgv`,
`TestFallbackCatalogs_pinnedValuesAndCopies` (the fallback pins) and the
`catalog_test.go` probe/parse tests against `models-0.144.1.json`; live
re-verification `TestCompat_Live_debugModelsProbe`. When this breaks: a
binary bump **no longer requires re-pinning the catalog values** — the
probe carries those. Re-verify the probe SCHEMA pins instead (field
names/shapes, `visibility` semantics, `priority` ordering, that the first
listed entry is still the intended spawn default) via
`TestCompat_Live_debugModelsProbe` and a fresh trimmed capture if the
schema moved; re-run a real spawn for the argv; and re-check the three
sandbox traps before ever softening the full-access posture.

## 2. Auth status + device-code login — live (0.133.0)

**Status shapes, both pinned live** (`codex login status`, combined
output + exit code — fixtures `testdata/login-status-in-0.133.0.txt` /
`login-status-out-0.133.0.txt`):

```
logged in  → exit 0, "Logged in using ChatGPT"
logged out → exit 1, "Not logged in"
```

- `ParseAuthStatus(out, exitOK)` needs BOTH signals: logged-in iff the
  command exited 0 AND the output carries "Logged in". A plain non-zero
  exit is a definitive logged-out answer, not an error. `Method` is the
  lowercased token after "using " (`chatgpt`); no email is printed —
  `AuthStatus.Email` stays empty for codex. live.
- The logged-out fixture carries a leading `WARNING: … could not update
  PATH …` line (emitted when `CODEX_HOME` sits under a temp dir; path
  anonymized in the fixture) — parsing is containment-based, so leading
  noise must stay tolerated.
- **Device-code login** (`codex login --device-auth` — the plain `codex
  login` spins up a localhost:1455 redirect server a server-first
  deployment cannot reach). Verbatim stdout, live 2026-07-10 (raw capture
  carried SGR color codes `\e[90m`/`\e[94m`; the fixture
  `testdata/device-auth-0.133.0.txt` is **ANSI-stripped**, because the
  production scrape source is tmux `capture-pane` rendered text, which
  never contains escape codes; the one-time code below expired 15 minutes
  after capture — safe to quote):

  ```
  Welcome to Codex [v0.133.0]
  OpenAI's command-line coding agent

  Follow these steps to sign in with ChatGPT using device code authorization:

  1. Open this link in your browser and sign in to your account
     https://auth.openai.com/codex/device

  2. Enter this one-time code (expires in 15 minutes)
     Y9HC-QKI85

  Device codes are a common phishing target. Never share this code.
  ```

- Scrape regexes (pinned): URL `https://auth\.openai\.com/\S+` with
  claudecode's trailing-punctuation trim cutset `.,;:!?'")>]`; user code
  `(?:^|[^A-Za-z0-9-])([A-Z0-9]{4}-[A-Z0-9]{5,8})(?:[^A-Za-z0-9-]|$)` —
  boundaries exclude alphanumerics AND `-`, so version strings and longer
  dashed tokens never match. The code is surfaced via
  `provider.LoginCodeReporter`; it is entered **browser-side only** —
  `LoginSubmitCode` is a hard `provider.ErrLoginCodeUnsupported` (409 at
  the API).
- **Completion-poller contract**: login completion is adapter-driven — a
  single background poller forces `codex login status` every `loginPoll`
  (3s) until the CLI records the login or the **15-minute device-code
  window** expires, then stops the disposable login session
  (`lab-login-codex`, ADR-0034) and publishes `provider.auth.changed`.
  The upstream tmux login-screen Ctrl-C bug (openai/codex#23820) is moot:
  no recipe ever emits Ctrl-C (§6) and teardown is `runner.Stop`.
- **Logout escalation path**: `codex logout` first (exit code ignored —
  success is decided from a force-refreshed status, the seam contract);
  if still logged in, delete `<codexHome>/auth.json` directly (the exact
  credentials file the binary reads: `$CODEX_HOME`, else `~/.codex`) and
  re-check. live (the host's real `auth.json` was created by this flow's
  inverse on 2026-07-10).

Pinned by `TestCompat_AuthStatusFixtures_parse` and
`TestCompat_DeviceAuthFixture_extracts` (plus the codex package's
`TestParseAuthStatus`, `TestLoginStart_*`, `TestLoginCompletionPoller_*`,
`TestLogout_*`); live re-verification `TestCompat_Live_loginStatusParses`.
When this breaks: re-run `codex login status` both ways (a scratch
`CODEX_HOME` gives the logged-out shape read-only) and `codex login
--device-auth` in a scratch home, re-pin the strings and both regexes.

## 3. Directory trust — live (0.133.0)

- **The argv route is DEAD**: `-c 'projects."<path>".trust_level="trusted"'`
  does **not** suppress the first-run trust prompt — live-refuted; the
  prompt appears even alongside explicit `--ask-for-approval never
  --sandbox workspace-write`. live (refuted).
- **The config append works**: appending to `~/.codex/config.toml`

  ```toml
  [projects."<absolute worktree dir>"]
  trust_level = "trusted"
  ```

  suppresses the prompt — validated live **2026-07-10**: an appended
  `[projects."<dir>"]` table suppressed the trust prompt on a real TUI
  spawn in a fresh directory. `SeedWorkspace` does this guarded append
  (`SeedTrust`); it is the working mechanism, not a fallback. live.
- **codex auto-writes trust entries itself** for directories it has run
  in (observed after `codex exec` runs), and rewrites `config.toml` on its
  own — so the guard must tolerate codex mutating the file between
  seedings: the check is plain string containment on the exact
  `[projects."<dir>"]` header line (TOML-escaping `\` and `"` in the
  path), and a hit means **hands off** — never rewrite, reorder, or
  "repair" an existing entry, whatever its `trust_level`. Append-only;
  atomic tmp+fsync+rename; the file is created (0o644) when missing. live.

Pinned by `TestCompat_TrustAppend_roundtrip` (plus the codex package's
`TestSeedTrust_*` and the Tier-1 conformance seeding subtests). When this
breaks: spawn a real TUI in a fresh directory with only the appended table
and confirm the composer appears without the trust prompt; re-check
whether the upgrade made the `-c` override work before touching the
append.

## 4. Context-file discovery (`AGENTS.local.md`) — live (0.133.0)

Lab's context file is the lab-owned `AGENTS.local.md` (ADR-0035 hard
rule: never the repo-trackable `AGENTS.md`). Making codex actually read it
is double-pathed, and **all three legs were live-verified end to end**
(spike 9):

1. **The fallback key works**: in a repo *without* its own `AGENTS.md`,
   `-c 'project_doc_fallback_filenames=["AGENTS.local.md"]'` made codex
   read the file (planted marker surfaced). The key exists and works on
   0.133. live.
2. **A tracked `AGENTS.md` silently skips the fallback**: the key is
   fallback-only *per directory level* — in a repo WITH a tracked
   `AGENTS.md`, only the tracked file's marker surfaced; the fallback was
   never consulted. Exactly the trap the spec predicted; this is why leg 3
   exists. live.
3. **The global bridge survives a tracked `AGENTS.md`**: the global
   `$CODEX_HOME/AGENTS.md` is ALWAYS concatenated regardless of repo
   files. With the bridge block installed, codex read `AGENTS.local.md` at
   runtime via a tool call and followed it, tracked `AGENTS.md`
   notwithstanding. live.

The bridge block `SeedAgentsBridge` maintains in the global AGENTS.md is
marker-guarded (marker first — its presence anywhere means installed,
hands off; append-only below an operator's own global instructions;
atomic write):

```
<!-- lab:agents-local-bridge -->
If a file named `AGENTS.local.md` exists at the workspace root, read it and
follow its instructions — it carries this workspace's session context.
```

Skills ride the same context file: 0.133 has native skills
(`$CODEX_HOME/skills/`, SKILL.md format) but discovery is **global-only**
(§8), so `NativeSkillDiscovery: false` and the seeder appends the
ADR-0035 skills index to `AGENTS.local.md`.

Pinned by `TestCompat_AgentsBridge_roundtrip` (plus the codex package's
`TestSeedAgentsBridge_createAppendSkip` and the Tier-1
seeding-exclude-coverage subtest). When this breaks: re-run the
three-legged spike — fallback-only repo, tracked-`AGENTS.md` repo, bridge
against the tracked repo — with planted markers; the bridge wording is
lab's own and freely tunable, but the *marker line* is the guard and must
survive any rewording.

## 5. Transcript location + rollout JSONL grammar — live (0.133.0) + fixture

**Location**: `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`
(a lazily-grown date tree; components zero-padded so string order is date
order). The first line of every rollout is a `session_meta` record whose
`payload.cwd` is the session's working directory — `LocateTranscript`
scans date dirs newest-first, files per day by filename descending, and
reads ONLY the first line of each candidate until the cwd matches. live.

- **Lazy birth — WARNING**: the rollout file does not exist until the FIRST
  TURN starts; codex creates nothing at session init. Live evidence (issue
  #87's spike sweep, 2026-07-10): a session whose
  `session_meta.payload.timestamp` read `01:25:38Z` had a file birth
  (mtime) of `01:27:40Z` — the rollout appeared only when the first
  message was sent, roughly two minutes after the TUI opened; a
  30-minute-idle interactive instance had no rollout file at all and no
  open rollout fd. `codex exec` has no such gap — its turn starts at once,
  so its rollout is written immediately. **Consequence:
  `LocateTranscript` misses forever for an idle TUI — nothing may ever
  gate first-message delivery on transcript existence** (issue #96: the
  operator's first message rides `SpawnSpec.InitialPrompt` on the spawn
  argv instead of waiting for a locatable transcript). live.
- **Local-time filename vs UTC payload — WARNING**: the *filename*
  timestamp is LOCAL time (`rollout-2026-07-10T03-18-27-…` on a
  Europe/Vienna host) while every *payload* timestamp is UTC
  (`2026-07-10T01:18:27.325Z` — same instant). Only filename order is
  used for recency; never compare a filename timestamp against a payload
  timestamp. live.
- **`/new` rotates natively**: it opens a fresh rollout file with a NEW
  session id; the old file is left untouched and the TUI prints a
  `codex resume <old-id>` hint. Clear/epoch identity therefore comes for
  free — the newest-first cwd match surfaces the post-clear file, no
  epoch synthesis (ADR-0026's clear obligation, satisfied like
  claudecode's `/clear` rotation). live.

**Record grammar** (as mapped; fixtures are REAL 0.133.0 rollouts from the
2026-07-10 probes with `session_meta.payload.base_instructions.text`
stubbed to one line — every other field verbatim, ids/paths as captured):
one line = `{"timestamp":"…","type":"…","payload":{…}}`, type ∈
`session_meta | turn_context | response_item | event_msg` (unknown types
skipped, half-written tail lines skipped, never fatal).

- `response_item`/**`message`** — **SKIPPED entirely**: developer/user
  response_items are synthetic context wrappers (`<environment_context>`,
  `<permissions instructions>`, `<turn_aborted>`…), and the operator's
  real prompt / the assistant's prose arrive via the `user_message` /
  `agent_message` event_msg records — consuming both would double-render
  every turn. live.
- `response_item`/**`reasoning`** — content is **encrypted**
  (`encrypted_content: "gAAAA…"`), never renderable; only a non-empty
  `summary` (`{type:"summary_text",text}` items) maps to assistant
  thinking. Every live 0.133 rollout carried empty summaries, so the
  summary shape is defensive (API-documented), not a live pin. live
  (encryption) / fixture (summary shape).
- `response_item`/**`function_call`** → tool chip, `Status: "running"`;
  for `exec_command` the `arguments` JSON string's `cmd` first line makes
  the chip title ("Ran git status --short"). **`call_id` back-patching**:
  the matching `function_call_output` resolves the chip by `call_id`.
  live.
- `response_item`/**`custom_tool_call`** (`apply_patch`) → tool chip
  ("Applied patch <first file>" from the `*** Add/Update/Delete File:`
  lines of the `input` patch body), resolved by
  `custom_tool_call_output` via the same `call_id` map (the payload's own
  `status` field is ignored — one resolution path). live.
- **Output status is always "ok"**: codex does not mark failure distinctly
  on outputs — a failed command's exit code rides inside the output TEXT
  ("Process exited with code 128"), which the model reads, not lab. The
  patch fixture preserves a real failing `git commit` (the read-only
  `.git` trap, §1) whose chip is `ok` with the 128 in the output. live.
- `event_msg`/**`user_message`** → user text (the operator's actual
  prompt, typed or seeded; multi-line preserved). A message **queued
  mid-turn** (§6 queue-with-steer) lands as an ordinary `user_message` in
  the same rollout. live.
- `event_msg`/**`agent_message`** → assistant text (`phase`
  commentary/final_answer not distinguished — both visible). live.
- `event_msg`/**`task_started` / `task_complete` / `turn_aborted`** —
  the state fold: the LAST one seen wins; `started` → `working`,
  `complete` and `aborted` → `needs_input`; an empty rollout is `idle`.
  **`turn_aborted` additionally emits a lifecycle message carrying its
  `reason`** — a single mid-turn Esc lands as
  `{"type":"turn_aborted","reason":"interrupted",…}` (the §6 interrupt's
  clean rollout marker). live.
- `event_msg`/`token_count`, `patch_apply_end`, and `turn_context`
  records are skipped (no chat content; `turn_context` re-states cwd and
  the sandbox/approval policy per turn). live.
- No dialogs are ever derived (`StateQuestion` unreachable) — never-ask
  posture, no structured question tool.
- Read-through only: lab persists `runs.transcript_path`; a vanished file
  is `provider.ErrTranscriptGone`.

Fixtures: `testdata/rollout-plain-0.133.0.jsonl` (exec tools + prose,
`codex exec` originator), `rollout-patch-0.133.0.jsonl` (apply_patch
custom_tool_call + the failing-commit ground truth + encrypted reasoning),
`rollout-aborted-0.133.0.jsonl` (a real TUI session: multi-line pastes,
Ctrl-J newlines, a queued mid-turn message, and the Esc interrupt tail —
`turn_aborted` reason `interrupted`).

Pinned by `TestCompat_RolloutPlainFixture_maps`,
`TestCompat_RolloutPatchFixture_maps`,
`TestCompat_RolloutAbortedFixture_maps` (plus the codex package's
`TestLocateTranscript_*`/`TestParseTranscript_*`); live re-verification
`TestCompat_Live_locateTranscript`. When this breaks: capture a fresh
rollout with a trivial `codex exec` run in a scratch dir, diff the record
grammar, re-derive the fixtures (stub `base_instructions` again), and
re-check `/new` rotation in a real TUI. Re-check the lazy-birth timing too:
spawn an idle TUI and confirm no rollout file appears until the first turn
starts; if a future codex version writes the rollout at session init
instead, this pin goes stale but harmless — first-message delivery must
still never gate on transcript existence.

## 6. Reply + interrupt recipes — live (0.133.0)

**Reply** = bracketed paste of the text, then a **separate paced Enter**
(`keyDelay` 250ms, claudecode's live-verified settling gap):

- Bracketed paste (tmux `paste-buffer -p`) lands multi-line text in the
  composer **without submitting**; Enter then submits it as one message.
  Ctrl-J also inserts a literal newline (verified; unused — paste covers
  it). live.
- **Mid-turn reply is queue-with-steer**: the TUI shows "Messages to be
  submitted after next tool call (press esc to interrupt and send
  immediately)", and the queued message was delivered **in the same
  turn** (both markers arrived; the queued `user_message` is visible in
  `rollout-aborted-0.133.0.jsonl`). Reply-while-working is legal; no
  typed drop error exists or is needed. live.
- **Slash-command burst-merge hazard**: a slash command's text and its
  Enter MUST be separate paced `send-keys` bursts — delivered in one
  burst, the command text merges with subsequent input instead of
  executing. The paste → delay → Enter recipe already satisfies this;
  never collapse the three steps. live.
- Control characters other than tab/newline are rejected before the paste
  (`validateReply`; CRLF normalizes to LF) so a stray escape cannot break
  out of the composer.

**Interrupt** = exactly ONE `Escape`, legal only while a turn is working:

- A single Esc mid-turn interrupts cleanly ("Conversation interrupted —
  tell the model what to do differently"; the working footer itself
  documents `esc to interrupt`), composer intact. It lands in the rollout
  as `turn_aborted` reason `interrupted` (§5). live.
- **Idle-Esc backtrack hazard**: Esc on an idle empty composer arms "esc
  again to edit previous message"; a second Esc opens the backtrack/edit
  overlay. It does not kill the session; **`q` recovers** to the
  composer. Lab's chat layer offers interrupt only while `working`, so
  the recipe never emits Esc when idle. live.
- **Ctrl-C is forbidden EVERYWHERE**: on the idle composer it quits codex
  immediately — no confirmation. The worst case, confirmed live. No
  recipe (reply, interrupt, login teardown) may ever emit it; session
  teardown is `runner.Stop`. live.

Pinned by the codex package's `TestReply_pasteThenEnter`,
`TestReply_slashCommandStaysTwoBursts`, `TestReply_rejectsEmptyAndControl`,
`TestInterrupt_singleEscape` (tmux-hermetic — the sends are exercised
against the fake runner; the codex-side behavior is the live pin above),
and the aborted-fixture lifecycle assertion in
`TestCompat_RolloutAbortedFixture_maps`. When this breaks: re-drive the
hazards in a scratch TUI over tmux — paste-without-submit, mid-turn queue,
single-Esc abort marker, idle double-Esc + `q`, and (in a DISPOSABLE
session only) Ctrl-C — before trusting any recipe on the new version.

## 7. Builtin slash-command catalog — live TUI scrape (0.133.0, 2026-07-10)

The catalog (`codex/commands.go`, served via `Commands`) is a **static
pinned table** — codex 0.133 has no project- or user-level command
discovery (skills load only from the global `$CODEX_HOME/skills`, §8; a
repo's `.codex` dir is not a command source), so the catalog is identical
for every session. The rows were scraped VERBATIM from the live TUI slash
popup (raw capture: `testdata/commands-popup-0.133.0.txt` — the initial
popup page plus 40 one-Down scroll steps; scrapes must tolerate the
leading update-banner noise, §10). The popup shows **40 distinct
commands** and the pinned table carries all **40**.
Curation rule (claudecode §10's): **ChatSafe=true** iff the
command executes inline and returns to the prompt; **false** for anything
that opens a picker/overlay lab cannot see (never-scrape: lab could
neither render nor answer it) or that fights lab's own management of the
session. The provider returns ALL rows with honest flags; the API layer
filters.

Chat-safe rows (served to the composer), in pinned order:

| command | description (verbatim) | notes |
| --- | --- | --- |
| `new` | start a new chat during a conversation | **Role=clear** (the "New conversation" binding) — rotates the rollout file natively (§5) |
| `clear` | clear the terminal and start a new chat | also rotates; only `/new` carries the clear role |
| `compact` | summarize conversation to prevent hitting the context limit | same rollout file (no rotation) |
| `status` | show current session configuration and token usage | inline panel |
| `diff` | show git diff (including untracked files) | inline |
| `mcp` | list configured MCP tools; use /mcp verbose for details | inline list |
| `ps` | list background terminals | inline |
| `stop` | stop all background terminals | inline action — a legitimate operator brake |
| `copy` | copy last response as markdown | output-only (OSC52 clipboard); harmless headless |

Curated out (ChatSafe=false, served but filtered from chat), with the
per-row reason:

| command | description (verbatim) | reason excluded |
| --- | --- | --- |
| `agent` | switch the active agent thread | thread picker |
| `approve` | approve one retry of a recent auto-review denial | approval overlay; never-ask posture keeps these out anyway |
| `exit` | exit Codex | session-ending — kills the session lab manages |
| `experimental` | toggle experimental features | toggle overlay |
| `feedback` | send logs to maintainers | external submit flow |
| `fork` | fork the current chat | picker; forks transcript identity under lab |
| `goal` | set or view the goal for a long-running task | unverified overlay behavior — curated out until live-checked |
| `hooks` | view and manage lifecycle hooks | management overlay |
| `ide` | include current selection, open files, and other context from your IDE | needs an IDE attach lab has no surface for |
| `init` | create an AGENTS.md file with instructions for Codex | writes a repo-tracked AGENTS.md (ADR-0035 violation from inside the TUI) |
| `keymap` | remap TUI shortcuts | overlay; remaps keys the recipes assume |
| `logout` | log out of Codex | machine-level auth is lab's own surface (§2) |
| `memories` | configure memory use and generation | config overlay (the `memories` feature flag reads `experimental false` in `codex features list`, but the popup row renders anyway) |
| `mention` | mention a file | file picker |
| `model` | choose what model and reasoning effort to use | picker |
| `permissions` | choose what Codex is allowed to do | picker; fights the pinned spawn posture (§1) |
| `personality` | choose a communication style for Codex | picker |
| `pets` | choose or hide the terminal pet | picker |
| `plan` | switch to Plan mode | mode switch ending in an approval flow lab cannot see |
| `plugins` | browse plugins | browser overlay |
| `raw` | toggle raw scrollback mode for copy-friendly terminal selection | rendering-mode toggle the recipes/pane scrapes assume off |
| `rename` | rename the current thread | input overlay |
| `resume` | resume a saved chat | picker; swaps transcript identity under lab (§5) |
| `review` | review my current changes and find issues | review picker overlay |
| `side` | start a side conversation in an ephemeral fork | side-conversation UI |
| `skills` | use skills to improve how Codex performs specific tasks | picker |
| `statusline` | configure which items appear in the status line | config overlay |
| `subagents` | switch the active agent thread | thread picker (alias-shaped sibling of `/agent`) |
| `theme` | choose a syntax highlighting theme | picker |
| `title` | configure which items appear in the terminal title | config overlay |
| `vim` | toggle Vim mode for the composer | composer-mode toggle; breaks the paste+Enter reply recipe (§6) |

Pinned by `TestCompat_BuiltinCommands_pinned` (count, pinned order of the
chat-safe set, `/new`'s clear role, table and popup fixture matching
exactly in both directions) plus the codex package's
`TestBuiltinCommands_pinnedGolden`. When this breaks: re-scrape the popup
(type `/`, one Down per step, capture-pane per step — tolerate the update
banner), re-diff names/descriptions, and re-curate row by row.

## 8. Skills & hooks — live (0.133.0), recorded as the LiveSignals follow-up substrate

**Skills — discovery is global-only, `NativeSkillDiscovery: false`
stands.** 0.133 has native skills (`/skills` command, SKILL.md format,
`$CODEX_HOME/skills/` with preinstalled system skills), but planted marker
skills in `<repo>/.codex/skills/` and `<repo>/.agents/skills/` were NOT
discovered — only the `$CODEX_HOME/skills/` marker was. No per-workspace
seedable path exists, so lab keeps `NativeSkillDiscovery: false` and the
seeder appends the ADR-0035 skills index to the context file (§4); the
skills dir `.codex/skills` is still seeded into the worktree for a future
codex that learns to read it. live.

**Hooks — stable in 0.133, deliberately unused in v1** (ADR-0037:
LiveSignals omitted — honest degradation; a half-verified live channel is
what honest degradation forbids). Recorded here as the follow-up route:

- `codex features list`: `hooks stable true`, `plugin_hooks stable true`
  (re-confirmed by cli extraction on 2026-07-10; the same listing shows
  `codex_git_commit removed false` — §9 — and `memories experimental
  false` — §7).
- Event vocabulary present in the binary: `SessionStart`,
  `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`, `SubagentStop`,
  `PreCompact`, `PermissionRequest`, with `hook_event_name` /
  `stop_hook_active` payload keys. live (binary probe).
- Legacy `notify` (`agent-turn-complete`) is also still present.
- The follow-up (a LiveSignals adapter on this substrate) must verify the
  actual hook payload shapes against lab's spool contract (ADR-0020)
  before shipping — vocabulary stability is necessary, not sufficient.

No hermetic pin — this section is recorded evidence, exercised indirectly
by the Tier-1 seeding subtests (skills dir + context file). When this
breaks (or when picking up the LiveSignals follow-up): re-run `codex
features list`, re-plant the three skill markers, and capture real hook
payloads in a scratch config before designing against them.

## 9. Attribution ground truth — live (0.133.0)

- **A real codex-authored commit carries NO attribution of any kind** —
  no `Co-authored-by` trailer, no "generated with" line; the commit
  message is verbatim what the agent wrote (spike 10 committed
  `add hello` via the agent and inspected `git log`; the successful
  commit's rollout is `testdata/rollout-plain-0.133.0.jsonl`). live.
- The `codex_git_commit` feature (the co-author-trailer feature
  attribution would ride on) is **`removed` / off** in 0.133
  (`codex features list`, cli extraction).
- **`commit_attribution` is an unknown config key on 0.133**: silently
  ignored normally, **hard error under `--strict-config`** — which is why
  the spawn argv carries no `-c commit_attribution=""` (§1) and
  `SeedOpts.Incogni` is a no-op for codex: there is nothing to disable at
  the source. live (refuted key).
- **Defensive `ScrubPatterns` stay anyway** (ADR-0033 BRE∩RE2 dialect) —
  the backstop layers (agent-API body sanitizer, pre-push guard) must
  keep stripping if a future codex turns attribution on:

  ```
  co-authored-by:[[:space:]]*codex
  co-authored-by:.*<[^>]*@openai\.com>
  generated with.*codex
  ```

  Their conformance marker samples (the Tier-1 `Fixture.AttributionSamples`,
  from the documented upstream default `Co-authored-by: Codex
  <noreply@openai.com>` plus variants): `Co-authored-by: Codex
  <noreply@openai.com>`, `Co-authored-by: ChatGPT Codex <bot@openai.com>`,
  `Generated with Codex`; clean lines that must never match:
  `Co-authored-by: Alice <alice@example.com>`, `The openai.com docs
  describe the responses API.`. These are DEFENSIVE shapes (documented
  upstream default), not live-observed output — the live observation is
  their absence.

Pinned by `TestCompat_ScrubPatterns_markerSamples`, the codex package's
`TestSeedMeta_scrubPatternsCompileAndMatchCanonicalSamples`, and the
Tier-1 `scrub-markers` conformance subtest (both real engines + the union
path). When this breaks — or on ANY codex upgrade — commit from a real
codex session and inspect `git log` (and a PR body if the git feature
returns): if attribution appears, reconcile the patterns against the REAL
marker (tighten/extend, never just delete) and flip the incogni measure
from no-op to a source-level disable if one now exists.

## 10. Terminal environment — live (0.133.0)

- Lab tmux runs `default-terminal = tmux-256color`; the codex TUI renders
  and runs fine under it across many starts. **No OSC 11 hang observed**
  (upstream issue #22761 did not reproduce on this host/version). live.
- `codex doctor` flags TERMINFO unreadable under the nix profile paths —
  **cosmetic here** (the TUI works); worth re-checking if rendering ever
  degrades. `codex doctor` is also the one-shot diagnostic (auth, state
  DB integrity, terminal) worth running first when anything live
  misbehaves. live.
- tmux `extended-keys` is off on this host — the recipes assume plain
  named keys (§6). live.
- **Update-banner noise**: the TUI shows an upgrade banner at startup
  (observed: `0.133.0 → 0.144.1` available) — any pane scrape (the §7
  popup scrape, a future login scrape) must tolerate leading banner
  lines; the `commands-popup-0.133.0.txt` fixture preserves the banner's
  tip text in situ. live.

No hermetic pin — operational notes. When this breaks: `codex doctor`
first, then re-check TERM/OSC behavior in a scratch tmux before blaming
the recipes.

## 11. Non-interactive refresh trigger (candidate) — NOT live-verified

Issue #222 (the credential-authority seam: lab core becomes the SINGLE
refresher per provider grant, so a per-instance CLI self-refresh can never
fork the shared OAuth token family) needs a codex analogue to claude's
probe-confirmed `claude -p` refresh trigger — a minimal non-interactive
invocation that makes one real API call so the CLI itself decides whether
its token is near expiry and rotates it. `Provider.RefreshCredentials`
(`internal/provider/codex/credauthority.go`) runs:

```
{codex} exec --sandbox read-only --skip-git-repo-check ok
```

with `CODEX_HOME` in the child's environment forced (filtered-then-
appended, never a bare inherited/duplicated value) to the MASTER codex home
directory — `credentialsPath()`'s own directory, the same master resolution
Logout's rm-escalation and InjectCredentials' copy source use — and the
working directory set to that same dir when it exists (otherwise the
command still runs and its own failure surfaces; callers gate the call on
`CredentialsSig("") != ""`).

Flag names confirmed against the installed codex-cli **0.144.4**'s `codex
exec --help` (one version-bump ahead of this record's 0.133.0/0.144.1
pins): `--sandbox read-only` locks the probe so it can never write through
an agent tool call even though the one-word prompt is not expected to
trigger one; `--skip-git-repo-check` is required because the master codex
home is not necessarily inside a git repository; the bare prompt `ok` rides
as the trailing positional exactly like a real spawn's `InitialPrompt`
(§1). No `-c` config override is used — these three purpose-built flags
cover the whole requirement more narrowly than any `-c` combination would.
cli extraction (`--help` text only — the recipe's actual EFFECT on a real
grant is exactly what remains unconfirmed; see below).

**This recipe is a CANDIDATE — it is NOT live-verified.** The
implementation host has no codex login (this is a lab dev/build
environment, not an operator's authenticated machine), and issue #222
explicitly forbids live-probing a real grant to check it: forcing a
near-expiry check against a genuine token risks double-spending its
refresh token — the exact same hazard that made issue #222 drop its
grace-window probe design ("blocking only that adapter's
RefreshCredentials, not the core loop"). Two questions from the issue stay
open until an operator runs the verification procedure below:

1. **Does this invocation actually refresh a near-expiry token** — i.e.,
   does `codex exec` perform the same refresh-if-near-expiry check the
   interactive TUI performs before an ordinary turn, or could `exec` mode
   skip it entirely?
2. **Does codex read its auth state fresh from disk before that check**,
   or could it cache credentials in memory in a way a short-lived,
   separate `exec` process would never observe? A stray in-memory-only
   cache would make this probe a structural no-op regardless of expiry.

**Operator-supervised verification procedure** — run on a host that IS
logged into codex, with NO lab instances running (anything else holding
the master token family must be idle, so nothing spends the refresh token
before this check does):

1. Forge near-expiry in the MASTER `auth.json`'s token-expiry field under
   the master `$CODEX_HOME` (or, if forging is impractical, simply wait
   for the real token's natural near-expiry window).
2. Run the recipe above with `CODEX_HOME` set to that same master
   directory: `codex exec --sandbox read-only --skip-git-repo-check ok`.
3. Run `codex login status` and confirm it still reports logged in (§2
   vocabulary).
4. Confirm the master `auth.json` was actually rewritten — compare its
   mtime/size (or a full byte diff; lab's no-token-parsing stance means
   this check must stay at the file level, never inspect token fields)
   against a copy saved before step 2.

A pass on both (3) and (4) around a genuinely near-expiry token answers
question 1 affirmatively. Seeing (4) rewrite unconditionally on every
invocation — with no expiry manipulation at all — would refute the CLI's
own expiry gating and should be reported back against issue #222 rather
than assumed away.

Pinned by nothing yet. The codex package's `credauthority_test.go`
(`TestRefreshCredentials_argvAndEnv`, `TestRefreshCredentials_failureSurfacesStderr`,
`TestRefreshCredentials_rewriteChangesMasterSig`) exercise the recipe's
argv/env-override/error-wrapping mechanics hermetically, against a STUBBED
codex binary only — no fixture or live test in this package or
`internal/provider/codex` exercises the REAL CLI's refresh decision. When
this breaks, or once it is first verified: run the procedure above, flip
this section's status from candidate to live/refuted, and add a
`TestCompat_Live_*` bullet to "Live re-verification" below mirroring the
other three (today it can only be added as a manual checklist row — see
below — because no repeatable automated probe is safe against a real
grant).

## Live re-verification

Three opt-in probes run against the installed binary and the real
`$CODEX_HOME` when `LAB_COMPAT_LIVE=1` is set (skipped otherwise, so CI
stays hermetic — the parent package's gating style, no build tags):

    LAB_COMPAT_LIVE=1 go test ./internal/compat/codex/ -run Live -v

- `TestCompat_Live_loginStatusParses` — runs `codex login status` and
  asserts the §2 vocabulary and exit-code agreement still hold, then
  round-trips the output through `ParseAuthStatus`. Read-only.
- `TestCompat_Live_locateTranscript` — walks the real
  `$CODEX_HOME/sessions` date tree for a genuine rollout's
  `session_meta.cwd`, then drives the production `codex.New` (real path
  defaults) `LocateTranscript` + `ReadChat` against it and asserts
  the located file belongs to that cwd and parses. Read-only; skips
  cleanly when codex or the sessions tree is absent.
- `TestCompat_Live_debugModelsProbe` — runs the exported
  `codex.ProbeModels` against the installed binary and asserts the §1
  probe schema still yields a well-formed catalog: the output parses, at
  least one `visibility: "list"` model survives the filter, values and
  labels are non-empty, every model carries a non-empty effort list,
  every non-empty DefaultEffort is a member of its model's own list, and
  the union covers every per-model effort (the same invariants the
  conformance suite pins on the served catalog). Read-only.
- **Non-interactive refresh trigger (§11, issue #222) — MANUAL, not
  `LAB_COMPAT_LIVE`-gated.** No automated test exists here: unlike the
  three probes above (read-only observations), this one exercises a
  MUTATING operation on a live grant, and a repeatable automated version
  would risk double-spending the refresh token on every CI-adjacent run.
  Verify instead via the §11 operator-supervised procedure — on a host
  logged into codex with no lab instances running, forge (or await)
  near-expiry in the master `auth.json`, run the recipe, then confirm both
  `codex login status` and the rewritten `auth.json`.

Use the three automated probes after a Codex upgrade before trusting any
§1–§7 pin; the §6 hazard checks, the §3/§4 seeding legs, and the §11
refresh trigger still need a by-hand scratch session (they mutate state
and/or cost model calls).

When any pin above breaks: update the codex port
(`internal/provider/codex`), the fixture, the affected tests, and this
document in the same commit.
