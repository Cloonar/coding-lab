# Claude Code compatibility pins

Pinned version: **Claude Code 2.1.198** — live probe on the dev host,
2026-07-05, re-confirmed by the M3 acceptance smoke on 2026-07-06. This
document tracks brief §11 (known-fragile couplings 1–4; item 5 —
provider-owned model/effort catalogs — is solved structurally in
`internal/provider`, D14). The implementation of every coupling lives in
`internal/provider/claudecode`; the probe tests in this package exercise
the same code paths against captured fixtures in `testdata/`.

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

## 4. Trust / attribution keys — folder trust live (2.1.198), MCP fixture (v0)

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
- Attribution/Co-Authored-By keys: **M7** (incogni); intentionally not
  pinned here yet.

## Live re-verification

`TestCompat_Live_authStatusParses` runs `claude auth status --json`
against the installed binary when `LAB_COMPAT_LIVE=1` is set (skipped
otherwise, so CI stays hermetic). Use it after a Claude Code upgrade:

    LAB_COMPAT_LIVE=1 go test ./internal/compat/ -run Live -v

When any pin above breaks: update the claudecode port, the fixture, the
affected tests, and this document in the same commit.
