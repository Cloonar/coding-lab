# The codex model/effort catalog is probed from `codex debug models` once at provider construction — the 0.133.0 pinned list demoted to a probe-failure fallback — and the provider seam grows per-model effort lists, with spawn effort validated against the resolved model's own list because codex does not clamp

ADR-0037 pinned the codex model catalog as compiled-in values read off
`codex debug models` on 0.133.0: `gpt-5.5` and `gpt-5.4-mini`, efforts
low/medium/high/xhigh, default medium. The deployed binary is 0.144.1 and
has already drifted past the pin: two new models (`gpt-5.6-terra`,
`gpt-5.6-luna`), a new `ultra` effort, **per-model** effort lists, and a
`visibility` field. Operators cannot pick models the binary supports; worse,
effort support now varies by model and codex does NOT clamp — an unsupported
model+effort combo 400s at the API (live-verified on 0.144.1: `gpt-5.5` +
`ultra` → 400). A pinned catalog was the right call when it was the cheapest
correct thing; it is now the thing serving wrong answers. Issue #156 grills
this to consensus (2026-07-13).

The shape of the fix is asymmetric on purpose: codex has a machine-readable
listing (`codex debug models` — the compat record's "cli extraction"
provenance, the binary's own contract), so codex gets true dynamic
discovery; claude-code has no machine-readable model listing at all, so it
stays pinned (drift *detection* for pinned catalogs is a separate issue,
#157). What is generic is the seam: effort-support-varies-by-model is a
provider-seam fact, not a codex quirk, so `provider.ModelOption` carries it
for every adapter and ADR-0036's Tier-1 conformance suite holds every
adapter to it.

The decisions, pinned (settled with the maintainer 2026-07-13 via /grill-me
— do not re-litigate):

- **Probe once at construction, fall back loudly.** `New` runs `codex debug
  models` exactly once, synchronously, via `exec.CommandContext` on the
  configured CodexBin — hard timeout 10s (`defaultProbeTimeout`), stdout
  capped at 8 MiB (the real 0.144.1 output is ~174 KB). The result is a
  process-lifetime cache: no TTL, no background refresh — the binary is
  nix-pinned, so a new binary implies a restart and restart is the one
  refresh event. On ANY probe failure — binary missing, timeout, nonzero
  exit, bad or oversize JSON, zero listed models, a model with an empty
  effort list, duplicate slugs — the compiled-in 0.133.0 catalog
  (`fallbackModels`/`fallbackEfforts` in source: `gpt-5.5` + `gpt-5.4-mini`,
  four efforts, default medium) is served and ONE loud structured Warn
  carries the probe error. The boot log records the catalog source (probe vs
  fallback) and the binary version (best-effort `codex --version`).
  `ProbeModels(ctx, codexBin)` is exported as the live compat test's entry
  point.
- **Trust the binary fully in the mapping.** Decode a minimal struct —
  `slug`, `display_name`, `visibility`, `priority`,
  `default_reasoning_level`, `supported_reasoning_levels[].effort` — and
  ignore every other field (`base_instructions` et al.). Serve exactly the
  `visibility == "list"` entries (replacing the pinned-era hardcoded
  `codex-auto-review` slug filter), ordered by `priority` ascending with the
  FIRST entry as the spawn default (on 0.144.1 the bare default becomes
  `gpt-5.6-terra`); Label = `display_name`; per-model efforts in binary
  order; per-model `DefaultEffort` = `default_reasoning_level`, sanitized to
  `""` when it is not a member of the model's own list. The union effort
  catalog is first-seen order across the sorted listed models. Effort labels
  are the one lab-side artifact: a pinned map (low → Low, medium → Medium,
  high → High, xhigh → "Extra high") with a title-case fallback for unknown
  values (max → "Max", ultra → "Ultra") — codex reports no display names for
  reasoning levels.
- **The seam grows per-model efforts: `provider.ModelOption`.** An `Option`
  plus the model's own `Efforts []Option` and reported `DefaultEffort`
  (pinned wire shape `{"value","label","efforts":[…],"default_effort"?}`);
  `AgentProvider.Models()` returns `[]ModelOption`; the helpers
  `HasModelOption`/`FindModelOption`/`CloneModelOptions` (a DEEP clone —
  outer slice and each nested `Efforts` slice) sit beside the existing
  `Option` helpers. **`Efforts()` stays the model-independent UNION
  catalog**: the repo/global defaults pickers render it, and a
  model-independent stored effort default is fine because skip-layer
  resolution (ADR-0030) falls through at spawn.
- **Spawn validation: strict against the resolved model, skip-layer for
  stored defaults, fallback = the model's reported default.** An explicit
  per-spawn effort validates against the RESOLVED model's effort list — an
  unsupported combo is a 400 even when the union carries the value, because
  codex passes it through to an API that rejects it. Stored repo/global
  defaults keep skip-layer semantics against the model's list (a foreign
  value is treated as unset and falls through — catalog drift can never
  wedge a spawn on a stored default; only explicit per-spawn values 400).
  **The all-layers-unset fallback changes**: the model's reported
  `DefaultEffort` wins when it declares one (codex: medium — was first-entry
  low); providers reporting none (claude) keep the first-entry rule.
- **Claude-code stays pinned.** No machine-readable model listing exists to
  probe; drift detection for pinned catalogs is issue #157. The adapter
  satisfies the enriched interface honestly: every model carries the same
  effort list (claude clamps unsupported efforts itself, so uniform lists
  are the truth, not a shim) and no per-model default — the first-entry
  fallback rule keeps working unchanged.
- **Composer follows the model; settings pickers keep the union.** The chat
  composer's effort options are the selected model's own list; a model
  switch that invalidates the current effort pick snaps it to the new
  model's default; a still-valid pick is kept. The repo/global defaults
  pickers keep the flat union list — unchanged UX, backed by the unchanged
  `Efforts()`.
- **No status surface for probe failures.** One loud structured warning plus
  the boot-log source line is the whole operator surface — no metric, no
  push, no UI badge. A fallback catalog is degraded but functional, and this
  is a single-operator tool whose boot log is read.

## Status

Accepted (2026-07-13). **Amends ADR-0037**: its "Model catalog pinned from
`codex debug models`" decision is superseded — the catalog is now probed at
provider construction and the pinned 0.133.0 list survives only as the
probe-failure fallback. Extends ADR-0036's Tier-1 suite with the per-model
catalog obligations (per-model effort lists non-empty/unique/labelled,
`DefaultEffort` membership in the model's own list, union coverage of every
per-model list, deep-clone aliasing checks on the nested `Efforts`), and the
codex Tier-2 compat record accordingly: the fragile coupling is now the
probe SCHEMA, not the catalog values, and a new live-gated probe test joins
the re-verification set. Leaves ADR-0030's skip-layer resolution model
intact in shape — the effort pass just runs against the resolved model's
own catalog. Resolves #156. Out of scope, deliberately: claude-code drift
detection (#157) and the gemini adapter's catalog (#126 owns its own
pinning).

## Considered options

- **A TTL or background refresh on the probe.** Rejected: the binary is
  nix-pinned — the only event that can change `codex debug models` output is
  a new binary, and a new binary is a restart. A TTL re-runs the probe
  forever to learn nothing, and a background refresher adds a concurrent
  writer to a catalog every spawn reads, for the same nothing. Probe once at
  construction; restart is the refresh.
- **Scraping the TUI `/model` picker instead of `codex debug models`.**
  Rejected on the fragile-recipe budget: a pane scrape couples to rendering,
  scroll behavior, and the update-banner noise the compat record already
  documents, and it costs a live TUI session at boot. `codex debug models`
  is the binary's own machine-readable contract — the compat record's "cli
  extraction" provenance, strictly stronger than a scrape.
- **Keeping the union-validated spawn (the pre-#156 rule).** Rejected: codex
  does not clamp — the unsupported combo is accepted by the CLI and 400s at
  the API (live-verified, `gpt-5.5` + `ultra` on 0.144.1). Union validation
  would accept exactly those spawns and hand the operator a session that
  fails on its first turn instead of a 400 that names the problem.
- **Lab-side clamping of unsupported combos to the nearest supported
  effort.** Rejected: silently changing what the operator asked for is worse
  than refusing it — a 400 names the mismatch at the boundary; a clamp hides
  it inside a session that behaves subtly differently than requested.
- **A status surface / metric / push for probe failures.** Rejected: this is
  a single-operator tool and the failure mode is "degraded to a working
  fallback catalog" — one loud structured warning at boot is proportionate;
  a dashboard for it is surface without a reader.

## Consequences

- `internal/provider/provider.go` gains `ModelOption` (Option + `Efforts` +
  `DefaultEffort`, pinned wire shape), changes `AgentProvider.Models()` to
  `[]ModelOption`, and adds `HasModelOption`/`FindModelOption`/
  `CloneModelOptions`; `Efforts()` is unchanged and documented as the union.
- `internal/provider/codex` gains the probe (`catalog.go`: decode, mapping,
  effort-label map, `ProbeModels`), renames the compiled-in catalog to
  `fallbackModels`/`fallbackEfforts`, runs the probe in `New`, and commits
  the trimmed real capture `testdata/models-0.144.1.json` (same trimming
  convention as compat's `models-0.133.0.json`) with `catalog_test.go`
  probe/parse tests against it.
- `internal/provider/claudecode/claudecode.go` adapts to the enriched
  interface: uniform per-model effort lists, no `DefaultEffort`.
- `internal/instance/credential.go`: `ResolveModelEffort`'s effort pass runs
  against the resolved model's own list (`FindModelOption`), and
  `layerSpawnDefault` gains the reported-default fallback ahead of the
  first-entry rule.
- `internal/provider/providertest` (`conformance.go`, `fake.go`): the
  Tier-1 catalog obligations above; the fake builds claude-shaped uniform
  enrichment via `uniformModelOptions`.
- `internal/httpapi/providers.go` ships the enriched models on the wire
  (each model carries `efforts` + `default_effort`); the settings surface is
  unchanged — it keeps rendering the union.
- `web/src`: `api.ts` gains `ProviderModelOption`; `lib/spawn.ts`'s
  `resolveEffortOption` resolves candidates against the selected model's
  list with the reported-default-then-first-entry fallback; `NewRun.tsx`'s
  effort chip follows the selected model and snaps an invalidated pick to
  the model's default; Settings/RepoSettings pickers keep the flat union.
- `internal/compat/codex`: `compat.md` §1 re-pins the coupling as the probe
  schema (field names/shapes, `visibility` semantics, `priority` ordering)
  plus the no-clamp 400 and the label map;
  `TestCompat_ModelCatalog_matchesDebugModelsFixture` now pins the FALLBACK
  catalog (a zero-value `Provider` that never probed) against
  `models-0.133.0.json`; the live-gated `TestCompat_Live_debugModelsProbe`
  joins the re-verification probes.
- `docs/adr/0037-codex-cli-adapter.md`'s Status notes the amendment.
