import { request } from './core';

// --- M3: providers ---

/** One dropdown option of a provider-owned model/effort catalog (D14). */
export interface ProviderOption {
  value: string;
  label: string;
}

/** One model entry of a provider catalog (issue #156): the option pair plus
 *  the model's own effort list and reported default — effort support varies
 *  per model and codex does not clamp, so the composer must never send an
 *  unsupported combo. */
export interface ProviderModelOption extends ProviderOption {
  efforts: ProviderOption[];
  default_effort?: string;
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
  models: ProviderModelOption[];
  /** The model-independent UNION of every model's efforts (issue #156) — the
   *  flat list the settings default pickers offer; the composer follows the
   *  selected model's own `efforts` instead. */
  efforts: ProviderOption[];
  /** Declared spawn options (always present; may be empty). */
  options: ProviderOptionSpec[];
  /**
   * Whether the provider has a remote-control knob at all (issue #163): a
   * session that registers itself with the provider's web bridge, which is what
   * produces the exact deep link. The ONLY thing the UI may key remote-control
   * behavior on — a provider without it ignores the setting entirely (the
   * server clamps its runs' `remote` to false), and its rows/affordances must
   * stay exactly as they are.
   */
  supports_remote: boolean;
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
