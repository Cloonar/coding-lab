// ProviderAuthCard behavioral contract (issue #51 decision 7):
// - the card is descriptor-driven: oauth-code renders the full login flow
//   (start → authorize link + code paste), device-code renders the
//   verification link + prominent one-time code with NO paste-back (issue
//   #87), oauth-redirect renders the descriptor's instructions (no code
//   form), api-key renders the injected-at-spawn note, external renders
//   status only;
// - logout exists only for the oauth flows, gated behind the confirm dialog
//   that names the running-instance count and the AFK warning (issue #46);
// - every string derives from the provider's display_name — nothing hardcodes
//   an agent brand;
// - the card refetches on provider.auth.changed for ITS provider id and
//   ignores other providers' events.

import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Provider, ProviderAuthStatus } from '../api';
import { EventsProvider } from '../events';
import ProviderAuthCard from './ProviderAuthCard';

/** EventSource stand-in that lets tests push SSE events into the app. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((event: { data: string }) => void)[]>();
  constructor() {
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, listener: (event: { data: string }) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }
  close(): void {}
  emit(type: string, payload: Record<string, unknown>): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data: JSON.stringify(payload) });
    }
  }
}

function provider(overrides: Partial<Provider> = {}): Provider {
  return {
    id: 'claude-code',
    display_name: 'Claude Code',
    models: [],
    efforts: [],
    options: [],
    supports_remote: true,
    auth: { kind: 'oauth-code' },
    ...overrides,
  };
}

const LOGGED_IN: ProviderAuthStatus = {
  logged_in: true,
  email: 'op@example.invalid',
  method: 'oauth',
  checked_at: '2026-07-08T00:00:00.000Z',
};
const LOGGED_OUT: ProviderAuthStatus = {
  logged_in: false,
  email: '',
  method: '',
  checked_at: '2026-07-08T00:00:01.000Z',
};

let authOnServer: ProviderAuthStatus;
let logoutCalls: number;
let codePosts: { code: string }[];
let statusGets: number;
/** login/start response body — device-code tests add user_code / blank urls. */
let startBody: { oauth_url: string; user_code?: string };
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function jsonResponse(status: number, body?: unknown) {
  const text = body === undefined ? '' : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () =>
      text === ''
        ? Promise.reject(new SyntaxError('empty body'))
        : Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

/** Serves the per-provider-id auth routes for `id` only (unknown → reject). */
function stubApi(id = 'claude-code'): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url.startsWith(`/api/v1/providers/${id}/auth/status`) && method === 'GET') {
        statusGets += 1;
        return Promise.resolve(jsonResponse(200, authOnServer));
      }
      if (url === `/api/v1/providers/${id}/auth/login/start` && method === 'POST') {
        return Promise.resolve(jsonResponse(200, startBody));
      }
      if (url === `/api/v1/providers/${id}/auth/login/code` && method === 'POST') {
        codePosts.push(JSON.parse(String(init?.body)) as { code: string });
        return Promise.resolve(jsonResponse(202));
      }
      if (url === `/api/v1/providers/${id}/auth/logout` && method === 'POST') {
        logoutCalls += 1;
        authOnServer = LOGGED_OUT;
        return Promise.resolve(jsonResponse(200, LOGGED_OUT));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountCard(p: Provider, activeRuns = 0): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(
    () => (
      <EventsProvider>
        <ProviderAuthCard provider={p} activeRuns={activeRuns} />
      </EventsProvider>
    ),
    container,
  );
  await settle();
}

function maybe<T extends Element>(selector: string): T | null {
  return container.querySelector<T>(selector);
}

function query<T extends Element>(selector: string): T {
  const el = maybe<T>(selector);
  if (!el) throw new Error(`missing ${selector}`);
  return el;
}

function buttonByText(text: string): HTMLButtonElement | undefined {
  return Array.from(container.querySelectorAll('button')).find(
    (b) => b.textContent?.trim() === text,
  );
}

