# Prebuilt static binaries ship from a `v*` tag as a GitHub Release, cross-compiled with `GOOS`/`GOARCH` rather than through nix, with the tag as the version

ADR-0002 made the flake the primary artifact and noted, almost in passing, that the binary "also runs on any Linux with `git`, `tmux`, and `claude` on PATH — Nix is first-class, not required". Nothing ever built that binary for anyone who was not already holding the source. The repo has no tags, no releases, and no published artifact but the agent-tools images; the only non-nix install path is clone + build, which asks an evaluator for Go 1.26 and Node before they have seen the UI at all. Someone who is not on NixOS and does not want a toolchain has nowhere to go.

This ADR adds a distribution channel and nothing else. `.github/workflows/release.yml`, on a push of a tag matching `v*`, publishes a GitHub Release carrying statically linked `lab` (SPA embedded, `ui` build tag) and `labctl` for `linux/amd64` and `linux/arm64`, a sha256 `checksums.txt`, and auto-generated release notes. The flake remains the authority on the hermetic build and the NixOS module — ADR-0002 is untouched in substance and in status. What follows is additive: where the release path and the flake can disagree (the build tool, the version string), each bullet says which one wins and why.

The release, decided:

- **Cross-compile with `GOOS`/`GOARCH` and `CGO_ENABLED=0`, not nix.** `nix build .#lab` already produces exactly this static artifact, so a nix-based release job is superficially the obvious move — but the arm64 leg would need an arm64 builder or qemu emulation, neither of which this repo's CI has, and with CGO off the Go toolchain cross-compiles natively and emits a static ELF that is byte-for-byte as static as the flake's. Nix buys nothing on this path and costs an emulator or a second runner architecture. The recipe is the one the native gate already runs (ADR-0023): build the SPA, copy `web/dist` → `internal/webui/dist` for the `ui`-tagged `go:embed`, then `go build -tags ui` for `lab` and untagged for `labctl`, `CGO_ENABLED=0` throughout. This is a *distribution* channel, not a second source of truth: the fleet still consumes `packages.lab` through `nixosModules.lab`, and nothing about what a NixOS host installs moves.

- **The tag is the version.** The release version is the tag with the leading `v` stripped, stamped through the existing `-X main.version=…` ldflags convention that both binaries already read. `nix/package.nix` carries `version = "0.1.0"` and stamps `<version>+<rev>`; the first tag is `v0.1.0`, so the two agree today. When they can diverge, derive from the tag rather than adding a mechanism that keeps `nix/package.nix` and the tag in sync — a sync mechanism is a thing that can be forgotten in the one commit where it mattered, and a derivation cannot be. The consequence is a deliberate shape difference in what `--version` prints, recorded below.

- **Built-in `GITHUB_TOKEN` only, least-privilege per job.** The build job holds `contents: read`; only the release job holds `contents: write`. No operator-provisioned secret is introduced, so a fork can cut a release into its own namespace with nothing to provision and nothing to rotate — the posture ADR-0064 established for the agent-tools publish, inherited here rather than re-argued.

- **The publish is not cancellable mid-flight.** `cancel-in-progress` is `${{ github.event_name == 'pull_request' }}`, deliberately asymmetric for the same reason agent-tools is: a superseded PR run is pure waste and should die, but a release that is killed between uploading `lab-linux-amd64` and uploading `checksums.txt` leaves a published release whose assets are a lie. A half-published release is worse than a slow one.

- **Two dry-run legs: `workflow_dispatch` and a path-gated `pull_request` leg.** Both build the full artifact set and upload it as a workflow artifact without creating a release. `workflow_dispatch` alone would not do, because GitHub only surfaces a dispatch button for workflows already on the default branch — the PR that introduces this workflow cannot dispatch itself, so without the PR leg the build recipe's first real execution would be the tag push that is supposed to publish it. The PR leg is gated on this workflow's own path, so it costs nothing on unrelated PRs and always runs on the change that could break it.

- **Asset names are arch-qualified and version-independent** (`lab-linux-amd64`, `lab-linux-arm64`, `labctl-linux-amd64`, `labctl-linux-arm64`, `checksums.txt`). GitHub's `releases/latest/download/<asset>` redirect resolves only version-independent names, and that stable URL is the documented install one-liner. Version-qualified names would be self-describing once on disk but would break the one URL the docs can print without an edit per release. The URL won; the arch qualifier stays because a downloaded file with no arch in its name is indistinguishable from the wrong one.

