// Open-affordance / connecting-state logic for instance rows (ADR-0017). The
// exact captured deep link always wins; while the background registry poll runs
// the row shows a "connecting…" pulse; a finished capture with no link falls
// back to the provider's generic web link (URL + tooltip from the providers
// API — never hardcoded here). A provider with no web surface (no fallback)
// instead offers a copyable `tmux attach` command, since its session is driven
// from a terminal on the lab host.
//
// On top of that sits the remote-control gate (issue #163): a run of a
// remote-capable provider that spawned WITHOUT remote control has no web session
// at all, so it gets no affordance whatsoever — see openState below.

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

/**
 * Whether the run's provider HAS a remote-control knob (issue #163), as the
 * same tri-state providerOpen uses: false = loaded and it has no such knob;
 * undefined = the providers list has not loaded, so nothing is known yet. An
 * unknown provider id reads as false — no knob we can prove, so nothing to gate.
 */
export function providerSupportsRemote(
  providers: Provider[] | undefined,
  providerID: string,
): boolean | undefined {
  if (providers === undefined) return undefined;
  return providers.find((p) => p.id === providerID)?.supports_remote ?? false;
}

/**
 * The remote-control gate itself (issue #163): does this run lack a web session
 * that a remote-capable provider would otherwise have registered?
 *
 * Factored out because TWO surfaces must agree on it and must never drift: the
 * Open affordance (openState below) and RunChat's "answer it elsewhere" hint,
 * which names the provider's web host in PROSE. Both send the operator to the
 * same web session, so both must fall silent for a run that never registered
 * one — a hint naming that host beside a hidden Open button is the same broken
 * promise, just spelled in words.
 *
 * The gate is CAPABILITY-SCOPED, never a bare `!remote`: a provider without the
 * knob (supports_remote false) has its runs' `remote` clamped to false by the
 * server, so a bare check would silently strip the tmux-attach affordance that
 * is such a provider's only way in. Hence:
 *
 *   gated  ⟺  supportsRemote && !instance.remote
 *
 * `supportsRemote` undefined (providers still loading) gates nothing — the same
 * "never guess" rule the `unknown` state follows.
 */
export function remoteGated(
  instance: { remote: boolean },
  supportsRemote: boolean | undefined,
): boolean {
  return supportsRemote === true && !instance.remote;
}

/**
 * The open affordance for one instance/run, or null for "render nothing".
 *
 * The remote-control gate (issue #163). The exact deep link exists only because
 * a remote-controlled session registered itself with the provider's web bridge;
 * with remote control off there IS no such session, so neither the link nor the
 * provider's generic web fallback may be offered — the fallback would point the
 * operator at a session that does not exist. See remoteGated above.
 */
export function openState(
  instance: {
    connecting: boolean;
    deep_link_url: string | null;
    session_name: string;
    remote: boolean;
  },
  fallback: ProviderFallbackOpen | null | undefined,
  supportsRemote: boolean | undefined,
): OpenState | null {
  if (remoteGated(instance, supportsRemote)) return null;
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
