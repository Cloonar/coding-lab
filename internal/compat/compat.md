# Claude Code compatibility pins

Pinned version: **Claude Code 2.1.198** — live probe on the dev host,
2026-07-05, re-confirmed by the M3 acceptance smoke on 2026-07-06. This
document tracks brief §11 (known-fragile couplings 1–4; item 5 —
provider-owned model/effort catalogs — is solved structurally in
`internal/provider`, D14) plus the four embedded-chat couplings 5–8 added
by issue #7 / ADR-0016 (transcript location + JSONL schema, the reply,
dialog, and interrupt send-keys recipes), the hook contract §9 added by
issue #17 / ADR-0020 (the PreToolUse/PostToolUse/Notification hook payloads +
the spool protocol that captures a pending dialog live), and the builtin
slash-command catalog §10 added by issue #51 (which also live-re-verified §7
against 2.1.198 on 2026-07-08, generalizing the dialog recipes to
multi-question forms and plan approval and fixing three live recipe bugs). The
implementation of every coupling lives in `internal/provider/claudecode`;
the probe tests in this package exercise the same code paths against
captured fixtures in `testdata/`.

**M3 live acceptance (2026-07-06, real claude 2.1.198, real registry
`~/.claude/sessions`).** A single real `--remote-control` instance was
spawned end to end through the wired `lab` binary against a throwaway
repo; observed live:

- The full spawn argv (`--remote-control <session> --permission-mode auto
  --model opus[1m] --effort max`) launched a working remote-control
  session that connected the claude.ai bridge (§1). live.
- A **real** `bridgeSessionId` appeared in `~/.claude/sessions/<pid>.json`
  with `cwd` == the instance worktree and was captured as
  `https://claude.ai/code/session_01YTsDs2ZCxXpoKCeRz2MN6w` within ~2s —
  **no generic fallback** (§2). live.
- `claude auth status --json` returned `loggedIn:true` with `email` and
  `authMethod:"claude.ai"`, parsed by `ParseAuthStatus` (§3). live.
- Real claude booted in the freshly **seeded** worktree and reached
  bridge-connected with **no interactive trust prompt** — the folder-trust
  seed (`hasTrustDialogAccepted`) is effective on 2.1.198 (§4). live.

Provenance legend:

- **live** — observed on the installed Claude Code 2.1.198 during the M3
  probe.
- **fixture** — pinned by a captured fixture/spec from an earlier version
  (or from v0 behavior) and asserted by tests; re-verify live when a
  Claude Code upgrade misbehaves.
- **schema extraction** — read out of the installed 2.1.198 CLI bundle's
  embedded settings schema (key names, types, and descriptions verbatim);
  stronger than a fixture (it is the shipped binary's own contract) but
  not observed end to end in a live flow.

## 1. Spawn argv (`--remote-control`) — live (2.1.198)

```
{claude} --remote-control <session> --permission-mode auto [--model M] [--effort E]
```

- `--remote-control [name]` exists on 2.1.198; the session name is an
  *optional argument to the flag* (matches v0 usage). live.
- `--permission-mode <mode>` exists. live.
- `--remote-control-session-name-prefix <prefix>` (default: hostname) is
  new since v0 — unused by lab, noted for the future.
- Flag order model-then-effort is lab's own pinned M3 constant (v0
  appended effort-then-model; claude accepts both orders). Snapshot test:
  `TestCompat_SpawnArgvSnapshot`.
