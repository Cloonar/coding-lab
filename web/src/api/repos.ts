import { request } from './core';

// --- M2: repositories ---

export type TrackerBinding = 'forge' | 'builtin';
export type ForgeKind = 'forgejo' | 'github' | 'none';
export type CloneStatus = 'cloning' | 'ready' | 'error';
/** The repo's host/container pane pick (issue #205). NOT NULL — no inherit state. */
export type Runner = 'container' | 'host';

export interface Repo {
  id: string;
  name: string;
  remote_url: string;
  credential_id: string | null;
  forge_credential_id: string | null;
  tracker_binding: TrackerBinding;
  forge_kind: ForgeKind;
  default_branch: string;
  /** Agent provider override (null = inherit the global provider_default). */
  provider: string | null;
  incogni: boolean;
  model_default: string | null;
  effort_default: string | null;
  /**
   * Remote-control override for manual spawns (issue #163). TRI-STATE: null =
   * inherit the global spawn_remote_default; `false` is an explicit "off", NOT
   * an absent value — never collapse it into the inherit branch.
   */
  remote_default: boolean | null;
  /** AFK-run overrides (null = inherit the global AFK default). */
  afk_provider_default: string | null;
  afk_model_default: string | null;
  afk_effort_default: string | null;
  /** Remote-control override for AFK runs (null = inherit); tri-state as above. */
  afk_remote_default: boolean | null;
  /** Provider spawn-option bag for AFK runs (null = inherit global). */
  afk_options: Record<string, string> | null;
  /** AFK seed-prompt override (issue #52); null = inherit afk_prompt_effective. */
  afk_prompt: string | null;
  /**
   * Read-only: what this repo would use if its own afk_prompt were empty —
   * the global override if set, else the built-in default (incogni-aware).
   * Tokens (<N>, <BRANCH>) are left un-interpolated. Always present.
   */
  afk_prompt_effective: string;
  git_author_name: string | null;
  git_author_email: string | null;
  afk_branch_pattern: string;
  manual_branch_prefix: string;
  afk_auto_enabled: boolean;
  consecutive_failures: number;
  budget_minutes: number | null;
  max_instances_override: number | null;
  clone_status: CloneStatus;
  clone_error: string | null;
  created_at: string;
  last_opened_at: string | null;
  /** Autoland (issue #181 / ADR-0048), per-repo and default off. */
  autoland_enabled: boolean;
  /** Fix-run spawn bound; default 2. */
  max_fix_attempts: number;
  /** On clean PASS: merge directly. Off: approve only, a human merges. Default true. */
  auto_merge: boolean;
  /** Lander run's provider override; null = inherit this repo's own provider. */
  lander_provider: string | null;
  /** Lander run's model override; null = inherit (resolved at lander launch). */
  lander_model: string | null;
  /** Lander run's effort override; null = inherit (resolved at lander launch). */
  lander_effort: string | null;
  /** Host (unsandboxed — full host access, break-glass) or container (rootless
   *  podman). NOT NULL; default host until the container preflight is proven
   *  (issue #205). */
  runner: Runner;
  /** Container-mode resource-limit overrides (issue #205); null = inherit the
   *  matching global container_memory/container_pids/container_nofile setting.
   *  Meaningless while runner is "host". */
  container_memory: string | null;
  container_pids: number | null;
  container_nofile: number | null;
  /** OCI image ref this repo's container sessions run in (issue #207); null =
   *  inherit the server's globally configured default dev image
   *  (--container-image). The server resolves and digest-pins the ref
   *  (https registries only) on save. */
  image_ref: string | null;
}

export interface CreateRepoRequest {
  remote_url: string;
  /** Omitted → the server derives it from the URL basename (sanitized). */
  name?: string;
  /** Git credential (ssh_key | https_token). Omitted → public remote. */
  credential_id?: string;
  /** Forge API credential (forge_token only). */
  forge_credential_id?: string;
  /** Omitted/"auto" → forge when a forge kind is detected, else builtin. */
  tracker_binding?: 'auto' | TrackerBinding;
  /** Agent provider override. Omitted → inherit the global provider_default. */
  provider?: string;
  incogni?: boolean;
}

/** PATCHable repo fields; null clears a nullable column back to the global default. */
export interface RepoPatch {
  name?: string;
  credential_id?: string | null;
  forge_credential_id?: string | null;
  tracker_binding?: TrackerBinding;
  default_branch?: string;
  /** null clears back to the global provider_default. */
  provider?: string | null;
  model_default?: string | null;
  effort_default?: string | null;
  /** Remote control for manual spawns; null clears back to spawn_remote_default. */
  remote_default?: boolean | null;
  /** AFK-run overrides; null or "" clears back to the global AFK default. */
  afk_provider_default?: string | null;
  afk_model_default?: string | null;
  afk_effort_default?: string | null;
  /** Remote control for AFK runs; null clears back to the global AFK default. */
  afk_remote_default?: boolean | null;
  afk_options?: Record<string, string> | null;
  /** null/""/whitespace-only clears back to afk_prompt_effective. */
  afk_prompt?: string | null;
  incogni?: boolean;
  git_author_name?: string | null;
  git_author_email?: string | null;
  afk_branch_pattern?: string;
  manual_branch_prefix?: string;
  afk_auto_enabled?: boolean;
  budget_minutes?: number | null;
  max_instances_override?: number | null;
  autoland_enabled?: boolean;
  max_fix_attempts?: number;
  auto_merge?: boolean;
  /** null clears back to inherit this repo's own provider. */
  lander_provider?: string | null;
  /** null/"" clears back to inherit; any non-empty string is accepted (no
   *  write-time catalog check — strictness is at lander launch, issue #189). */
  lander_model?: string | null;
  /** null/"" clears back to inherit; any non-empty string is accepted (no
   *  write-time catalog check — strictness is at lander launch, issue #189). */
  lander_effort?: string | null;
  /** NOT NULL (issue #205) — a PATCH must send a concrete value, never null. */
  runner?: Runner;
  /** Container-mode limit overrides (issue #205); null clears back to inherit
   *  the global default. */
  container_memory?: string | null;
  container_pids?: number | null;
  container_nofile?: number | null;
  /** null clears back to inherit the server's global default dev image
   *  (issue #207). A non-null value is resolved and digest-pinned server-side
   *  on save — that can 400 with a resolution error. */
  image_ref?: string | null;
}

/** 201 with clone_status "cloning" — the bare clone runs async, watch SSE. */
export function createRepo(req: CreateRepoRequest): Promise<Repo> {
  return request<Repo>('POST', '/repos', req);
}

export async function listRepos(): Promise<Repo[]> {
  const res = await request<{ repos: Repo[] }>('GET', '/repos');
  return res.repos;
}

export function getRepo(id: string): Promise<Repo> {
  return request<Repo>('GET', `/repos/${encodeURIComponent(id)}`);
}

export function updateRepo(id: string, patch: RepoPatch): Promise<Repo> {
  return request<Repo>('PATCH', `/repos/${encodeURIComponent(id)}`, patch);
}

/** 409s while the clone is still running unless force is set. */
export function deleteRepo(id: string, force = false): Promise<void> {
  const path = `/repos/${encodeURIComponent(id)}` + (force ? '?force=true' : '');
  return request<void>('DELETE', path);
}

/** Only valid from clone_status "error"; answers 202. */
export function retryClone(id: string): Promise<void> {
  return request<void>('POST', `/repos/${encodeURIComponent(id)}/clone/retry`);
}
