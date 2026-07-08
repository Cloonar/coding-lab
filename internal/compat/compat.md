# Claude Code compatibility pins

Pinned version: **Claude Code 2.1.198** — live probe on the dev host,
2026-07-05, re-confirmed by the M3 acceptance smoke on 2026-07-06. This
document tracks brief §11 (known-fragile couplings 1–4; item 5 —
provider-owned model/effort catalogs — is solved structurally in
`internal/provider`, D14) plus the four embedded-chat couplings 5–8 added
by issue #7 / ADR-0016 (transcript location + JSONL schema, the reply,
dialog, and interrupt send-keys recipes) and the hook contract §9 added by
issue #17 / ADR-0020 (the PreToolUse/PostToolUse/Notification hook payloads +
the spool protocol that captures a pending dialog live). The implementation
of every coupling lives in `internal/provider/claudecode`; the probe tests
in this package exercise the same code paths against captured fixtures in
`testdata/`.

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
transcript. Four coupled facts, all in `internal/provider/claudecode`
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
- **Read-through only**: the transcript file is the source of truth; lab
  persists only `runs.transcript_path` (captured async by cwd-match, the
  `deep_link_url` pattern) so ended runs stay readable while claude retains
  the file. A vanished file is `provider.ErrTranscriptGone` → the UI's
  "transcript no longer available" state.
- **Flush-on-resolve (pending dialogs are invisible live)**: a pending
  `AskUserQuestion` / `ExitPlanMode` `tool_use` is **not** written to the JSONL
  while it is pending — the transcript file does not change *at all* during the
  pending window; the `tool_use` **and** its `tool_result` are flushed together,
  retroactively (original timestamps), only when the dialog resolves. live
  (2.1.198, 2026-07-07): a question sat pending in the TUI for minutes while the
  transcript stayed byte-frozen at the user-prompt line. Consequence: the
  transcript is not a live source for a pending dialog — see §9, which captures
  it from a PreToolUse hook instead. The transcript-scan dialog path (an
  unanswered `tool_use` in §5) stays as a dormant fallback for a future Claude
  Code that flushes pending `tool_use`.
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

## 6. Reply send-keys — fixture (2.1.198 send path)

