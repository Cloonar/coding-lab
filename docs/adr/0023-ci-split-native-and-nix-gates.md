# CI split: a fast native gate on every PR, the hermetic `nix flake check` gate only on nix/dep changes

ADR-0002 made `nix flake check` the single CI gate — "local `nix flake check` == CI (the CI workflow is essentially that one command)" — so local green == CI green, one command, one truth. That property is worth keeping, but it has a cost that only showed up under load: measured against the Forgejo Actions API over the last ~10 runs, the PR `flake-check` job takes **~12–13 min** (16m35s on a failure). The Determinate installer is only ~1.5–2 min of that (the `bump-nixos-pin` deploy job runs the *same* installer and early-exits at ~2 min); the other **~10 min is `nix flake check` realizing the whole closure stone-cold every run**. The Docker-backed runner is ephemeral, `/nix` is wiped per job, so the Go toolchain + node + golangci-lint closures (hundreds of MB from cache.nixos.org), the ~160 MB npm deps, and the Go vendor modules are re-fetched and rebuilt on every PR. Merely "putting nix in an image" would recover only the ~2 min install, not the ~10 min cold store.

This ADR splits the gate so the common path skips nix entirely. It **relaxes ADR-0002's single-gate rule on the common path**; the hermetic gate stays the authority, just not on every PR.

**Two workflows.**

- **`.forgejo/workflows/ci.yml` → a fast native gate.** `on: pull_request`, no path filter — it always runs and is the **required** status check. It builds and tests directly on the stock runner with `actions/setup-go` + `actions/setup-node` caching (no nix), mirroring the flake's non-nix checks: build the SPA and run eslint + prettier + vitest, copy `web/dist` → `internal/webui/dist` (the `go:embed` target, same as `make build-ui`), `go build -tags ui`, `go test -tags ui ./...`, then `golangci-lint run ./...` **untagged**, `CGO_ENABLED=0` throughout. The job id is **`native`** and kept stable so the required-check string is deterministic. Common-path PRs drop from ~12–13 min to ~2–4 min.
- **`.forgejo/workflows/ci-nix.yml` → the existing `nix flake check -L`.** Same in-job Determinate install as before, but `on: pull_request` with a `paths:` filter so it runs only when nix or the Go dependency set changes. It stays authoritative and is *also* the merge-to-main backstop: `deploy.yml`'s `bump-nixos-pin` job rebuilds the full NixOS configuration with the new pin (`test-configuration` → `packages.lab`) before pushing, so nothing reaches the fleet without a hermetic build.

**The `paths:` filter for the nix gate** (binding):

```
**/*.nix
flake.lock
go.mod
go.sum
```

The only *intrinsically*-nix check is `nixos-module` (it greps the systemd unit built from `nix/module.nix`), so `**/*.nix` and `flake.lock` are the obvious triggers. The **load-bearing** entries are `go.mod`/`go.sum`. `nix/package.nix` pins `vendorHash`, a fixed-output hash over the vendored Go module set; a dependency change makes that hash stale so `nix flake check` fails — **but the native gate, which runs `go mod download` live, passes green.** Without gating the nix job on `go.mod`/`go.sum`, a dependency bump would merge clean on the native gate and silently rot the nix build for the next nix-touching PR (and the deploy). Gating the nix job on `go.mod`/`go.sum` closes that trap. `web/package-lock.json` is deliberately **not** a trigger: `importNpmLock` reads the lockfile directly (no `npmDepsHash`), so npm deps never rot the nix build (per the `nix/package.nix` comment), and the SPA's vitest/build coverage already runs in the native gate.

**The native gate is version-matched to the flake**, so the two gates agree on the common path:

