import { request } from './core';
import type { Repo } from './repos';
import type { Run } from './runs';
import type { IssueSummary } from './issues';

// --- M5: AFK engine ---

/**
 * POST /repos/{id}/afk/start: claim + launch the lowest claimable
 * ready-for-agent issue right now (manual AFK run). 202 {run}; 409 when the
 * repo is paused, over the cap, the provider is logged out, or
 * {"error":"no ready-for-agent issues to start"}. The documented contract is
 * the {run} envelope; a bare run body is tolerated (same tolerance precedent
 * as extractSpawnDefaults).
 */
export async function startAFK(repoID: string): Promise<Run> {
  const res = await request<{ run?: Run }>(
    'POST',
    `/repos/${encodeURIComponent(repoID)}/afk/start`,
  );
  return res.run ?? (res as unknown as Run);
}

/** PUT /repos/{id}/afk/auto {enabled}: persists afk_auto_enabled → 200 repo. */
export function setAFKAuto(repoID: string, enabled: boolean): Promise<Repo> {
  return request<Repo>('PUT', `/repos/${encodeURIComponent(repoID)}/afk/auto`, { enabled });
}

/**
 * POST /repos/{id}/afk/reset: consecutive_failures → 0 (the ONLY un-pause;
 * never touches the auto toggle) → 200 repo.
 */
export function resetAFK(repoID: string): Promise<Repo> {
  return request<Repo>('POST', `/repos/${encodeURIComponent(repoID)}/afk/reset`);
}

/**
 * The CLAIMABLE ready queue: ready-for-agent issues minus those whose claim
 * branch already exists (parked / in flight). GET /repos/{id}/ready with
 * ?claimable=1 — same {issues} envelope as the plain ready list, filtered
 * server-side; the SPA's '(N ready)' hint is this list's length.
 */
export async function listClaimableIssues(repoID: string): Promise<IssueSummary[]> {
  const res = await request<{ issues: IssueSummary[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/ready?claimable=1`,
  );
  return res.issues;
}
