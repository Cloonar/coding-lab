import { request } from './core';

// --- issue #23: credential-gateway health ---

/** One monitored component's reachability (the lab-facing API, or the gateway). */
export interface OneCLIComponentHealth {
  configured: boolean;
  reachable: boolean;
  /** Omitted when empty. */
  url?: string;
  /**
   * The component's own self-reported health word, carried through verbatim
   * and only sent for the API component (the gateway probe is a bare TCP dial,
   * which has no word to report). Lab does not interpret it — `state` is
   * derived from reachability alone — so render it as-is or not at all.
   */
  status?: string;
  /** The raw dial error, omitted when empty — surfaced to the operator verbatim. */
  error?: string;
}

/**
 * `off` means the credential gateway integration is not configured at all —
 * that is normal, not a failure, and must never render as an error state.
 */
export type OneCLIHealthState = 'off' | 'ok' | 'degraded' | 'unreachable';

export interface OneCLIHealth {
  state: OneCLIHealthState;
  api: OneCLIComponentHealth;
  gateway: OneCLIComponentHealth;
}

/** GET /onecli/health: always answers 200, even when unreachable or off. */
export function getOneCLIHealth(): Promise<OneCLIHealth> {
  return request<OneCLIHealth>('GET', '/onecli/health');
}