- The builder takes a `provider.SpawnSpec` (issue #19 / ADR-0021), so a new
  provider spawn option never churns the argv signature. `spec.Options` is
  applied provider-side: `ultracode` (the AFK-only multi-agent keyword)
  prepends a **lab-owned directive** to a *non-empty* seed prompt — it is
  claude's own prompt-trigger keyword (renamed from "workflow" in v2.1.186),
  NOT a CLI flag/env/settings field, so this is prompt injection, not argv.
  The directive wording is OURS and freely tunable — **not** a pinned Claude
  coupling; the snapshot (`TestCompat_SpawnArgvUltracodeSnapshot`) guards only
  that the builder still prepends it as one trailing positional.

## 2. Deep-link registry (`~/.claude/sessions/<pid>.json`) — live (2.1.198)

- One file per live claude process; observed 2.1.198 shape captured in
  `testdata/registry-2.1.198.json` (pid/ids/timestamps anonymized, keys
  and value shapes verbatim). live.
- Fields lab reads: `pid` (int), `cwd` (absolute string, kernel-reported),
  `startedAt` (unix millis int64), `bridgeSessionId` (string). All present
  on 2.1.198; all extra fields (`sessionId`, `procStart`, `version`,
  `peerProtocol`, `kind`, `entrypoint`, `name`, `nameSource`, `status`,
  `updatedAt`, `statusUpdatedAt`) are ignored. live; parse pinned by
  `TestCompat_RegistryFixture_parses`.
- `bridgeSessionId` arrives already `session_`-prefixed on 2.1.198 (live);
  the `cse_` → `session_` normalization is kept for tolerance (claude's
  own `toCompatSessionId`; transcripts carry the `cse_` spelling).
  fixture/v0. Pinned by `TestCompat_BridgeURLNormalization`.
- Deep link URL shape: `https://claude.ai/code/<bridgeSessionId>`. live.
- Generic fallback when capture misses: `https://claude.ai/code` (the
  claude.ai session picker) + a loud log (brief §11.2). lab-owned
  contract.
- History pin: claude stopped printing the remote-control deep link into
  the terminal between 2.1.156 and 2.1.170 — the pane-scrape source is
  dead; the registry is the only source. Registry files are removed on
  graceful exit only (SIGKILL leaves stale files ⇒ pid-alive +
  newest-startedAt filtering).

## 3. Auth status + login flow — status live (2.1.198), URL shape fixture (2.1.150)

- `claude auth status --json`: exits 0 when logged in on 2.1.198;
  top-level keys observed live: `apiProvider`, `authMethod`, `email`,
  `loggedIn`, `orgId`, `orgName`, `subscriptionType`. lab reads
  `loggedIn`, `email`, `authMethod`; everything else ignored. Shape
  fixture: `testdata/auth-status-2.1.198.json`; parse pinned by
  `TestCompat_AuthStatusFixture_parses`.
- Stdout is parsed **regardless of exit code** — older claudes exit
  non-zero when logged out while still emitting `{"loggedIn":false}`,
  which is a definitive answer. fixture/v0 (2.1.198 not observed logged
  out during the probe).
- `claude auth login --claudeai` exists on 2.1.198 (`--claudeai` is the
  documented default; `--console`, `--sso`, `--email` also exist). live.
- OAuth authorize URL shape: served from **claude.com under a `/cai/`
  prefix** — `https://claude.com/cai/oauth/authorize?...`, NOT
  `claude.ai/oauth`. Captured live on claude **2.1.150**; not re-captured
  on 2.1.198 (needs a logged-out machine). fixture. Regex pin:
  `https://claude\.(?:com|ai)/\S*oauth/\S+` with trailing-punctuation trim
  cutset `.,;:!?'")>]` — `TestCompat_OAuthURLRegex_realAuthorizeLine`.
- Flow constants (v0-pinned, port-spec §5): captureTimeout 3s, loginTimeout
  20s, loginPoll 1s, authTTL 30s, bridgeTimeout 30s, poll cadence 200ms,
  login code cap 4096.

## 4. Trust / attribution keys — folder trust live (2.1.198), MCP fixture (v0), attribution schema-extracted (2.1.198)

- Folder trust: `~/.claude.json` → `projects.<absolute worktree dir>.
  hasTrustDialogAccepted: true`. **live (2.1.198)** — the M3 acceptance
  smoke spawned a real claude in a freshly seeded worktree and it reached
  bridge-connected with no trust dialog, which is exactly the live
  re-verification this section previously flagged as open. Round-trip also
  pinned by `TestCompat_TrustKeys_roundtrip`.
- MCP approval: `<worktree>/.claude/settings.local.json` →
  `enableAllProjectMcpServers: true`. Claude does **not** read MCP
  approval from the `~/.claude.json` project entry (shipped v0 bug,
  regression-guarded in the same test). fixture/v0 — the smoke worktree
  had no project MCP servers to approve, so this specific key stays a
  fixture pin (not independently re-observed on 2.1.198).
- Both writes preserve unknown keys at every level and are atomic
  (tmpfile + fsync + rename); already-granted files are not rewritten. The
  smoke confirmed the write is additive and non-destructive against a real
  `~/.claude.json` carrying dozens of pre-existing project entries.
- Attribution/Co-Authored-By keys — **schema extraction (2.1.198)**,
  probed 2026-07-06 on this host's installed bundle
  (`claude-code-2.1.198` nix store path; `claude config list` no longer
  exists as a subcommand on 2.1.198, and the docs site was unreachable, so
  the pins come from the settings zod schema embedded in the CLI bundle —
  descriptions quoted verbatim below). Lab's incogni measure 1
  (`claudecode.SeedAttributionOff`) seeds all four keys into the worktree's
  `.claude/settings.local.json` (project-local settings override the user's
  `~/.claude/settings.json`):
  - `attribution.commit` (string): "Attribution text for git commits,
    including any trailers. Empty string hides attribution." Default:
    `Co-Authored-By: <model> <noreply@anthropic.com>`. Seeded `""`.
  - `attribution.pr` (string): "Attribution text for pull request
    descriptions. Empty string hides attribution." Default:
    `🤖 Generated with [Claude Code](…)`. Seeded `""`.
  - `attribution.sessionUrl` (bool): "Whether to append the claude.ai
    session link to commits and PRs created from web or Remote Control
    sessions (default: true). Set to false to omit the Claude-Session
    trailer and PR-body link." **Load-bearing for lab**: every lab session
    is a `--remote-control` session, so without this the `Claude-Session`
    trailer leaks a claude.ai link into every commit even with commit/pr
    blanked. Seeded `false`.
  - `includeCoAuthoredBy` (bool): the schema marks it "Deprecated: Use
    attribution instead", but the resolution logic in the bundle still
    honors it — attribution.commit/pr win when either is set; else
    `includeCoAuthoredBy === false` blanks both; else defaults apply.
    Seeded `false` as defense in depth (and for older claudes that predate
    the `attribution` object).
  Pinned by `TestCompat_AttributionKeys_seed`; the merge/idempotence
  contract by the `claudecode` attribution tests. Provenance:
  binary-schema extraction, not observed in a live commit flow — when a
  Claude Code upgrade misbehaves, verify by committing from an incogni
  worktree and inspecting `git log`.

  **Defense-in-depth matchers (measures 3+7) key on the EMAIL, not the
  model name.** The trailer default is `Co-Authored-By: <model>
  <noreply@anthropic.com>` — the model display name is variable (a rename
  from "Claude" to "Fable" already happened in this family), so both the
  agent-API body sanitizer (`internal/agentapi` `attributionLine`) and the
  pre-push guard (`internal/seeder` `hookMessagePatterns`) match a
  `<…@anthropic.com>` email in addition to a "Claude" display prefix. A
  measure-1 regression (Claude Code stops honoring the attribution keys)
  therefore still cannot leak an Anthropic-authored trailer through the
  other two layers.

## 4a. First-run onboarding key — live (2.1.198)

- First-run onboarding: `~/.claude.json` → top-level
  `hasCompletedOnboarding: true`. **Machine-global**, NOT keyed by dir —
  seeded into the global config alongside folder trust, in the one atomic
  pass `claudecode.seedGlobalConfig` already performed each spawn.
- **Why lab must seed it.** `claude auth login --claudeai` (§3) performs
  only the OAuth exchange; it does **not** complete onboarding. So on a
  genuinely fresh install — the one path the auth section notes was never
  exercised during the port ("2.1.198 not observed logged out") —
  `hasCompletedOnboarding` stays unset and the first interactive
  `--remote-control` spawn runs the onboarding wizard. lab drives that pane
  over the bridge with no human at the TUI, so it blocks forever: the
  composer never initializes, no session-registry entry / deep link is
  captured, and "open a project" hangs. Auth appears fine because the login
  subcommand is a separate non-interactive flow.
- **live (2.1.198), reproduced 2026-07-07.** A throwaway `HOME` launched
  through the same tmux/PTY path lab uses showed, as its *first* screen:
  `Welcome to Claude Code v2.1.198 / Let's get started. / Choose the text
  style that looks best with your terminal`. Seeding **only**
  `{"hasCompletedOnboarding": true}` into that HOME's `~/.claude.json`
  skipped the entire wizard — the next screen was the workspace-trust dialog
  (§4), which lab already seeds — confirming this single top-level flag is
  the whole-wizard gate.
- **`theme` is deliberately NOT seeded.** It reads `null` even on a fully
  onboarded host (four-digit `numStartups`), so it is not an independent
  gate; `hasCompletedOnboarding` alone suppresses the theme picker. Seeding
  a theme would pin a value claude does not require.
- The write preserves unknown keys and is atomic (tmpfile + fsync + rename);
  an already-onboarded config is not rewritten. Pinned by
  `TestCompat_OnboardingKey_seed`. Provenance: live first-screen
  reproduction — when a Claude Code upgrade regresses this, re-verify by
  pointing `HOME` at an empty dir and confirming `claude --remote-control`
  reaches the composer rather than a wizard.

## 5. Transcript location + JSONL schema — location live (2.1.198), schema fixture

The embedded chat (issue #7 / ADR-0016) reads claude's live session
transcript. Seven coupled facts, all in `internal/provider/claudecode`
(`chat.go`, `chat_types.go`), pinned by `TestCompat_SlugForDir`,
`TestCompat_TranscriptFixture_maps`, and `TestCompat_RegistryFixture_hasSessionID`:

- **Transcript path**: `~/.claude/projects/<cwd-slug>/<sessionId>.jsonl`.
  The `sessionId` is read from the **same** registry file the deep link
  comes from (`~/.claude/sessions/<pid>.json`, §2) — formerly one of the
  ignored keys, now a fifth field on `RegistryEntry`. Located by the same
  newest-live-cwd-match as the deep link (`sessionIDForDir`), so the chat
  reuses the proven capture pattern. live (2.1.198): the directories claude
  created under `~/.claude/projects` during the M3 probe matched the slug
  rule exactly.
- **cwd slug**: every byte of the absolute worktree path that is not an
  ASCII letter or digit becomes `-`. A `/.` boundary therefore doubles
  (`/home/x/.local` → `-home-x--local`). `SlugForDir`; pinned against the
  real observed directory names. live.
- **JSONL event grammar**: one event per line; the fields lab maps are a
  small subset (`type`, `subtype`, `content`, `timestamp`, `isMeta`,
  `isApiErrorMessage`, and `message.{role,content}` where content is a
  string or a `[]block` of `text|thinking|tool_use|tool_result`). A failed
  tool is flagged by `is_error` **on the `tool_result` block itself** (its
  `content` is most often a plain string) — verified against live 2.x
  transcripts; an `is_error` on an inner content item is tolerated as a
  secondary signal. Every other key is ignored. Captured shape:
  `testdata/transcript-2.1.198.jsonl` (ids/paths anonymized, field names +
  value shapes verbatim). Mapped to the universal schema
  (`text|tool|dialog|lifecycle`) by `ParseTranscript`. fixture (assembled
  from real 2.1.198 line shapes; re-verify live when an upgrade
  misbehaves).
- **Non-conversational user text (isMeta + local-command echo)**: two kinds
  of `user`-role text are UI breadcrumbs, not turns. (1) `isMeta:true`
  injected context — dropped entirely. (2) A **local slash-command echo** and
  its output, which carry **no** isMeta flag: a `user` message whose (trimmed)
  string content begins with `<command-name>`, `<command-message>`, or
  `<command-args>` — the breadcrumb Claude Code writes when the operator runs
  a local command (`/clear`, `/rewind`, `/help`, …) — and the command's
  captured output, `<local-command-stdout>`. Ground truth (live, 2.1.198,
  2026-07-08): content is a plain string like
  `"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"`;
  tag order varies and there is leading whitespace, so classification is a
  trimmed **prefix** match and extraction is by tag (`commandEcho`), never by
  position.

  **Rendering (issue #51 decision 2, reversing half of issue #45).** The echo
  maps to a **visible user text message** carrying the command line — the
  `<command-name>` body plus the `<command-args>` body when non-empty (e.g.
  `/clear`, `/foo bar baz`); a sent slash command is real conversational
  context and renders as plain user text, no schema change. A non-empty
  (trimmed) `<local-command-stdout>` maps to a follow-up **lifecycle** message
  with the (truncated) output; an empty one is dropped. An echo with no
  extractable command name renders nothing (degrades to issue #45's drop).
  History: issue #45 dropped both lines entirely; only the rendering half is
  reversed here.

  **State neutrality is load-bearing and UNCHANGED (issue #45, kept by #51):**
  echoes never touch the state fold (`lastKey`). `/clear`/`/rewind` rotate
  to a fresh transcript whose only tail is this echo (with no following
  assistant turn, ever); counted as an ordinary `user:text` it would derive
  `working` forever. Under the then-current ADR-0022 morph that lockout was
  the issue #45 stuck-composer root cause — a false `working` locked the
  composer into the pulsing Interrupt with no Send. Since ADR-0029 (issue
  #61) the composer no longer reads `working` at all, so a false `working`
  now costs only the state badge; state neutrality still matters for that
  badge (and any future needs-you cue) to stay honest, even though it no
  longer gates Send, Clear, or Interrupt. Rendered-but-excluded, the fresh
  transcript still has no trailing *turn* → the idle default; a tail
  echo after real turns keeps the pre-echo state. A genuine plain-text
  reply is unchanged — it still derives `working`, now surfaced only via
  the state badge rather than the retired Send→Interrupt morph. Some
  commands (`/triage`) *do* invoke the model; the brief window before
  their first token reads prior-state/idle rather than `working` and
  self-corrects within one poll — no per-command special-casing. fixture
  (the echo + output lines in `transcript-2.1.198.jsonl` map to the `/help`
  text + stdout lifecycle pair, `TestCompat_TranscriptFixture_maps`;
  the state edges in `TestCompat_TranscriptEcho_stateNeutral` and
  `claudecode.TestParseTranscript_commandEchoNeverDrivesState`).
- **Read-through only**: the transcript file is the source of truth; lab
  persists only `runs.transcript_path` (captured async by cwd-match, the
  `deep_link_url` pattern) so ended runs stay readable while claude retains
  the file. A vanished file is `provider.ErrTranscriptGone` → the UI's
  "transcript no longer available" state.
- **Flush-on-resolve (pending dialogs are invisible live)**: a pending
  `AskUserQuestion` / `ExitPlanMode` `tool_use` is **not** written to the JSONL
  while it is pending; the `tool_use` **and** its `tool_result` are flushed
  together, retroactively (original timestamps), only when the dialog resolves.
  live (2.1.198, 2026-07-07): a question sat pending in the TUI for minutes
  while the transcript stayed byte-frozen at the user-prompt line. **But the
  file is NOT guaranteed byte-frozen during the pending window** — live
  (2.1.198, 2026-07-10, grill transcript d4be520a): a message queued while the
  picker was up (composer/claude.ai) appended `queue-operation` + `attachment`
  entries mid-window. Consequences: the transcript is not a live source for a
  pending dialog — see §9, which captures it from a PreToolUse hook instead —
  and transcript mtime must never be used to judge a dialog spool stale (that
  heuristic hid a genuinely pending dialog after a queued message; the spool's
  own `session_id` is the staleness key, §9). The transcript-scan dialog path
  (an unanswered `tool_use` in §5) stays as a dormant fallback for a future
  Claude Code that flushes pending `tool_use`.
- **Dialog resolution shapes (`toolUseResult` / `toolDenialKind`) + the 60s
  afkTimeout — live (2.1.198, 2026-07-08, the issue #51 verification runs;
  full transcripts in `testdata/transcript-{askuserquestion,exitplanmode,multiselect-timeout}-live-2.1.198.jsonl`,
  captured on this host in a scratch dir, ids/paths verbatim).** The user
  event that carries a dialog tool's `tool_result` block also carries
  top-level resolution ground truth lab's verification backstop reads
  (`tItem.ToolUseResult`/`ToolDenialKind`, `dialogintent.go`) — and, since
  issue #56, the same ground truth derives the answered dialog's `Outcome`
  for history display (`dialogoutcome.go`: a resolved dialog stays a dialog
  message carrying its recorded answer, never a demoted tool chip):
  - **Answered `AskUserQuestion`**: `toolUseResult` is an OBJECT
    `{"questions":[…input echo…],"answers":{"<question text>":"<label>" |
    "<l1, l2>"},"annotations":{}}` — multi-select labels comma+space-joined in
    row order, an Other/free-text answer recorded **verbatim** (live:
    `"Favorite pet?":"Ferret"`). The `tool_result` content string is the
    human form (`Your questions have been answered: "<q>"="<label>", …`).
  - **Declined**: `toolUseResult` is the plain STRING `"User rejected tool
    use"` with `toolDenialKind:"user-rejected"` and `is_error:true` on the
    `tool_result` block.
  - **60s unattended timeout (afkTimeout)**: a picker left undriven resolves
    ITSELF after 60s — content `"No response after 60s — the user may be away
    from keyboard. Proceed using your best judgment…"`, `toolUseResult`
    `{"questions":[…],"answers":{},"annotations":{},"afkTimeoutMs":60000}`,
    NOT an error (claude proceeds). Consequences: a pending dialog can vanish
    on its own (the answer-time tool_id re-read already turns a late answer
    into a 409); the backstop treats `afkTimeoutMs` + a recorded intent as
    "lab's answer did not land" (warn) — with no intent there is nothing to
    verify (silent).
  - **`ExitPlanMode` approved**: `toolUseResult` is an OBJECT
    `{"plan":"…","isAgent":…,"filePath":"…","planWasEdited":…}`; content
    starts `"User has approved your plan."`.
  - **`ExitPlanMode` rejected with feedback**: `toolUseResult` is the STRING
    `"Error: The user doesn't want to proceed with this tool use. … To tell
    you how to proceed, the user said:\n<feedback>"` with
    `toolDenialKind:"user-rejected"` — the typed feedback rides inside the
    denial string.

  **Post-resolve verification backstop (issue #51 decision 3).** AnswerDialog
  records the intended answer in-memory (per tool_use_id, bounded at 100,
  oldest-evicted); `Provider.ReadTranscript` — NOT the pure `ParseTranscript`,
  which compat fixtures drive — compares the recorded resolution against the
  intent and emits a lifecycle message with `Error=true` immediately after the
  tool result on a mismatch (wrong label, a denial lab did not intend, an
  afkTimeout after lab answered). Matches verify silently and clear the
  intent; a mismatch warning re-emits deterministically per parse while the
  intent exists. **Restart caveat (accepted, advisory-only):** intents are
  in-memory, so after a lab restart the warning disappears from subsequent
  parses (later seqs shift down one). Pinned by the `TestBackstop_*` tests in
  `claudecode` and `TestCompat_LiveTranscripts_resolutionShapes`.
- **Rotation on `/clear` and `/rewind` (the sessionId → transcript file
  rotates; lab follows by effect)**: `/clear` calls `setConversationId(newUUID)`
  → a brand-new `<newSessionId>.jsonl`; the old file is left intact and
  `claude --resume`-able (refs: anthropics/claude-code#37451, #3046). `/rewind`,
  and a clear run out-of-band inside claude.ai, rotate the same way. Because lab
  pins each run to a single `runs.transcript_path`, it must **re-point** that
  value: for an ACTIVE run `internal/chat`'s tailer and every `Read` re-ask
  `LocateTranscript` each tick and adopt a **different, non-empty** result
  (`locateActive`), keyed off the observable *effect* — the located file
  changing — so lab core never parses any agent's clear command. `LocateTranscript`
  re-resolves to the current sessionId because `~/.claude/sessions/<pid>.json`'s
  `sessionId` updates promptly on `/clear` (live-verified 2.1.198, 2026-07-08).
  An ENDED run never re-locates (successor-safety, above). Two consequences lab
  handles: (1) the messages response carries `transcript_id` (a stable hash of
  the path) the SPA keys a stream reset on — the fresh transcript restarts `seq`
  at 1; (2) a pre-rotation **dialog spool** does not self-heal (its `tool_use_id`
  is absent from the new file), so `PendingDialog` also treats a spool older than
  the current transcript as stale (§9). `/compact` keeps the **same** sessionId
  and appends to the same file, so it is **unaffected** (issue #34).

## 6. Reply send-keys — live (2.1.198, 2026-07-08)

A mid-session reply is a **bracketed paste** of the text followed by a
separate `Enter` (`Provider.Reply`, `internal/tmuxx` `PasteText` +
`SendNamedKeys`). Bracketed paste (`tmux load-buffer` + `paste-buffer -p`)
means embedded newlines insert lines in the composer instead of submitting
turn-by-turn — the argv-only stance (§1) covers the *initial* prompt only;
mid-session replies are exempt (issue #7 decision 4). A mid-turn agent
queues the reply in its own TUI. Control characters other than tab/newline
are rejected before the paste (`validateReply`) so a stray escape cannot
break out of the composer. The send mechanism is exercised hermetically
(`internal/tmuxx/integration_test.go`, real tmux private socket); the
claude-side "queue while mid-turn" behavior is re-verified live on upgrades.

**Paste→Enter pacing (LIVE 2026-07-08, 2.1.198):** the submitting `Enter` is
paced by the same `keyDelay` gap as the dialog recipes (§7). An `Enter` sent
in the same instant as a **multi-line** paste races the composer's paste
processing and is dropped — observed end to end: a ~600-char 7-line paste
with an immediate `Enter` sat unsubmitted in the composer indefinitely, while
the same paste with a settling gap submits reliably (found by
`TestCompat_Live_askUserQuestionRecipe`; short replies never hit the window
because it scales with paste size).

**UI surfacing (ADR-0029, superseding ADR-0022):** this Reply path is
surfaced **at all times** — the composer's Send is never gated on `working`
(ADR-0022's Send↔Interrupt morph is reversed). While the agent is mid-turn
the SPA still POSTs the reply immediately via the same `replyRun` path,
and the paste rides Claude Code's own TUI queue exactly as before — the
send-keys recipe above is unchanged. The UI shows **no queue affordance**:
no optimistic echo, no "queued" hint — the reply becomes visible only
when the transcript reflects it.

## 7. Dialog keystroke recipes — live (2.1.198, 2026-07-08; multi-select re-driven 2026-07-09)

An interactive dialog is an **unanswered** `tool_use` in the transcript (or,
live, the §9 spool — one mapper, two sources) for a recognised tool. Option
buttons are built from the structured tool input plus **write-side pinned
constants** for rows the TUI synthesizes (issue #51 decision 3): the
free-text row and the whole plan picker are pinned like the keystroke
recipes themselves — the never-scrape rule is untouched, nothing reads the
pane. Every row model and key semantics below was **driven live on 2.1.198
on 2026-07-08** (the ADR-0020 process, real tmux, lab's spawn shape for the
plan run); that session found **three live bugs in the previously shipped
recipes** (the wrapping normalize-climb, the type-first Other row, and the
multi-select Submit row — the reply paste-race in §6 was a fourth defect that
same session found), all fixed and pinned below. A **2026-07-09 live session
re-drove the multi-select picker** end to end (five committed rounds) after
the field report "multi-select submit does nothing": it found the batched-walk
key-burst drop (universal rule below) and captured the free-text row's
paste-fills-and-checks behavior, replacing the previous "Other untoggleable in
multi-select" policy. Recipe snapshots: `TestCompat_DialogKeystrokes*`; the
`Up/Down/Space/Enter` key names are standard tmux `send-keys` arguments.

**Universal recipe rules.**

- **No normalize-to-top climb — downward-only walk (LIVE BUG FIXED,
  2026-07-08)**: the recipes navigate purely DOWN from the picker's top row.
  The first cut (and the pre-#51 single-select recipe) opened every picker
  with an `Up × N` climb to "normalise the cursor to the top", assuming Up
  **clamps**. Driving the real pickers disproved both halves: **(1) Up
  WRAPS** — one Up from the top option jumps to the LAST option, and on the
  review screen from "Submit answers" to "Cancel", so a climb lands on the
  wrong row (and on the review screen would silently **cancel the whole
  form**); **(2)** every freshly-presented picker already starts on row 0,
  and lab always answers a fresh picker in one `AnswerDialog` shot, so there
  is nothing to normalise. No climb, no per-shape trailing-synth-row
  constants. If a future version stops opening at the top, the §5 backstop
  catches the mismatch.
- **Inter-keystroke pacing (live, 2026-07-07/-08)**: `Provider.AnswerDialog`
  sleeps `keyDelay` (`defaultDialogKeyDelay` = 250ms) between ops, and
  `Provider.Reply` sleeps the same gap between the paste and the submitting
  `Enter` (§6). Unpaced, the committing `Enter` intermittently raced ahead of
  the `Down` navigation over the remote-control bridge and selected the wrong
  row; 0ms was flaky, 150ms+ reliable. Re-verify on upgrades.
- **One named key per op — never batch a walk (LIVE BUG FIXED,
  2026-07-09)**: the picker DROPS a burst of key names delivered in one
  `send-keys` call. Driven live: `send-keys Down Down Down Down` (one call)
  moved the cursor **zero rows**, after which the recipe's Enters merely
  toggled the top row and the dialog hung pending forever — the field-reported
  "multi-select submit does nothing" root cause; the same walk as four
  separate keyDelay-paced calls navigated correctly on every round. (A
  two-key burst landed on one occasion, so the drop threshold is
  burst-size/timing dependent — the pinned rule is therefore *never* batch.)
  Every recipe emits one named key per op so the pacing above applies
  between every key; `SendNamedKeys` keeps its variadic signature, recipes
  just never hand it a multi-key walk.
- **Answer validation is strict at the door** (issue #51): a misencoded
  answer returns `provider.ErrInvalidReply` (400) or
  `ErrDialogNotAnswerable` (409) before any key plays — blind keystrokes
  built from a misread answer are exactly the desync the §5 backstop exists
  to catch, and failing fast beats warning after. (This also tightened the
  pre-#51 flat path: stray `other_text` on a non-free-text row is now
  rejected instead of silently dropped.)

**Single-select `AskUserQuestion` picker** (whether flat or one question of a
multi-question form). Row model (live):

```
☐ Pet                      ← header chip tab bar
Favorite pet?
❯ 1. Dog                   (description on the next line)
  2. Cat
  3. Type something.       ← free-text row, ALWAYS present (modeled as "Other", IsOther)
──────────────
  4. Chat about this       ← trailing synth row, NOT modeled
```

- lab models the listed options **plus** the free-text row in `Options`; its
  stable label is **"Other"** while the 2.1.198 TUI renders **"Type
  something."** — a deliberate, documented divergence: the label is lab UI
  vocabulary, and the recipe navigates by **index**, so only the row's
  existence/position couple to claude.
- Recipe: `[Down×idx][Enter]` (no climb — see the universal rules). `Enter`
  on an option row selects it and resolves a single-question single-select
  form immediately (**no review step**).
- **Other/free-text rows — LIVE BUG FIXED (2026-07-08)**: the correct recipe
  is `[Down×idx][PasteText][Enter]` — **type first**. The text fills
  the row inline ("3. Ferret"), then `Enter` submits; the recorded answer is
  the text verbatim. The previously shipped `[…][Enter][PasteText][Enter]`
  was wrong on 2.1.198: **`Enter` on the empty "Type something." row DECLINES
  the whole dialog** (recorded `toolUseResult:"User rejected tool use"`,
  `toolDenialKind:"user-rejected"`). The same type-first path drives the plan
  feedback row.

**Multi-select picker** (`multiSelect:true`, flat or in a form) — the tab bar
gains a "✔ Submit" tab. Row model (live):

```
Which toppings?
❯ 1. [ ] Olives
  2. [ ] Onions
  3. [ ] Type something    ← free-text row: pasting text FILLS + CHECKS it
     Submit                ← UNNUMBERED, navigable row BELOW "Type something"
──────────────
  4. Chat about this
```

- Navigation row order: options… (incl. the free-text row), Submit, Chat
  about this. The Submit row's 0-based navigation index = len(modeled rows),
  so the commit walks `Down × (len(modeled rows) − cursor)` onto it — one
  Down per op (universal rule above).
- Recipe: `[Down/Space walk toggling each selected row][optional: Down-walk
  onto the free-text row, PasteText][Down onto Submit][Enter]`, then the
  review step (below). Selected indices are validated (in-range, no
  duplicates, ascending walk, never the free-text row's index — its text IS
  its toggle, riding `other_text`).
- **LIVE BUG FIXED (2026-07-08)**: the previously shipped recipe confirmed
  with a bare `Enter` after the Space walk — but **`Enter` on an option row
  TOGGLES exactly like Space** (observed: it flipped the last selection OFF,
  Cherry `[✔]`→`[ ]`) and never commits. Commit = navigate onto the Submit
  row, `Enter` there.
- **Free-text row IN a multi-select — captured (LIVE, 2026-07-09)**:
  bracketed-pasting text onto the "Type something" row **fills the row and
  checks it in one move** (`4. [✔] no anchovies`) — supported through
  `other_text` since then (previously rejected as an uncaptured path).
  Hazards, both live-observed: **(1) no Space toggle first** — on this row
  Space TYPES a literal space into the field (a Space-then-type round
  recorded `" extra cheese"`, leading space and all — the checkbox state
  follows the field being non-empty); **(2) single-line only** — a newline
  inside/after the pasted text lands in the recorded answer as a literal
  `\r` (probe recorded `"no anchovies\r"` from a trailing newline), so
  `other_text` is validated single-line at the door.
- **Recorded answer shape (live)**: the chosen labels joined `", "` in
  TOGGLE order — not option order (2026-07-08: Apple then Cherry recorded
  `"Cherry, Apple"`) — with a filled free-text row riding as one more
  segment (2026-07-09: toggle Onions + paste "no anchovies" recorded
  `"Onions, no anchovies"`; intent verification compares segment multisets,
  see `sameAnswer`).

**Multi-question forms (N ≥ 2)** — tab bar `←  ☐ Q1hdr  ☐ Q2hdr  ✔ Submit  →`
(headers render as chips, ☐ → ☒ as answered). Mapped to `Dialog{Kind:
question, Prompt: "<N> questions", Questions: […]}` with per-question
options (+ the Other row each) — answerable since issue #51 (previously a
render-degraded deep-link hint). Sequencing (live): questions present one
picker at a time; **committing a question auto-advances** to the next
picker, so the recipe is simply the per-question recipes concatenated in
question order — no transition keys (keyDelay pacing still applies) — then
the review step. Answers are **positional** (`answers[i]` answers
`Questions[i]`; `POST /runs/{id}/answer` carries them in `answers`).

**Review step** — present iff the form carries the "✔ Submit" tab: **N ≥ 2
questions, or a single multiSelect question**. (Live-driven for N=2 and for
the single multiSelect case; a multi-question form of ONLY single-select
questions was not separately driven — its review step is derived from the
tab model, flagged honestly here, and the §5 backstop catches drift.) Screen
(live):

```
Review your answers
 ● Which color do you prefer?  → Red
Ready to submit your answers?
❯ 1. Submit answers
  2. Cancel
```

Two rows, cursor **defaults to "Submit answers"** (row 0); recipe: `[Enter]`
(a bare commit — no climb). **LIVE BUG FIXED (2026-07-08)**: the first cut
sent `[Up][Enter]` to "normalise", but **Up WRAPS** from "Submit answers" to
"Cancel", so `[Up][Enter]` silently **cancelled the whole form** (discarding
every answer; the model then re-asked). Enter alone commits.

**`ExitPlanMode` (plan approval)** — answerable since issue #51 (previously
plan text + deep-link hint; ADR-0016's free-text-reply description and issue
#17's lock-only stance are both superseded). `Kind=plan`, Prompt = the
(truncated) plan markdown, Options = **four rows pinned 1:1 by INDEX** with
the live picker:

```
Claude has written up a plan and is ready to execute. Would you like to proceed?
❯ 1. Yes, and use auto mode          / Yes, auto-accept edits   (row 0 TUI text VARIES)
  2. Yes, manually approve edits
  3. No, refine with Ultraplan on Claude Code on the web
  4. Tell Claude what to change        ← free-text feedback row (IsOther)
```

- **Labels are lab's, not the TUI's**: the `DialogOption` labels lab surfaces
  are its OWN operator-facing wording — `Approve — auto-accept edits`,
  `Approve — review each edit`, `Reject — refine the plan`, `Reject with
  feedback` — NOT a mirror of the TUI text. Two live runs on 2026-07-08 under
  the **same** spawn flag showed **row 0's TUI label vary with session state**
  ("Yes, and use auto mode" → "Yes, auto-accept edits"; the bundle builds the
  set from the permission mode + auto-mode state — option values
  `yes-accept-edits[-keep-context]`, `yes-auto-clear-context`,
  `yes-resume-auto-mode`, `no`, …). Mirroring that drifting text would desync
  the SPA's rendered buttons run to run; the recipe couples only to the
  **index**, and the stable semantic is what matters: **rows 0–1 approve,
  rows 2–3 reject**, row 3 the free-text feedback row. The §5 backstop
  (approve-vs-denial, never label) catches a genuine index-order change.
- **No review screen** — Enter on the chosen row resolves the plan directly.
- Recipe: `[Down×idx][Enter]` (no climb); row 3 takes the type-first free-text
  path (`[Down×3][PasteText][Enter]`) and rejects the plan with the typed
  feedback. Rows 0–1 approve, rows 2–3 reject; the row shape was
  identical on a revised-plan picker (stable across re-presentations).
- Recorded outcomes (§5): approved → content "User has approved your plan…",
  `toolUseResult` object; rejected via row 4 → the denial string with the
  feedback after "the user said:\n".

**Timeout**: any of these pickers left unanswered resolves ITSELF after 60s
(§5 afkTimeout) — an answer sent after that 409s on the tool_id re-read, and
the §5 backstop warns if lab's keys were sent but the timeout landed.

**Unknown** interactive tools (and question shapes lab cannot render from
the input, e.g. a question with no listed options) degrade to
`Answerable=false` + the deep-link hint; the composer stays locked while
they pend (issue #17 dec 5). Answers the operator gives in claude.ai flow
back through the transcript — no divergence handling is needed.

## 8. Interrupt keystroke — fixture (2.1.198)

The chat's stop-generating affordance sends a single `Escape`
(`Provider.Interrupt` → `SendNamedKeys(session, "Escape")`), fired by a
**one-tap** Interrupt control (ADR-0022 dropped the earlier confirm tap
— interrupt is non-destructive, so a confirmation is friction — and
ADR-0029 (issue #61) relocated the control itself). It now lives in the
chat header: a `pause` icon-button immediately left of Stop on desktop,
a `•••` menu item above Stop on mobile — gated on the run being
**live**, not `working` (§5's state-neutrality bullet), plus the two
locked-state composer escape hatches (dialog-pending, degraded question),
which keep the same one-tap `pause` control. The danger-red header Stop
keeps its two-step verbal confirm; the two controls no longer share a glyph
(ADR-0029 decision 6). It is distinct from a run Stop: it never touches
the session lifecycle, the budget clock, the claim, or the three-strikes
counter (issue #7 decision 12) — intervention neutrality is structural
(nothing in the chat path writes a run outcome). fixture — the send
is tmux-hermetic; the claude-side Escape-interrupts-the-turn behavior is
re-verified live on upgrades.

## 9. Dialog-capture hook contract — live end-to-end (2.1.198)

Because a pending dialog is invisible in the transcript live (§5,
flush-on-resolve), lab captures it from Claude Code **hooks** injected per run.
This is issue #17 / ADR-0020's fifth pinned coupling. Implementation:
`internal/provider/claudecode/dialogspool.go`; pinned by
`TestCompat_HookPayload_maps` against the Appendix fixtures. Verified end-to-end
live in a throwaway 2.1.198 session on 2026-07-07 (the throwaway session is the
§9 fixture ground truth).

**Injection.** `claude --settings <file>` accepts a **file path** and merges the
file's `hooks` block **additively** over the repo-shipped `.claude` settings
(hooks accumulate across scopes; they do not replace). lab writes a per-run
settings file under its runtime dir and appends `--settings <path>` to the spawn
argv **before** the trailing prompt positional (`claude [options] [prompt]`), so
the flag is never swallowed as prompt text. Hooks fire normally under
`--remote-control <name> --permission-mode auto`.

**Settings shape** (the block lab generates):

```json
{
  "hooks": {
    "PreToolUse":  [{ "matcher": "AskUserQuestion|ExitPlanMode",
                      "hooks": [{ "type": "command", "command": "mkdir -p <dir> && cat > <spool>.tmp && mv <spool>.tmp <spool>" }] }],
    "PostToolUse": [{ "matcher": "AskUserQuestion|ExitPlanMode",
                      "hooks": [{ "type": "command", "command": "rm -f <spool>" }] }],
    "Notification": [{ "hooks": [{ "type": "command", "command": "mkdir -p <dir> && cat > <marker>.tmp && mv <marker>.tmp <marker>" }] }]
  }
}
```

- The `matcher` is a regex over the **tool name** for Pre/PostToolUse;
  `AskUserQuestion|ExitPlanMode` is an alternation. The Notification matcher (a
  filter over `notification_type`) is omitted → matches all notification types.
- Each `command` runs under `/bin/sh`, receiving the hook event JSON on
  **stdin**. The Pre/Notification commands atomically write stdin (temp +
  rename) to a per-run spool; the Post command deletes it.
- **The PreToolUse hook is purely observational**: it exits 0 with no stdout, so
  the tool proceeds normally and the local operator's TUI picker still shows.
  Only a PreToolUse **exit code 2** (or a JSON `permissionDecision:"deny"`)
  blocks a tool — the spool command never emits either; a write failure exits
  non-zero-but-not-2, which Claude Code logs and ignores. Default hook timeout is
  600 s (a timeout is logged and the tool proceeds).

**Spool protocol (lab-owned layout under the runtime dir).**

- Dialog spool: `<runtime>/dialogs/<runID>.json` — the whole PreToolUse payload;
  overwritten (one dialog pending per session). Fields lab reads: `tool_name`,
  `tool_use_id`, `tool_input` (the exact structured input `dialogFromToolUse`
  also reads from a transcript `tool_use` block — one mapper, two sources).
- Blocked marker: `<runtime>/state/<runID>.json` — the whole Notification
  payload; field lab reads: `notification_type`.
- Per-run settings: `<runtime>/settings.<runID>.json` — the `--settings` target.
- **Answer guard / resolution.** A spooled dialog is suppressed once its
  `tool_use_id` appears in the transcript (the retro-flush landed = resolved);
  the PostToolUse hook is the primary spool delete, this scan the backstop. It
  is **also** suppressed when the transcript has rotated past it (a `/clear` or
  `/rewind` re-point, §5): `PendingDialog` compares the spool payload's
  `session_id` (falling back to its `transcript_path` stem) against the current
  transcript's filename stem — a mismatch means the spool was captured against a
  rotated-out session, so it cannot keep the composer locked against the fresh
  one (issue #34). NEVER an mtime comparison: the transcript is not byte-frozen
  during a pending window (`queue-operation`/`attachment` entries land live, §5)
  — the former spool-older-than-transcript mtime rule suppressed a genuinely
  pending dialog the moment the operator queued a message (found live
  2026-07-10). A payload with no session identity degrades to the old mtime
  backstop. The chat GCs the three per-run files once the run is no longer
  active (an active run's spool survives a lab restart — the file persists).

**Appendix payloads (2.1.198 live, ids/paths anonymized — the fixture ground
truth):**

```
PreToolUse:
  {"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","session_id":"…",
   "transcript_path":"…/<sessionId>.jsonl","cwd":"…","permission_mode":"auto",
   "tool_use_id":"toolu_01KU2pbDQNUNFim79s9FKf6i",
   "tool_input":{"questions":[{"question":"Which flavor of test question do you prefer?",
     "header":"Test","multiSelect":false,"options":[
       {"label":"Option A","description":"The first test option."},
       {"label":"Option B","description":"The second test option."},
       {"label":"Option C","description":"The third test option."}]}]}}

Notification:
  {"hook_event_name":"Notification","notification_type":"permission_prompt",
   "message":"Claude needs your permission","session_id":"…","transcript_path":"…","cwd":"…"}

PostToolUse:
  {"hook_event_name":"PostToolUse","tool_name":"AskUserQuestion",
   "tool_use_id":"toolu_01KU2pbDQNUNFim79s9FKf6i","tool_input":{"questions":[…]},
   "tool_response":{"answers":{"Which flavor of test question do you prefer?":"Option B"},"annotations":{}}}
```

Provenance: hook schema + `--settings` semantics read from the 2.1.198 docs and
`claude --help`; the payload shapes captured live in the throwaway session.
When a Claude Code upgrade breaks live dialog capture, re-verify: the settings
`hooks` shape, the three payload field names, the observational exit-0 contract,
and that `--settings` still merges additively — then update the port, the
fixtures, the tests, and this section in one commit.

## 10. Builtin slash-command catalog — bundle extraction (2.1.198, 2026-07-08)

The command catalog (issue #51 decision 5; `claudecode/commands.go`, served
as `GET /api/v1/runs/{id}/commands`) merges a **pinned builtin table** with
worktree scans. The builtin rows were extracted from the shipped 2.1.198
binary's embedded JS — strings dump via `tr -c '[:print:]' '\n' <
.claude-wrapped`, then matching the `{type:"local|local-jsx|prompt",
name:"…",description:"…",argumentHint:"…",aliases:[…]}` command definitions —
descriptions and argHints **verbatim**, primary names only. Pinned by
`TestCompat_BuiltinCommands_pinned` (clear's exact row + the chat-safe set).

Curation rule: **ChatSafe=true** iff the command executes inline and returns
to the prompt; **false** for anything that opens an interactive
picker/editor/UI lab cannot see (never-scrape: lab could neither render nor
answer it) or that fights lab's own management of the session. The provider
returns ALL rows with honest flags; the API layer filters.

Chat-safe rows (served to the composer), in pinned order:

| command | argHint | notes |
| --- | --- | --- |
| `clear` | `[name]` | **Role=clear** (the "New conversation" binding, decision 2). Aliases `reset`,`new` (aliases are documented here, never served). |
| `compact` | `<optional custom summarization instructions>` | |
| `context` | `[all]` | interactive-TUI variant ("colored grid"); a thin-client variant with different text exists, gated off in lab's spawn shape |
| `usage` | | aliases `cost`,`stats` |
| `status` | | prints the status panel inline |
| `export` | `[filename]` | |
| `release-notes` | | |
| `init` | | description getter is env-dependent (`CLAUDE_CODE_NEW_INIT` alt text); pinned to the default branch |
| `review` | `[pr number]` | type `prompt` — runs as a model turn |
| `security-review` | | type `prompt` |

Curated out (ChatSafe=false, served to the API but filtered from chat), with
the per-row reason:

| command | reason excluded |
| --- | --- |
| `add-dir` | interactive UI; would widen the session beyond lab's worktree isolation |
| `agents` | 2.1.198 ships a removal stub (description literally starts "(removed)"); zero value in chat |
| `config` | opens the settings panel (aliases `settings`) |
| `doctor` | interactive diagnostics UI |
| `exit` | quits the CLI — kills the session lab manages (aliases `quit`; description getter env-dependent, pinned to "Exit the CLI") |
| `feedback` | interactive submission UI |
| `hooks` | interactive viewer — and lab injects its own hooks (§9) |
| `ide` | IDE-integration UI; no IDE exists in a lab session |
| `install-github-app` | interactive wizard + browser flow |
| `login` / `logout` | machine-level auth is lab's own surface (§3); an in-TUI flow would fight it |
| `mcp` | interactive manager |
| `memory` | opens $EDITOR in the TUI |
| `model` | opens the model picker (the stable local-variant text/argHint are pinned; the TUI getter appends "(currently <model>)" dynamically) |
| `permissions` | interactive rules UI (aliases `allowed-tools`) |
| `plan` | flips plan mode / opens plan surfaces the chat cannot see |
| `privacy-settings` | interactive panel |
| `resume` | session picker; also breaks the run ↔ transcript identity lab tracks (§5 rotation) |
| `rewind` | interactive rewind UI (aliases `checkpoint`,`undo`; description "Restore the code and/or conversation to a previous point") |
| `statusline` | terminal-UI setup flow (type `prompt`) targeting user-global settings — pointless and invasive from lab |
| `terminal-setup` | local-terminal keybinding install; description getter is terminal-dependent, pinned to the default branch |
| `upgrade` | billing/browser flow |
| `usage-credits` | billing configuration UI |

Not in the 2.1.198 bundle at all (checked during extraction, not served, kept
here so the next re-extraction doesn't hunt for them): `output-style`, `vim`,
`pr-comments`. Also present in the bundle but deliberately NOT pinned:
internal/hidden/experimental commands (`btw`, `fork`, `radio`, `stickers`,
`heapdump`, `teleport`, …) — the table pins the operator-relevant surface,
not the bundle's entire command registry; re-curate on upgrade.

Scanned sources merged after the builtins (both ChatSafe — a custom
command/skill is a prompt template, claude runs it as a normal turn):

- **project**: `<worktree>/.claude/commands/*.md` (name = basename sans
  `.md`) and the seeded skills `<worktree>/.claude/skills/*/SKILL.md`, one
  group sorted alphabetically. Frontmatter keys read: `name`, `description`,
  `argument-hint`, `user-invocable` — a skill with `user-invocable: false` is
  hidden (2.1.198 schema, verbatim: "If false, hides the slash command from
  users; only the model can invoke it via the Skill tool.").
- **user**: `~/.claude/commands/*.md` (injectable for tests), sorted
  alphabetically.

Missing dirs are silently empty; only real I/O failures error.

## Live re-verification

`TestCompat_Live_authStatusParses` runs `claude auth status --json`
against the installed binary when `LAB_COMPAT_LIVE=1` is set (skipped
otherwise, so CI stays hermetic). Use it after a Claude Code upgrade:

    LAB_COMPAT_LIVE=1 go test ./internal/compat/ -run Live -v

**End-to-end recipe re-verification (issue #51).** `live_recipes_test.go`
adds `TestCompat_Live_askUserQuestionRecipe` and
`TestCompat_Live_exitPlanModeApproval` under the same gate: each spawns a
REAL claude (haiku) in a scratch tmux session, prompts it to raise the
pinned dialog shape, waits for the picker, plays the answer through the real
`DialogKeystrokes`/`AnswerDialog` path with production pacing, then reads
the transcript back and asserts the RECORDED answers (§5 `toolUseResult`)
match the intent — the same comparison the backstop makes. They exist so the
next Claude Code upgrade re-verifies the §7 recipes with one command instead
of a by-hand TUI session; they need tmux + a logged-in claude and take a few
minutes. (They watch the pane via `capture-pane` to know WHEN the picker is
up — that is the verification harness observing its own probe, not a
production scrape; the recipes themselves stay blind.)

When any pin above breaks: update the claudecode port, the fixture, the
affected tests, and this document in the same commit.
