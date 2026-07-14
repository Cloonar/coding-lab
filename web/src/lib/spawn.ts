// Spawn-form option resolution: pick a repo's provider catalog and the
// pre-selected model/effort. Preference order mirrors D12d layering — repo
// default over global settings default over the catalog's first option — and
// a candidate counts only when the catalog actually offers it (a stale
// persisted value must not pre-select something the form can't submit).

import type { Provider, ProviderModelOption, ProviderOption } from '../api';

/**
 * Skip-layer provider resolution (ADR-0030): the first candidate id that is a
 * registered provider wins; unknown/null/'' layers are skipped; nothing
 * matching falls back to the first registered provider (null when none).
 */
export function providerFor(
  providers: Provider[],
  ...candidateIds: (string | null | undefined)[]
): Provider | null {
  for (const id of candidateIds) {
    if (id == null || id === '') continue;
    const match = providers.find((p) => p.id === id);
    if (match !== undefined) return match;
  }
  return providers[0] ?? null;
}

/**
 * First candidate present in the options wins; none match → the first
 * option's value; empty catalog → ''.
 */
export function resolveSpawnOption(
  options: ProviderOption[],
  ...candidates: (string | null | undefined)[]
): string {
  for (const candidate of candidates) {
    if (candidate != null && candidate !== '' && options.some((o) => o.value === candidate)) {
      return candidate;
    }
  }
  return options[0]?.value ?? '';
}

/**
 * Client-side mirror of the server's per-model effort pass in layerSpawnDefault
 * (internal/instance/credential.go): the effort catalog belongs to the SELECTED
 * MODEL (issue #156) — effort support varies per model and codex does not
 * clamp, so an unsupported combo must never reach a spawn. The first candidate
 * present in the model's efforts wins; none matching falls back to the model's
 * reported default_effort when it is itself a member, else the first effort;
 * no efforts (or no model at all) resolves ''.
 */
export function resolveEffortOption(
  model: ProviderModelOption | undefined,
  ...candidates: (string | null | undefined)[]
): string {
  if (model === undefined) return '';
  const efforts = model.efforts ?? [];
  for (const candidate of candidates) {
    if (candidate != null && candidate !== '' && efforts.some((o) => o.value === candidate)) {
      return candidate;
    }
  }
  const reported = model.default_effort;
  if (reported != null && reported !== '' && efforts.some((o) => o.value === reported)) {
    return reported;
  }
  return efforts[0]?.value ?? '';
}

/**
 * Client-side mirror of the server's remote-control resolution (issue #163) —
 * the skip-layer walk the other resolvers above do, over a BOOLEAN knob:
 *
 *   manual: request → repo.remote_default → spawn_remote_default → false
 *   AFK:    repo.afk_remote_default → spawn_remote_default_afk
 *             → repo.remote_default → spawn_remote_default → false
 *
 * Callers pass the layers in that order; this only mirrors the server so the UI
 * can DISPLAY the effective value and know which picks are redundant to send.
 *
 * The trap this exists to avoid: every other knob here spells "unset" as ''
 * because '' is never a legal model/effort/provider id. `false` IS a legal value
 * for this one, so unset can only be null/undefined — a layer that is present
 * and `false` is an explicit OFF and WINS over any `true` further down the
 * chain. Nothing set anywhere resolves to false (remote control defaults off).
 */
export function resolveRemote(...candidates: (boolean | null | undefined)[]): boolean {
  for (const candidate of candidates) {
    if (candidate != null) return candidate;
  }
  return false;
}
