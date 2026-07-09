// Spawn-form option resolution: pick a repo's provider catalog and the
// pre-selected model/effort. Preference order mirrors D12d layering — repo
// default over global settings default over the catalog's first option — and
// a candidate counts only when the catalog actually offers it (a stale
// persisted value must not pre-select something the form can't submit).

import type { Provider, ProviderOption } from '../api';

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
