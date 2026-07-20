# Agent Brief: `lab` — remote coding-agent manager

**Audience**: the implementing agent (Fable 5, ultracode) building the MVP end to end in this repository.
**Status**: all decisions below are settled with the product owner (2026-07-05). Do not re-litigate them. Where this brief is silent, make the smallest choice consistent with the decision log and record it as an ADR.

---

## 1. Mission

Build the production version of `lab`: a self-hosted server with a phone-first web interface for managing remote coding agents against git repositories. The operator adds repositories and credentials in the UI, starts **manual instances** (interactive `claude --remote-control` sessions reached via claude.ai deep links) or **AFK runs** (unattended sessions that resolve one `ready-for-agent` issue and open a PR), from any device. Repos without a usable forge tracker get a built-in issue tracker with lab-internal change requests, reviewable and mergeable from the phone.

A working prototype exists and is vendored read-only at [`docs/reference/lab-v0/`](reference/lab-v0/README.md). It is the **behavioral specification** for the session/worktree/AFK core: port its decision logic, don't re-derive it. Its `*_test.go` tables are the most precise statement of intended behavior; its five ADRs (in `reference/lab-v0/adr/`) explain why the model is shaped the way it is.

## 2. How to work this brief

1. Read `docs/reference/lab-v0/README.md`, then the five reference ADRs, then skim the Go source it maps.
2. Implement milestone by milestone (§12), in order. Each milestone is a vertical slice: it ends with tests green, CI green, and the acceptance criteria demonstrably met.
3. Commit per coherent change with Conventional Commits. This repo's own issue tracker is Forgejo (`tea`, see `docs/agents/issue-tracker.md`) — that governs *developing this repo* and is unrelated to the product's runtime tracker integrations.
4. Produce the documentation as you go (§13): `CONTEXT.md` glossary, `docs/adr/` entries for the decisions in §4 (one ADR per decision or coherent group, written in the style of the reference ADRs), README, ops doc.
5. The **known-fragile couplings** (§11) must be verified against the installed Claude Code version early (milestone M3), not discovered late.

## 3. Product shape

