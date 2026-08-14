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

// --- issue #26: OneCLI dashboard exposure ---

/**
 * How the OneCLI dashboard is reached from a browser, as lab resolved it once
 * at startup out of its own flags: `port` (lab reverse-proxies the dashboard
 * on a second, authenticated listener), `subdomain` (the operator's own
 * reverse proxy fronts it and delegates auth to lab), or `off`.
 *
 * `off` means the dashboard is not exposed at all. That is the default lab and
 * a normal, healthy configuration — the same rule `OneCLIHealthState` states —
 * so it must never render as an error: a consumer hides its dashboard link
 * rather than showing one that cannot work.
 */
export type OneCLIDashboardMode = 'off' | 'port' | 'subdomain';

/**
 * The resolved exposure, in one body shape for every mode. The grant picker
 * (issue #25) is the intended consumer: it renders or hides its link-out from
 * this and nothing else, so no screen has to know the topology — and the link
 * integration itself lands there, not here.
 *
 * The login bounce-back (routes/Login.tsx) reads it for a second reason: port
 * mode's proxy sends an unauthenticated browser to `/login?next=<keyword>`,
 * and the destination ORIGIN is then composed from this authenticated answer
 * and never from the query string. That is what keeps the bounce structurally
 * incapable of being an open redirect — see `oneCLIDashboardGate` in
 * internal/httpapi/onecli_proxy.go for the whole argument.
 */
export interface OneCLIDashboardExposure {
  mode: OneCLIDashboardMode;
  /**
   * The browser-facing origin, e.g. `https://lab.example.com:8443`. Never
   * carries a trailing slash, so a path concatenates onto it directly.
   *
   * Omitted when the mode is `off`: nothing is exposed, so there is no URL to
   * report. Testing for the key and testing for a non-empty string therefore
   * reach the same conclusion — treat missing and empty alike.
   */
  url?: string;
}

/**
 * GET /onecli/dashboard: always answers 200, in every mode — `off` included,
 * which is a complete answer rather than a failure to report.
 */
export function getOneCLIDashboard(): Promise<OneCLIDashboardExposure> {
  return request<OneCLIDashboardExposure>('GET', '/onecli/dashboard');
}
