# Codex CLI compatibility pins

Pinned version: **codex-cli 0.167.0** — re-verified 2026-11-10 against the
sha256-verified `codex-x86_64-unknown-linux-musl` release binaries of BOTH
the outgoing 0.133.0 and the incoming 0.167.0 (CLI probes, `codex debug
models`, `codex features list`, `codex login status`, a real `codex login
--device-auth`, a real unauthenticated `codex exec` rollout, a pty smoke of
the TUI with lab's exact argv, the three `LAB_COMPAT_LIVE=1` probes, and a
read of the upstream source at tag `rust-v0.167.0` for everything a
logged-out host cannot drive — see the 0.167.0 re-verification note below).
The bump was taken because the nixos-unstable `codex` package (the host-PATH
copy on the Cloonar dev host) broke at 0.167.0 and the agent-tools pin had
fallen 34 releases behind. **Four couplings moved and were fixed in the same
commit**: the spawn argv gained `--no-daemon` (§1 — 0.167 auto-starts a
shared background daemon; the flag is now the pin's hard version floor),
the rollout grammar gained the paginated-history `item_completed` mapping
(§5 — `user_message`/`agent_message` are no longer written), the
slash-command table was re-derived from source with `/clear` taking the
clear role from `/new` (§7 — `/new` now opens a worktree picker in a git
repo), and §9's "no attribution at source" is refuted: 0.167 carries a
server-driven git-attribution extension, so the ADR-0033 scrub patterns are
now the load-bearing backstop. The live TUI recipes (§3, §4, §6) still date
from the 0.133.0 spike sweep of 2026-07-10 (issue #87's Tier-2 sweep, all
eleven live-spike questions, recorded in the issue's Amendment 2): no
authenticated 0.167.0 host was available, so they are re-verified at the
source level only and flagged below. The §1 model-catalog probe SCHEMA was
live-verified on 0.144.1 (2026-07-13, issue #156 / ADR-0043) and again on
0.167.0 — the catalog *values* are not pinned; they are probed from the
binary at boot. This is the committed Tier-2 compat record ADR-0036 requires
for a new adapter (`docs/agents/provider-authoring.md`):
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
  machine-readable output (`codex debug models`, `codex features list`,
  `--help`) — stronger than a fixture (it is the shipped binary's own
  contract) but not observed end to end in a live flow.
- **source extraction** — read from the upstream Rust source at the pinned
  release tag (`openai/codex` `rust-v0.167.0`, paths under `codex-rs/`):
  the behavior is what the shipped binary was built from, but it was NOT
  observed on a running TUI. Used on 2026-11-10 for everything a logged-out
  host cannot drive; each such pin names the file it was read from and is
  the first thing to re-drive live on an authenticated host.

**0.167.0 re-verification (2026-11-10).** Motivation: the nixos-unstable
`codex` build broke at 0.167.0 (upstream nixpkgs issue #585124, a
`cargoSetupHook` change forcing system `aws-lc`), which surfaced that the
agent-tools pin was 34 releases behind. Both binaries verified against the
GitHub release digests (0.167.0 musl tar.gz
`9d64836106d5b374e90ee4454c7ebcde7ca7106558bd7e41a34b9f26ebda251a`). The
three automated live probes passed on BOTH binaries (logged-out status
shape, `ProbeModels` — 5 listed models / 4 efforts on 0.133, 9 / 6 on
0.167 — and the sessions-tree walk, which skipped on the scratch home).
Drift found, by section: §1 shared daemon (FIXED, `--no-daemon`), §1
catalog values (max/ultra efforts, `gpt-6.1-sol` default with a LOW default
effort — probed, no lab change; new 0.167.0 fixture pins the parse), §2
WARNING wording + phishing sentence (fixtures re-captured; a pre-existing
`Method` parse nit RECORDED), §5 paginated history (FIXED, `item_completed`
mapping; a real 0.167.0 exec rollout is the new fixture), §5 rollout
compression after 7 days (RECORDED), §7 catalog (FIXED, 62 rows from
source, `/clear` carries the clear role, `/copy` and `/new` curated out,
`/hooks` `/plugins` `/pwd` `/recap` `/debug-config` in), §8 `plugin_hooks`
removed and `memories` now stable-off (RECORDED), §9 git-attribution
extension (RECORDED + scrub samples extended), §11 flags re-confirmed on
0.167 plus a `--ephemeral` option (RECORDED). NOT re-verified live — the
by-hand scratch-session items that need a logged-in codex: the §1 sandbox
traps, the §3 trust prompt (the pty smoke reaches the sign-in chooser
before any trust prompt could render, with and without the appended
table), the §4 context-file legs, the §5 lazy birth and `/clear` rotation,
the §6 paste/Esc/Ctrl-C hazards, and the §9 commit ground truth under an
attribution-enabled ChatGPT account. The local `build.sh codex` leg
fetched and sha256-verified the artifact but could not pull
`golang:1.26-alpine` from docker.io in this environment; the injection
smoke is CI's PR leg.

## 1. Spawn argv — live (0.133.0), re-verified 0.167.0 (cli extraction + pty smoke)

```
{codex} --no-daemon --ask-for-approval never --sandbox danger-full-access
        -c project_doc_fallback_filenames=["AGENTS.local.md"]
        [--model M] -c model_reasoning_effort=E [prompt]
```

- **`--no-daemon` (0.167.0+) — the pin's hard version floor.** 0.167.0
  auto-starts a SHARED background app-server daemon for every interactive
  launch (`codex features list`: `daemon_auto_start stable true`;
  `tui/src/startup_orchestration.rs`: `auto_start_daemon = DaemonAutoStart
  && implicit_connection && !cli.no_daemon`, then
  `codex_app_server_daemon::start_with_features` spawns it as its own
  process and the TUI becomes a socket client) and hosts the thread there.
  That is a process lab never spawned, outside the tmux pane `runner.Stop`
  tears down, holding the instance `CODEX_HOME` (and its injected
  `auth.json`, ADR-0055) after the session ends. The `-c` overrides do NOT
  disable it (only `remote-control`'s `daemon_eligible` checks raw
  overrides); nixpkgs patches the feature off for the same reason. The
  flag forces the embedded server — the 0.133 process model — and is
  REJECTED by 0.133.0 (`error: unexpected argument '--no-daemon' found`,
  exit 2), so the argv no longer runs on anything older than the release
  that introduced it. cli extraction (0.167.0 `--help` lists it under the
  root command; `exec` has no such flag and uses no daemon) + pty smoke
  (lab's exact argv with `--no-daemon`, trusted scratch repo: the TUI
  rendered its sign-in chooser, no daemon socket appeared under
  `CODEX_HOME`, no codex process survived the pane).
- Every other flag re-confirmed on 0.167.0 `--help`: `--ask-for-approval`
  now offers only `on-request` and `never` (`untrusted` and `on-failure`
  are gone — lab never used them), `--sandbox` keeps `danger-full-access`,
  `-c` and `-m/--model` and the trailing `[PROMPT]` are unchanged. New
  flags lab ignores: `--approve-for-me`, `--worktree`,
  `--dangerously-bypass-hook-trust`, `--enable/--disable <FEATURE>`
  (sugar for `-c features.<name>=…`); `-p/--profile` changed meaning
  (profile v2 — lab passes none). cli extraction.

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
  the real 0.144.1 output is ~174 KB, 0.167.0's ~745 KB — the
  `base_instructions` blobs grew). Process-lifetime cache: no TTL, no
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
  extraction (schema live-verified on 0.144.1, 2026-07-13, and again on
  0.167.0, 2026-11-10: every pinned field present and unchanged; 0.167.0
  adds many fields — `comp_hash`, `model_specialty`, `tool_mode`,
  `default_service_tier`, … — all ignored). 0.167.0 values, for the
  record (NOT pinned — the probe carries them): 12 models, 9 `"list"`
  (`gpt-6.1-sol`, `gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`, `gpt-5.6-sol`,
  `gpt-6.1-terra`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`), 3 `"hide"`
  (the Daybreak pair and `codex-auto-review`); efforts gain `max` and
  `ultra` (rendered through the title-case fallback, "Max"/"Ultra"); the
  bare spawn default is `gpt-6.1-sol` whose `default_reasoning_level` is
  **`low`** — the first pinned catalog whose default is not medium, which
  the per-model DefaultEffort already carries. Trimmed capture:
  `internal/provider/codex/testdata/models-0.167.0.json`, pinned by
  `TestProbeModels_fixtureMapping_0167`.
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
`catalog_test.go` probe/parse tests against `models-0.144.1.json` and
`models-0.167.0.json`; live re-verification
`TestCompat_Live_debugModelsProbe` (passed on 0.133.0 and 0.167.0,
2026-11-10). When this breaks: a
binary bump **no longer requires re-pinning the catalog values** — the
probe carries those. Re-verify the probe SCHEMA pins instead (field
names/shapes, `visibility` semantics, `priority` ordering, that the first
listed entry is still the intended spawn default) via
`TestCompat_Live_debugModelsProbe` and a fresh trimmed capture if the
schema moved; re-run a real spawn for the argv; and re-check the three
sandbox traps before ever softening the full-access posture. The three
sandbox traps and the never-ask posture were NOT re-driven live on
0.167.0 (no logged-in host); `--sandbox danger-full-access` and
`--ask-for-approval never` still exist with the same help text, and
nothing in the 0.167.0 sandbox source suggests the read-only `.git` trap
was lifted — re-check them first on an authenticated host.

## 2. Auth status + device-code login — live (0.133.0), logged-out shape + device-auth re-captured live on 0.167.0

**Status shapes, both pinned live** (`codex login status`, combined
output + exit code — fixtures `testdata/login-status-in-0.133.0.txt` (the
0.133.0 logged-in capture; no authenticated 0.167.0 host was available,
and the verdict strings are unchanged in `codex-rs/cli` source) /
`login-status-out-0.167.0.txt` (REAL 0.167.0, 2026-11-10 — identical to
the 0.133.0 capture except the leading WARNING's wording)):

```
logged in  → exit 0, "Logged in using ChatGPT"
logged out → exit 1, "Not logged in"
```

- `ParseAuthStatus(out, exitOK)` needs BOTH signals: logged-in iff the
  command exited 0 AND the output carries "Logged in". A plain non-zero
  exit is a definitive logged-out answer, not an error. `Method` is the
  lowercased token after "using " (`chatgpt`); no email is printed —
  `AuthStatus.Email` stays empty for codex. live.
- The logged-out fixture carries a leading `WARNING: … could not create
  PATH aliases …` line (0.133.0 said "could not update PATH"; emitted when
  `CODEX_HOME` sits under a temp dir; path anonymized in the fixture) —
  parsing is containment-based, so leading noise must stay tolerated.
  **FOUND, NOT FIXED (identical on both binaries, 2026-11-10)**: that
  warning's "Ref**using** to create helper binaries" is the first "using "
  in the output, so `ParseAuthStatus` reads `Method` from the warning's
  tail whenever the warning is present. Harmless today — `Method` only
  matters when `LoggedIn` is true, and production codex homes never sit
  under `/tmp` — but a logged-in status printed with that warning would
  report the wrong method. Pinned harmless by
  `TestCompat_AuthStatusFixtures_parse`; the fix (cut "using " from the
  "Logged in" line only) is owed with the next touch of `auth.go`.
- **Device-code login** (`codex login --device-auth` — the plain `codex
  login` spins up a localhost:1455 redirect server a server-first
  deployment cannot reach). Verbatim stdout, live 2026-11-10 on 0.167.0
  (the 2026-07-10 0.133.0 capture differed only in the closing sentence,
  "Device codes are a common phishing target. Never share this code.";
  raw captures carry SGR color codes `\e[90m`/`\e[94m`; the fixture
  `testdata/device-auth-0.167.0.txt` is **ANSI-stripped**, because the
  production scrape source is tmux `capture-pane` rendered text, which
  never contains escape codes; the one-time code below expired 15 minutes
  after capture — safe to quote):

  ```
  Welcome to Codex [v0.167.0]
  OpenAI's command-line coding agent

  Follow these steps to sign in with ChatGPT using device code authorization:

  1. Open this link in your browser and sign in to your account
     https://auth.openai.com/codex/device

  2. Enter this one-time code (expires in 15 minutes)
     A8LJ-AROO3

  Continue only if you started this login in Codex. If a website or another person gave you this code, cancel.
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
`TestLogout_*`); live re-verification `TestCompat_Live_loginStatusParses`
(passed on 0.133.0 and 0.167.0, 2026-11-10). `codex login --help` is
byte-identical between the two versions; `codex logout` and the
`auth.json` location are unchanged in source.
When this breaks: re-run `codex login status` both ways (a scratch
`CODEX_HOME` gives the logged-out shape read-only) and `codex login
--device-auth` in a scratch home, re-pin the strings and both regexes.

## 3. Directory trust — live (0.133.0), source-checked 0.167.0

0.167.0: `projects."<path>".trust_level = "trusted"` still exists
(`codex-rs/config/src/config_toml.rs`, `TrustLevel::Trusted`), so the
append below still targets a real key; whether the `-c` route now works,
and whether the prompt still appears at all, was NOT re-driven live — the
pty smoke with lab's argv reached the sign-in chooser before any trust
prompt could render, with and without the appended table. First item to
re-drive on an authenticated host. source extraction.

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

## 4. Context-file discovery (`AGENTS.local.md`) — live (0.133.0), source-checked 0.167.0

0.167.0: `project_doc_fallback_filenames` still exists
(`config_toml.rs` line ~341, consumed by `core/src/agents_md.rs`), and the
global `$CODEX_HOME/AGENTS.md` concatenation is intact in source. The
three legs below were NOT re-driven live (they need model turns). source
extraction.

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

## 5. Transcript location + rollout JSONL grammar — live (0.133.0) + fixture, 0.167.0 paginated history live (exec) + source

**Location**: `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`
(a lazily-grown date tree; components zero-padded so string order is date
order). The first line of every rollout is a `session_meta` record whose
`payload.cwd` is the session's working directory — `LocateTranscript`
scans date dirs newest-first, files per day by filename descending, and
reads ONLY the first line of each candidate until the cwd matches. live
(0.133.0; re-confirmed on 0.167.0 by a real `codex exec` rollout at
`sessions/2026/11/10/rollout-2026-11-10T10-54-59-<uuid>.jsonl`, and in
`codex-rs/rollout/src/rollout_file_name.rs` — a `thread/revert` can
produce `rollout-<ts>-<uuid>_<rollout_id>.jsonl`, which the same prefix
scan still finds).

- **0.167.0 PAGINATED HISTORY — the grammar moved (FIXED).** Every new
  0.167.0 thread is written in `history_mode: "paginated"`
  (`session_meta.payload.history_mode`; 0.133 rollouts carry no such key
  and are "legacy"), and `codex-rs/rollout/src/policy.rs`
  `persisted_event_msg` persists the `user_message`, `agent_message`,
  `agent_reasoning`, `patch_apply_end` … event_msgs ONLY in legacy mode.
  In paginated mode the prompt and the prose are persisted as
  `event_msg`/**`item_completed`** records carrying a `TurnItem`
  (`codex-rs/protocol/src/items.rs`, serde tag `type` with the variant
  name VERBATIM): `{"type":"item_completed","thread_id":…,"turn_id":…,
  "item":{"type":"UserMessage","id":…,"content":[{"type":"text","text":…,
  "text_elements":[]},…]}}` and `{"item":{"type":"AgentMessage","id":…,
  "phase":"commentary|final_answer","content":[{"type":"text","text":…}]}}`.
  `ParseTranscript` now maps both grammars: `UserMessage` → user text,
  `AgentMessage` → assistant text (text parts only, joined by newlines;
  images/mentions render nothing), every other item kind
  (`CommandExecution`, `FileChange`, `Reasoning`, `McpToolCall`, …) is
  skipped. No double-render is possible: a rollout is written in exactly
  one mode, and legacy mode persists `item_completed` only for
  `FunctionCallOutput`/`Plan`/`Sleep`/`SubAgentActivity`. The
  `response_item` records (`function_call`, `custom_tool_call`, their
  outputs, `reasoning`) are persisted in BOTH modes
  (`should_persist_response_item` is mode-independent) and remain the
  chip/thinking source — so the exec chip titles, `call_id` back-patching
  and encrypted-reasoning rules below are untouched. live (the
  `UserMessage` item, from the real 0.167.0 exec rollout) / source
  extraction (the `AgentMessage` shape and the response_item persistence —
  no authenticated capture; the `AgentMessageContent` enum has a single
  `Text` variant with no `rename_all`, so both `"text"` and `"Text"` tags
  are accepted). New record types seen in the real rollout and skipped:
  `world_state`, `event_msg/thread_settings_applied`,
  `response_item/additional_tools`, `response_item/configuration_update`;
  `session_meta` grew `session_id`, `source`, `thread_source`,
  `runtime_workspace_roots`, `history_mode`, `context_window`, `git`
  (lab reads only `cwd`). The `codex exec` originator is `codex_exec`.
  Fixture: `testdata/rollout-exec-0.167.0.jsonl` (the real unauthenticated
  run — the turn 401s at the API, so it carries `task_started`, the
  `UserMessage` item and a `task_complete` with an `error`;
  `base_instructions`, the developer-prompt blobs and the gateway error
  body are stubbed, scratch paths rewritten to `/work/repo`, all else
  verbatim). Pinned by `TestCompat_RolloutExecFixture_paginated` and the
  codex package's `TestParseTranscript_paginatedItemCompleted`. **An
  authenticated 0.167.0 TUI rollout is owed**: it is the only way to
  confirm the `AgentMessage` item shape, the `function_call` tool names
  (`exec_command`/`shell` on 0.133 — 0.167 may emit `unified_exec`, which
  would show the bare name until re-pinned), and the apply_patch path
  (`custom_tool_call` vs the `FileChange` item).
- **Event names survive**: `task_started`/`task_complete` are the v1 wire
  spellings (`#[serde(rename = "task_started", alias = "turn_started")]`
  on `EventMsg::TurnStarted`, likewise `task_complete`/`turn_complete`),
  `turn_aborted` with `reason` `interrupted|replaced|review_ended|budget_limited`
  (snake_case), and `token_count.info.last_token_usage.{total_tokens,
  reasoning_output_tokens}` + `info.model_context_window` are all unchanged
  in `codex-rs/protocol/src/protocol.rs`. live (`task_started`/`task_complete`
  in the real 0.167.0 rollout) / source extraction (the rest).
- **Rollout compression (RECORDED, not handled)**: 0.167.0 ships a
  maintenance worker that compresses rollouts older than 7 days
  (`codex-rs/rollout/src/compression.rs`, `MIN_ROLLOUT_AGE` = 7 days) to
  `rollout-….jsonl.zst` and removes the plain file; the recorder
  re-materializes a compressed rollout for append. `LocateTranscript`
  matches `rollout-*.jsonl` only, so an instance idle for more than a week
  could lose its transcript to `ErrTranscriptGone` until the next turn
  materializes it. Not observed; owed a follow-up (accept `.jsonl.zst` or
  pin the worker's trigger).
- **Archived sessions**: `/archive` moves a rollout to
  `$CODEX_HOME/archived_sessions/`, which the scan never visits — one
  more reason `/archive` and `/delete` are curated out (§7). source
  extraction.

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
- `event_msg`/**`token_count`** → the context-pressure meter
  (`Chat.ContextUsage`, issue #243 / ADR-0061). lab reads ONLY
  `info.last_token_usage.total_tokens` and `.reasoning_output_tokens` and
  the self-reported `info.model_context_window`; the LATEST event with a
  usable `info` wins (`Used = total_tokens − reasoning_output_tokens` —
  codex's own `tokens_in_context_window`, since reasoning output is dropped
  from the window between turns; `Limit = model_context_window`, the
  spawn-time model is ignored — codex knows its own window). The degenerate
  `{"info":null}` shape codex also writes never clears a tracked value, and
  a non-positive `Used` yields no meter (`ContextUsage` nil).
  `total_token_usage` (cumulative session spend) and `rate_limits` are
  deliberately NOT read — the meter is occupancy, not cost. The plain
  fixture's last usable record (line 23) reads 11894 of 258400. live.
- `event_msg`/`patch_apply_end` and `turn_context` records are skipped (no
  chat content; `turn_context` re-states cwd and the sandbox/approval
  policy per turn). live.
- No dialogs are ever derived (`StateQuestion` unreachable) — never-ask
  posture, no structured question tool.
- Read-through only: lab persists `runs.transcript_path`; a vanished file
  is `provider.ErrTranscriptGone`.

Fixtures: `testdata/rollout-plain-0.133.0.jsonl` (exec tools + prose,
`codex exec` originator), `rollout-patch-0.133.0.jsonl` (apply_patch
custom_tool_call + the failing-commit ground truth + encrypted reasoning),
`rollout-aborted-0.133.0.jsonl` (a real TUI session: multi-line pastes,
Ctrl-J newlines, a queued mid-turn message, and the Esc interrupt tail —
`turn_aborted` reason `interrupted`) — all three legacy-mode, still the
pins for the response_item chip grammar — plus
`rollout-exec-0.167.0.jsonl` (paginated mode, above).

Pinned by `TestCompat_RolloutPlainFixture_maps`,
`TestCompat_RolloutPatchFixture_maps`,
`TestCompat_RolloutAbortedFixture_maps`,
`TestCompat_RolloutExecFixture_paginated` (plus the codex package's
`TestLocateTranscript_*`/`TestParseTranscript_*`); live re-verification
`TestCompat_Live_locateTranscript` (skipped on 2026-11-10 — the scratch
homes had no TUI sessions tree; the exec rollout above was parsed by the
fixture test instead). The lazy-birth timing and the `/new` rotation were
NOT re-verified on 0.167.0; note `/new` is no longer the rotation command
lab sends — `/clear` is (§7), and its `ThreadStartSource::Clear` →
`start_fresh_session` path is the native rotation in 0.167 source. When this breaks: capture a fresh
rollout with a trivial `codex exec` run in a scratch dir, diff the record
grammar, re-derive the fixtures (stub `base_instructions` again), and
re-check `/new` rotation in a real TUI. Re-check the lazy-birth timing too:
spawn an idle TUI and confirm no rollout file appears until the first turn
starts; if a future codex version writes the rollout at session init
instead, this pin goes stale but harmless — first-message delivery must
still never gate on transcript existence.

## 6. Reply + interrupt recipes — live (0.133.0), NOT re-verified on 0.167.0

No logged-in host was available on 2026-11-10, so none of the hazards
below were re-driven on 0.167.0. Source-level notes only: the composer's
bracketed-paste handling, the Esc-to-interrupt footer and the Ctrl-C quit
are all still present in `codex-rs/tui/src/bottom_pane`/`app`, and
`instant_interrupt` is a new under-development feature (default on) that
may change how fast an Esc lands but not that it lands. Re-drive every
hazard in a scratch TUI over tmux before trusting the recipes on 0.167.0.

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

## 7. Builtin slash-command catalog — source extraction (0.167.0, 2026-11-10)

The catalog (`codex/commands.go`, served via `Commands`) is a **static
pinned table** — codex 0.167 still has no project- or user-level command
discovery (skills load from the global `$CODEX_HOME/skills` plus plugin
roots, §8; a repo's `.codex` dir is not a command source), so the catalog
is identical for every session. The rows are the `SlashCommand` enum of
`codex-rs/tui/src/slash_command.rs` at tag `rust-v0.167.0` in declaration
order — the popup renders `built_in_slash_commands()`, i.e. the enum
filtered by `is_visible()` — with names from strum's kebab-case plus the
explicit overrides (`setup-default-sandbox`, `approve`, `pwd`, `pets`,
`stop`, `subagents`, `debug-m-drop`, `debug-m-update`) and descriptions
from `description()`. On a Linux release build **62 commands are
visible** (`App` is macOS/Windows-only; `Rollout` and `TestApproval` are
debug-build-only) and the pinned table carries all **62**. The popup
additionally hides some rows at runtime
(`bottom_pane/slash_commands.rs` `builtins_for_input`: `/plan` behind
collaboration modes, `/apps` behind connectors, `/plugins` behind the
`plugins` feature, `/usage` behind ChatGPT-backend auth, `/goal` behind
`goals`, `/worktree` behind local worktree operations, `/voice` behind
realtime availability, `/daybreak` behind account eligibility,
`/setup-default-sandbox` on Windows only) — and a hidden command typed into
the composer is NOT dispatched as a command, so gated rows are curated out
unless their gate is known-on by default (`plugins`). The 0.133.0 table was
a live tmux scrape (40 rows); this one is a source extraction rendered in
the same `  /name  description` row form
(`testdata/commands-source-0.167.0.txt`) — the first thing to re-scrape on
an authenticated host. Curation rule (claudecode §10's): **ChatSafe=true**
iff the command executes inline and returns to the prompt (verified
against each `SlashCommand::X =>` arm in
`tui/src/chatwidget/slash_dispatch.rs`); **false** for anything that opens
a picker/overlay lab cannot see, that the popup may hide, or that fights
lab's own management of the session. The provider returns ALL rows with
honest flags; the API layer filters.

Chat-safe rows (served to the composer), in pinned order:

| command | description (verbatim) | notes |
| --- | --- | --- |
| `clear` | clear the terminal and start a new chat | **Role=clear** (the "New conversation" binding, moved here from `/new`): `AppEvent::ClearUi` → `start_fresh_session(ThreadStartSource::Clear)` — a fresh thread and rollout, no picker (§5) |
| `compact` | summarize conversation to prevent hitting the context limit | same rollout file (no rotation); refused inline under parent-owned input |
| `status` | show current session configuration and token usage | inline panel |
| `diff` | show git diff (including untracked files) | inline |
| `mcp` | list MCP tools; use /mcp verbose or /mcp login <name> | bare `/mcp` is an inline list (`McpServerStatusDetail::ToolsAndAuthOnly`); description changed from 0.133 |
| `ps` | list background terminals | inline |
| `stop` | stop all background terminals | inline action — a legitimate operator brake |
| `pwd` | show the current working directory | inline info message (new) |
| `recap` | summarize the current conversation now | `AppEvent::GenerateRecap`: a model turn rendered inline like `/compact`; refused inline while a turn runs (new) |
| `hooks` | view and manage lifecycle hooks | `add_hooks_output`: inline listing (0.133 curated it out as a management overlay) |
| `plugins` | browse plugins | `add_plugins_output`: inline listing; gate `plugins` is stable+on (0.133 curated it out as a browser overlay) |
| `debug-config` | show config layers and requirement sources for debugging | `add_debug_config_output`: inline (new) |

Curated out (ChatSafe=false, served but filtered from chat), with the
per-row reason:

| command | description (verbatim) | reason excluded |
| --- | --- | --- |
| `agents` | open the agent command center | overview overlay (replaces 0.133's `/agent`) |
| `approve` | approve one retry of a recent auto-review denial | approval overlay; never-ask posture keeps these out anyway |
| `apps` | manage apps | inline listing, but popup-gated on `connectors_enabled` |
| `archive` | archive this session | confirmation picker; session-ending, moves the rollout to `archived_sessions/` (§5) |
| `btw` | start a side conversation in an ephemeral fork | side-conversation UI (alias-shaped sibling of `/side`) |
| `cd` | change the current working directory | moves the session out of the lab worktree |
| `config` | configure Codex preferences | config panel overlay |
| `copy` | copy the last response or part of it | `show_copy_picker`: a picker on 0.167 (0.133's `/copy` was output-only and chat-safe) |
| `daemon` | Manage the local background server | daemon menu; lab spawns with `--no-daemon` (§1) |
| `daybreak` | turn Daybreak on or off | popup-gated on account eligibility; a toggle the recipes do not model |
| `debug-m-drop` | DO NOT USE | upstream-marked debugging command |
| `debug-m-update` | DO NOT USE | upstream-marked debugging command |
| `delete` | permanently delete this session | confirmation picker; destroys the transcript lab reads |
| `exit` | exit Codex | session-ending — kills the session lab manages |
| `experimental` | toggle experimental features | toggle overlay |
| `export` | export the conversation as markdown | export popup |
| `feedback` | send logs to maintainers | external submit flow |
| `fork` | fork the current chat | worktree picker in a git repo; forks transcript identity under lab |
| `goal` | set or view the goal for a long-running task | popup-gated on `goals`; unverified overlay behavior |
| `ide` | include current selection, open files, and other context from your IDE | needs an IDE attach lab has no surface for |
| `import` | import setup, this project, and recent chats from Claude Code | migration overlay |
| `init` | create an AGENTS.md file with instructions for Codex | submits a turn that writes a repo-tracked AGENTS.md (ADR-0035 violation from inside the TUI) |
| `keymap` | remap TUI shortcuts | overlay; remaps keys the recipes assume |
| `logout` | log out of Codex | machine-level auth is lab's own surface (§2) |
| `memories` | configure memory use and generation | config overlay (`memories` reads `stable false` in `codex features list`) |
| `mention` | mention a file | inserts `@` and opens the file picker |
| `model` | choose what model and reasoning effort to use | picker |
| `new` | start a new chat during a conversation | `show_session_checkout_picker`: inside any git repo (every lab worktree, `local_worktree_operations` defaults true) a "Where should the new conversation run?" picker — the clear role moved to `/clear` |
| `permissions` | choose what Codex is allowed to do | picker; fights the pinned spawn posture (§1) |
| `pets` | choose or hide the terminal pet | picker |
| `plan` | switch to Plan mode | popup-gated on collaboration modes; mode switch ending in an approval flow lab cannot see |
| `presets` | run or save reusable prompts and slash commands | quick-dial menu |
| `quit` | exit Codex | session-ending (alias-shaped sibling of `/exit`) |
| `raw` | toggle raw scrollback mode for copy-friendly terminal selection | rendering-mode toggle the recipes/pane scrapes assume off |
| `rename` | rename the current thread | input overlay |
| `resume` | resume a saved chat | picker; swaps transcript identity under lab (§5) |
| `review` | review my current changes and find issues | review picker overlay |
| `setup-default-sandbox` | set up elevated agent sandbox | Windows-only setup flow; the popup hides it on Linux |
| `side` | start a side conversation in an ephemeral fork | side-conversation UI |
| `skills` | use skills to improve how Codex performs specific tasks | picker |
| `statusline` | configure which items appear in the status line | config overlay |
| `subagents` | switch between this session's subagents | agent picker |
| `theme` | choose a syntax highlighting theme | picker |
| `title` | configure which items appear in the terminal title | config overlay |
| `tui` | choose the TUI mode for the next launch | picker |
| `usage` | view account usage or use a usage limit reset | usage menu; popup-gated on ChatGPT-backend auth |
| `vim` | toggle Vim mode for the composer | composer-mode toggle; breaks the paste+Enter reply recipe (§6) |
| `voice` | start or stop voice; use /voice settings to choose a voice | popup-gated on realtime availability; an audio surface lab has none of |
| `warnings` | view retained warnings and diagnostic details | warnings overlay |
| `worktree` | start or continue a conversation in a new worktree | worktree picker; moves the session out of the lab worktree |

Gone since 0.133: `personality` (the `personality` feature is `removed`),
`agent` (→ `agents`). Pinned by `TestCompat_BuiltinCommands_pinned`
(count, pinned order of the chat-safe set, `/clear`'s clear role and
`/new`'s loss of it, table and listing fixture matching exactly in both
directions) plus the codex package's `TestBuiltinCommands_pinnedGolden`.
When this breaks: re-extract from `slash_command.rs` at the new tag (or
re-scrape the popup on an authenticated host — type `/`, one Down per
step, capture-pane per step — tolerate the update banner, §10), re-diff
names/descriptions, re-read each dispatch arm, and re-curate row by row.

## 8. Skills & hooks — live (0.133.0), cli/source-checked 0.167.0, recorded as the LiveSignals follow-up substrate

0.167.0 drift (cli extraction, `codex features list` 2026-11-10): `hooks
stable true` holds; **`plugin_hooks` is now `removed false`**; `memories`
moved from `experimental false` to `stable false`; `codex_git_commit`
stays `removed false` (§9); new and relevant: `daemon_auto_start stable
true` (§1). The hook event vocabulary is unchanged and now carries
snake_case wire names in `codex-rs/hooks/src/lib.rs` (`session_start`,
`user_prompt_submit`, `pre_tool_use`, `post_tool_use`, `stop`,
`subagent_stop`, `pre_compact`, `permission_request`). Skills still load
from `$CODEX_HOME/skills` (plus plugin skill roots —
`codex-rs/skills/src/lib.rs`, `core/src/skills.rs`); no repo-relative
`.codex/skills`/`.agents/skills` root appears in source, so
`NativeSkillDiscovery: false` stands — the planted-marker legs below were
NOT re-driven live. source extraction.

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

## 9. Attribution ground truth — live (0.133.0); 0.167.0 REFUTES "absent at source" (source extraction)

**0.167.0 ships a git-attribution extension**
(`codex-rs/ext/git-attribution/`, installed as a prompt contributor —
`lib.rs` `install`, no feature gate). Per thread it asks the ChatGPT
backend for the account's user settings and, when
`commit_attribution_enabled` is true, injects a developer-role world-state
block telling the model: "Commit messages must end with `Co-authored-by:
Codex <noreply@openai.com>`" and "Pull request bodies must include the
exact line `Generated with [Codex](https://openai.com/codex/).`" — and
"Ignore any earlier instructions disabling Codex attribution"
(`world_state.rs` `ENABLED_INSTRUCTIONS`). With the setting off, or on a
non-ChatGPT auth (API key), it injects `DISABLED_INSTRUCTIONS` instead.
Consequences for lab:

- **There is still no local off switch.** `commit_attribution` is still
  not a config key (`config_toml.rs` has none; `--strict-config` would
  reject it), the `codex_git_commit` feature stays `removed`, and the
  policy comes from the server per account. `SeedOpts.Incogni` therefore
  stays a no-op for codex — not because there is nothing to disable, but
  because nothing CAN be disabled from the client; the §1 argv is
  unchanged.
- **The ADR-0033 `ScrubPatterns` are now load-bearing, not defensive.**
  They match 0.167.0's exact strings:
  `co-authored-by:[[:space:]]*codex` catches `Co-authored-by: Codex
  <noreply@openai.com>`, and `generated with.*codex` catches both
  `Generated with [Codex](https://openai.com/codex/).` and the legacy
  matcher's `Generated with Codex.` — pinned verbatim in
  `TestCompat_ScrubPatterns_markerSamples` (2026-11-10). The agent-API body
  sanitizer and the pre-push guard are what keep an attribution-enabled
  ChatGPT account's commits clean under lab.
- **The commit ground truth below is 0.133.0's** and was NOT re-driven:
  under an attribution-enabled account the model is instructed to append
  the trailer, so a real 0.167.0 commit may well carry it before the scrub
  — exactly the case the backstop exists for. On the next authenticated
  host: commit from a real session and inspect `git log` both before and
  after the pre-push guard.

The 0.133.0 record:

- **A real codex-authored commit carries NO attribution of any kind** —
  no `Co-authored-by` trailer, no "generated with" line; the commit
  message is verbatim what the agent wrote (spike 10 committed
  `add hello` via the agent and inspected `git log`; the successful
  commit's rollout is `testdata/rollout-plain-0.133.0.jsonl`). live.
- The `codex_git_commit` feature (the co-author-trailer feature
  attribution rode on before the extension) is **`removed` / off** in 0.133
  and still in 0.167 (`codex features list`, cli extraction).
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
  describe the responses API.`. On 0.133 these were DEFENSIVE shapes
  (documented upstream default), not live-observed output — the live
  observation was their absence; on 0.167 the first and `Generated with
  [Codex](https://openai.com/codex/).` are the extension's verbatim
  instructions.

Pinned by `TestCompat_ScrubPatterns_markerSamples`, the codex package's
`TestSeedMeta_scrubPatternsCompileAndMatchCanonicalSamples`, and the
Tier-1 `scrub-markers` conformance subtest (both real engines + the union
path). When this breaks — or on ANY codex upgrade — commit from a real
codex session and inspect `git log` (and a PR body if the git feature
returns): if attribution appears, reconcile the patterns against the REAL
marker (tighten/extend, never just delete) and flip the incogni measure
from no-op to a source-level disable if one now exists.

## 10. Terminal environment — live (0.133.0); 0.167.0 pty smoke only

0.167.0 under `script` with `TERM=xterm-256color` (not tmux — none on the
verification host): the TUI started, rendered its banner and sign-in
chooser, and emitted `?` queries for OSC 10/11 (foreground/background
color) — the same class as the OSC-11 hang noted below, so the tmux
`default-terminal` pin stays worth re-checking on the first real 0.167.0
session. The update banner is absent on 0.167.0 (it is upstream `latest`).

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
exec --help` and re-confirmed on **0.167.0** (2026-11-10 — `--sandbox
read-only`, `--skip-git-repo-check` and the `[PROMPT]` positional all
present; `codex exec` uses no daemon, so §1's `--no-daemon` is not needed
here; two notes: 0.167.0 appends piped stdin as a `<stdin>` block even
when a prompt is given, which `exec.CommandContext`'s nil Stdin — /dev/null
— makes empty, and a new `--ephemeral` "Run without persisting session
files to disk" would stop each refresh probe from leaving a ~100 KB
rollout in the MASTER codex home's sessions tree — RECORDED as an option,
not adopted, since the recipe itself is still unverified): `--sandbox read-only` locks the probe so it can never write through
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

**Logged-out guard (issue #101) — shape assumed, clear-on-failure
UNVERIFIED.** `CredentialsSig`/`AdoptCredentials` treat an `auth.json` as
logged in only when it parses as a JSON object AND carries either a non-empty
string `OPENAI_API_KEY` or a `tokens` object with non-empty `access_token`
and `refresh_token` (the known codex-cli shape; no real `auth.json` is
captured in this repo). Anything else reads as logged out (sig `""`) and is
refused by adopt-back. This is the minimum guard: what codex actually writes
to disk after a failed refresh has not been observed, so capture a real
cleared file and tighten `authLoggedIn` accordingly.

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
and/or cost model calls). The 2026-11-10 bump ran the three probes on
both binaries (the sessions walk skipped — no TUI tree on a logged-out
host) and additionally: `--help` diffs (root/exec/login), `codex features
list` diffs, a real `codex login --device-auth`, a real unauthenticated
`codex exec` rollout, and a pty smoke of lab's argv; everything the
logged-out host could not drive is marked source extraction above and is
the checklist for the first authenticated 0.167.0 session.

When any pin above breaks: update the codex port
(`internal/provider/codex`), the fixture, the affected tests, and this
document in the same commit.
