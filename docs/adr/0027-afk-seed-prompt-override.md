# AFK seed prompt override layers repo over global over the built-in template, replacing it wholesale with no silent edits

AFK's seed prompt (`SeedPrompt`, `internal/afk/decide.go:214`) is the one part
of the launch path ADR-0021's layering never reached: model, effort, and the
options bag all resolve through base ?? AFK-override before a run spawns, but
the eight-step instruction text stayed a single hardcoded template, so an
operator who wanted different framing or house conventions baked into every
run had no lever short of a code change and a redeploy.

Issue #52 grills this to consensus, reusing ADR-0021's layering apparatus
rather than inventing a new one. But a prompt is prose, not a knob:
replacement is whole-text, not field-by-field, and two open questions — does
an override get the incogni sentence spliced in, does the frontend need to
know the built-in words — are answered by making lab's own edits WYSIWYG and
the built-in text an API-served value.

This ADR **extends ADR-0021** (the layering pattern, and its discipline of
serving a source of truth through the API instead of duplicating it in the
frontend) and settles issue #52. The decisions, pinned (settled with the
operator 2026-07-08 — do not re-litigate):

- **Layering extends ADR-0021 one rung deeper.** `repos.afk_prompt` → global
  setting `afk_prompt` → the built-in `SeedPrompt` template; empty or NULL at
  a layer means inherit the next. Both AFK kinds resolve through the same
  `launch()` call, so `afk_manual` and `afk_auto` never diverge for the same
  repo. **No per-run override** — `POST /repos/{id}/afk/start` stays
  bodyless, the same stance ADR-0021 used for `ultracode`. Naming:
  `afk_prompt` names both the repo column (fitting the existing
  `afk_`-prefixed family) and the settings key, which follows the
  `afk_budget_minutes` grammar — an AFK-only key, prefixed — not the
  `_afk`-suffix grammar of `spawn_model_default_afk`, an override of a base
  default that also exists; there is no base prompt to suffix.
- **Full replacement, tokens interpolated everywhere.** A non-empty override
  replaces the entire built-in text, not a clause at a time. `<N>` (reusing
  `gitx.NToken`) and `<BRANCH>` (the rendered claim branch) interpolate at
  **every** occurrence via `strings.ReplaceAll` — looser than a branch
  pattern's exactly-one `<N>`, since prose may reference either more than
  once. Both tokens are optional (`labctl issue view` resolves the run's own
  claimed issue with no argument), and an unknown token passes through as
  literal text rather than erroring.
- **WYSIWYG — lab never edits what the operator typed.** The incogni step-5
  sentence is **not** auto-appended to overrides; the textarea always shows
  exactly the run's prompt. Safe because the sentence was redundant defense,
  not the enforcement — incogni is mechanically enforced elsewhere (the
  pre-push hook, `agentapi`'s `sanitizeBody`, the provider's
  `SeedOpts.Incogni`) regardless of prompt text. The `ultracode` provider
  prepend (ADR-0021) is a **different**, provider-owned layer applied after
  prompt *source* resolves, and still fires on an overridden prompt.
- **The built-in default is API-served, never frontend-hardcoded.** `GET
  /api/v1/settings` gains read-only `afk_prompt_default` (built-in template,
  tokens un-interpolated); a repo `GET` gains read-only
  `afk_prompt_effective` (global override if set, else built-in,
  incogni-aware). Both Settings pages show the effective text as the empty
  textarea's placeholder, with a **Customize** button that copies it in for
  editing; clearing the textarea returns to inherit. The AFK strip is
  unchanged — Settings is the only display surface.
- **Validation is shape-only; the transcript is the audit trail.**
  Whitespace-only input normalizes to `NULL` (inherit). A 16 KiB cap, since
  the prompt travels as spawn argv. No content validation beyond that —
  instead a static Settings hint: a run is detected done only by an open PR
  on its branch, so a prompt that never opens one burns its budget, counts a
  failure, and three failures auto-pause the repo's AFK. No per-run
  snapshot: the seed prompt is already the run's first transcript message
  (ADR-0016 read-through).

## Status

Accepted. Extends ADR-0021's layering (base, then an AFK-specific layer) to
the seed prompt, and reuses its discipline of surfacing a source of truth
through the API rather than duplicating it in the frontend. Settled via issue
#52. One migration (0005, sqlite + postgres) adds nullable `repos.afk_prompt`;
the settings key `afk_prompt` is intentionally unseeded — absent means
inherit — matching the AFK-override precedent ADR-0021 set for
`spawn_model_default_afk`. Non-breaking: an operator who sets no override sees
today's `SeedPrompt` text, byte-for-byte (`TestSeedPrompt` still pins it).

## Considered options

- **Keep splicing the incogni sentence into overrides.** Rejected: the
  sentence never did the enforcing — the pre-push hook, `sanitizeBody`, and
  `SeedOpts.Incogni` do — so silently editing the operator's text buys no
  safety and breaks WYSIWYG.
- **A per-run prompt field on `POST /repos/{id}/afk/start`.** Rejected on the
  same ground ADR-0021 used for `ultracode`: AFK is unattended and
  defaults-only, so the endpoint stays bodyless.
- **Persist a per-run prompt snapshot on the `runs` row.** Rejected:
  read-through (ADR-0016) already puts the seed prompt in the transcript as
  the first message, so a snapshot would duplicate an existing record.

## Consequences

- `internal/afk/decide.go`: `SeedPrompt` gains an override param — empty
  keeps today's template, non-empty replaces it wholesale with
  `<N>`/`<BRANCH>` interpolated by `ReplaceAll`; incogni untouched by the
  override path. `TestSeedPrompt` extends accordingly.
- `internal/instance` gains a resolver alongside `ResolveSpawnOptions` —
  `repo.AFKPrompt ?? global afk_prompt ?? ""` — feeding
  `internal/afk/launch.go`'s `SeedPrompt` call for both AFK kinds.
- `internal/store`: migration 0005 (sqlite + postgres) adds `repos.afk_prompt`;
  settings gains the `afk_prompt` key, unseeded. `internal/httpapi`: settings
  `PATCH` validates trim + 16 KiB, `GET` adds `afk_prompt_default`; repos
  `PATCH`/`GET` add `afk_prompt` / `afk_prompt_effective`.
- `web/src/routes/Settings.tsx` and `RepoSettings.tsx`: a textarea in the
  existing "AFK defaults" card, placeholder-as-effective-text, a Customize
  copy-to-edit affordance.
