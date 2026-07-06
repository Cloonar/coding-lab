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
