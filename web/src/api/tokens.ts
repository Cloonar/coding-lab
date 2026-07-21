import { request } from './core';

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
