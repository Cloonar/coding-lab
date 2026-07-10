# lab

`lab` is a self-hosted server with a phone-first web interface for managing remote coding agents against git repositories. The operator adds repositories and credentials in the UI, then starts **manual instances** (interactive `claude --remote-control` sessions reached via claude.ai deep links) or **AFK runs** (unattended sessions that resolve one `ready-for-agent` issue and open a PR) — from any device. Repos without a usable forge tracker get a built-in issue tracker with lab-internal change requests, reviewable and mergeable from the phone.

This is the production rewrite of the `lab` prototype vendored read-only at [`docs/reference/lab-v0/`](docs/reference/lab-v0/README.md), which serves as the behavioral specification for the session/worktree/AFK core.

## Status

All eight milestones are shipped.

| Milestone | Scope | Status |
|---|---|---|
| M1 | Walking skeleton: flake (Go + embedded SPA), NixOS module, admin auth + sessions + CSRF, proxy-auth, SQLite/Postgres store + goose migrations, healthz/readyz, SSE heartbeat, CI | **shipped** |
| M2 | Credentials vault (AES-256-GCM, master key file) + repos: async bare clones with SSE progress, forge detection, guarded removal | **shipped** |
| M3 | Sessions core: manual instances, deep-link capture, guarded teardown, Parked view + Discard, startup re-adoption, fragile-couplings pinned against Claude Code 2.1.198 ([`internal/compat/compat.md`](internal/compat/compat.md)) | **shipped** |
| M4 | Trackers: `Tracker` seam, Forgejo REST client, built-in issues/labels/comments, per-repo tracker binding, five triage labels seeded | **shipped** |
| M5 | AFK engine + `labctl`: run tokens, agent API, scheduler/reaper/claims, persisted budget clock, three-strikes pause, PATs | **shipped** |
| M6 | Change requests: CR create/diff/merge (ff or merge commit), `Closes #N`, CR as done-signal | **shipped** |
| M7 | Incogni mode (all seven measures incl. the pre-push guard) + skills bundle seeding + `CLAUDE.local.md` | **shipped** |
| M8 | Hardening: `/metrics`, PWA, Playwright smoke, full ops docs, definition-of-done pass | **shipped** |

Verification status per definition-of-done item — including what is automated versus what needs a real host — is tracked in [`docs/definition-of-done.md`](docs/definition-of-done.md).

## Architecture

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
│   ├─ ProviderRegistry   AgentProvider interface → claude-code | codex
│   ├─ SessionRunner      tmux wrapper (ported from v0) + prlimit cap
│   ├─ Seeder             trust, settings, skills bundle, CLAUDE.local.md, incogni
│   └─ Store              SQLite/Postgres repositories + goose migrations
│
└─ External: tmux · git · ssh · claude CLI · Forgejo REST · (GitHub REST #1)
```

Source-of-truth layering (load-bearing): **tmux** answers "is the session alive"; **git refs** answer "what is claimed / what work exists"; **the tracker** answers "is there a PR / is the issue open"; **the DB** stores configuration, credentials, the built-in tracker, and run *history*. Reconciliation (startup + throttled sweep) re-derives live state; the DB is never the only witness to something the world can contradict.

## Dev quickstart

```sh
nix develop        # go, gopls, golangci-lint, node, git, tmux, util-linux, sqlite
make lab           # build SPA + server binary with embedded UI → bin/lab
make labctl        # agent-side CLI → bin/labctl
make test          # go test ./...
make lint          # golangci-lint run
```

Web dev loop (no embedding, live reload):

```sh
go run ./cmd/lab        # API on :8080
cd web && npm run dev   # Vite dev server; proxies /api and /healthz → :8080
```

`nix flake check` is the CI truth: package builds (which carry the Go test suite against real git/tmux/prlimit and the SPA vitest suite), golangci-lint, and an eval-proven NixOS module with its unit invariants asserted.

## Surfaces at a glance

**Operator API** (`/api/v1`, session cookie or `lab_pat_…` bearer token, CSRF-guarded for ambient auth): first-run setup + login, PAT CRUD, credentials CRUD (no secret readback; delete 409s while referenced), repos (add → async bare clone with SSE progress, settings PATCH, guarded delete, clone retry), instances (start/stop/stop-all), Parked list + Discard, AFK (start / auto toggle / three-strikes reset), built-in issues + comments + labels + ready queue, change requests (list, detail with live diff, merge, close), run history, provider catalog + Claude auth (status / login start / login code), runtime settings, and `GET /api/v1/events` (SSE: `repo.changed`, `run.changed`, `parked.changed`, `clone.progress`, `claude.auth.changed`, `issue.changed`, `cr.changed`, `heartbeat`).

**Agent API** (`/agent/v1`, run-token auth, scoped to the run's repo): issue view (the run's claimed issue) / list / create / comment / close, label add/remove/list, idempotent label create, PR create — routes everything to the repo's tracker binding (forge or built-in), injects/validates `Closes #N` on PR create, and applies incogni sanitization server-side to every agent-authored body.

**`labctl`** (on every session's PATH; reads `LAB_URL`/`LAB_TOKEN` from the session env):

```
labctl issue view [n]                 show the run's claimed issue (or issue n), with comments
labctl issue list                     list open issues (number, state, created, labels, title)
labctl issue create --title T --body B [--labels a,b]
                                      file a new issue, labels attached at creation
labctl issue comment <n> <body>       comment on issue n
labctl issue label add <n> <a,b>      add labels (comma-separated) to issue n
labctl issue label remove <n> <a,b>   remove labels from issue n
labctl issue close <n>                close issue n (comment the reason first)
labctl label list                     list the repo's labels (name, color, description)
labctl label create --name N [--color C --description D]
                                      create the label if missing (idempotent)
labctl pr create --title T --body B   open a PR/CR for the current branch
```

**Probes**: `/healthz` (liveness, always 200), `/readyz` (503 while the DB is unreachable), `/metrics` (Prometheus). All three are mounted outside auth.

## Layout

```
cmd/lab, cmd/labctl    binaries (module git.cloonar.com/Cloonar/coding-lab)
internal/              config, store, vault, gitx, tmuxx, startguard, provider,
                       tracker, afk, instance, reconcile, seeder, events,
                       httpapi, agentapi, metrics, webui, labctl, compat, …
web/                   SolidJS SPA (Vite, TypeScript, vitest)
migrations/            goose migrations, sqlite + postgres (parity-tested)
assets/skills/         vendored skills bundle, embedded and seeded per worktree
nix/, flake.nix        packages, NixOS module, devshell, checks
docs/                  brief, ADRs, ops, definition of done, read-only v0 reference
```

## Documentation

- [`CONTEXT.md`](CONTEXT.md) — the domain glossary; identifiers and UI copy use these terms verbatim.
- [`docs/adr/`](docs/adr/) — decisions 0001–0013 for this rewrite (repo/language, nix, store layering, auth, SPA/API, vault, repos, sessions/provider seam, tracker seam, AFK port, CR merge, incogni/skills, hardening/metrics/PWA).
- [`docs/agent-brief.md`](docs/agent-brief.md) — the product contract (decision log D1–D17, milestones, production bar).
- [`docs/ops.md`](docs/ops.md) — deployment (NixOS + bare metal), configuration reference, state-dir layout, backup/restore, CI runner prerequisites, observability.
- [`docs/definition-of-done.md`](docs/definition-of-done.md) — the brief §15 checklist with per-item verification.
- [`docs/reference/lab-v0/`](docs/reference/lab-v0/README.md) — the v0 prototype, **read-only**: behavioral spec and ADR rationale.
