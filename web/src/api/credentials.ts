import { request } from './core';

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
