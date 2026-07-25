import { request } from './core';

// --- Repo secrets (issue #104) ---

/**
 * A per-repo secret's metadata — write-only, like Credential: the value is
 * never fetched or shown anywhere on this client. Agents consume it via
 * `labctl secret exec`, not through this API.
 */
export interface RepoSecret {
  id: string;
  name: string;
  description: string;
  created_at: string;
  updated_at: string;
  /**
   * The sticky exposure flag (issue #108): both null means never exposed
   * since creation or the last rotation; both set names the run whose
   * transcript first surfaced the value and when. Rotating clears both.
   */
  exposed_run_id: string | null;
  exposed_at: string | null;
}

/** GET /repos/{id}/secrets — metadata only, ordered by name; never a value. */
export async function listRepoSecrets(repoID: string): Promise<RepoSecret[]> {
  const res = await request<{ secrets: RepoSecret[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/secrets`,
  );
  return res.secrets;
}

/**
 * POST /repos/{id}/secrets {name, description, value} -> 201 metadata.
 * `name` must match ^[A-Z][A-Z0-9_]*$ (the env-var grammar); 400 otherwise.
 * 409 on a name collision within the repo.
 */
export function createRepoSecret(
  repoID: string,
  name: string,
  description: string,
  value: string,
): Promise<RepoSecret> {
  return request<RepoSecret>('POST', `/repos/${encodeURIComponent(repoID)}/secrets`, {
    name,
    description,
    value,
  });
}

/**
 * PATCH /repos/{id}/secrets/{sid} {value} — rotate only, wholesale: the old
 * value is never fetched, so there is nothing to prefill.
 */
export function rotateRepoSecret(
  repoID: string,
  secretID: string,
  value: string,
): Promise<RepoSecret> {
  return request<RepoSecret>(
    'PATCH',
    `/repos/${encodeURIComponent(repoID)}/secrets/${encodeURIComponent(secretID)}`,
    { value },
  );
}

/** DELETE /repos/{id}/secrets/{sid} — immediate, 204; secrets are never referenced. */
export function deleteRepoSecret(repoID: string, secretID: string): Promise<void> {
  return request<void>(
    'DELETE',
    `/repos/${encodeURIComponent(repoID)}/secrets/${encodeURIComponent(secretID)}`,
  );
}
