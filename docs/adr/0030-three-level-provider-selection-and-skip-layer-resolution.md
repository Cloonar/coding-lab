# The agent CLI becomes a three-level layered spawn knob, defaults resolve skip-layer against the effective catalog, and one searchable select replaces every spawn picker

Issue #66 names four operator-visible defects on the spawn surfaces and one
structural gap behind them. The composer's repo picker clips off-screen:
`.composer-pop-panel` hard-codes open-above (`bottom: calc(100% + 6px)`, a
fixed `max-height: 15rem` in `web/src/base.css`) while the composer sits near
the top of the page, so with many repos the panel's scroll region is
unreachable above the viewport — worst on a phone. The start screen mixes a
custom repo popover with native `<select>` model/effort chips, no typeahead
anywhere. RepoSettings' base model/effort are free-text inputs under a stale
"provider catalogs arrive in M3" hint while the AFK section directly below
already uses `CatalogSelect`. And the agent CLI is not selectable at all:
`repos.provider` is stamped once from the `reposvc.DefaultProvider` const,
`PATCH /repos/{id}` rejects `provider`, and AddRepo never offers it.

The structural gap is that resolution is not agent-agnostic.
`ResolveModelEffort` (`internal/instance/credential.go`) hard-400s any
resolved value missing from the provider's catalog. The seeded global default
is `opus[1m]` — claude-shaped — so the moment a second provider exists, an
AFK run on its repo resolves the claude default and every launch 400s. A
provider with an empty efforts catalog can never spawn at all
(`HasOption([], "") == false`). The catalog seam itself is not the problem
and stays exactly as ADR-0021/D14 and ADR-0026 pinned it:
`AgentProvider.Models()/Efforts()` are provider-owned, statically pinned
lists served on `GET /api/v1/providers`, and the start screen already
re-catalogs by the selected repo's provider — Codex (#2) just declares its
lists.

This ADR **amends ADR-0021** (provider joins model/effort as a layered,
AFK-symmetric spawn knob, and default-layer resolution semantics change from
hard-400 to skip-layer), **touches ADR-0025** (the composer surface gains a
provider chip, and its repo-picker popover is replaced by the unified
select), and **references ADR-0026 unchanged** (the seam: catalogs stay
provider-owned pinned lists; `GET /providers` is untouched). The decisions,
pinned (settled with the operator 2026-07-09 — do not re-litigate):

- **Three-level provider selection, D12d-style layering.** The effective
  provider of a spawn = per-spawn pick → repo override → global default.
  `repos.provider` becomes **nullable** (NULL = inherit); migration 0006 also
  NULLs every existing row — they were stamped from the now-deleted
  `reposvc.DefaultProvider` const, never operator-chosen. A new **seeded**
  global setting `provider_default` holds the first registered provider id
  (`claude-code` today). Symmetric AFK overrides mirror ADR-0021: nullable
  `repos.afk_provider_default` + global `spawn_provider_default_afk` (empty =
  inherit), so the AFK chain is repo.afk_provider_default → global
  spawn_provider_default_afk → repo.provider → global provider_default. One
  migration (0006, sqlite + postgres). `POST .../instances` accepts an
  optional `provider` (the per-spawn pick, strict); `PATCH /repos/{id}`
  accepts `provider` and `afk_provider_default`; `POST /repos` accepts an
  optional `provider`. Run rows already store `runs.provider`, so an ended
  chat survives its repo switching agents.
