# AFK spawn defaults diverge from the manual pre-fill, and provider options ride a generic bag beside typed model/effort

ADR-0008 built the spawn path around two typed knobs — model and effort —
layered by D12d (request → repo default → global default) and drawn from the
provider's own catalogs (D14). That shape carried one silent assumption: a
single default set serves every spawn. It doesn't. A **manual** instance has an
operator in front of the Start form, so its defaults are only a *pre-fill* — a
soft starting point the operator overrides per spawn. An **AFK** run has no
operator: its defaults are *hard*, literally what runs unattended. Wanting an
AFK fleet to run, say, a cheaper model or `ultracode` while manual spawns keep
opening on opus/max is simply not expressible when both draw the same numbers.

The second gap is extensibility. model/effort are the only spawn knobs the
interface knows, and they are typed end to end. Claude Code grew a new
spawn-time knob — `ultracode` — that lab must be able to set for AFK runs, and
Codex/Gemini (#2) will each bring their own. Threading every such knob as
another typed field — another column, another API param, another positional
argument to `SpawnArgv` — churns the whole interface every time a provider grows
an option.

This ADR **evolves ADR-0008** (the `AgentProvider` seam) and brief decisions
**D12d** (spawn model/effort layering) and **D14** (provider-owned catalogs),
and **relates to #2** (OpenAI Codex — the first external consumer of the generic
bag). The decisions, pinned (settled with the operator 2026-07-07 — do not
re-litigate):

- **A hybrid, not a full generalization.** model/effort stay first-class and
  typed across the whole interface — run row, repo columns, global settings,
  providers API, and UI (D14 unchanged). Beside them sits exactly **one** generic
  bag: the provider *declares* its option schema (`SpawnOptions() []OptionSpec`),
  lab *stores, validates, and renders* the bag generically, and the provider
  *applies* it. `ultracode` is claude-code's first and currently only entry. This
  is the extensible seam that makes "the interface works for all providers" real:
  when Codex/Gemini land, each declares its own options and applies them behind
  the same seam — zero refactor of the typed core.
- **The manual/AFK split, layered as base + AFK-override.** The existing globals
  `spawn_model_default` / `spawn_effort_default` and the repo columns
  `model_default` / `effort_default` keep their meaning as the **base** — the
  manual pre-fill, and what an AFK run inherits when its own override is empty.
  New optional **AFK-override** slots sit beside them at both scopes. Manual
  resolution is unchanged (request → repo base → global base); AFK resolution
  becomes repo.afk ?? global.afk ?? repo.base ?? global.base. Non-breaking: an
  operator who sets no override sees today's behavior exactly.
- **ultracode is AFK-only, defaults-only, and applied as provider-owned prompt
  injection.** It is a boolean spawn option applied by **prepending a
  provider-owned directive line to a non-empty `InitialPrompt`**. A manual
  instance passes an empty prompt, so every prompt-scoped option is a natural
  no-op — a manual operator who wants ultracode simply types the keyword. There
  is **no per-run override**: `POST /repos/{id}/afk/start` stays bodyless.
  Verified against Claude Code's docs, changelog, and `--help`: ultracode is a
  *prompt trigger keyword* (renamed from "workflow" in v2.1.186), **not** a CLI
  flag, env var, or `settings.json` field — which is exactly why application is
  prompt injection and not argv. The directive text is provider-owned and freely
  tunable; it is **not** a new pinned Claude coupling.
- **The `SpawnSpec` seam.** The positional `SpawnArgv(session, model, effort,
  prompt)` is replaced by `SpawnArgv(SpawnSpec{SessionName, Model, Effort,
  Options, InitialPrompt})`, so a future option never churns the signature.
  Application is **provider-owned and not argv**: claude-code reads
  `Options["ultracode"]` and rewrites its own prompt; the generic core never
  learns what any option means. `internal/afk` stays provider-agnostic —
  **ultracode must not leak into the afk package** (unlike incogni, which is a
  lab-domain repo flag lab owns and acts on).
- **Validation mirrors model/effort.** The bag validates against the resolving
  repo's provider schema: an unknown option key or a bad value is a **400**, the
  same discipline D12d applies to an unknown model/effort. An empty AFK
  model/effort is explicitly allowed (it means inherit). A global bag may span
  providers once more than one exists; at spawn it is filtered to the repo's
  provider before it is applied.
- **Storage, one migration.** Global settings gain rows `spawn_model_default_afk`,
  `spawn_effort_default_afk` (empty = inherit), and `spawn_options_afk` (JSON,
  e.g. `{"ultracode":"true"}`). Repos gain nullable columns `afk_model_default`,
  `afk_effort_default`, `afk_options` (JSON). One migration (0004), mirrored
  across sqlite and postgres. **Deferred:** no `runs.options` history column and
  no run-row options persistence — re-adoption re-adopts a live tmux session,
  never re-spawns, so nothing needs the bag after launch; bool is the only option
  type (enum/string reserved).
