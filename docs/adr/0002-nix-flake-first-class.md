# Nix flake is the first-class packaging; NixOS module; no Docker

The fleet is NixOS and cloonar-nixos consumes flake inputs, so the flake is not an afterthought but the primary artifact (D3). The flake exposes `packages.{lab,labctl,web,default=lab}` for x86_64-linux and aarch64-linux, `nixosModules.lab`, `devShells.default` (go, gopls, golangci-lint, node, tmux, util-linux, sqlite), and `checks` running the full suite — package builds, Go tests with **real git, tmux, and util-linux (prlimit)** in `nativeCheckInputs`, golangci-lint, and the SPA's vitest/eslint. `nix flake check` locally is the same truth CI runs (D17).

The web bundle builds with `buildNpmPackage` using `npmDeps = importNpmLock { … }`, so dependency hashes derive from `package-lock.json` and there is no `npmDepsHash` churn commit on every lockfile change. The `lab` package copies the web dist into `internal/webui/dist` and builds with `-tags ui`, `CGO_ENABLED=0`, `ldflags "-s -w"` (see ADR-0005 for the embedding mechanics). The binary also runs on any Linux with `git`, `tmux`, and `claude` on PATH — Nix is first-class, not required. No Docker in the MVP; an OCI image is explicit roadmap.

`nixosModules.lab` replaces v0's hardcoded unit with options: `enable, package, claudePackage, user, group, stateDir, listenAddr, baseUrl, db` (default sqlite path from stateDir), `environmentFile` (secret DSN via `LAB_DB`, LoadCredential-friendly), `masterKeyFile` (point at a sops-nix path, D9), `maxInstances, sessionNofile, proxyAuth{enable,header,trustedProxies}, openFirewall` (default false), `extraFlags`. The unit is `Type=simple`, **`KillMode=process`**, `Restart=on-failure`, `RestartSec=5`, with git, tmux, openssh, util-linux, and the claude package on the unit PATH. `KillMode=process` is load-bearing: the tmux server lab spawns — and every claude session under it — lives in the unit's cgroup, and `KillMode=process` kills only the lab process on restart, so a deploy never drops a session; lab re-adopts the surviving tmux server on start (D4). openssh on PATH is equally deliberate: origins are SSH remotes and git forks `ssh` off PATH — without it `git fetch` dies with "cannot run ssh".

## Status

Accepted. Implements D3, carrying D4's KillMode=process invariant into the module.

## Considered options

- **Docker/OCI image as the primary artifact.** Rejected: the fleet is NixOS, cloonar-nixos consumes flakes directly, and an image adds a build/publish pipeline the MVP doesn't need. OCI stays on the explicit roadmap (brief §14).
- **Stay a vendored module in the consuming repo** (v0's reference ADR-0004/0020 pattern). Rejected: that trajectory ends here by design (ADR-0001). What carries over is the opt-in property: hosts import `nixosModules.lab` explicitly; nothing is surfaced globally.
- **`npmDepsHash` pinning for the web build.** Rejected: every lockfile bump forces a hash-churn commit; `importNpmLock` derives hashes from `package-lock.json` itself.
- **Default `KillMode` (control-group).** Rejected: it would kill the tmux server and every agent session on each deploy. Sessions surviving service restarts is a design pillar (D4); restart re-adoption depends on it.

## Consequences

- Local `nix flake check` == CI (the CI workflow is essentially that one command plus a postgres service for the store suite from M2 on).
- A NixOS host enables the module and points `masterKeyFile` at a sops-nix/`LoadCredential` path; bare-metal deployment remains supported and documented (`docs/ops.md`).
- The nixpkgs input must ship `go_1_26` (unstable or the first release that does); the pin is recorded in `docs/ops.md`.
- Integration tests against real git/tmux run inside the Nix sandbox; test helpers must be hermetic (HOME redirected, no global git config) or `checks` breaks in ways `go test` on a workstation doesn't show.
