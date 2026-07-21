import { request } from './core';

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
