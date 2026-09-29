import { request } from './core';

// --- issue #39 / ADR-0068: Warpgate SSH bastion ---
//
// The non-HTTP sibling of the OneCLI credential gateway (onecli.ts): OneCLI
// fronts outbound HTTPS, Warpgate fronts outbound SSH. Same house pattern —
// an always-200 health endpoint folding `off`/`ok`/`degraded`/`unreachable`,
// and a per-repo picker over operator-defined resources (here: SSH targets
// instead of the OneCLI pool) — so this module mirrors onecli.ts's shapes
// field-for-field against the DESIGN.md HTTP contract.

/** The Warpgate admin API component of health. `version`/`authenticated` are
 *  only meaningful once `reachable` — the server omits them (and `error`)
 *  otherwise, so every field but `configured`/`reachable` stays optional. */
export interface WarpgateAPIHealth {
  configured: boolean;
  reachable: boolean;
  /** Omitted when empty; redacted the same way the OneCLI health's is. */
  url?: string;
  /** Warpgate's own reported version, once reachable. */
  version?: string;
  /** Whether the admin token was accepted — false with `reachable: true`
   *  means the token itself was rejected, a distinct failure from a dial. */
  authenticated?: boolean;
  /** The raw dial/request error, omitted when empty. */
  error?: string;
}

/** The Warpgate SSH listener component of health — a bare reachability probe,
 *  so it carries no version or authenticated flag, only an address and error. */
export interface WarpgateSSHHealth {
  configured: boolean;
  reachable: boolean;
  /** `host:port` a run reaches the listener at — omitted when empty. */
  addr?: string;
  /** The raw dial error, omitted when empty. */
  error?: string;
}

/**
 * `unpinned` means lab has never successfully scanned a host key (SSH not yet
 * reachable, or reachable but not yet scanned); `pinned` is the normal,
 * healthy state; `mismatch` means a rescan disagrees with the stored pin —
 * target-bearing spawns are blocked until an operator calls the accept
 * endpoint with one of the currently observed fingerprints; `unreachable`
 * means the scan itself failed (`error` carries why).
 */
export type WarpgateHostKeyState = 'unpinned' | 'pinned' | 'mismatch' | 'unreachable';

/** SHA256 host-key fingerprints, e.g. `SHA256:abcd…`. `pinned` is empty when
 *  unpinned; `observed` is empty when the last scan was unreachable. */
export interface WarpgateHostKey {
  state: WarpgateHostKeyState;
  pinned: string[];
  observed: string[];
  /** The scan error text, only present when `state === 'unreachable'`. */
  error?: string;
}

/**
 * `off` means the Warpgate integration is not configured at all — normal, not
 * a failure, and must never render as an error (mirrors `OneCLIHealthState`).
 */
export type WarpgateHealthState = 'off' | 'ok' | 'degraded' | 'unreachable';

/**
 * GET /warpgate/health's body. `hostKey` is omitted entirely when the SSH
 * listener itself is not configured — there is nothing to have scanned.
 */
export interface WarpgateHealth {
  state: WarpgateHealthState;
  api: WarpgateAPIHealth;
  ssh: WarpgateSSHHealth;
  hostKey?: WarpgateHostKey;
}

/** GET /warpgate/health: always answers 200, even when unreachable or off. */
export function getWarpgateHealth(): Promise<WarpgateHealth> {
  return request<WarpgateHealth>('GET', '/warpgate/health');
}

/**
 * POST /warpgate/host-key/accept: accepts a NEW host key observed during the
 * last scan, replacing the stored pin — the operator's explicit action after
 * verifying the fingerprint out of band. Returns the resulting `hostKey`
 * object so the caller can render the outcome without a second round trip.
 * 409 when SSH isn't configured or the fingerprint isn't among the currently
 * observed keys; 400 when the fingerprint is missing; 502 on a failed scan.
 */
export function acceptWarpgateHostKey(fingerprint: string): Promise<WarpgateHostKey> {
  return request<WarpgateHostKey>('POST', '/warpgate/host-key/accept', { fingerprint });
}

/**
 * One Warpgate SSH target as the per-repo picker sees it: named and described
 * for display, plus whether the repo's role currently carries it. Never
 * carries a credential — targets and their credentials are operator-defined
 * in Warpgate's own admin UI, which this client never creates, edits or reads.
 */
export interface RepoSSHTarget {
  id: string;
  name: string;
  description: string;
  assigned: boolean;
}

/**
 * GET /repos/{id}/warpgate/targets's body. Like `OneCLIPool`/`OneCLIGrants`,
 * `configured: false` is the normal "integration not set up" answer, not an
 * error, and `targets` is always an array (`[]` when empty).
 */
export interface RepoSSHTargets {
  configured: boolean;
  targets: RepoSSHTarget[];
}

/** GET /repos/{id}/warpgate/targets: the repo's view of every Warpgate SSH
 *  target, each flagged with whether the repo's role is currently assigned. */
export function listRepoSSHTargets(repoId: string): Promise<RepoSSHTargets> {
  return request<RepoSSHTargets>('GET', `/repos/${encodeURIComponent(repoId)}/warpgate/targets`);
}

/**
 * PUT /repos/{id}/warpgate/targets/{targetId} -> 204, no body: assigns the
 * repo's Warpgate role to the target, so its runs can reach `ssh <name>`
 * through the bastion. 409 when Warpgate isn't configured; 404 when the
 * target id isn't one of Warpgate's current SSH targets.
 */
export function assignRepoSSHTarget(repoId: string, targetId: string): Promise<void> {
  return request<void>(
    'PUT',
    `/repos/${encodeURIComponent(repoId)}/warpgate/targets/${encodeURIComponent(targetId)}`,
  );
}

/**
 * DELETE /repos/{id}/warpgate/targets/{targetId} -> 204, no body: unassigns
 * the repo's Warpgate role from the target.
 */
export function unassignRepoSSHTarget(repoId: string, targetId: string): Promise<void> {
  return request<void>(
    'DELETE',
    `/repos/${encodeURIComponent(repoId)}/warpgate/targets/${encodeURIComponent(targetId)}`,
  );
}
