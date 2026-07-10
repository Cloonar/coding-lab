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

/** Which tracker family a forge_token drives (ADR-0015). */
export type ForgeFlavor = 'forgejo' | 'github';

export interface ForgeTokenPayload {
  host: string;
  token: string;
  /** Absent → forgejo server-side; the UI always sends it explicitly. */
  forge: ForgeFlavor;
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
  /** Agent provider override (null = inherit the global provider_default). */
  provider: string | null;
  incogni: boolean;
  model_default: string | null;
  effort_default: string | null;
  /** AFK-run overrides (null = inherit the global AFK default). */
  afk_provider_default: string | null;
  afk_model_default: string | null;
  afk_effort_default: string | null;
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
  /** AFK-run overrides; null or "" clears back to the global AFK default. */
  afk_provider_default?: string | null;
  afk_model_default?: string | null;
  afk_effort_default?: string | null;
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

/**
 * A provider's generic "open on the web" affordance (ADR-0017): the URL to
 * open when no exact deep link was captured, plus the tooltip explaining it.
 * Present only for providers with a web surface (a DeepLinker); absent for
 * link-less providers, whose rows show a copyable tmux-attach affordance.
 */
export interface ProviderFallbackOpen {
  url: string;
  title: string;
}

/**
 * One provider-owned spawn option (issue #19). `type` is "bool" for now — the
 * value is the string "true"/"false"; unknown types are ignorable/no-render.
 * `default` is the value applied when the operator hasn't set the option.
 */
export interface ProviderOptionSpec {
  key: string;
  label: string;
  type: string;
  default: string;
}

/**
 * The provider's auth-flow kinds (issue #51 decision 7). 'device-code' (issue
 * #87): start-login yields a verification URL plus a short one-time code the
 * operator enters on that page — no code paste-back into lab.
 */
export type ProviderAuthKind =
  'oauth-code' | 'oauth-redirect' | 'api-key' | 'device-code' | 'external';

/**
 * Provider-declared auth flow descriptor (issue #51 decision 7): which login
 * shape the auth card renders. `instructions` carries operator guidance for
 * the oauth-redirect flow (e.g. an SSH port-forward hint); `credential_ref`
 * is reserved for the api-key flow (schema-only for now).
 */
export interface ProviderAuthFlow {
  kind: ProviderAuthKind;
  instructions?: string;
  credential_ref?: string;
}

export interface Provider {
  id: string;
  /** Human name driving all UI copy (issue #51 decision 9) — never hardcode it. */
  display_name: string;
  models: ProviderOption[];
  efforts: ProviderOption[];
  /** Declared spawn options (always present; may be empty). */
  options: ProviderOptionSpec[];
  auth: ProviderAuthFlow;
  fallback_open?: ProviderFallbackOpen;
}

export async function listProviders(): Promise<Provider[]> {
  const res = await request<{ providers: Provider[] }>('GET', '/providers');
  return res.providers;
}

// --- M3: provider auth (per-provider-id routes, issue #51 decision 7) ---

export interface ProviderAuthStatus {
  logged_in: boolean;
  email: string;
  method: string;
  checked_at: string;
}

/** force=true bypasses the server's 30s status cache. Unknown id → 404. */
export function providerAuthStatus(id: string, force = false): Promise<ProviderAuthStatus> {
  return request<ProviderAuthStatus>(
    'GET',
    `/providers/${encodeURIComponent(id)}/auth/status` + (force ? '?force=1' : ''),
  );
}

/**
 * 409s when already logged in — the caller refetches status instead.
 * `user_code` arrives for device-code flows: a short one-time code (expires in
 * ~15 minutes) the operator enters on the oauth_url page. `oauth_url` may be
 * "" on a scrape miss — retry by starting again.
 */
export function providerLoginStart(id: string): Promise<{ oauth_url: string; user_code?: string }> {
  return request<{ oauth_url: string; user_code?: string }>(
    'POST',
    `/providers/${encodeURIComponent(id)}/auth/login/start`,
  );
}

/** 202: the code was delivered; completion arrives via provider.auth.changed. */
export function providerLoginCode(id: string, code: string): Promise<void> {
  return request<void>('POST', `/providers/${encodeURIComponent(id)}/auth/login/code`, { code });
}

/**
 * Drops the machine's current account for this provider so a fresh one can be
 * logged in (issue #46). Machine-wide and bare by decision: it does NOT stop
 * running instances — they keep working on their in-memory token until it
 * refreshes, then fail. 200 echoes the now-logged-out status; the server also
 * emits provider.auth.changed so every open card flips live. A logout that
 * could not drop the account is a 500.
 */
export function providerLogout(id: string): Promise<ProviderAuthStatus> {
  return request<ProviderAuthStatus>('POST', `/providers/${encodeURIComponent(id)}/auth/logout`);
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

// --- Embedded chat (issue #7 / ADR-0016) ---

export type MessageKind = 'text' | 'tool' | 'dialog' | 'lifecycle';

export interface ToolInfo {
  name: string;
  title: string;
  input?: string;
  output?: string;
  status: 'running' | 'ok' | 'error';
}

export interface DialogOption {
  label: string;
  description?: string;
  is_other?: boolean;
}

/** Dialog kinds (issue #51 decision 3). "approval" = generic yes/no approval. */
export type DialogKind = 'question' | 'plan' | 'approval';

/**
 * One question of a multi-question dialog. The synthesized free-text "Other"
 * row (is_other) IS the "free text allowed" signal, same convention as the
 * flat single-question shape.
 */
export interface Question {
  /** Short chip label rendered above the question text. */
  header?: string;
  text: string;
  options: DialogOption[];
  multi_select?: boolean;
}

/**
 * The recorded resolution of an answered dialog (provider.go DialogOutcome).
 * Its PRESENCE on a Dialog is the answered signal: every field is omitempty,
 * so a plan rejected without typed feedback serializes as `"outcome": {}` —
 * never key "answered" on the inner fields.
 */
export interface DialogOutcome {
  /** Resolved without an answer: denial, interrupt, unattended timeout, or unreadable result. */
  dismissed?: boolean;
  /** Question kind: one per question, dialog order. */
  results?: QuestionResult[];
  /** Plan kind: the plan was approved. */
  approved?: boolean;
  /** Plan kind: rejection feedback text, if typed. */
  feedback?: string;
}

/**
 * One question's resolved answer for display (provider.go QuestionResult).
 * Neither `chosen` nor `other_text` means that question got no recorded
 * answer; a multi-select result can carry BOTH.
 */
export interface QuestionResult {
  /** The question text. */
  question: string;
  /** Chosen listed option label(s), recorded toggle order. */
  chosen?: string[];
  /** Free-text answer (the synthesized Other row). */
  other_text?: string;
}

export interface Dialog {
  tool_id: string;
  dialog_kind: DialogKind;
  /** For the plan kind, the FULL plan markdown (issue #56 — no length cap). */
  prompt: string;
  options?: DialogOption[];
  multi?: boolean;
  /** Present ONLY for multi-question dialogs (issue #51 decision 3). */
  questions?: Question[];
  answerable: boolean;
  /**
   * Resolution of an answered dialog (issue #56 decision 3): non-null MEANS
   * answered; absent means still pending. Answered dialogs keep their full
   * options/questions; the live pending_dialog field never carries one.
   */
  outcome?: DialogOutcome;
}

/** One entry of the universal chat schema. */
export interface ChatMessage {
  seq: number;
  kind: MessageKind;
  role?: 'user' | 'assistant';
  time?: string;
  text?: string;
  thinking?: boolean;
  error?: boolean;
  tool?: ToolInfo;
  dialog?: Dialog;
}

/** transcript: whether the file is readable yet. */
export type TranscriptStatus = 'available' | 'locating' | 'gone';

export interface MessagesResponse {
  messages: ChatMessage[];
  state: ConversationState;
  cursor: number;
  has_more: boolean;
  /**
   * The run's live interactive dialog from the provider's live-signals spool
   * (ADR-0020), null when none is pending. Top-level, NOT a message in the
   * stream — the agent CLI never flushes a pending tool_use to the transcript,
   * so this is the only live source. Present alongside state:"question".
   * Prefer it over the messages-scan, which stays a dormant fallback for a
   * future flushed tool_use. Optional on the client (older responses omit
   * it) — treat absent as null.
   */
  pending_dialog?: Dialog | null;
  transcript: TranscriptStatus;
  /**
   * Opaque identity of the located transcript (issue #34). It changes when the
   * run's transcript rotates — a /clear or /rewind starts a new sessionId →
   * transcript file and the backend re-points this same run at it. The chat
   * view keys its stream reset on a change here (the fresh transcript restarts
   * seq at 1, which would otherwise collide with the accumulated messages).
   * Empty/absent while locating or gone; older responses omit it → treat as
   * unchanged.
   */
  transcript_id?: string;
}

/** A window of an instance's messages. after=<seq> tails; before=<seq> pages up. */
export function getRunMessages(
  id: string,
  opts: { after?: number; before?: number; limit?: number } = {},
): Promise<MessagesResponse> {
  const params = new URLSearchParams();
  if (opts.after !== undefined) params.set('after', String(opts.after));
  if (opts.before !== undefined) params.set('before', String(opts.before));
  if (opts.limit !== undefined) params.set('limit', String(opts.limit));
  const query = params.toString();
  return request<MessagesResponse>(
    'GET',
    `/runs/${encodeURIComponent(id)}/messages` + (query === '' ? '' : `?${query}`),
  );
}

/** Free-text reply. 409 for an ended run or a pending dialog; 400 for empty. */
export function replyRun(id: string, text: string): Promise<void> {
  return request<void>('POST', `/runs/${encodeURIComponent(id)}/reply`, { text });
}

/**
 * One question's answer inside a multi-question dialog. Association is
 * POSITIONAL — answers[i] answers questions[i]; there is no question-index
 * field. Each entry mirrors the flat single-question shape (provider.go
 * QuestionAnswer is authoritative): a single-select question sends `index` =
 * the chosen OPTION index (`other_text` rides along only when that row is the
 * synthesized is_other row); a multi_select question sends `selected` = the
 * toggled REAL option indices ascending (`index` absent) plus `other_text`
 * when the Other row is toggled — the is_other row's index never appears in
 * `selected` (its free text IS its toggle; the adapter pastes it onto the
 * TUI's "Type something" row). `other_text` alone with an empty `selected`
 * is a valid multi-select answer.
 */
export interface QuestionAnswer {
  index?: number;
  selected?: number[];
  other_text?: string;
}

export interface AnswerRequest {
  tool_id: string;
  index?: number;
  selected?: number[];
  other_text?: string;
  /**
   * Per-question answers, positionally aligned with dialog.questions; when
   * non-empty the flat fields are ignored.
   */
  answers?: QuestionAnswer[];
}

export function answerRun(id: string, req: AnswerRequest): Promise<void> {
  return request<void>('POST', `/runs/${encodeURIComponent(id)}/answer`, req);
}

export function interruptRun(id: string): Promise<void> {
  return request<void>('POST', `/runs/${encodeURIComponent(id)}/interrupt`);
}

// --- Slash-command catalog (issue #51 decision 5) ---

/**
 * One chat-safe slash command of the run's provider catalog. `name` comes
 * WITHOUT the leading slash; `source` is builtin|project|user; `role` is the
 * optional semantic tag (only "clear" defined for now) the UI may bind an
 * affordance to. The server curates out non-chat-safe entries, but the flag
 * is still serialized.
 */
export interface RunCommand {
  name: string;
  description?: string;
  arg_hint?: string;
  source: string;
  role?: string;
  chat_safe: boolean;
}

/** The run's slash-command catalog (worktree-dependent, server-cached ~30s). */
export async function fetchRunCommands(id: string): Promise<RunCommand[]> {
  const res = await request<{ commands: RunCommand[] }>(
    'GET',
    `/runs/${encodeURIComponent(id)}/commands`,
  );
  return res.commands;
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
  provider?: string;
  model?: string;
  effort?: string;
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

// --- M5: API tokens (D7 PATs) ---

/** Token metadata — the secret is only ever present in the create response. */
export interface ApiToken {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string | null;
}

/** POST /tokens response: `token` (lab_pat_…) appears here ONCE, never again. */
export interface CreatedApiToken {
  id: string;
  name: string;
  token: string;
}

export async function listTokens(): Promise<ApiToken[]> {
  const res = await request<{ tokens: ApiToken[] }>('GET', '/tokens');
  return res.tokens;
}

export function createToken(name: string): Promise<CreatedApiToken> {
  return request<CreatedApiToken>('POST', '/tokens', { name });
}

export function deleteToken(id: string): Promise<void> {
  return request<void>('DELETE', `/tokens/${encodeURIComponent(id)}`);
}

// --- Web Push devices (issue #98) ---

/**
 * One registered push subscription — a single browser on a single device.
 * `label` is derived server-side from the User-Agent at create time; `endpoint`
 * is the push service URL that also identifies the browser's local
 * subscription, so it is what the UI matches to mark "this device".
 */
export interface PushDevice {
  id: string;
  endpoint: string;
  label: string;
  created_at: string;
}

/**
 * The VAPID application server key: a base64url-encoded 65-byte uncompressed
 * P-256 point. Decoded to a Uint8Array and passed as pushManager.subscribe's
 * applicationServerKey.
 */
export async function pushKey(): Promise<string> {
  const res = await request<{ public_key: string }>('GET', '/push/key');
  return res.public_key;
}

/** Every registered push device — device-level trust, not scoped to a user. */
export async function listPushDevices(): Promise<PushDevice[]> {
  const res = await request<{ subscriptions: PushDevice[] }>('GET', '/push/subscriptions');
  return res.subscriptions;
}

/**
 * Registers the browser's PushSubscription — pass the fields off
 * subscription.toJSON(). The server derives the label from the User-Agent and
 * upserts by endpoint, so re-registering the same browser is idempotent.
 */
export function createPushDevice(sub: {
  endpoint: string;
  keys: { p256dh: string; auth: string };
}): Promise<PushDevice> {
  return request<PushDevice>('POST', '/push/subscriptions', sub);
}

export function deletePushDevice(id: string): Promise<void> {
  return request<void>('DELETE', `/push/subscriptions/${encodeURIComponent(id)}`);
}

/**
 * Fires a real test notification through the sender (202). Push delivery is
 * otherwise silent server-side, so this is the self-hoster's way to confirm
 * HTTPS and egress actually reach the push service.
 */
export function testPushDevice(id: string): Promise<void> {
  return request<void>('POST', `/push/subscriptions/${encodeURIComponent(id)}/test`);
}

// --- M5: settings ---

export const INT_SETTING_KEYS = [
  'max_instances',
  'afk_budget_minutes',
  'afk_tick_seconds',
  'afk_schedule_seconds',
  'sweep_interval_minutes',
] as const;

export const TEXT_SETTING_KEYS = [
  /** Root agent-provider default (seeded); the base of every provider chain. */
  'provider_default',
  'spawn_model_default',
  'spawn_effort_default',
  /** AFK provider override ("" = inherit provider_default). */
  'spawn_provider_default_afk',
  'spawn_model_default_afk',
  'spawn_effort_default_afk',
  'git_author_name',
  'git_author_email',
  'afk_prompt',
  /**
   * Read-only, server-injected (issue #52): the built-in seed-prompt template
   * with literal <N>/<BRANCH> tokens un-interpolated. Listed here only so
   * normalizeSettings' loop below picks it up for Settings.tsx to read as the
   * textarea placeholder — callers MUST NOT include it in an updateSettings
   * patch (the server 400s). Settings.tsx's buildPatch iterates its own
   * explicit key list rather than this array, which keeps it out of PATCHes.
   */
  'afk_prompt_default',
] as const;

export type IntSettingKey = (typeof INT_SETTING_KEYS)[number];
export type TextSettingKey = (typeof TEXT_SETTING_KEYS)[number];

/**
 * The runtime-mutable settings (§3a keys). All fields optional: GET returns
 * whatever is stored, PATCH sends only the fields being changed.
 *
 * `spawn_options_afk` is the provider spawn-option bag for AFK runs. The server
 * stores it as a JSON string and returns it as one on GET; normalizeSettings
 * parses it to an object here, and PATCH sends it back as an object.
 *
 * `afk_prompt_default` is read-only (see TEXT_SETTING_KEYS above) — present on
 * every GET, must never be sent back in a PATCH.
 */
export type Settings = Partial<Record<IntSettingKey, number> & Record<TextSettingKey, string>> & {
  spawn_options_afk?: Record<string, string>;
};

/**
 * Normalizes a settings payload: tolerates both a flat {key: value} object
 * and a {settings: {key: value}} envelope (like extractSpawnDefaults), and
 * coerces int keys that arrive as numeric strings (the settings table stores
 * TEXT). Unknown keys and garbage values are dropped.
 */
export function normalizeSettings(raw: unknown): Settings {
  if (typeof raw !== 'object' || raw === null) return {};
  let map = raw as Record<string, unknown>;
  const nested = map['settings'];
  if (typeof nested === 'object' && nested !== null) map = nested as Record<string, unknown>;
  const out: Settings = {};
  for (const key of TEXT_SETTING_KEYS) {
    const value = map[key];
    if (typeof value === 'string') out[key] = value;
  }
  for (const key of INT_SETTING_KEYS) {
    const value = map[key];
    const n =
      typeof value === 'number'
        ? value
        : typeof value === 'string' && value.trim() !== ''
          ? Number(value)
          : NaN;
    if (Number.isInteger(n)) out[key] = n;
  }
  // spawn_options_afk arrives as a raw JSON string (GET) or an object (PATCH
  // echo); parse defensively and coerce values to strings. A missing key or
  // garbage (parse failure / non-object / array) is omitted entirely.
  const rawOptions = map['spawn_options_afk'];
  let parsed: unknown;
  if (typeof rawOptions === 'string') {
    try {
      parsed = JSON.parse(rawOptions);
    } catch {
      parsed = undefined;
    }
  } else if (typeof rawOptions === 'object' && rawOptions !== null) {
    parsed = rawOptions;
  }
  if (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)) {
    const options: Record<string, string> = {};
    for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
      options[key] = String(value);
    }
    out.spawn_options_afk = options;
  }
  return out;
}

export async function getSettings(): Promise<Settings> {
  return normalizeSettings(await request<unknown>('GET', '/settings'));
}

/**
 * PATCH /settings {key: value, …} → 200 with the updated settings. Ints go
 * as JSON numbers; the server validates (ints parse, model/effort against
 * the provider catalogs) and 400s {"error"} on a bad value. Runtime loops
 * re-read settings every tick, so saves apply without a restart.
 */
export async function updateSettings(patch: Settings): Promise<Settings> {
  return normalizeSettings(await request<unknown>('PATCH', '/settings', patch));
}

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