A mid-session reply is a **bracketed paste** of the text followed by a
separate `Enter` (`Provider.Reply`, `internal/tmuxx` `PasteText` +
`SendNamedKeys`). Bracketed paste (`tmux load-buffer` + `paste-buffer -p`)
means embedded newlines insert lines in the composer instead of submitting
turn-by-turn — the argv-only stance (§1) covers the *initial* prompt only;
mid-session replies are exempt (issue #7 decision 4). A mid-turn agent
queues the reply in its own TUI. Control characters other than tab/newline
are rejected before the paste (`validateReply`) so a stray escape cannot
break out of the composer. fixture — the send mechanism is exercised
hermetically (`internal/tmuxx/integration_test.go`, real tmux private
socket); the claude-side "queue while mid-turn" behavior is re-verified
live on upgrades.

**UI surfacing (ADR-0022):** this Reply path is **retained at the backend**
but is **no longer surfaced in the UI mid-turn** — while the agent is working
the chat composer shows a one-tap Interrupt in place of Send, so a reply can
only be sent once the agent returns to idle/needs_input. The send-keys recipe
above is unchanged; it is simply unreachable from the SPA during a turn.

## 7. Dialog keystroke recipes — fixture (2.1.198)

An interactive dialog is an **unanswered** `tool_use` in the transcript for
a recognised tool. Option buttons are built **only** from the structured
tool input — never scraped from the TUI widget (issue #7 decision 5):

- **`AskUserQuestion`**: a single question renders its `options` as tappable
  buttons plus a synthesized free-text **Other** row (the tool always
  offers Other). The answer recipe (`DialogKeystrokes`, played by
  `Provider.AnswerDialog`): normalise the cursor to the top option, `Down` ×
  chosen-index, `Enter`. Multi-select (`multiSelect:true`) toggles each chosen
  option with `Space` then confirms with `Enter`. **Other** selects the row,
  pastes the free text, `Enter`. A **multi-question** `AskUserQuestion` is not
  answerable through a single-picker recipe → degrades to the deep-link hint.
  - **Picker geometry (re-verified live, 2.1.198, 2026-07-07).** The picker has
    **two** synthesized trailing rows below the tool's options: "Type
    something." (the **Other** row, modelled in `d.Options`) **and** "Chat about
    this" (**not** modelled), which sits *below* Other:

    ```
    ❯ 1. Option A / 2. Option B / 3. Option C
      4. Type something.     ← the "Other" free-text row (modeled)
      5. Chat about this     ← NOT modeled; sits below Other
    Enter to select · ↑/↓ to navigate · Esc to cancel
    ```

    Down-navigation to a listed option or Other is unaffected (both sit above
    "Chat about this"), but the normalise-to-top climb was one row short of the
    true picker height. It is now `Up × (len(d.Options) − 1 +
    pickerTrailingSynthRows)` (`pickerTrailingSynthRows` = 1, the "Chat about
    this" row). Over-climbing is safe — the picker **clamps** at the top row (the
    same clamp the pre-existing normalise relied on). When a Claude Code upgrade
    adds/removes a trailing synth row, bump the constant. live.
- **`ExitPlanMode`** (plan approval): the plan text is shown, but the
  approve/reject choices are **TUI-owned** (not in the tool input), so per
  the never-scrape rule answering degrades to the "open in claude.ai"
  deep-link hint. It is a pending dialog, so the composer is locked (issue #17
  dec 5 — stray free text must not land in the focused plan picker); the
  operator approves in claude.ai or interrupts (Escape). Revisit when the
  plan-approval option widget is captured live. (ADR-0016 originally described a
  free-text reply working here; issue #17's uniform composer-lock supersedes it.)
- **Unknown** interactive tools degrade to the deep-link hint. Answers the
  operator gives in claude.ai flow back through the transcript — no
  divergence handling is needed.

Recipe snapshot: `TestCompat_DialogKeystrokes`. The `Up/Down/Space/Enter`
key names are standard tmux `send-keys` arguments.

- **Inter-keystroke pacing (live-verified, 2.1.198, 2026-07-07).** The recipe's
  ops MUST be paced — `Provider.AnswerDialog` sleeps `keyDelay`
  (`defaultDialogKeyDelay` = 250ms) between each op. Driving the picker
  back-to-back with no gap over the remote-control bridge **intermittently**
  raced the committing `Enter` ahead of the `Down` navigation and selected the
  wrong row (index 0 instead of the intended one). Observed live: 0ms was flaky
  (wrong option on some trials), 150ms+ reliable across trials. This is the
  brief's "robustify the recipe" — the answer path never drove a real picker
  before issue #17. When an upgrade changes picker responsiveness, re-verify the
  delay is still sufficient. live.

## 8. Interrupt keystroke — fixture (2.1.198)

The chat's stop-generating affordance sends a single `Escape`
(`Provider.Interrupt` → `SendNamedKeys(session, "Escape")`), fired by a
**one-tap** Interrupt square in the composer (ADR-0022 dropped the earlier
confirm tap — interrupt is non-destructive, so a confirmation is friction; the
danger-red header Stop keeps its two-step confirm). It is distinct from a run
Stop: it never touches the session lifecycle, the budget clock, the claim, or
the three-strikes counter (issue #7 decision 12) — intervention neutrality is
structural (nothing in the chat path writes a run outcome). fixture — the send
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
  `/rewind` re-point, §5): `PendingDialog` OR's a spool-older-than-transcript
  mtime check with the tool-id check (mirroring the blocked-marker staleness
  guard), so a pre-clear spool cannot keep the composer locked against the fresh
  session (issue #34). Safe for the genuine pending case — the transcript stays
  byte-frozen during a pending dialog (§5) while the spool is written after it,
  so the spool is always the newer file. The chat GCs the three per-run files
  once the run is no longer active (an active run's spool survives a lab restart
  — the file persists).

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

## Live re-verification

`TestCompat_Live_authStatusParses` runs `claude auth status --json`
against the installed binary when `LAB_COMPAT_LIVE=1` is set (skipped
otherwise, so CI stays hermetic). Use it after a Claude Code upgrade:

    LAB_COMPAT_LIVE=1 go test ./internal/compat/ -run Live -v

When any pin above breaks: update the claudecode port, the fixture, the
affected tests, and this document in the same commit.