beforeEach(() => {
  authOnServer = LOGGED_IN;
  logoutCalls = 0;
  codePosts = [];
  statusGets = 0;
  startBody = { oauth_url: 'https://auth.example/oauth/x' };
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('ProviderAuthCard logout gate (oauth flows)', () => {
  it('shows the Log out button only while logged in', async () => {
    await mountCard(provider());
    expect(maybe('button.provider-logout')).not.toBeNull();

    dispose?.();
    container.remove();

    // A logged-out card offers login instead, never a Log out button.
    authOnServer = LOGGED_OUT;
    await mountCard(provider());
    expect(maybe('button.provider-logout')).toBeNull();
    expect(container.textContent).toContain('Log in to Claude Code');
  });

  it('gates the logout POST behind the confirm dialog', async () => {
    await mountCard(provider());

    // Clicking Log out reveals the confirm — no POST yet.
    query<HTMLButtonElement>('button.provider-logout').click();
    await settle();
    expect(maybe('[role="alertdialog"]')).not.toBeNull();
    expect(logoutCalls).toBe(0);

    // Cancel dismisses it without logging out.
    buttonByText('Cancel')?.click();
    await settle();
    expect(maybe('[role="alertdialog"]')).toBeNull();
    expect(logoutCalls).toBe(0);

    // Re-open and confirm: the POST fires and the card flips to logged-out.
    query<HTMLButtonElement>('button.provider-logout').click();
    await settle();
    query<HTMLButtonElement>('button.provider-logout-confirm').click();
    await settle();

    expect(logoutCalls).toBe(1);
    expect(maybe('button.provider-logout')).toBeNull();
    expect(container.textContent).toContain('Log in to Claude Code');
  });

  it('names the running-instance count and the AFK warning in the confirm', async () => {
    await mountCard(provider(), 3);

    query<HTMLButtonElement>('button.provider-logout').click();
    await settle();

    const dialog = query('[role="alertdialog"]');
    expect(dialog.textContent).toContain('3 running instance');
    expect(dialog.textContent).toContain('AFK auto stays on');
  });
});

describe('ProviderAuthCard descriptor-driven flows', () => {
  it('oauth-code: start-login yields the authorize link and the code paste form posts per-provider', async () => {
    authOnServer = LOGGED_OUT;
    stubApi('codex');
    await mountCard(provider({ id: 'codex', display_name: 'Codex CLI' }));

    buttonByText('Log in to Codex CLI')!.click();
    await settle();
    expect(container.querySelector('a.oauth-link')?.getAttribute('href')).toBe(
      'https://auth.example/oauth/x',
    );

    const input = query<HTMLInputElement>('input[name="provider-login-code"]');
    input.value = 'the-code';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    buttonByText('Submit code')!.click();
    await settle();

    // The code went to /providers/codex/... (the stub rejects other ids) and
    // the card waits on the SSE completion, named by display_name.
    expect(codePosts).toEqual([{ code: 'the-code' }]);
    expect(container.textContent).toContain('waiting for Codex CLI…');
  });

  it('oauth-redirect: renders the descriptor instructions, no code form, no start button', async () => {
    authOnServer = LOGGED_OUT;
    await mountCard(
      provider({
        auth: { kind: 'oauth-redirect', instructions: 'Forward port 1455 and log in there.' },
      }),
    );

    expect(container.textContent).toContain('Forward port 1455 and log in there.');
    expect(maybe('input[name="provider-login-code"]')).toBeNull();
    expect(buttonByText('Log in to Claude Code')).toBeUndefined();

    // Logged in, the oauth-redirect flow still offers logout.
    dispose?.();
    container.remove();
    authOnServer = LOGGED_IN;
    await mountCard(provider({ auth: { kind: 'oauth-redirect' } }));
    expect(maybe('button.provider-logout')).not.toBeNull();
  });

  it('api-key: renders the injected-at-spawn note and no login form or logout', async () => {
    authOnServer = LOGGED_OUT;
    await mountCard(provider({ auth: { kind: 'api-key' } }));

    expect(container.textContent).toContain('injected at spawn');
    expect(maybe('input[name="provider-login-code"]')).toBeNull();
    expect(maybe('button.provider-logout')).toBeNull();
    expect(buttonByText('Log in to Claude Code')).toBeUndefined();
  });

  it('external: renders status only — no login affordance, no logout', async () => {
    await mountCard(provider({ auth: { kind: 'external' } }));
    expect(container.textContent).toContain('logged in');
    expect(maybe('button.provider-logout')).toBeNull();

    dispose?.();
    container.remove();
    authOnServer = LOGGED_OUT;
    await mountCard(provider({ auth: { kind: 'external' } }));
    expect(container.textContent).toContain('logged out');
    expect(maybe('input[name="provider-login-code"]')).toBeNull();
    expect(buttonByText('Log in to Claude Code')).toBeUndefined();
  });

  it('drives the card title and copy from display_name', async () => {
    authOnServer = LOGGED_OUT;
    stubApi('gemini-cli');
    await mountCard(provider({ id: 'gemini-cli', display_name: 'Gemini CLI' }));

    expect(container.querySelector('.card-title')?.textContent).toBe('Gemini CLI');
    expect(container.textContent).toContain('Log in to Gemini CLI');
    expect(container.textContent).not.toMatch(/claude/i);
  });
});

describe('ProviderAuthCard device-code flow (issue #87)', () => {
  const deviceProvider = () =>
    provider({
      id: 'acme-cli',
      display_name: 'Acme CLI',
      auth: { kind: 'device-code', instructions: 'Any browser works; the code is single-use.' },
    });

  it('start-login yields the verification link, the one-time code, the waiting indicator — and no paste-back', async () => {
    authOnServer = LOGGED_OUT;
    stubApi('acme-cli');
    startBody = { oauth_url: 'https://auth.example/device', user_code: 'Y9HC-QKI85' };
    await mountCard(deviceProvider());

    buttonByText('Log in to Acme CLI')!.click();
    await settle();

    // Step 1: the verification link, same anchor affordance as oauth-code.
    expect(container.querySelector('a.oauth-link')?.getAttribute('href')).toBe(
      'https://auth.example/device',
    );
    // Step 2: the one-time code, prominent and selectable, with expiry copy.
    expect(container.querySelector('code.device-user-code')?.textContent).toBe('Y9HC-QKI85');
    expect(container.textContent).toContain('expires in about 15 minutes');
    // The descriptor's instructions render below the steps.
    expect(container.textContent).toContain('Any browser works; the code is single-use.');
    // No code paste-back into lab — the operator enters it on the page.
    expect(maybe('input[name="provider-login-code"]')).toBeNull();
    expect(buttonByText('Submit code')).toBeUndefined();
    // The CLI login already runs in the background: waiting immediately.
    expect(container.textContent).toContain('waiting for Acme CLI…');
    expect(buttonByText('Restart login')).not.toBeUndefined();
  });

  it('scrape miss (oauth_url "") shows the retry hint; Restart login re-scrapes', async () => {
    authOnServer = LOGGED_OUT;
    stubApi('acme-cli');
    startBody = { oauth_url: '', user_code: '' };
    await mountCard(deviceProvider());

    buttonByText('Log in to Acme CLI')!.click();
    await settle();

    expect(container.textContent).toContain("The verification URL wasn't captured yet");
    expect(maybe('a.oauth-link')).toBeNull();
    expect(maybe('code.device-user-code')).toBeNull();
    // No code yet → not waiting yet.
    expect(container.textContent).not.toContain('waiting for Acme CLI…');

    // Restart re-scrapes: the next start lands the URL and the code.
    startBody = { oauth_url: 'https://auth.example/device', user_code: 'AAAA-BBBBB' };
    buttonByText('Restart login')!.click();
    await settle();

    expect(container.querySelector('a.oauth-link')?.getAttribute('href')).toBe(
      'https://auth.example/device',
    );
    expect(container.querySelector('code.device-user-code')?.textContent).toBe('AAAA-BBBBB');
    expect(container.textContent).toContain('waiting for Acme CLI…');
  });

  it('logged-in flip via provider.auth.changed clears the flow and offers logout', async () => {
    authOnServer = LOGGED_OUT;
    stubApi('acme-cli');
    startBody = { oauth_url: 'https://auth.example/device', user_code: 'Y9HC-QKI85' };
    await mountCard(deviceProvider());

    buttonByText('Log in to Acme CLI')!.click();
    await settle();
    expect(container.textContent).toContain('waiting for Acme CLI…');

    // The adapter's background poll lands the login; SSE flips the card.
    authOnServer = LOGGED_IN;
    FakeEventSource.instances[0]?.emit('provider.auth.changed', {
      type: 'provider.auth.changed',
      provider: 'acme-cli',
    });
    await settle();

    expect(container.textContent).toContain('logged in');
    expect(maybe('a.oauth-link')).toBeNull();
    expect(maybe('code.device-user-code')).toBeNull();
    expect(container.textContent).not.toContain('waiting for Acme CLI…');
    // device-code is a lab-driven flow: logout is offered.
    expect(maybe('button.provider-logout')).not.toBeNull();
  });
});

describe('ProviderAuthCard SSE wiring', () => {
  it('refetches on provider.auth.changed for its own provider id and ignores others', async () => {
    authOnServer = LOGGED_OUT;
    await mountCard(provider());
    expect(container.textContent).toContain('logged out');
    const before = statusGets;

    // Another provider's event: no refetch.
    FakeEventSource.instances[0]?.emit('provider.auth.changed', {
      type: 'provider.auth.changed',
      provider: 'codex',
    });
    await settle();
    expect(statusGets).toBe(before);

    // Its own event: refetch flips the card live.
    authOnServer = LOGGED_IN;
    FakeEventSource.instances[0]?.emit('provider.auth.changed', {
      type: 'provider.auth.changed',
      provider: 'claude-code',
    });
    await settle();
    expect(statusGets).toBeGreaterThan(before);
    expect(container.textContent).toContain('logged in');
  });
});
