import { request } from './core';

// --- M3: instances & runs ---

export type RunKind =
  | 'manual'
  | 'afk_manual'
  | 'afk_auto'
  | 'lander'
  | 'fix'
  | 'escalate'
  /** A Schedule's firing (issue #247 / ADR-0062): no issue, no claim. */
  | 'scheduled';
export type RunOutcome = 'active' | 'success' | 'death' | 'timeout' | 'stopped' | 'escalated';

/** One row of the runs table, as the API serializes it. */
export interface Run {
  id: string;
  repo_id: string;
  kind: RunKind;
  provider: string;
  issue_number: number | null;
  branch: string;
  worktree_path: string;
  session_name: string;
  /** User-set display title (issue #111); null = no override. */
  title: string | null;
  model: string;
  effort: string;
  /**
   * Whether this run spawned with remote control on (issue #163) — the RESOLVED
   * value, stamped at launch, never re-read from the layers afterwards. Always
   * false for a provider whose supports_remote is false (the server clamps it),
   * so `remote` alone never says "the Open affordance is gone": that gate is
   * `provider.supports_remote && !run.remote` (see lib/deepLink.ts).
   */
  remote: boolean;
  deep_link_url: string | null;
  started_at: string;
  budget_deadline: string | null;
  ended_at: string | null;
  outcome: RunOutcome;
  failure_reason: string | null;
  /**
   * Names of repo secrets this run's transcript has exposed (issue #108),
   * sorted. Only GET /runs/{id} (the chat header's source) populates this —
   * the runs list never does, to avoid an exposure lookup per row — so it is
   * optional rather than nullable: absent everywhere but the chat header.
   */
  exposed_secrets?: string[];
  /**
   * Commits on origin/<base> not yet in this run's branch (issue #149), from
   * GET /runs/{id} and GET /instances rows (which embed the run shape).
   * Omitted — not zero — whenever there's nothing to report: no commits
   * behind, the run has ended, or the count couldn't be computed, so absence
   * is the "hide the chip" signal throughout the UI, same convention as
   * `exposed_secrets`.
   */
  commits_behind?: number;
}

/** The chat tailer's per-instance conversational state (issue #7). */
export type ConversationState = 'idle' | 'working' | 'needs_input' | 'question' | 'ended' | '';

/** Run JSON + liveness derived from tmux + the in-flight deep-link capture. */
export interface Instance extends Run {
  repo_name: string;
  live: boolean;
  connecting: boolean;
  state: ConversationState;
}

export interface StartInstanceRequest {
  label?: string;
  /** Per-spawn provider pick (strict: an unknown id is a 400). */
  provider?: string;
  model?: string;
  effort?: string;
  /**
   * Per-spawn remote-control pick (issue #163). OMITTED = let the server
   * resolve the layers (repo.remote_default → spawn_remote_default → false);
   * `false` is a real, explicit pick, so it must ride the request whenever the
   * operator turned an inherited-on default off.
   */
  remote?: boolean;
  /**
   * The operator's first chat message (issue #96): delivered on the spawn argv,
   * so it needs no post-spawn send — the queued-message hop (issue #41) that
   * deadlocked on a lazily-created transcript is retired. Omitted for a plain
   * spawn with an empty composer.
   */
  first_message?: string;
}

/** 201 run JSON | 409 (cap / provider logged out / repo not ready) | 400. */
export function startInstance(repoID: string, req: StartInstanceRequest): Promise<Run> {
  return request<Run>('POST', `/repos/${encodeURIComponent(repoID)}/instances`, req);
}

export async function listInstances(): Promise<Instance[]> {
  const res = await request<{ instances: Instance[] }>('GET', '/instances');
  return res.instances;
}

/** Guarded teardown: "removed" (clean+merged) or "parked" (work preserved). */
export function stopInstance(session: string): Promise<{ outcome: 'removed' | 'parked' }> {
  return request<{ outcome: 'removed' | 'parked' }>(
    'DELETE',
    `/instances/${encodeURIComponent(session)}`,
  );
}

export function stopAll(repoID: string): Promise<{ stopped: number }> {
  return request<{ stopped: number }>('POST', `/repos/${encodeURIComponent(repoID)}/stop-all`);
}

export async function listRuns(opts: { repo?: string; limit?: number } = {}): Promise<Run[]> {
  const params = new URLSearchParams();
  if (opts.repo !== undefined && opts.repo !== '') params.set('repo', opts.repo);
  if (opts.limit !== undefined) params.set('limit', String(opts.limit));
  const query = params.toString();
  const res = await request<{ runs: Run[] }>('GET', '/runs' + (query === '' ? '' : `?${query}`));
  return res.runs;
}

export function getRun(id: string): Promise<Run> {
  return request<Run>('GET', `/runs/${encodeURIComponent(id)}`);
}

export interface RunPatch {
  /** Display title override (issue #111); null (or empty after trim) clears it. */
  title?: string | null;
}

/** 200 with the full updated run; the server trims and rejects >120 chars. */
export function updateRun(id: string, patch: RunPatch): Promise<Run> {
  return request<Run>('PATCH', `/runs/${encodeURIComponent(id)}`, patch);
}
