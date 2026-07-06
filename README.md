# lab

`lab` is a self-hosted server with a phone-first web interface for managing remote coding agents against git repositories. The operator adds repositories and credentials in the UI, then starts **manual instances** (interactive `claude --remote-control` sessions reached via claude.ai deep links) or **AFK runs** (unattended sessions that resolve one `ready-for-agent` issue and open a PR) — from any device. Repos without a usable forge tracker get a built-in issue tracker with lab-internal change requests, reviewable and mergeable from the phone.

This is the production rewrite of the `lab` prototype vendored read-only at [`docs/reference/lab-v0/`](docs/reference/lab-v0/README.md), which serves as the behavioral specification for the session/worktree/AFK core.

## Status

| Milestone | Scope | Status |
|---|---|---|
| M1 | Walking skeleton: flake (Go + embedded SPA), NixOS module, admin auth + sessions + CSRF, proxy-auth, SQLite/Postgres store + migrations, healthz/readyz, SSE heartbeat, CI | **in progress** |
| M2 | Credentials vault + repos (async bare clones, forge detection, guarded removal) | pending |
| M3 | Sessions core: manual instances, deep links, guarded teardown, Parked view, re-adoption, fragile-couplings verification | pending |
| M4 | Trackers: Forgejo REST + built-in issues/labels/comments, per-repo binding | pending |
| M5 | AFK engine + `labctl`: run tokens, agent API, scheduler/reaper/claims, three-strikes | pending |
| M6 | Change requests: CR create/diff/merge, `Closes #N`, CR as done-signal | pending |
| M7 | Incogni mode (all seven measures) + skills bundle seeding | pending |
| M8 | Hardening: metrics, PWA, Playwright smoke, full ops docs | pending |

## Dev quickstart

```sh
nix develop        # go, gopls, golangci-lint, node, tmux, util-linux, sqlite
make lab           # build SPA + server binary with embedded UI → bin/lab
make labctl        # agent-side CLI → bin/labctl
make test          # go test ./...
make lint          # golangci-lint run
```

Web dev loop (no embedding, live reload):

```sh
go run ./cmd/lab   # API on :8080
cd web && npm run dev   # Vite dev server; proxies /api and /healthz → :8080
```

`nix flake check` runs the same suite CI runs (package builds, Go tests against real git/tmux, lint, SPA tests).

## Layout

```
cmd/lab, cmd/labctl    binaries (module git.cloonar.com/Cloonar/coding-lab)
internal/              config, store, gitx, tmuxx, afk, tracker, provider, httpapi, …
web/                   SolidJS SPA (Vite, TypeScript)
migrations/            goose migrations, sqlite + postgres (parity-tested)
assets/skills/         vendored skills bundle, embedded and seeded per worktree
nix/, flake.nix        packages, NixOS module, devshell, checks
docs/                  brief, ADRs, ops, read-only v0 reference
```

## Documentation

- [`CONTEXT.md`](CONTEXT.md) — the domain glossary; identifiers and UI copy use these terms verbatim.
- [`docs/adr/`](docs/adr/) — decisions for this rewrite (repo/language, nix, store, auth, SPA/API).
- [`docs/agent-brief.md`](docs/agent-brief.md) — the product contract (decision log D1–D17, milestones, production bar).
- [`docs/ops.md`](docs/ops.md) — deployment, CI runner prerequisites, backup surface.
- [`docs/reference/lab-v0/`](docs/reference/lab-v0/README.md) — the v0 prototype, **read-only**: behavioral spec and ADR rationale.
