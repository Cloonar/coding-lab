# Contributing

Issues and pull requests live on GitHub: [github.com/Cloonar/coding-lab](https://github.com/Cloonar/coding-lab). A PR that resolves an issue references it with `Closes #N` in the body.

The project is licensed under the [GNU AGPL-3.0](LICENSE); by contributing you agree that your contributions are licensed under the same terms.

## Dev environment

The devshell has everything:

```sh
nix develop        # go, gopls, golangci-lint, node, git, tmux, util-linux, sqlite, mkdocs
```

Without nix you need: Go 1.26, Node 24, golangci-lint, and `git`, `tmux`, `prlimit` (util-linux) on PATH — the Go suite shells out to the real binaries.

The docs site (ADR-0066) renders from this same checkout: `mkdocs serve` previews it live, and `nix develop .#docs --command mkdocs build --strict` is exactly what CI runs — `.#docs` is a lean shell holding only the mkdocs toolchain.

## Build, test, lint

```sh
make lab           # build SPA + server binary with embedded UI → bin/lab
make labctl        # agent-side CLI → bin/labctl
make test          # go test ./...
make lint          # golangci-lint run
cd web && npm test # SPA unit tests (vitest)
```

Web dev loop (no embedding, live reload):

```sh
go run ./cmd/lab        # API on :8080
cd web && npm run dev   # Vite dev server; proxies /api and /healthz → :8080
```

The Go tests are integration-heavy by design: they run against real git repos, real tmux on a private socket, and real `prlimit` — not mocks. The store suite runs on SQLite always and additionally against a real PostgreSQL when `LAB_TEST_POSTGRES_DSN` is set.

`nix flake check` is the authoritative gate: package builds (which carry the Go test suite and the SPA vitest suite), golangci-lint, and an eval-proven NixOS module with its unit invariants asserted.

## CI

Two GitHub Actions gates run on pull requests (ADR-0023), both required checks:

- **native** (`.github/workflows/ci.yml`) — runs on every PR: SPA eslint + prettier + vitest + `vite build`, the `ui`-tagged Go build and test suite, and golangci-lint. Typically 2–4 min.
- **flake-check** (`.github/workflows/ci-nix.yml`) — the full `nix flake check`, path-gated to nix and Go-dependency changes (`**/*.nix`, `flake.lock`, `go.mod`, `go.sum`). A dependency bump must go through it: it revalidates `nix/package.nix`'s `vendorHash`, which the native gate cannot catch. On a PR touching none of those paths the same check name is reported by a no-op twin (`.github/workflows/ci-nix-noop.yml`) instead, so a required check never hangs a PR at "Expected".

Two further path-gated workflows are **not** required checks. `agent-tools.yml` builds and smoke-tests the agent-tools OCI images when `containers/**` changes. `docs.yml` builds the documentation site with `mkdocs build --strict` when `docs/**`, `mkdocs.yml`, `mkdocs_hooks.py`, the three root markdown files it injects, or the flake files change — `--strict` makes a broken internal link or a missing anchor fail the PR. On a `v*` tag it publishes the site to GitHub Pages ([cloonar.github.io/coding-lab](https://cloonar.github.io/coding-lab/)); merging to main does not (ADR-0066).

## Conventions

- **Conventional Commits** — one commit per coherent change (`feat(web): …`, `fix(afk): …`, `refactor: …`).
- **Vocabulary** — [`CONTEXT.md`](CONTEXT.md) is the domain glossary. Identifiers, UI copy, issue titles, and docs use its terms verbatim; each entry lists synonyms to avoid. If the concept you need isn't in the glossary, that's a signal — don't invent parallel language.
- **ADRs** — significant design decisions are recorded in [`docs/adr/`](docs/adr/), one decision per file, in the established style. If your change contradicts an existing ADR, surface that in the PR rather than silently overriding it.
- **Tests are the spec** — behavior changes come with test changes; the decision tables in `*_test.go` files are the most precise statement of intended behavior.
- **Fragile couplings** — anything that depends on a provider CLI's observed behavior (transcript format, session registry, keystroke recipes) is pinned in [`internal/compat/compat.md`](internal/compat/compat.md) with fixtures. If you touch that area, update the compat doc and its fixtures together.
- **Production bar** — [`docs/definition-of-done.md`](docs/definition-of-done.md) tracks what is verified automatically vs. what needs a real host; keep it honest when you add or change behavior it covers.

## For coding agents

This repo is routinely developed by coding agents (via lab itself). [`docs/agents/`](docs/agents/) holds the agent-facing conventions: issue-tracker usage, the triage-label state machine, how to consume the domain docs, and how to author a new provider adapter.