- **Resolver.** `ResolveModelEffort` becomes run-kind-aware; a new
  `ResolveSpawnOptions(ctx, prov, repo, kind)` returns the empty map for manual
  and `repo.afk_options ?? global.spawn_options_afk` (filtered + validated) for
  AFK. Both feed a new `LaunchSpec.Options` → `SpawnSpec.Options`.
- **Schema-driven UI.** `GET /api/v1/providers` grows an `options` field (the
  declared schema). Global Settings and Repo Settings render an "AFK defaults"
  section from it — model/effort selects with an explicit *inherit* entry, and a
  bool option (ultracode) as a checkbox. A future provider's options render
  automatically. The manual `StartInstanceForm` is unchanged.

## Status

Accepted. Evolves ADR-0008 (the `AgentProvider` seam) and brief decisions
D12d/D14; settled with the operator 2026-07-07 (issue #19). model/effort stay
typed and first-class; the generic options bag is additive beside them. One
schema migration (0004) adds the AFK-override slots and the options bag at both
scopes, sqlite + postgres. Non-breaking: an operator who sets no AFK override or
option keeps today's manual-and-AFK-share-one-default behavior. Unblocks #2
(Codex): a new provider declares its own `SpawnOptions()` and applies them
behind `SpawnSpec`, with zero refactor of the typed core. ultracode is
claude-code's first entry and is provider-owned — not a pinned compat coupling.

## Considered options

- **Fully generalize model/effort into the bag too.** Rejected: it discards the
  typed, first-class handling model/effort earn across the whole interface
  (validated catalogs, typed run-row columns, dedicated selects) and churns every
  layer to reach a knob that never needed generalizing. The hybrid keeps the two
  proven knobs typed and adds one bag beside them for everything else.
- **A per-run AFK override form (or making ultracode a per-run flag).** Rejected:
  AFK is defaults-only by design — `POST /repos/{id}/afk/start` is bodyless, and
  an unattended run has no one to fill a form. Options belong to the repo/global
  defaults that AFK resolves at launch, not to a per-spawn request. A manual user
  who wants ultracode types the keyword themselves.
- **Treat ultracode as a CLI flag or a `settings.json` field.** Rejected on the
  evidence: ultracode is a *prompt trigger keyword* in Claude Code (renamed from
  "workflow" in v2.1.186), verified against its docs, changelog, and `--help`.
  There is no flag or settings key to set, so the only faithful application is
  prepending the keyword to the prompt — which also makes it a natural no-op for
  the promptless manual spawn.
- **Keep the positional `SpawnArgv` and add a fifth positional per option.**
  Rejected: it churns the interface signature — and every provider double and
  call site — each time any provider grows an option. `SpawnSpec` absorbs a new
  option in a struct field the generic core never has to understand.
- **Let ultracode reach the provider through the `afk` package (like incogni).**
  Rejected: incogni is a lab-domain repo flag with cross-cutting behavior lab
  owns and enacts; ultracode is opaque provider option data. Routing it as a
  generic `Options` bag the provider alone interprets keeps `internal/afk`
  provider-agnostic and prevents a claude-specific keyword from leaking into the
  AFK engine.

## Consequences

- `internal/provider` gains `OptionSpec`, the `SpawnOptions() []OptionSpec`
  catalog method, `OptionTypeBool`, and the `SpawnSpec` struct; `SpawnArgv` takes
  a `SpawnSpec` instead of four positionals. claude-code declares the single
  `ultracode` bool and applies it by rewriting a non-empty `InitialPrompt`; the
  `providertest` fakes gain a scriptable schema.
- `internal/store` adds three global settings rows and three nullable repo
  columns in migration 0004 (sqlite + postgres). No `runs` change — resolved
  options are not persisted, because re-adoption never re-spawns.
- `internal/instance` makes `ResolveModelEffort` run-kind-aware and adds
  `ResolveSpawnOptions`; both feed a new `LaunchSpec.Options`. `internal/afk`
  stays provider-agnostic — it carries the bag through, never reads it.
- `GET /api/v1/providers` gains the `options` schema; Global + Repo Settings
  render an "AFK defaults" section (model/effort selects with an explicit inherit
  entry, ultracode as a checkbox) generically from it. The manual Start form is
  untouched.
- Adding a provider option is now additive: declare it in `SpawnOptions()`, apply
  it in the provider, and the store bag, validation, and Settings UI carry it
  with no typed-core change. Codex (#2) is the first external consumer.
