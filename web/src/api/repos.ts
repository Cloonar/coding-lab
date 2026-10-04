import { request } from './core';

// --- M2: repositories ---

export type TrackerBinding = 'forge' | 'builtin';
export type ForgeKind = 'forgejo' | 'github' | 'none';
export type CloneStatus = 'cloning' | 'ready' | 'error';
/**
 * Where an instance's pane command executes (issue #205). A repo's own pick is
 * nullable — null means "inherit the global runner default" (the `runner_default`
 * setting) — but a concrete Runner is always one of these two.
 */
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
  /** Lander run's model override; null = inherit the global lander default,
   *  then this repo's base chain (resolved at lander launch). */
  lander_model: string | null;
  /** Lander run's effort override; null = inherit the global lander default,
   *  then this repo's base chain (resolved at lander launch). */
  lander_effort: string | null;
  /** Host (unsandboxed — full host access, break-glass) or container (rootless
   *  podman) (issue #205). null = inherit the global `runner_default` setting
   *  (live: a change there moves every inheriting repo's next spawn); newly
   *  created repos start null, an existing repo keeps its explicit pin.
   *  The effective runner is `runner ?? settings.runner_default`. */
  runner: Runner | null;
  /** Container-mode resource-limit overrides (issue #205); null = inherit the
   *  matching global container_memory/container_pids/container_nofile setting.
   *  Meaningless while runner is "host". */
  container_memory: string | null;
  container_pids: number | null;
  container_nofile: number | null;
  /** OCI image ref this repo's container sessions run in (issue #207); null =
   *  inherit the global default dev image (the `dev_image_default` setting,
   *  else the server's --container-image). The server resolves and digest-pins
   *  the ref (https registries only) on save. */
  image_ref: string | null;
  /**
   * What lab knows about the repo right now without asking anyone (issue
   * #61): built from stored state and from the outcome of the most recent
   * fetch and tracker read — never a forge request or a git network
   * operation per page view. Present on every repo response, list included.
   */
  summary: RepoSummary;
}

// --- Readiness (issue #61) ---

/** One check's verdict. `pending` = still settling (a clone in flight). */
export type ReadinessState = 'passing' | 'failing' | 'pending';

/** The six checks, in the server's canonical order. */
export type ReadinessCheckID =
  'clone' | 'git_credential' | 'tracker' | 'agent_login' | 'dev_image' | 'imports';

/**
 * Where the fix for a failing check lives. `repo` = this repo's settings page
 * (`section` is a repo-settings slug, `field` the PATCH key of the offending
 * field); `global` = global Settings (`section` is its slug); `credentials` =
 * the Credentials page, where the agent login cards live.
 */
export interface ReadinessFix {
  scope: 'repo' | 'global' | 'credentials';
  section?: string;
  field?: string;
}

export interface ReadinessCheck {
  id: ReadinessCheckID;
  state: ReadinessState;
  /** One operator-facing sentence: what is wrong, or what was verified. */
  detail: string;
  /** Present on a failing clone check: POST /repos/{id}/clone/retry fixes it. */
  action?: 'retry_clone';
  /** Present on a failing check that a setting fixes. Never with `action`. */
  fix?: ReadinessFix;
}

/**
 * Whether a run can start in this repo right now. `checks` is in canonical
 * order and holds ONLY the checks lab can evaluate from what it already
 * knows: one it cannot evaluate is left out, never reported as passing (so
 * the list may be shorter than six, and `dev_image` only exists while the
 * effective Runner is `container`). `state` is the roll-up: failing if any
 * check fails, else pending if any is pending, else passing.
 */
export interface Readiness {
  state: ReadinessState;
  checks: ReadinessCheck[];
}

export interface RepoSummary {
  /**
   * The claimable count (ready queue minus claimed and blocked issues) as
   * last computed; null = not known yet. A forge-bound repo's count is the
   * engine's or a strip's most recent read, never a fresh forge call.
   */
  claimable: number | null;
  /** Open issue count as last read; null = not known yet. */
  open_issues: number | null;
  readiness: Readiness;
}

// --- Inherited values (issue #61) ---

/**
 * What each overridable repo field resolves to when the repo's OWN value is
 * null — computed by the server with the same resolvers the spawn path uses,
 * so the settings page never derives an effective value a second way. Keys
 * are the repo field names. A null entry = the chain below could not be
 * resolved (show the field as inherited without naming a value).
 */
export interface RepoInherited {
  /** Provider ids. */
  provider: string | null;
  afk_provider_default: string | null;
  lander_provider: string | null;
  /** Model / effort ids of the provider the field belongs to. */
  model_default: string | null;
  effort_default: string | null;
  afk_model_default: string | null;
  afk_effort_default: string | null;
  lander_model: string | null;
  lander_effort: string | null;
  remote_default: boolean | null;
  afk_remote_default: boolean | null;
  /** The option bag that applies while the repo has none of its own. */
  afk_options: Record<string, string> | null;
  budget_minutes: number | null;
  /** The instance cap that applies without a repo override. */
  max_instances_override: number | null;
  git_author_name: string | null;
  git_author_email: string | null;
  runner: Runner | null;
  /** '' = no dev image is configured anywhere below the repo. */
  image_ref: string | null;
  container_memory: string | null;
  container_pids: number | null;
  container_nofile: number | null;
}

/**
 * Draft values to resolve against instead of the saved repo: the same keys
 * and null/value semantics as the PATCH (absent = the saved value, null =
 * inherit). Only fields other fields' chains read are accepted.
 */
export type RepoInheritedDrafts = Pick<
  RepoPatch,
  | 'provider'
  | 'model_default'
  | 'effort_default'
  | 'remote_default'
  | 'afk_provider_default'
  | 'afk_model_default'
  | 'afk_effort_default'
  | 'lander_provider'
>;

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
  /** null clears back to inherit the global `runner_default` setting; the
   *  server rejects any other non-enum value. */
  runner?: Runner | null;
  /** Container-mode limit overrides (issue #205); null clears back to inherit
   *  the global default. */
  container_memory?: string | null;
  container_pids?: number | null;
  container_nofile?: number | null;
  /** null clears back to inherit the global default dev image (issue #207).
   *  A non-null value is resolved and digest-pinned server-side on save — that
   *  can 400 with a resolution error. */
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

/**
 * GET /repos/{id}/readiness — the readiness report in one call (issue #61).
 * The same report rides every repo response as `summary.readiness`; this
 * endpoint refreshes it alone. It reads only what lab already holds.
 */
export function getRepoReadiness(id: string): Promise<Readiness> {
  return request<Readiness>('GET', `/repos/${encodeURIComponent(id)}/readiness`);
}

/**
 * POST /repos/{id}/inherited — the inherited value of every overridable field
 * (issue #61). A dry run, never a write: `drafts` are unsaved edits to
 * resolve against, so a dependent field's "inherited" value follows an edit
 * of the field above it (the AFK agent follows the agent, a model follows its
 * agent) before anything is saved. An empty body resolves the saved repo.
 */
export function getRepoInherited(
  id: string,
  drafts: RepoInheritedDrafts = {},
): Promise<RepoInherited> {
  return request<RepoInherited>('POST', `/repos/${encodeURIComponent(id)}/inherited`, drafts);
}

/** Only valid from clone_status "error"; answers 202. */
export function retryClone(id: string): Promise<void> {
  return request<void>('POST', `/repos/${encodeURIComponent(id)}/clone/retry`);
}
