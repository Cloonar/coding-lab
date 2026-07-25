import { request } from './core';

// --- M6: change requests ---

/** Persisted CR state. The list filter adds the synthetic "all". */
export type CRState = 'open' | 'merged' | 'closed';
export type CRStateFilter = CRState | 'all';

/** List-row shape (§8.1): branches, closes refs and the merge record. */
export interface CRSummary {
  number: number;
  title: string;
  state: CRState;
  head_branch: string;
  base_branch: string;
  closes: number[];
  created_at: string;
  merged_at: string | null;
  merge_commit: string | null;
}

/**
 * Full CR: body plus the diff computed live from the bare repo. `diff` is
 * omitted — with `note` explaining why — when the head branch no longer
 * exists; `diff_truncated` flags the server's ~1MiB output bound.
 */
export interface CRDetail extends CRSummary {
  body: string;
  diff?: string;
  diff_truncated?: boolean;
  note?: string;
}

/** CR list, filtered by state server-side (open|merged|closed|all). */
export async function listCRs(repoID: string, state: CRStateFilter = 'open'): Promise<CRSummary[]> {
  const res = await request<{ crs: CRSummary[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/crs?state=${encodeURIComponent(state)}`,
  );
  return res.crs;
}

/** Full CR with the live diff (or a note when the head branch is gone). */
export function getCR(repoID: string, number: number): Promise<CRDetail> {
  return request<CRDetail>('GET', `/repos/${encodeURIComponent(repoID)}/crs/${number}`);
}

/**
 * POST .../merge → 200 {cr}. Whether the merge fast-forwards or builds a
 * merge commit is the server's concern. A push rejection (protected branch,
 * non-ff race), a non-open CR or a missing head branch answers 409 whose
 * {"error"} body carries the actionable git/hook message — the UI surfaces
 * it verbatim. A bare cr body is tolerated (same precedent as startAFK).
 */
export async function mergeCR(repoID: string, number: number): Promise<CRSummary> {
  const res = await request<{ cr?: CRSummary }>(
    'POST',
    `/repos/${encodeURIComponent(repoID)}/crs/${number}/merge`,
  );
  return res.cr ?? (res as unknown as CRSummary);
}

/** Builtin-bound repos only — 409 {"error"} on forge-bound or non-open CRs. */
export async function closeCR(repoID: string, number: number): Promise<CRSummary> {
  const res = await request<{ cr?: CRSummary }>(
    'POST',
    `/repos/${encodeURIComponent(repoID)}/crs/${number}/close`,
  );
  return res.cr ?? (res as unknown as CRSummary);
}