- **Skip-layer resolution.** A DEFAULT-layer value — model, effort, or
  provider id — that is not in the effective provider's catalog (not in the
  registry, for provider ids) is treated as **unset**: resolution falls
  through to the next layer, and the final fallback is the catalog's first
  entry (the first registered provider). An EMPTY catalog — a provider
  without that knob — resolves to `""`, omits the CLI flag, and skips
  validation, which fixes both the claude-shaped-global-default AFK 400 trap
  (the seeded `opus[1m]` poisoning every launch on a second provider's repo)
  and the empty-efforts-can-never-spawn bug. EXPLICIT per-spawn request
  values stay strict: an unknown requested model/effort/provider is still a
  **400**, the same discipline D12d always applied to a bad request. This is
  exactly what the SPA's `resolveSpawnOption` (`web/src/lib/spawn.ts`)
  already did; the backend now mirrors it. Catalogs stay statically pinned
  per adapter (compat discipline, ADR-0026) — no live discovery from the CLI.
- **One select component.** One hand-rolled Solid module — no new dependency
  — used on the composer (chip trigger skin) AND the settings pages
  (form-field trigger skin). It absorbs `CatalogSelect`'s inherit-entry and
  "(not in catalog)" semantics as props and the repo picker's disabled/status
  rows (cloning %, clone failed); `CatalogSelect` is deleted and every call
  site migrates — no legacy shim. A search input appears automatically at ≥8
  options (a prop can force it on or off); the filter is a case-insensitive
  substring match over label + value. ARIA listbox pattern with
  Arrow/Enter/Escape navigation; the filter input sits at the top of the
  panel, the trigger stays a button. Positioning: prefer below, flip+clamp —
  free space is measured against `visualViewport` (on-screen-keyboard-aware),
  the panel flips above only when below lacks room and above has more, and
  `max-height` clamps to the free space so the whole panel (with internal
  scroll) always fits. This is the fix for the clipping composer repo panel.
- **UI consequences of the layering.** The composer gets a provider chip
  ONLY when ≥2 providers are registered — today's single-provider UI stays
  pixel-identical. The per-spawn pick is **ephemeral**: it resets on repo
  change and page load; repo/global defaults are the durable levers.
  Model/effort picks, the auth/logged-out banner, and the catalogs all follow
  the EFFECTIVE provider, and model/effort picks reset when the effective
  provider changes — fixing a latent bug where a foreign pick survived a repo
  switch and 400'd on submit. The effort chip hides entirely when the
  effective provider's effort catalog is empty. AddRepo and RepoSettings get
  provider selects with an explicit inherit entry; global Settings gets base
  + AFK provider selects, with base defaults rendered against the global
  default provider and AFK defaults against the AFK-effective provider
  (previously both hardcoded `providers[0]`). A provider switch in
  RepoSettings re-catalogs the model/effort selects; stored foreign values
  stay, shown with the "(not in catalog)" marker, and NOTHING auto-clears —
  skip-layer makes a stale value harmless at spawn, and flipping the provider
  back restores the old defaults. The stale "provider catalogs arrive in M3"
  hint is gone.

**Out of scope:** the Codex adapter itself (#2 — this issue only makes lab
ready for it), a per-run provider history UI, and live catalog discovery.

## Status

Accepted. Amends ADR-0021: provider joins model/effort as a layered,
AFK-symmetric spawn knob (the same base + AFK-override apparatus, one rung
wider), and DEFAULT-layer resolution changes from hard-400 to skip-layer —
explicit request validation stays strict. Touches ADR-0025: the composer
surface gains the conditional provider chip, and the repo-picker popover is
replaced by the unified select (the composer's anatomy and behavior are
otherwise unchanged). References ADR-0026 without change: catalogs remain
provider-owned pinned lists and `GET /providers` keeps its shape. One
migration (0006, sqlite + postgres). Settled via issue #66. Non-breaking for
a single-provider deployment: the composer stays pixel-identical, and a repo
with no override resolves to the same `claude-code` it was stamped with —
now via `provider_default` instead of a const. Unblocks #2: registering a
second provider makes the chip appear and the selects populate, with zero
refactor.

## Considered options

- **Validate defaults strictly at write time everywhere instead of
  skip-layer at resolve time.** Rejected: it turns every provider switch into
  forced data loss or a hard error — a repo whose stored model no longer
  matches the new provider's catalog could not even be saved. Skip-layer
  keeps stored values harmless and reversible; the write path stays honest by
  validating only what the operator explicitly sends.
- **Auto-clear stored model/effort on provider switch.** Rejected: it
  destroys operator data for no safety gain — skip-layer already makes a
  stale value inert at spawn — and flipping the provider back should restore
  the old defaults, which a cleared column cannot.
- **Make the per-spawn provider pick sticky.** Rejected: a sticky foreign
  pick silently poisons later spawns long after the operator forgot it. The
  pick is ephemeral by design; the repo override and the global default are
  the durable levers.
- **Live catalog discovery from the CLI.** Rejected: compat discipline —
  ADR-0026 pins catalogs statically per adapter version, verified with the
  pinned binary, and a runtime probe would trade that determinism for a
  moving target.
- **Adopt a dependency for the select.** Rejected: the repo's discipline is
  hand-rolled, dependency-free UI (ADR-0018/0019/0025), the component is
  small, and it now owns app-specific semantics (inherit entries, catalog
  markers, clone-status rows) no off-the-shelf listbox carries.
- **Keep `CatalogSelect` beside the new select as a shim.** Rejected: two
  select systems is precisely the defect this issue removes; every call site
  migrates in the same change.

## Consequences

- `internal/store`: migration 0006 (sqlite + postgres) makes `repos.provider`
  nullable and NULLs existing rows, adds nullable
  `repos.afk_provider_default`, seeds the `provider_default` setting to the
  first registered provider id, and adds `spawn_provider_default_afk`
  (empty = inherit). `reposvc.DefaultProvider` is deleted — the seeded
  setting is the single source of the default.
- `internal/instance`: the resolver gains provider resolution beside
  `ResolveModelEffort`/`ResolveSpawnOptions`, all three run-kind-aware and
  skip-layer for DEFAULT-layer values; explicit request values keep the 400
  discipline. An empty catalog resolves the knob to `""` and the provider
  omits the flag.
- `internal/httpapi`: `POST .../instances` gains optional `provider`;
  `PATCH /repos/{id}` gains `provider` + `afk_provider_default`;
  `POST /repos` gains optional `provider`. `GET /providers` is unchanged.
- The SPA gains the one select module and loses `CatalogSelect.tsx` and the
  composer's repo popover; NewRun, AddRepo, RepoSettings, and Settings all
  render it. `resolveSpawnOption` stops being a frontend-only kindness — the
  backend now enforces the same skip-layer rule, so the two can no longer
  disagree about what a spawn will resolve to.
- The seeded `opus[1m]` global default stops being a foot-gun: on a repo
  whose effective provider cannot serve it, it reads as unset and resolution
  falls through instead of 400ing every unattended launch.
- An ended run's chat keeps reading through `runs.provider`, so switching a
  repo's agent never orphans its history.
