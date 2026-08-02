import { request } from './core';

// --- Repo imports (issue #261) ---

/**
 * A declared import: another registered lab repo whose code this repo's
 * instances may read as a read-only snapshot mounted at spawn. Directional
 * and consumer-declared — this repo (the one whose settings page you're on)
 * is the consumer; `id`/`name` name the repo being imported FROM.
 */
export interface RepoImport {
  id: string;
  name: string;
}

/** GET /repos/{id}/imports — declared imports, sorted by name. */
export async function listRepoImports(repoID: string): Promise<RepoImport[]> {
  const res = await request<{ imports: RepoImport[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/imports`,
  );
  return res.imports;
}

/**
 * POST /repos/{id}/imports {target_repo_id} -> 201 {id, name}. Idempotent:
 * declaring an already-declared target also 201s. 400s on self-import
 * ("imports: a repository cannot import itself") or an unknown target
 * ("imports: unknown target repository \"...\"").
 */
export function addRepoImport(repoID: string, targetRepoID: string): Promise<RepoImport> {
  return request<RepoImport>('POST', `/repos/${encodeURIComponent(repoID)}/imports`, {
    target_repo_id: targetRepoID,
  });
}

/** DELETE /repos/{id}/imports/{targetId} — immediate, 204; idempotent. */
export function removeRepoImport(repoID: string, targetRepoID: string): Promise<void> {
  return request<void>(
    'DELETE',
    `/repos/${encodeURIComponent(repoID)}/imports/${encodeURIComponent(targetRepoID)}`,
  );
}
