# Greenfield Go rewrite in its own repo, single-operator and product-shaped

`lab` v0 grew inside cloonar-nixos as a vendored module (reference ADR-0004, then ADR-0020): ~11k lines of stdlib-only Go whose only gate was "does the derivation build" — no CI as Go code, and every edit dry-built the whole fleet at commit time. ADR-0020 accepted that cost rather than solving it, and named the exit condition in advance. The production version takes that exit: a greenfield rewrite in its own repository, `git.cloonar.com/Cloonar/coding-lab`, consumed by cloonar-nixos as a flake input (D1).

The backend stays Go, current stable. `lab` is an orchestrator — it shells out to `git`, `tmux`, and `claude`; its own overhead must stay negligible next to the agent processes it manages (target: idle RSS in the tens of MB, one static binary). Go gives the lowest footprint per capability for that workload, and the v0 decision logic ports nearly 1:1. v0 itself is vendored read-only at `docs/reference/lab-v0/` as the **behavioral specification**: its `*_test.go` tables are the most precise statement of intended behavior, and ported pure functions are transcribed from them, not re-derived.

The product shape is **single-operator** (D2): one admin user, a self-contained deployable with its own auth and its own storage. The schema leaves room for multi-user later — a `users` table with one row, user FKs where cheap — but there is no tenancy logic anywhere. Claude auth is per-OS-user, so true multi-user would need OS or container isolation per user; that is out of scope by decision, not omission.

## Status

Accepted. Implements D1 and D2 of the brief (`docs/agent-brief.md` §4).

## Considered options

- **Keep evolving the vendored module in cloonar-nixos.** Rejected: ADR-0020 accepted (not solved) the fleet-wide dry-build cost, and the Go code had no CI, no lint, no test gate of its own. The production bar (D17 — real git/tmux integration tests, Forgejo Actions, observability, docs) requires a first-class repo. ADR-0004's own exit condition ("if `lab` ever grows to be useful on another machine…") was met at two hosts already.
- **Another language/runtime.** Rejected: v0 is Go and its decision logic ports nearly 1:1; anything else re-derives debugged behavior for zero footprint gain. Go's static binary keeps deployment a single artifact.
- **Multi-user tenancy now.** Rejected: Claude auth is machine-level (per OS user), so real isolation needs OS/container boundaries — a different product. Product-shaped single-operator keeps the door open (users table, user FKs) without paying for tenancy logic nobody uses.
- **Incremental refactor of v0 in place.** Rejected: v0's storage (JSON store), UI (html/template + polling), and tracker surface (`tea` shellouts) are all replaced by decision (D5, D6, D10); a rewrite against the frozen reference is cheaper than a migration through it.

## Consequences

- v0 stays frozen and read-only at `docs/reference/lab-v0/`; where ported behavior is in question, its test tables win.
- This repo carries the full production bar from day one: Forgejo Actions CI, `nix flake check` as local truth, slog JSON, health/readiness/metrics endpoints, ops and backup docs.
- One admin user; PATs and trusted-proxy auth exist (ADR-0004), but there is no roles or permissions model.
- cloonar-nixos consumes `nixosModules.lab` from this repo's flake (ADR-0002); the vendored copy in cloonar-nixos is retired.