- **Scope is held narrow on purpose.** `linux/amd64` and `linux/arm64` only — the two the flake already targets. No macOS, no Windows, no container quickstart image (tracked separately in #11), no package-manager publishing. Cutting the first tag stays a maintainer action: this ADR ships the mechanism, not the release.

## Status

Accepted. Settled via issue #10 (2026-08-05).

**Additive to ADR-0002, which is unchanged.** The flake stays the first-class packaging: it is the hermetic build, the source of `nixosModules.lab`, and what the fleet consumes. This ADR does not make binaries a second packaging authority, and nothing in the flake, `nix/`, or the module moves. It delivers the property ADR-0002 already asserted — "runs on any Linux … Nix is first-class, not required" — to people who do not have the source.

**Reuses ADR-0023's native build recipe, and does not touch its gates.** The release build is the native gate's ordering (SPA → dist copy → `go build -tags ui`) run for two architectures; `native` and the path-gated `flake-check` are untouched, and `release.yml` adds no required check.

**Inherits ADR-0064's release posture verbatim.** Built-in `GITHUB_TOKEN`, no operator-provisioned secrets, fork-safe by construction, publish never cancelled mid-flight. The one asymmetry with agent-tools is auth scope: this workflow needs `contents: write`, not `packages: write`, and touches no registry — so ADR-0064's ghcr visibility trap has no analogue here, because a GitHub Release is public the moment it is published.

## Considered options

- **Build the release through `nix build .#lab` on both architectures.** Rejected: the artifact is identical, and the arm64 leg needs an arm64 runner or qemu emulation — an emulated hermetic build to obtain a binary the Go toolchain cross-compiles natively in seconds. It would also make the release path depend on the ~10-minute cold closure realization ADR-0023 exists to keep off the common path.

- **goreleaser.** Rejected: it is a config-and-conventions layer over a build this repo can already express in the same shell steps its CI runs today, and it would introduce a second recipe that can drift from the `make lab` / native-gate ordering. The asset set here is four binaries and a checksums file; that does not justify a release framework.

- **Sync the release version from `nix/package.nix` (or fail the tag when they disagree).** Rejected: it adds a step someone must remember in the exact commit where forgetting it is expensive, and a mismatch check turns a tag push — the least recoverable moment — into a place a release can fail. Deriving from the tag has no state to keep in sync and no way to be skipped.

- **Version-qualified asset names** (`lab-0.1.0-linux-amd64`). Rejected: `releases/latest/download/<asset>` cannot resolve a name that contains the version, so the documented install command would need editing at every release, or the docs would have to point at a version-pinned URL that goes stale the moment the next tag lands. Self-describing filenames are worth less than a URL that stays true.

- **Only a `workflow_dispatch` dry run, no `pull_request` leg.** Rejected: a `workflow_dispatch` button exists only for workflows already on the default branch, so the PR introducing this workflow could not run it, and the recipe's first execution would be the publishing tag push. The PR leg is what makes "the workflow was proven before it published anything" true rather than aspirational.

- **Provision a PAT for the release job.** Rejected on ADR-0064's grounds: it reproduces exactly what the built-in token removes — a human-owned credential to scope, rotate, and re-provision in every fork — for a capability `GITHUB_TOKEN` with `contents: write` already has.

- **Add macOS/Windows builds, a container image, or Homebrew/AUR publishing in the same change.** Rejected for this ADR: each is a distinct distribution channel with its own signing, notarization, or packaging story, and none of them is what an evaluator on a Linux box needs first. The container quickstart is tracked separately (#11); the rest are unclaimed.

## Consequences

- **The release binaries are a second build path, not the hermetic one, so CI has to keep it honest.** The flake gate never sees these artifacts. Two things close the gap: the workflow asserts static linking with `file` on every built binary and fails if CGO ever creeps in, and the two dry-run legs mean the recipe is exercised on every change to it rather than only at tag time. Neither makes the release path hermetic — it is a knowingly-accepted second path, bounded by an assertion and a rehearsal.

- **`--version` prints a different shape depending on where the binary came from.** A release binary reports the bare tag version (`0.1.0`); a flake-built binary reports `<version>+<rev>`. Deliberate, and the reason a bug report should say which install path produced the binary — the presence or absence of `+<rev>` answers that on its own.

- **`releases/latest/download/lab-linux-amd64` becomes a documented contract.** Renaming an asset breaks every install instruction in the wild, not just the ones in this repo. Asset names are as stable as the CLI's flags.

- **Nothing publishes until a maintainer cuts a tag.** The workflow's existence changes no state; the first `v0.1.0` is a human act, and until it happens the `releases/latest/download/` URL the docs print resolves to nothing.

- **Forks release into their own namespace with zero provisioning.** A fork's tag push creates a release on the fork under the fork's own `GITHUB_TOKEN`; it cannot publish at ours, and it needs no secret from us.

- **arm64 is released but never natively built or tested in CI.** Cross-compilation guarantees it links, not that it runs — the flake's `aarch64-linux` outputs are equally unexercised on x86 CI. An arm64 regression that is not a compile error surfaces on a user's machine, which is the accepted cost of not maintaining an arm64 runner.