- **Go 1.26** (the `go.mod` gate) via `actions/setup-go` with the module + build cache.
- **Node 24** via `actions/setup-node` with the npm cache — the major `pkgs.nodejs` resolves to at the current `flake.lock` (nixpkgs `d407951…`).
- **golangci-lint 2.12.2**, the version nixpkgs ships at the current `flake.lock`, run untagged with `CGO_ENABLED=0` (matching the flake's `golangci-lint` check — the placeholder `embed_ui.go` variant). Drift between `flake.lock` bumps is acceptable: the full untagged lint re-runs in the nix gate whenever `flake.lock` changes.
- **`git`, `tmux`, and `prlimit` (util-linux)** on PATH for `go test` — the D17 real-subprocess bar the flake enforces via `nativeCheckInputs`. `tmux` is installed on the runner if the base image lacks it.

The order mirrors what `make` + local dev runs without nix: `cd web && npm ci && npm run lint && npm run format:check && npm test && npm run build` → copy the dist → `go build -tags ui ./cmd/...` → `go test -tags ui ./...` (needs the dist for the `ui`-tagged `go:embed`) → `golangci-lint run ./...` (untagged — the placeholder embed variant typechecks without the dist).

## Status

Accepted. Pure CI + docs: `.forgejo/workflows/ci.yml` (rewritten), `.forgejo/workflows/ci-nix.yml` (new), `docs/ops.md` (CI section), and this ADR. **No** change to `flake.nix`, `nix/**`, product source (Go/TS), or `deploy.yml`; the `store-postgres` commented template stays where it is in `ci.yml`.

**Relaxes ADR-0002 on the common path.** ADR-0002's "local `nix flake check` == CI" no longer holds for a Go/TS-only PR: native-green no longer *guarantees* nix-green, because the flake can fail for reasons the native gate cannot see — sandbox-only behavior (hermetic HOME, no network, `GOPROXY=off`/vendored modules), the `ui`-tagged `go:embed` compiling against the real copied dist inside `packages.lab`'s `checkPhase`, and checks that exist *only* in the flake (`nixos-module`, and the `web`/`labctl` derivations). The hermetic gate remains the authority; it just runs on every nix/dep change and as the deploy backstop rather than on every PR. Local `nix flake check` is still the right pre-push command for anyone touching nix or dependencies.

**Requires one repo-setting change the agent cannot make.** After the split the required status check must switch from **`flake-check`** to **`native`**. The nix job reports a status only when its `paths:` match, so on a Go/TS-only PR it never runs — a branch protection rule that still *requires* a never-reporting check would wedge every such PR at "expected". Whoever merges this must flip branch protection's required check to the native job's name (kept stable as `native` for exactly this reason) as part of the merge.

## Considered options

- **Keep the single `nix flake check` gate on every PR (ADR-0002 status quo).** Rejected: it pays the ~10 min cold-closure realization on every PR, most of which touch only Go/TS and never exercise anything nix-specific. The property it buys (native-green ⇒ nix-green) is preserved where it actually matters — nix/dep changes — and enforced again at deploy.
- **Bake nix into a prebuilt runner image.** Rejected: it recovers only the ~2 min installer, not the ~10 min cold store (the closure is still realized per ephemeral job), and adds an image to build and maintain.
- **Warm the nix store — a persistent runner or a binary cache/substituter.** Not rejected, but **out of scope and tracked separately (issue #33).** It is a complementary infra lever that would speed up the nix gate itself; this ADR is the software-side split that makes the common path fast regardless.
- **Make the native gate the *only* PR gate (drop nix from PRs entirely).** Rejected: the flake carries checks that have no native equivalent (`nixos-module` eval, the sandboxed `packages.lab` `ui`-embed compile) and it is the artifact the fleet actually consumes. Keeping it path-gated preserves those checks on the changes that can break them at a fraction of the cost.
- **Add `web/package-lock.json` to the nix `paths:` filter.** Rejected: `importNpmLock` derives npm fetches from the lockfile with no `npmDepsHash`, so an npm bump cannot rot the nix build, and its vitest/build coverage is already in the native gate. Adding it would fire the expensive nix gate for no correctness gain.
- **Omit `go.mod`/`go.sum` from the nix `paths:` filter** (trigger only on `*.nix`/`flake.lock`). Rejected: this is the one correctness trap — a dependency bump would go green on the native gate while staling `nix/package.nix`'s `vendorHash`, rotting the nix build for the next nix-touching PR and the deploy. `go.mod`/`go.sum` are non-negotiable triggers.

## Consequences

- **Branch protection must move its required check from `flake-check` to `native`** (a repo setting, not in this diff) — called out above and in the PR description. Keep the `native` job id stable.
- **Native-green is authoritative for the common path only.** A contributor touching nix or dependencies should still run `nix flake check` locally before pushing; the CI nix gate will run on those PRs. A Go/TS-only PR that somehow breaks the flake for a sandbox-specific reason would land green and surface on the next nix-touching PR or at deploy — an accepted, bounded exposure the deploy backstop (`test-configuration`) still catches before the fleet.
- **The golangci-lint / Node pins in `ci.yml` can drift from nixpkgs between `flake.lock` bumps.** Acceptable by design: any `flake.lock` change triggers the nix gate, which re-runs the authoritative untagged lint and the full suite against the pinned toolchain. Bump the `ci.yml` pins opportunistically when they drift far.
- `docs/ops.md` "CI runner prerequisites" now describes the two gates (native egress: proxy.golang.org, registry.npmjs.org, and the `actions/*` source; nix egress unchanged for the path-gated job).
- The `store-postgres` M2 template stays commented in `ci.yml`; when it lands it joins the native gate (the sqlite store suite already runs in both gates).