- **Single-operator, product-shaped** (D2): one admin user; self-contained deployable (own auth, own storage); schema leaves room for multi-user later (a `users` table with one row, user FKs where cheap) but no tenancy logic.
- **Performance priority**: low idle footprint. The app is an orchestrator — it shells out to `git`/`tmux`/`claude`; its own overhead must stay negligible next to the agent processes it manages (target: idle RSS in the tens of MB, single static binary).
- **MVP interaction model**: lab manages lifecycle; the operator *drives* sessions in the claude.ai app/web via captured deep links. The embedded remote interface (chat/terminal inside lab's own UI) is explicit roadmap, not MVP — but the API/SSE architecture exists to enable it.

## 4. Decision log (settled)

| # | Decision | Rationale |
|---|---|---|
| D1 | **Greenfield in this repo, Go backend** (current stable Go). | Orchestrator workload; lowest footprint per capability; the v0 logic ports nearly 1:1. |
| D2 | **Single-operator, product-shaped** tenancy. | See §3. Claude auth is per-OS-user, so true multi-user needs OS/container isolation — out of scope. |
| D3 | **Nix flake first-class**: `packages.{lab,labctl}`, `nixosModules.lab`, devshell, `nix flake check` running the full test suite. Binary also runs on any Linux with `git`/`tmux`/`claude` on PATH. No Docker in MVP. | Fleet is NixOS; cloonar-nixos consumes the flake input. |
| D4 | **tmux behind a `SessionRunner` interface** owns agent processes; keep the `prlimit --nofile` containment. | PTY for the interactive TUI; sessions survive service restarts (`KillMode=process`); send-keys/pane-scrape for login; attach for debugging; future embedded-terminal path. |
| D5 | **SolidJS SPA + Go JSON API + SSE**, SPA built by Vite, embedded via `go:embed` (single artifact); PWA manifest. | Future-proof (API is the extension surface for embedded UI/CLI/Codex) while runtime-light; Node exists only at build time. |
| D6 | **SQLite (default) + PostgreSQL** behind one repository layer; plain `database/sql` (`modernc.org/sqlite`, `pgx`), embedded goose migrations applied on startup; portable SQL subset. **DB stores durable data only** — liveness stays derived (tmux = session truth, git branches = claims, tracker = PRs). | The layering principle prevented v0's whole label-flapping bug class (reference ADR-0013). CGO-free keeps the static binary. |
| D7 | **Auth**: built-in admin login (Argon2id, secure session cookie, remember-me) + opt-in **trusted-proxy mode** (Authelia `Remote-User` header from configured proxy CIDRs) + **personal access tokens** for the API. Passkeys deferred. | Works bare anywhere; no double-login behind Authelia; machine auth for CLI/automation. |
| D8 | **Repos are configured in the web UI and cloned by the app** into lab-owned **bare reference clones**; a credential is selected per repository at add time. No directory scanning, no linked paths. | Bare = structurally never dirty, native worktree parent; explicit add is production behavior. |
| D9 | **Credentials encrypted at rest** (AES-256-GCM) with a **master key file whose path is configurable** (auto-generated 0600 on first start if absent; point it at a sops-nix/`LoadCredential` path on NixOS). Kinds: SSH key, HTTPS token, forge API token. Applied via `GIT_SSH_COMMAND`/`GIT_ASKPASS` with key material materialized 0600 in a private runtime dir — never in URLs or repo config. Secrets are never displayed back; delete blocked while referenced. **Claude auth stays machine-level** via the ported `claude auth login` flow. | Product-shaped secret handling without external infra; sops-compatible where it exists. |
| D10 | **`labctl` + scoped run tokens** are the agent's only tracker surface. Lab ships the companion CLI; each session gets `LAB_URL` + `LAB_TOKEN` (short-lived, scoped to that run's repo) and `labctl` on PATH. Behind lab's agent API sits a **`Tracker` interface**: **Forgejo REST** and **built-in** in MVP; **GitHub REST is fast-follow ([#1](https://git.cloonar.com/Cloonar/coding-lab/issues/1))**. The `tea` dependency is dropped entirely. The surface covers the full triage set (ADR-0014): issue create, label add/remove/list, idempotent label create, close — identical in interactive instances and AFK runs. | One seed-prompt vocabulary for all trackers; per-repo tokens instead of ambient personal logins; agents can only touch their own repo. |
| D11 | **Built-in tracker = issues + change requests** (lab-internal PRs: head branch, title, body, `Closes #N`, state) with **diff view and merge from the UI**. Tracker binding is per-repo: forge (auto-detected from remote) or built-in (forceable). | Full symmetry: one agent contract, one reaper done-signal, non-forge repos first-class. |
| D12 | **AFK engine: port the v0 model + 4 deltas** — (a) run history table for all runs, (b) budget clock persisted on the run row, (c) caps/budgets/intervals configurable globally with per-repo overrides, (d) per-repo model/effort defaults with per-spawn override. Everything else unchanged: claim-is-the-branch, selection under one mutex, PR/CR-with-head-branch as done-signal, death/timeout = failure, ~2h default budget, 3 consecutive failures pause the repo until human Reset, one auto run per repo, manual runs additive under the global cap (default 6), user Stop neutral, guarded teardown, restart re-adoption, Parked view with unguarded Discard. | The ported rules encode debugged failure modes; the deltas fix known v0 gaps (invisible outcomes, restart budget reset, constants). |
| D13 | **Skills**: vendored pinned bundle ([`assets/skills/`](../assets/skills/README.md), mattpocock/skills @ `4369256` + local tdd patch) embedded in the binary and **copied into each worktree's `.claude/skills/`** at spawn; plus a generated **`CLAUDE.local.md`** (tracker binding, `labctl` vocabulary, triage-label mapping, explicit note that `labctl` supersedes any committed `tea`/`gh` docs). Both listed in `.git/info/exclude`. Configurable bundle source is roadmap. | Works on fresh hosts regardless of user-level installs; never committable. |
| D14 | **`AgentProvider` seam, Claude Code as the only implementation** (spawn argv, auth flow, attach/deep-link, workspace seeding, model/effort catalogs). `provider` column on repos and runs, default `claude-code`. Codex is [#2](https://git.cloonar.com/Cloonar/coding-lab/issues/2). | Adding Codex must be one new implementation, zero refactor. |
| D15 | **Incogni mode: full package** (§9). | Leak-proof by construction, not by hope. |
| D16 | **MVP boundary**: everything in this brief except the explicit roadmap (§14). GitHub tracker deferred to #1. | Agreed cut. |
| D17 | **Production bar: full** (§13): v0's test discipline + real git/tmux integration tests, Forgejo Actions CI, `slog` JSON logs, `/healthz` `/readyz` `/metrics`, ops/backup docs, CONTEXT.md + ADRs. | Operable, not just shippable. |

## 5. Architecture

```
┌─ SolidJS SPA (embedded static) ── PWA, SSE client
│
├─ HTTP server (net/http)
│   ├─ /api/v1/*      operator API (session cookie / PAT)
│   ├─ /agent/v1/*    agent API (scoped run tokens; labctl talks here)
│   ├─ /api/v1/events SSE stream
│   └─ /healthz /readyz /metrics
│
├─ Core services
│   ├─ RepoService        add(clone)/settings/remove; bare reference clones
│   ├─ CredentialVault    AES-256-GCM at rest; materialize-for-op; master key file
│   ├─ InstanceService    start/stop manual instances; deep-link capture
│   ├─ AFKEngine          scheduler + reaper + claims (ported from v0)
│   ├─ GitEngine          fetch/worktree/branch/teardown/sweep (ported from v0)
│   ├─ TrackerRegistry    Tracker interface → forgejo | builtin   (github: #1)
│   ├─ ProviderRegistry   AgentProvider interface → claude-code   (codex: #2)
│   ├─ SessionRunner      tmux wrapper (ported from v0) + prlimit cap
│   ├─ Seeder             trust, settings, skills bundle, CLAUDE.local.md, incogni
│   └─ Store              SQLite/Postgres repositories + goose migrations
│
└─ External: tmux · git · ssh · claude CLI · Forgejo REST · (GitHub REST #1)
```

Source-of-truth layering (load-bearing, from D6): **tmux** answers "is the session alive"; **git refs** answer "what is claimed / what work exists"; **the tracker** answers "is there a PR / is the issue open"; **the DB** stores configuration, credentials, the built-in tracker, and run *history*. Reconciliation (startup + throttled sweep) re-derives live state; the DB must never be the only witness to something the world can contradict.

## 6. Domain vocabulary (seed `CONTEXT.md` with these)

Carry over from v0: **instance**, **AFK run**, **reference repo** (now: the bare clone), **parked work / Parked view**, **claim** (= the run branch existing), **done-signal**, **guarded teardown** / **unguarded Discard**, **three-strikes pause**, **neutral Stop**. New terms: **change request** (lab-internal PR in the built-in tracker), **tracker binding** (forge | builtin), **provider**, **run token** (short-lived repo-scoped agent credential), **incogni mode**, **skills bundle**, **master key**. Reuse the exact definitions/avoid-lists from `reference/lab-v0/` ADRs and the vocabulary above; don't drift to synonyms.

## 7. Data model sketch

Final schema is yours to design; these entities and relationships are fixed:

- `users` (single row for now): id, username, password_hash (argon2id), created_at.
- `api_tokens`: id, user_id, name, token_hash, created_at, last_used_at.
- `run_tokens`: id, run_id, token_hash, expires_at (run lifetime + slack). Repo scope derived via run.
- `credentials`: id, name, kind (`ssh_key` | `https_token` | `forge_token`), encrypted_payload (kind-specific JSON: key+passphrase / username+token / host+token), created_at, updated_at.
- `repos`: id, name (sanitized, unique), remote_url, credential_id (nullable = public), tracker_binding (`forge`|`builtin`), forge_kind (detected: `forgejo`|`github`|`none`), default_branch, provider (default `claude-code`), model/effort defaults (nullable → global), incogni (bool), git_author_name/email (nullable → global), afk_branch_pattern (default `afk/<N>`; incogni default `issue-<N>`), manual_branch_prefix (default `lab/`; incogni default `wip/`), afk_auto_enabled, consecutive_failures, budget/cap overrides (nullable), created_at, last_opened_at.
- `runs` (all instances, manual and AFK): id, repo_id, kind (`manual`|`afk_manual`|`afk_auto`), provider, issue_number (nullable), branch, worktree_path, session_name, model, effort, deep_link_url (nullable), started_at, budget_deadline (persisted clock, D12b), ended_at, outcome (`active`|`success`|`death`|`timeout`|`stopped`), failure_reason.
- Built-in tracker: `issues` (repo_id, number per-repo sequence, title, body md, state, timestamps), `issue_comments` (author = operator or run ref), `labels` (repo-scoped name/color/description; seed the five triage labels per repo: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`) + `issue_labels`, `change_requests` (repo_id, number, title, body, head_branch, base_branch, state `open`|`merged`|`closed`, closes-issue refs, merged_at, merge_commit).
- `settings`: global config that is runtime-mutable (spawn defaults, global cap, budgets/intervals).

## 8. Surfaces

### 8.1 Operator API (`/api/v1`, cookie or PAT auth, CSRF-guarded)

Auth: `POST /auth/setup` (only while `users` empty — first-run page), `POST /auth/login`, `POST /auth/logout`, `GET /me`. Tokens: CRUD `/tokens`.
Credentials: CRUD `/credentials` (no secret readback; delete 409s while referenced).
Repos: `GET|POST /repos`, `GET|PATCH|DELETE /repos/{id}` (add → async clone job with SSE progress; delete guarded by the teardown rule across all its worktrees, with an explicit force).
Instances: `POST /repos/{id}/instances` {label?, model?, effort?}, `GET /instances`, `DELETE /instances/{session}`, `POST /repos/{id}/stop-all`.
AFK: `POST /repos/{id}/afk/start`, `PUT /repos/{id}/afk/auto` {enabled}, `POST /repos/{id}/afk/reset`.
Parked: `GET /repos/{id}/parked`, `POST /repos/{id}/parked/discard`.
Built-in tracker: issues + comments + labels CRUD under `/repos/{id}/issues...`; change requests `GET /repos/{id}/crs`, `GET /repos/{id}/crs/{n}` (incl. diff), `POST /repos/{id}/crs/{n}/merge`, `POST .../close`.
Runs: `GET /runs?repo=...` (history).
Provider: `GET /providers/claude/auth/status`, `POST /providers/claude/auth/login/start`, `POST /providers/claude/auth/login/code`.
Settings: `GET|PATCH /settings`.
Events: `GET /events` (SSE: `repo.changed`, `run.changed`, `parked.changed`, `clone.progress`, `claude.auth.changed`, `issue.changed`, `cr.changed`, `run.messages.changed`).

### 8.2 Agent API (`/agent/v1`, run-token auth)

`GET /issue` (the run's claimed issue, with comments) · `GET /issues/{n}` · `POST /issues` {title, body, labels} · `PATCH /issues/{n}` {title?, body?} (edit title/body; empty or whitespace-only title 400) · `POST /issues/{n}/comments` · `POST /issues/{n}/labels` {labels} · `DELETE /issues/{n}/labels` {labels} · `POST /issues/{n}/close` · `GET /labels` · `POST /labels` {name, color?, description?} (idempotent ensure) · `POST /prs` {title, body} (routes to forge PR or built-in CR; server injects/validates `Closes #N`) · `GET /prs/{n}/checks` ({state, checks:[…]} — the aggregate CI verdict plus per-check rows, ADR-0032) · `GET /prs/{n}` also carries a `reviews:[{reviewer, state, body, dismissed}]` array (always present, `[]` when none — the reject → re-queue read) · `POST /prs/{n}/reject` {body} (post a changes-requested review; body required — the findings; answers {number, state}) · `POST /prs/{n}/approve` {body?} (post an approving review; body optional; answers {number, state}) · `POST /prs/{n}/rerequest` (re-request review from every reviewer whose latest verdict requested changes; no such reviewer is a no-op success; answers {number}) · `POST /prs/{n}/comments` {body} (plain PR discussion comment; body required; no `Closes #N` injection; answers {number}). A forge refusal of a review write (approving one's own PR, a re-request the forge declines) surfaces verbatim as a 409 — the merge-refusal twin; the built-in binding, having no forge review model, answers 409 for the four write verbs. **Incogni sanitization happens here, on every agent-authored body** — issue, comment, PR/CR. Label names resolve strictly: an unknown name is a 400, never an implicit create.

### 8.3 `labctl`

Reads `LAB_URL`/`LAB_TOKEN` from env. Commands: `labctl issue view [n]`, `labctl issue list` (number, state, created-at, labels, title), `labctl issue create --title … --body … [--labels a,b]`, `labctl issue edit <n> [--title …] [--body …]` (at least one flag required; empty title errors, empty body clears it), `labctl issue comment <n> <body>`, `labctl issue label add <n> <labels>`, `labctl issue label remove <n> <labels>`, `labctl issue close <n>`, `labctl label list`, `labctl label create --name … [--color … --description …]` (idempotent), `labctl pr create --title … --body …`, `labctl pr view <n>`, `labctl pr list`, `labctl pr merge <n>` (fixed method; the forge/base enforces mergeability, a refusal surfaces verbatim), `labctl pr checks <n> [--wait]` (aggregate + per-check rows; `--wait` polls client-side ~10s up to a ~5 min cap; exit 0 green/none · 2 red · 3 pending, ADR-0032), `labctl pr reject <n> <body>` (post a changes-requested review; body required), `labctl pr approve <n> [body]` (post an approving review; body optional), `labctl pr rerequest <n>` (re-request review from every reviewer whose latest verdict requested changes; no such reviewer is a no-op success), `labctl pr comment <n> <body>` (plain PR discussion comment; no `Closes #N` injection). `labctl pr view <n>` also renders each submitted review after the body, under a `--- review by <reviewer> (<state>)` separator (`, dismissed` appended for a dismissed review); no reviews leaves the output unchanged. Plain, parseable output; exit codes meaningful (0 success · 1 API/HTTP error · 2 usage/configuration error). Ship as a second small binary from the same module, present on every session's PATH.

### 8.4 AFK seed prompt (template — adapt v0's `afkSeedPrompt`)

1. Run `labctl issue view <N>` and read it fully, including comments.
2. Work only on branch `<branch>` in this worktree; never switch branches.
3. Implement the issue completely, following the repository's own conventions (CLAUDE.md / AGENTS.md).
4. Run the project's tests, build, and linters; fix what you break.
5. Commit in Conventional Commits style.<incogni: no AI attribution, no Co-Authored-By, no generated-with footers anywhere.>
6. `git push -u origin <branch>`.
7. `labctl pr create --title "…" --body "…"` — the body must include `Closes #<N>`.
8. Then stop working. Do not start unrelated work.

### 8.5 Configuration (flags/env, all documented)

`--addr` (`:8080`) · `--state-dir` (`~/.local/state/lab`) · `--db` (default `sqlite:<state-dir>/lab.db`; `postgres://…` switches backend) · `--master-key-file` (default `<state-dir>/master.key`, auto-generated; **configurable for sops-nix / LoadCredential**, D9) · binary paths `--claude --tmux --git --prlimit` (PATH lookup default) · `--max-instances` (6) · `--session-nofile` (16384) · proxy auth: `--proxy-auth`, `--proxy-auth-header` (`Remote-User`), `--trusted-proxies` (CIDRs) · `--base-url` (for deep links in notifications later). Runtime-mutable knobs (spawn defaults, budgets, intervals) live in `settings`.

State dir layout: `lab.db` · `master.key` · `repos/<id>.git/` (bare clones) · `worktrees/<repo>-<label>/` · `runtime/` (0700; materialized key files, per-repo `known_hosts`).

## 9. Behavioral specifications

**Port targets** (file map in the reference README): worktree lifecycle & naming (`<repo>~<label>` sessions, `-` joined worktree dirs — never `~` in paths, the Windows-8.3 pattern stalls Claude; timestamp labels with same-minute bump), synchronous fail-loud Start with rollback, the guarded teardown rule verbatim, startup reconciliation + throttled merged-sweep + the starting-set race guard, AFK classification/scheduler/reaper semantics, restart re-adoption (now: reconcile `runs.outcome=active` against live tmux sessions; adopt or mark dead), deep-link capture with generic-link fallback, Claude login flow (20s code timeout, 30s status cache, force-refresh before spawn), trust seeding (preserve unknown JSON keys, atomic writes).

**Git auth (new)**: clones/fetches/pushes run with `GIT_SSH_COMMAND="ssh -i <materialized-key> -o IdentitiesOnly=yes -o UserKnownHostsFile=<state>/runtime/known_hosts -o StrictHostKeyChecking=accept-new"` or `GIT_ASKPASS` for HTTPS tokens; the same env is set on spawned sessions so the agent's own push uses the repo's credential. Key files are removed when the operation/session ends.

**Change-request merge (new)**: diff = `git diff merge-base(default, head)...head` in the bare repo. Merge = fast-forward when possible (`git push origin <sha>:refs/heads/<default>` after ancestry check), else a merge commit built in a temporary worktree, pushed, then CR marked merged and `Closes #N` issues closed. Surface push rejections (protected branches) as errors.

**Incogni mode (D15)** — per-repo flag; when set, all seven measures apply:
1. Worktree `.claude/settings.local.json` seeded with attribution disabled (Co-Authored-By and PR attribution off — verify the current settings keys against the installed Claude Code version).
2. Seed prompts instruct plain professional commits/PR bodies, no AI mentions.
3. The agent API strips known attribution footers/trailers from PR/CR bodies (defense in depth).
4. Neutral branch names from the repo's patterns (`issue-<N>`, `wip/<label>`); claim parsing uses the repo's configured pattern — nothing may assume the literal `afk/` prefix outside per-repo config defaults.
5. Commits authored with the repo's configured real git identity, never a bot identity.
6. Everything lab seeds into a worktree is listed in `.git/info/exclude` (never in `.gitignore`).
7. A **pre-push hook installed in the bare reference repo** (shared by its worktrees) scans outgoing commits and rejects pushes containing `Co-Authored-By: Claude`, generated-with footers, or seeded-file paths.
Operator note for the docs: incogni cannot hide the forge account identity of the token used, nor statistical style/timing signals — state this honestly.

**Skills seeding (D13)**: on every spawn, copy the embedded bundle into `<worktree>/.claude/skills/`, generate `CLAUDE.local.md` (tracker binding, labctl vocabulary, triage-label table, supersedes-tea note), extend `.git/info/exclude`. Same mechanism regardless of incogni (incogni only adds measures 1–7).

## 10. Security requirements

Argon2id for passwords; token storage as hashes only; login rate-limiting; session cookies `Secure`/`HttpOnly`/`SameSite=Strict`; CSRF: mutating API calls require a custom header (SPA sets it) and Origin validation; proxy-auth mode only trusts the header when the peer is in `--trusted-proxies`; run tokens expire with their run and authorize only that repo's tracker surface; secrets never logged, never in URLs, never readable back through the API; master key file must be 0600 (refuse to start otherwise); encrypted payloads use AES-256-GCM with per-secret nonces.

## 11. Known-fragile couplings — verify in M3 against the installed Claude Code

1. `claude --remote-control` argv shape (v0 passes the session name as its argument) and `--permission-mode auto`.
2. Deep-link capture: `~/.claude/sessions/<pid>.json` registry, cwd matching, `cse_` → `session_` normalization, `https://claude.ai/code/<id>` URL shape. Has broken before (pane-scrape era); build the fallback (generic `claude.ai/code` link) and a loud log when capture fails.
3. `claude auth login --claudeai` flow and the OAuth URL regex; `claude auth status --json` parsing (stdout regardless of exit code).
4. Trust/attribution settings keys in `~/.claude.json` and `.claude/settings.local.json` (`hasTrustDialogAccepted`, `enableAllProjectMcpServers`, attribution/Co-Authored-By keys).
5. Model/effort allowlists (v0: `opus[1m]|sonnet|fable|haiku` × `low…max`) — make them per-provider config with sane defaults, not constants.
Write a small `internal/compat` doc or test that pins what was verified and against which Claude Code version.

## 12. Milestones (each ends: tests green, CI green, acceptance demonstrated)

- **M1 — Walking skeleton.** Flake builds Go+SPA (embedded); NixOS module; first-run setup page → admin login (password + sessions + CSRF); proxy-auth mode; SQLite + Postgres wiring with goose migrations; `/healthz` `/readyz`; slog JSON; SSE endpoint with a heartbeat event; Forgejo Actions CI (lint, Go tests, SPA tests, `nix flake check`). *Accept: fresh VM → module enabled → browse, set admin password, log in, empty dashboard live-updates.*
- **M2 — Credentials + repos.** Vault (AES-GCM, key file bootstrap/config); credentials CRUD UI; add-repo flow (URL + credential → async bare clone with SSE progress, forge_kind detection, default-branch detection); repo settings UI; guarded repo removal. *Accept: add SSH-cloned private Forgejo repo and an HTTPS+token repo from the phone UI.*
- **M3 — Sessions core.** Claude login flow in UI; manual instances end to end: worktree add (fail-loud, rollback) → seeding → tmux spawn with prlimit → deep-link capture → Open/Stop; guarded teardown; Parked view + Discard; startup re-adoption + reconciliation + sweeps; run history rows for manual runs; **fragile-couplings verification (§11) recorded**. *Accept: two concurrent instances on one repo, service restart in between, both re-adopted; stop-clean removes, stop-dirty parks.*
- **M4 — Trackers + built-in issues.** `Tracker` interface; Forgejo REST client (ready queue by label, issue read/comments, PR list by head, PR create); built-in tracker issues/labels/comments with phone-first UI; per-repo tracker binding; five triage labels seeded. *Accept: browse/create/label issues on a builtin-bound repo from the phone; Forgejo repo shows its ready-for-agent queue.*
- **M5 — AFK engine + labctl.** Run tokens; agent API; `labctl`; seed prompt; ported scheduler/reaper/claim/budget (persisted) /three-strikes/neutral-Stop; PATs UI; run history UI with outcomes. *Accept: a real `ready-for-agent` issue on a Forgejo repo is claimed, resolved, PR opened, run reaped as success; a doomed run times out, three failures pause the repo, Reset re-arms.*
- **M6 — Change requests.** `labctl pr create` on builtin-bound repos creates a CR; CR list/detail with diff; merge (ff / merge-commit) + push; `Closes #N` closes built-in issues; reaper treats CR-with-head-branch as the done-signal. *Accept: full AFK cycle on a repo with no forge tracker, reviewed and merged entirely from the lab UI.*
- **M7 — Incogni + skills.** All seven incogni measures incl. the pre-push guard (test: a poisoned commit is rejected); skills bundle embedding + per-worktree seeding + `CLAUDE.local.md`; per-repo branch-pattern config live. *Accept: on an incogni repo, a completed AFK run's remote shows neutral branch, clean commits, sanitized PR body, and `git log`/diff contain zero AI markers; seeded files never appear in `git status`.*
- **M8 — Hardening + polish.** `/metrics` (instances gauge, AFK outcomes/durations, tracker request counters, clone jobs); PWA manifest/icons/offline shell; Playwright smoke (login → add repo → start instance); README + `CONTEXT.md` + ADRs + ops doc (backup/restore: db + master key + bare repos; state-dir layout; NixOS + bare-metal deploy); final pass that every §8.5 knob works. *Accept: definition of done below.*

## 13. Production bar (D17)

Testing: table-driven tests on every ported pure decision function (teardown, classification, scheduling, claim selection — keep them pure, clock-injected); fakes for Tracker/Git/SessionRunner/Provider; integration tests against **real git and real tmux** (provided in flake checks and CI, as v0 does); `httptest` API tests on in-memory SQLite; vitest for SPA logic; one Playwright smoke. CI: Forgejo Actions on PRs + main — golangci-lint, eslint/prettier, both test suites, `nix flake check`. Observability: slog JSON, health/readiness, Prometheus metrics. Ops: migrations on startup, graceful shutdown (sessions survive by design), documented backup surface.

## 14. Non-goals now / explicit roadmap (architecture must not block these)

Embedded remote interface (chat/terminal in lab's UI; the API+SSE+tmux choices are its foundation) · Codex provider (**#2**) · GitHub tracker (**#1**) · Web Push notifications (PWA groundwork exists) · passkeys · multi-user · OCI image · configurable skills source · MySQL. Nothing beyond this list without an ADR.

## 15. Definition of done (MVP)

- [ ] Fresh NixOS host: enable module (with sops-provided master key), set admin password, log in from a phone.
- [ ] Add credentials and repos (SSH + HTTPS) entirely via UI; clone progress visible live.
- [ ] Claude login via UI; manual instance → claude.ai deep link on the phone; Stop honors the guarded rule; Parked works.
- [ ] AFK: Forgejo-tracked repo and builtin-tracked repo each complete the full issue→PR/CR→reap cycle; three-strikes + Reset verified.
- [ ] CR review + merge from the phone.
- [ ] Incogni repo leaks nothing (automated test + manual `git log` inspection).
- [ ] Service restart mid-run: sessions survive, runs re-adopted, budgets intact.
- [ ] SQLite and Postgres both pass the integration suite.
- [ ] CI green; `nix flake check` green; metrics visible; docs complete (README, CONTEXT.md, ADRs, ops).
