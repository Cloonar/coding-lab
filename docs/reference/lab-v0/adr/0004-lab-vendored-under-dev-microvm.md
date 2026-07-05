# Lab vendored under the dev microvm, not in `utils/pkgs/`

> **Status:** superseded by [ADR-0020](0020-lab-shared-module-in-utils.md). `dev-new` became a second lab host, so `lab` moved to `utils/modules/lab/` as a shared, explicitly-imported module — the exact migration anticipated under Consequences below. The single-host vendoring pattern this ADR describes still applies to future single-host tools.

Custom binaries in this repo conventionally live at `utils/pkgs/<name>/` and are surfaced to every host via `utils/overlays/packages.nix`. `lab` (the small Go web service on the `dev` microvm that lists git projects and starts/stops `claude --remote-control` tmux sessions) breaks that convention: its source, derivation, and module live under `hosts/fw/microvms/dev/modules/lab/` instead.

Two reasons. First, `lab` is dev-microvm-specific by design — it scans `~/projects/` and shells out to `claude` + `tmux` in that filesystem, with no useful meaning on any other host. Putting it in `utils/pkgs/` would surface it to every host's package set for no benefit. Second and more importantly, `scripts/pre-commit` rebuilds *every* host when anything under `utils/` is staged (the `shared` regex matches `^utils/`). `lab` is the kind of tool we iterate on frequently — a quick HTML tweak, an added handler — and triggering a 7-host dry-build for every keystroke is unworkable. Vendoring under `hosts/fw/microvms/dev/modules/lab/` keeps the staged-path prefix at `hosts/fw/`, so only `fw` dry-builds.

## Considered options

- **`utils/pkgs/lab/` + `utils/overlays/packages.nix` entry + module under `hosts/fw/microvms/dev/utils/modules/`** (the conventional shape). Rejected: surfaces the package to all 7 hosts' overlays for a tool that only makes sense on `dev`, and any Go source edit triggers an all-host dry-build at commit time.
- **`utils/pkgs/lab/` but skip the overlay registration**, so it's a derivation file only callable explicitly via `pkgs.callPackage`. Rejected: still under `utils/` so still triggers the all-host dry-build, and now diverges from the other `utils/pkgs/*` entries which *are* in the overlay — inconsistency for no payoff.
- **Separate Forgejo repo `git.cloonar.com/Cloonar/lab`** consumed via `fetchFromGitea` + `buildGoModule`. Rejected: every code change becomes two commits across two repos, the pin-bump dance adds friction, and the tool is too small and too dev-coupled to justify a separate repo.
- **Special-case `utils/pkgs/lab/` in `scripts/pre-commit`** to only trigger `fw`. Rejected: hardcoded coupling in an otherwise generic script (same anti-pattern rejected in ADR-0003 for the `dev`-microvm-as-peer-host layout).

## Consequences

- `lab` is invisible to every host except `dev`. Trying to import or reference it from elsewhere requires moving the directory first (and accepting the all-host rebuild cost).
- The Go source lives in the NixOS repo's git history. No CI for it as Go code — the only gate is "does the derivation build", run by `scripts/pre-commit` as part of `fw`'s dry-build.
- Future microvm-local tools should follow this pattern: `hosts/<host>/microvms/<vm>/modules/<tool>/` with vendored source, single `default.nix` for module + derivation. Tools intended for multiple hosts still belong in `utils/pkgs/`.
- If `lab` ever grows to be useful on another machine, migration is `git mv` plus adding an overlay entry — not a rewrite. The lock-in is one-directional and modest.
