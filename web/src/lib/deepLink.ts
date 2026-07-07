// Open-affordance / connecting-state logic for instance rows (ADR-0017). The
// exact captured deep link always wins; while the background registry poll runs
// the row shows a "connecting…" pulse; a finished capture with no link falls
// back to the provider's generic web link (URL + tooltip from the providers
// API — never hardcoded here). A provider with no web surface (no fallback)
// instead offers a copyable `tmux attach` command, since its session is driven
// from a terminal on the lab host.

import type { Provider, ProviderFallbackOpen } from '../api';

/** Tooltip on the tmux-attach affordance (provider-neutral). */
export const ATTACH_TITLE =
  'This provider has no web session — attach from a terminal on the lab host';

export type OpenState =
  | { kind: 'unknown' } // provider metadata not resolved yet (providers list still loading)
  | { kind: 'connecting' }
  | { kind: 'link'; url: string; exact: true }
  | { kind: 'link'; url: string; exact: false; title: string }
  | { kind: 'attach'; command: string; title: string };

/**
 * The provider's fallback-open metadata by id (ADR-0017), as a tri-state so the
 * row never guesses while the providers list is still loading:
 *   - a {url,title}  → the provider has a web surface (render the generic link);
 *   - null           → the provider is loaded and has NO web surface (tmux-attach);
 *   - undefined      → the providers list has not loaded yet (unknown).
 * Pass the raw resource value (Provider[] | undefined); an empty array is a
 * loaded-but-empty list, not "loading".
 */
export function providerOpen(
  providers: Provider[] | undefined,
  providerID: string,
): ProviderFallbackOpen | null | undefined {
  if (providers === undefined) return undefined;
  return providers.find((p) => p.id === providerID)?.fallback_open ?? null;
}

export function openState(
  instance: { connecting: boolean; deep_link_url: string | null; session_name: string },
  fallback: ProviderFallbackOpen | null | undefined,
): OpenState {
  // A captured URL beats a still-set connecting flag: the capture wrote the
  // link the moment it landed, the flag clears a beat later. Neither this nor
  // the connecting pulse depends on the providers list.
  if (instance.deep_link_url !== null && instance.deep_link_url !== '') {
    return { kind: 'link', url: instance.deep_link_url, exact: true };
  }
  if (instance.connecting) return { kind: 'connecting' };
  // Providers not loaded yet: don't guess an affordance — render nothing until
  // the capability is known (avoids flashing tmux-attach on a web provider).
  if (fallback === undefined) return { kind: 'unknown' };
  if (fallback !== null && fallback.url !== '') {
    return { kind: 'link', url: fallback.url, exact: false, title: fallback.title };
  }
  // Loaded and no web surface: the session is reachable only from a terminal.
  return {
    kind: 'attach',
    command: `tmux attach -t ${instance.session_name}`,
    title: ATTACH_TITLE,
  };
}
