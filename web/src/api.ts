// Typed fetch client for the lab operator API (/api/v1).
//
// Every mutating request carries the `X-Lab-Csrf: 1` header the server's CSRF
// middleware requires for ambient-credential (cookie) auth. Error responses
// are always JSON envelopes `{"error": "<message>"}`; they surface as
// ApiError(status, message) so the real message reaches the operator.

const BASE = '/api/v1';

const MUTATING_METHODS = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

/** Maps any thrown value to operator-facing text (v0 voice, kept verbatim). */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof TypeError) return 'Network error — is lab still running?';
  return 'Something went wrong — please try again.';
}

type UnauthorizedHandler = () => void;

let unauthorizedHandler: UnauthorizedHandler | null = null;

/**
 * Registers a callback invoked whenever any API call returns 401 — the app
 * uses it to refresh the auth state so route guards bounce to /login.
 */
export function setUnauthorizedHandler(handler: UnauthorizedHandler | null): void {
  unauthorizedHandler = handler;
}

function errorFromBody(body: unknown): string | null {
  if (typeof body === 'object' && body !== null) {
    const message = (body as { error?: unknown }).error;
    if (typeof message === 'string' && message !== '') return message;
  }
  return null;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (MUTATING_METHODS.has(method)) headers['X-Lab-Csrf'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  const res = await fetch(BASE + path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  if (!res.ok) {
    // The handler refreshes the auth state, i.e. it refetches /auth/state.
    // Exempting that path makes the refresh loop-proof by construction: even
    // an auth gateway that answers 401 to every request (expired forward-auth
    // session) cannot trigger refetch-on-401 of the very endpoint that 401s.
    // authState()'s caller sees the ApiError(401) and settles as errored.
    if (res.status === 401 && unauthorizedHandler && path !== '/auth/state') {
      unauthorizedHandler();
    }
    let message = `Request failed (${res.status})`;
    try {
      const parsed = errorFromBody(await res.json());
      if (parsed !== null) message = parsed;
    } catch {
      // Non-JSON error body — keep the generic message.
    }
    throw new ApiError(res.status, message);
  }

  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (text === '') return undefined as T;
  return JSON.parse(text) as T;
}

// --- M1 endpoint payloads ---

export interface AuthState {
  setup_required: boolean;
  authenticated: boolean;
  username?: string;
}

export interface Me {
  username: string;
}

// --- M1 endpoint helpers ---

/** Public: whether first-run setup is needed and whether a session exists. */
export function authState(): Promise<AuthState> {
  return request<AuthState>('GET', '/auth/state');
}

/** First-run admin account creation; only valid while `users` is empty. */
export function setup(username: string, password: string): Promise<void> {
  return request<void>('POST', '/auth/setup', { username, password });
}

export function login(username: string, password: string, remember: boolean): Promise<void> {
  return request<void>('POST', '/auth/login', { username, password, remember });
}

export function logout(): Promise<void> {
  return request<void>('POST', '/auth/logout');
}

export function me(): Promise<Me> {
  return request<Me>('GET', '/me');
}

// --- M2: credentials ---

export type CredentialKind = 'ssh_key' | 'https_token' | 'forge_token';

export interface SshKeyPayload {
  private_key: string;
  passphrase?: string;
}

export interface HttpsTokenPayload {
  username: string;
  token: string;
}

export interface ForgeTokenPayload {
  host: string;
  token: string;
}

export type CredentialPayload = SshKeyPayload | HttpsTokenPayload | ForgeTokenPayload;

/** Credential metadata — the payload is write-only and never read back. */
export interface Credential {
  id: string;
  name: string;
  kind: CredentialKind;
  created_at: string;
  updated_at: string;
}

export interface CredentialListItem extends Credential {
  /** True while any repo references this credential (either FK column). */
  referenced: boolean;
}

export interface CredentialPatch {
  name?: string;
  /** Rotates the secret. Kind is immutable; the old payload is never shown. */
  payload?: CredentialPayload;
}

export function createCredential(
  name: string,
  kind: CredentialKind,
  payload: CredentialPayload,
): Promise<Credential> {
  return request<Credential>('POST', '/credentials', { name, kind, payload });
}

export async function listCredentials(): Promise<CredentialListItem[]> {
  const res = await request<{ credentials: CredentialListItem[] }>('GET', '/credentials');
  return res.credentials;
}

export function updateCredential(id: string, patch: CredentialPatch): Promise<Credential> {
  return request<Credential>('PATCH', `/credentials/${encodeURIComponent(id)}`, patch);
}

/** 409s with the server's message while the credential is referenced. */
export function deleteCredential(id: string): Promise<void> {
  return request<void>('DELETE', `/credentials/${encodeURIComponent(id)}`);
}

// --- M2: repositories ---

export type TrackerBinding = 'forge' | 'builtin';
export type ForgeKind = 'forgejo' | 'github' | 'none';
export type CloneStatus = 'cloning' | 'ready' | 'error';

export interface Repo {
  id: string;
  name: string;
  remote_url: string;
  credential_id: string | null;
  forge_credential_id: string | null;
  tracker_binding: TrackerBinding;
  forge_kind: ForgeKind;
  default_branch: string;
  provider: string;
  incogni: boolean;
  model_default: string | null;
  effort_default: string | null;
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
  incogni?: boolean;
}

/** PATCHable repo fields; null clears a nullable column back to the global default. */
export interface RepoPatch {
  name?: string;
  credential_id?: string | null;
  forge_credential_id?: string | null;
  tracker_binding?: TrackerBinding;
  default_branch?: string;
  model_default?: string | null;
  effort_default?: string | null;
  incogni?: boolean;
  git_author_name?: string | null;
  git_author_email?: string | null;
  afk_branch_pattern?: string;
  manual_branch_prefix?: string;
  afk_auto_enabled?: boolean;
  budget_minutes?: number | null;
  max_instances_override?: number | null;
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

// --- M3: providers ---

/** One dropdown option of a provider-owned model/effort catalog (D14). */
export interface ProviderOption {
  value: string;
  label: string;
}

export interface Provider {
  id: string;
  models: ProviderOption[];
  efforts: ProviderOption[];
}

export async function listProviders(): Promise<Provider[]> {
  const res = await request<{ providers: Provider[] }>('GET', '/providers');
  return res.providers;
}

// --- M3: Claude auth ---

export interface ClaudeAuthStatus {
  logged_in: boolean;
  email: string;
  method: string;
  checked_at: string;
}

/** force=true bypasses the server's 30s status cache. */
export function claudeAuthStatus(force = false): Promise<ClaudeAuthStatus> {
  return request<ClaudeAuthStatus>(
    'GET',
    '/providers/claude/auth/status' + (force ? '?force=1' : ''),
  );
}

/** 409s when already logged in — the caller refetches status instead. */
export function claudeLoginStart(): Promise<{ oauth_url: string }> {
  return request<{ oauth_url: string }>('POST', '/providers/claude/auth/login/start');
}

/** 202: the code was delivered; completion arrives via claude.auth.changed. */
export function claudeLoginCode(code: string): Promise<void> {
  return request<void>('POST', '/providers/claude/auth/login/code', { code });
}

// --- M3: instances & runs ---

export type RunKind = 'manual' | 'afk_manual' | 'afk_auto';
export type RunOutcome = 'active' | 'success' | 'death' | 'timeout' | 'stopped';

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
  model: string;
  effort: string;
  deep_link_url: string | null;
  started_at: string;
  budget_deadline: string | null;
  ended_at: string | null;
  outcome: RunOutcome;
  failure_reason: string | null;
}

/** Run JSON + liveness derived from tmux + the in-flight deep-link capture. */
export interface Instance extends Run {
  repo_name: string;
  live: boolean;
  connecting: boolean;
}

export interface StartInstanceRequest {
  label?: string;
  model?: string;
  effort?: string;
}

/** 201 run JSON | 409 (cap / claude logged out / repo not ready) | 400. */
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
  model?: string;
  effort?: string;
}

/**
 * Pulls the two spawn-default keys out of a GET /settings payload, tolerating
 * both a flat {key: value} object and a {settings: {key: value}} envelope —
 * the settings surface is an M4 contract, M3 only peeks at it.
 */
export function extractSpawnDefaults(raw: unknown): SpawnDefaults {
  if (typeof raw !== 'object' || raw === null) return {};
  let map = raw as Record<string, unknown>;
  const nested = map['settings'];
  if (typeof nested === 'object' && nested !== null) map = nested as Record<string, unknown>;
  const out: SpawnDefaults = {};
  const model = map['spawn_model_default'];
  if (typeof model === 'string' && model !== '') out.model = model;
  const effort = map['spawn_effort_default'];
  if (typeof effort === 'string' && effort !== '') out.effort = effort;
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

// --- M4: issues, comments, labels, ready queue ---

/** Persisted issue state. The list filter adds the synthetic "all". */
export type IssueState = 'open' | 'closed';
export type IssueStateFilter = 'open' | 'closed' | 'all';

/** One comment as the issue-detail endpoint serializes it (author + time). */
export interface IssueComment {
  author: string;
  body: string;
  created_at: string;
}

/** List-row shape: label names and a comment count, but no comment bodies. */
export interface IssueSummary {
  number: number;
  title: string;
  body: string;
  state: IssueState;
  labels: string[];
  comments_count: number;
  created_at: string;
  updated_at: string;
}

/** Full issue: label names plus the loaded comments (detail view). */
export interface IssueDetail {
  number: number;
  title: string;
  body: string;
  state: IssueState;
  labels: string[];
  comments: IssueComment[];
  created_at: string;
  updated_at: string;
}

/**
 * GET /repos/{id}/issues. `binding` is authoritative for the UI's
 * mutation gate: only a builtin tracker lets lab create/edit issues.
 */
export interface IssuesResponse {
  binding: TrackerBinding;
  issues: IssueSummary[];
}

export interface CreateIssueRequest {
  title: string;
  body?: string;
  labels?: string[];
}

export interface IssuePatch {
  title?: string;
  body?: string;
  state?: IssueState;
}

/** A repo label: the five seeded triage labels plus any custom ones. */
export interface Label {
  id: string;
  name: string;
  color: string;
  description: string;
}

export interface CreateLabelRequest {
  name: string;
  color?: string;
  description?: string;
}

export interface LabelPatch {
  name?: string;
  color?: string;
  description?: string;
}

/** Issue list, filtered by state server-side (open|closed|all). */
export function listIssues(
  repoID: string,
  state: IssueStateFilter = 'open',
): Promise<IssuesResponse> {
  return request<IssuesResponse>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/issues?state=${encodeURIComponent(state)}`,
  );
}

/** The ready-for-agent queue (Tracker.ReadyIssues, either backend). */
export async function listReadyIssues(repoID: string): Promise<IssueSummary[]> {
  const res = await request<{ issues: IssueSummary[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/ready`,
  );
  return res.issues;
}

/** Full issue with its comments loaded. */
export function getIssue(repoID: string, number: number): Promise<IssueDetail> {
  return request<IssueDetail>('GET', `/repos/${encodeURIComponent(repoID)}/issues/${number}`);
}

/** Builtin only — 409 {"error"} on a forge-bound repo. */
export function createIssue(repoID: string, req: CreateIssueRequest): Promise<IssueDetail> {
  return request<IssueDetail>('POST', `/repos/${encodeURIComponent(repoID)}/issues`, req);
}

/** Builtin only. Sends only the given fields (title/body/state). */
export function updateIssue(
  repoID: string,
  number: number,
  patch: IssuePatch,
): Promise<IssueDetail> {
  return request<IssueDetail>(
    'PATCH',
    `/repos/${encodeURIComponent(repoID)}/issues/${number}`,
    patch,
  );
}

/** Builtin only. The comment is attributed to "operator" server-side. */
export function createIssueComment(
  repoID: string,
  number: number,
  body: string,
): Promise<IssueComment> {
  return request<IssueComment>(
    'POST',
    `/repos/${encodeURIComponent(repoID)}/issues/${number}/comments`,
    { body },
  );
}

/** Builtin only. Replaces the issue's label set; unknown name → 400. */
export function setIssueLabels(
  repoID: string,
  number: number,
  labels: string[],
): Promise<IssueDetail> {
  return request<IssueDetail>(
    'PUT',
    `/repos/${encodeURIComponent(repoID)}/issues/${number}/labels`,
    { labels },
  );
}

/** Builtin repos: the repo's label set (id, name, color, description). */
export async function listLabels(repoID: string): Promise<Label[]> {
  const res = await request<{ labels: Label[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/labels`,
  );
  return res.labels;
}

export function createLabel(repoID: string, req: CreateLabelRequest): Promise<Label> {
  return request<Label>('POST', `/repos/${encodeURIComponent(repoID)}/labels`, req);
}

export function updateLabel(repoID: string, labelID: string, patch: LabelPatch): Promise<Label> {
  return request<Label>(
    'PATCH',
    `/repos/${encodeURIComponent(repoID)}/labels/${encodeURIComponent(labelID)}`,
    patch,
  );
}

/** 409s with the server's message when the name collides within the repo. */
export function deleteLabel(repoID: string, labelID: string): Promise<void> {
  return request<void>(
    'DELETE',
    `/repos/${encodeURIComponent(repoID)}/labels/${encodeURIComponent(labelID)}`,
  );
}
