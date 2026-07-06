# Claude Code compatibility pins

Pinned version: **Claude Code 2.1.198** — live probe on the dev host,
2026-07-05, re-confirmed by the M3 acceptance smoke on 2026-07-06. This
document tracks brief §11 (known-fragile couplings 1–4; item 5 —
provider-owned model/effort catalogs — is solved structurally in
`internal/provider`, D14) plus the four embedded-chat couplings 5–8 added
by issue #7 / ADR-0016 (transcript location + JSONL schema, the reply,
dialog, and interrupt send-keys recipes). The implementation of every
coupling lives in `internal/provider/claudecode`; the probe tests in this
package exercise the same code paths against captured fixtures in
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
  string or a `[]block` of `text|thinking|tool_use|tool_result`). Every
  other key is ignored. Captured shape: `testdata/transcript-2.1.198.jsonl`
  (ids/paths anonymized, field names + value shapes verbatim). Mapped to
  the universal schema (`text|tool|dialog|lifecycle`) by `ParseTranscript`.
  fixture (assembled from real 2.1.198 line shapes; re-verify live when an
  upgrade misbehaves).
- **Read-through only**: the transcript file is the source of truth; lab
  persists only `runs.transcript_path` (captured async by cwd-match, the
  `deep_link_url` pattern) so ended runs stay readable while claude retains
  the file. A vanished file is `provider.ErrTranscriptGone` → the UI's
  "transcript no longer available" state.

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

## 7. Dialog keystroke recipes — fixture (2.1.198)

An interactive dialog is an **unanswered** `tool_use` in the transcript for
a recognised tool. Option buttons are built **only** from the structured
tool input — never scraped from the TUI widget (issue #7 decision 5):

- **`AskUserQuestion`**: a single question renders its `options` as tappable
  buttons plus a synthesized free-text **Other** row (the tool always
  offers Other). The answer recipe (`DialogKeystrokes`, played by
  `Provider.AnswerDialog`): normalise the cursor to the top option
  (`Up` × rows−1), `Down` × chosen-index, `Enter`. Multi-select
  (`multiSelect:true`) toggles each chosen option with `Space` then
  confirms with `Enter`. **Other** selects the row, pastes the free text,
  `Enter`. A **multi-question** `AskUserQuestion` is not answerable through
  a single-picker recipe → degrades to the deep-link hint.
- **`ExitPlanMode`** (plan approval): the plan text is shown, but the
  approve/reject choices are **TUI-owned** (not in the tool input), so per
  the never-scrape rule answering degrades to the "open in claude.ai"
  deep-link hint; Interrupt and a free-text reply still work. Revisit when
  the plan-approval option widget is captured live.
- **Unknown** interactive tools degrade to the deep-link hint. Answers the
  operator gives in claude.ai flow back through the transcript — no
  divergence handling is needed.

Recipe snapshot: `TestCompat_DialogKeystrokes`. The `Up/Down/Space/Enter`
key names are standard tmux `send-keys` arguments; the picker navigation is
the fragile part — re-verify against a live picker on upgrades. fixture.

## 8. Interrupt keystroke — fixture (2.1.198)

The chat's stop-generating affordance sends a single `Escape`
(`Provider.Interrupt` → `SendNamedKeys(session, "Escape")`), behind a
confirm tap in the UI. It is distinct from a run Stop: it never touches the
session lifecycle, the budget clock, the claim, or the three-strikes
counter (issue #7 decision 12) — intervention neutrality is structural
(nothing in the chat path writes a run outcome). fixture — the send is
tmux-hermetic; the claude-side Escape-interrupts-the-turn behavior is
re-verified live on upgrades.

## Live re-verification

`TestCompat_Live_authStatusParses` runs `claude auth status --json`
against the installed binary when `LAB_COMPAT_LIVE=1` is set (skipped
otherwise, so CI stays hermetic). Use it after a Claude Code upgrade:

    LAB_COMPAT_LIVE=1 go test ./internal/compat/ -run Live -v

When any pin above breaks: update the claudecode port, the fixture, the
affected tests, and this document in the same commit.
