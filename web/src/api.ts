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
