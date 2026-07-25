import { request } from './core';
import { coerceBool } from './settings';

// --- M3: parked ---

/** Work the guarded teardown preserved: a managed branch no session owns. */
export interface ParkedEntry {
  branch: string;
  /** "" for a bare branch without a worktree. */
  worktree_path: string;
  dirty: boolean;
  commits_ahead: number;
  unpushed: number;
}

export async function listParked(repoID: string): Promise<ParkedEntry[]> {
  const res = await request<{ parked: ParkedEntry[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/parked`,
  );
  return res.parked;
}

/**
 * UNGUARDED destruction: kills the session if any, force-removes the
 * worktree, force-deletes the branch — dirty or not, merged or not. The UI
 * gates this behind the typed-confirm dialog. Answers 204.
 */
export function discardParked(repoID: string, branch: string): Promise<void> {
  return request<void>('POST', `/repos/${encodeURIComponent(repoID)}/parked/discard`, { branch });
}

// --- M3: spawn defaults from settings ---

export interface SpawnDefaults {
  provider?: string;
  model?: string;
  effort?: string;
  /**
   * The base remote-control default (issue #163). Absent = the key is unset;
   * `false` is a real value, so the composer must key "unset" on absence, never
   * on falsiness.
   */
  remote?: boolean;
}

/**
 * Pulls the spawn-default keys out of a GET /settings payload, tolerating
 * both a flat {key: value} object and a {settings: {key: value}} envelope —
 * the settings surface is an M4 contract, M3 only peeks at it.
 */
export function extractSpawnDefaults(raw: unknown): SpawnDefaults {
  if (typeof raw !== 'object' || raw === null) return {};
  let map = raw as Record<string, unknown>;
  const nested = map['settings'];
  if (typeof nested === 'object' && nested !== null) map = nested as Record<string, unknown>;
  const out: SpawnDefaults = {};
  const provider = map['provider_default'];
  if (typeof provider === 'string' && provider !== '') out.provider = provider;
  const model = map['spawn_model_default'];
  if (typeof model === 'string' && model !== '') out.model = model;
  const effort = map['spawn_effort_default'];
  if (typeof effort === 'string' && effort !== '') out.effort = effort;
  const remote = coerceBool(map['spawn_remote_default']);
  if (typeof remote === 'boolean') out.remote = remote;
  return out;
}

/** Best-effort: an absent or failing settings endpoint yields no defaults. */
export async function getSpawnDefaults(): Promise<SpawnDefaults> {
  try {
    return extractSpawnDefaults(await request<unknown>('GET', '/settings'));
  } catch {
    return {};
  }
}
