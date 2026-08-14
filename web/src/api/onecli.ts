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

// --- issue #25: per-repo secret grant picker ---

/**
 * One resource in the lab's single OneCLI project pool — a stored secret or
 * an app connection, named and typed for display but never carrying a value:
 * the OneCLI dashboard is the only place a value is created or edited (see
 * `OneCLIDashboardExposure`), and this client grows no such screen.
 */
export interface OneCLIPoolEntry {
  id: string;
  name: string;
  provider: string;
}

/**
 * GET /onecli/pool's body: the whole lab-wide pool the grant picker offers
 * an operator to attach, split by resource kind. `secrets` and `connections`
 * are always present as arrays — `[]` when the pool is empty, never absent
 * or null — so a consumer never has to guard against a missing key.
 *
 * `configured: false` means the credential-gateway integration is not set up
 * in lab at all (the same "off" rule `OneCLIHealthState` states): a normal,
 * healthy answer, never an error. That is one of three states a consumer
 * must render distinctly and must not conflate: unconfigured (this, 200 with
 * `configured: false`), unreachable (a non-2xx status, thrown as `ApiError`
 * by `request`), and an empty pool (200, `configured: true`, both arrays
 * empty — the gateway is reachable and simply has nothing in it yet).
 */
export interface OneCLIPool {
  configured: boolean;
  secrets: OneCLIPoolEntry[];
  connections: OneCLIPoolEntry[];
}

/** GET /onecli/pool: the lab-wide pool of grantable secrets and connections. */
export function getOneCLIPool(): Promise<OneCLIPool> {
  return request<OneCLIPool>('GET', '/onecli/pool');
}

/** Which half of the pool a grant references — mirrors `OneCLIPool`'s two arrays. */
export type OneCLIGrantKind = 'secrets' | 'connections';

/**
 * One pool resource attached to a repo's OneCLI agent identity — the gateway
 * sense of "grant" (CONTEXT.md's flagged ambiguity): never the read-only
 * import's repo→repo code-read permission, which is always written out in
 * full as "read-only import."
 */
export interface OneCLIGrant {
  kind: OneCLIGrantKind;
  id: string;
  name: string;
}

/**
 * GET /repos/{id}/onecli/grants's body: the repo's current grant set. Like
 * `OneCLIPool`, `configured: false` is the normal, healthy "integration not
 * set up" answer rather than an error, and `grants` is always an array.
 */
export interface OneCLIGrants {
  configured: boolean;
  grants: OneCLIGrant[];
}

/** GET /repos/{id}/onecli/grants: the repo's currently-attached pool resources. */
export function listRepoOneCLIGrants(repoId: string): Promise<OneCLIGrants> {
  return request<OneCLIGrants>('GET', `/repos/${encodeURIComponent(repoId)}/onecli/grants`);
}

/**
 * PUT /repos/{id}/onecli/grants/{kind}/{resourceId} -> 204, no body: attaches
 * one pool resource to the repo's OneCLI agent identity, idempotently (a
 * repeat attach of an already-granted resource still answers 204).
 */
export function attachRepoOneCLIGrant(
  repoId: string,
  kind: OneCLIGrantKind,
  resourceId: string,
): Promise<void> {
  return request<void>(
    'PUT',
    `/repos/${encodeURIComponent(repoId)}/onecli/grants/${encodeURIComponent(kind)}/${encodeURIComponent(resourceId)}`,
  );
}

/**
 * DELETE /repos/{id}/onecli/grants/{kind}/{resourceId} -> 204, no body:
 * detaches one pool resource from the repo's OneCLI agent identity.
 */
export function detachRepoOneCLIGrant(
  repoId: string,
  kind: OneCLIGrantKind,
  resourceId: string,
): Promise<void> {
  return request<void>(
    'DELETE',
    `/repos/${encodeURIComponent(repoId)}/onecli/grants/${encodeURIComponent(kind)}/${encodeURIComponent(resourceId)}`,
  );
}
