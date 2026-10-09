// More page contract (issue #76; docs/reference/tab-bar-mockup.html):
// - below 1024px: a "More" header, then the rows Credentials, Tokens and
//   Settings — plus Install app only while the PWA is installable (the same
//   gate as the Settings row; tapping it reopens the install sheet) — each
//   with one line of state: "<Provider> logged in", "N active", the settings
//   category names;
// - while the default agent provider is logged out: an alert (icon, bold
//   title, sentence) with a Reconnect link to /credentials above the rows,
//   and a `logged out` chip on the Credentials row instead of its hint —
//   neither while logged in;
// - the account block: the username, the SSE live dot and Log out (logout,
//   then the auth refresh that bounces the guard to /login);
// - from 1024px the route redirects to /settings.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ApiToken, Provider } from '../api';
import App from '../App';
import More from './More';

// The install controller is a module singleton fed by browser events; the
// page only reads its Settings-row gate and calls its re-entry action.
const h = vi.hoisted(() => ({
  installable: false,
  openFromSettings: (): void => {},
}));
vi.mock('../lib/install', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/install')>();
  return {
    ...actual,
    install: {
      ...actual.install,
      settingsRowVisible: () => h.installable,
      openFromSettings: () => h.openFromSettings(),
    },
  };
});

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor() {
    FakeEventSource.instances.push(this);
  }
  addEventListener(): void {}
  close(): void {}
}

const PROVIDERS: Provider[] = [
  {
    id: 'claude-code',
    display_name: 'Claude Code',
    supports_remote: true,
    auth: { kind: 'oauth-code' },
    models: [],
    efforts: [],
    options: [],
  },
];

const TOKENS: ApiToken[] = [
  { id: 't1', name: 'ci', created_at: '2026-10-01T00:00:00Z', last_used_at: null },
  { id: 't2', name: 'cli', created_at: '2026-10-02T00:00:00Z', last_used_at: null },
];

let loggedIn: boolean | null;
let authenticated: boolean;
let authStateRequests: number;
let logoutRequests: number;
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

function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === '/api/v1/auth/state' && method === 'GET') {
        authStateRequests += 1;
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated, username: 'dominik' }),
        );
      }
      if (url === '/api/v1/auth/logout' && method === 'POST') {
        logoutRequests += 1;
        authenticated = false;
        return Promise.resolve(jsonResponse(204));
      }
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: PROVIDERS }));
      }
      if (url === '/api/v1/settings' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, {}));
      }
      if (url === '/api/v1/providers/claude-code/auth/status' && method === 'GET') {
        // null = the status never answers: the login state stays unknown.
        if (loggedIn === null) return new Promise(() => {});
        return Promise.resolve(
          jsonResponse(200, {
            logged_in: loggedIn,
            email: '',
            method: '',
            checked_at: '2026-10-09T00:00:00Z',
          }),
        );
      }
      if (url === '/api/v1/tokens' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { tokens: TOKENS }));
      }
      if (url === '/api/v1/presence' && method === 'POST') {
        return Promise.resolve(jsonResponse(204));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

/** A matchMedia fake answering only the desktop breakpoint. */
function desktopViewport(): void {
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches: query === '(min-width: 1024px)',
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      onchange: null,
      dispatchEvent: () => false,
    })),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flush();
}

async function mountMore(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/more' });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/more" component={More} />
        <Route path="/settings" component={() => <p class="settings-stub">settings</p>} />
        <Route path="/login" component={() => <p class="login-stub">login</p>} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

const page = () => container.querySelector<HTMLElement>('main.more-page');
/** The row titles, in order. */
const rowTitles = () =>
  Array.from(container.querySelectorAll('.more-list .settings-index-title')).map(
    (el) => el.textContent,
  );
/** A row (link or button) by its title. */
const row = (title: string) =>
  Array.from(container.querySelectorAll<HTMLElement>('.more-list .settings-index-row')).find(
    (el) => el.querySelector('.settings-index-title')?.textContent === title,
  );
const hint = (title: string) => row(title)?.querySelector('.more-row-hint')?.textContent ?? null;
const banner = () => container.querySelector<HTMLElement>('.more-alert');

beforeEach(() => {
  FakeEventSource.instances = [];
  h.installable = false;
  h.openFromSettings = () => {};
  loggedIn = true;
  authenticated = true;
  authStateRequests = 0;
  logoutRequests = 0;
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('More page', () => {
  it('lists Credentials, Tokens and Settings with their state lines', async () => {
    await mountMore();

    expect(page()?.querySelector('.section-head h2')?.textContent).toBe('More');
    expect(rowTitles()).toEqual(['Credentials', 'Tokens', 'Settings']);
    expect(row('Credentials')?.getAttribute('href')).toBe('/credentials');
    expect(row('Tokens')?.getAttribute('href')).toBe('/tokens');
    expect(row('Settings')?.getAttribute('href')).toBe('/settings');
    expect(hint('Credentials')).toBe('Claude Code logged in');
    expect(hint('Tokens')).toBe('2 active');
    expect(hint('Settings')).toBe('General · Agents · Notifications · Runner');
  });

  it('adds Install app only while installable, and tapping it reopens the sheet', async () => {
    const opened = vi.fn();
    h.openFromSettings = opened;
    h.installable = true;
    await mountMore();

    expect(rowTitles()).toEqual(['Credentials', 'Tokens', 'Settings', 'Install app']);
    const install = row('Install app')!;
    expect(install.tagName).toBe('BUTTON');
    install.click();
    expect(opened).toHaveBeenCalledTimes(1);
  });

  it('shows the logged-out banner and chip only while the provider is logged out', async () => {
    loggedIn = false;
    await mountMore();

    expect(banner()?.getAttribute('role')).toBe('alert');
    // The mockup's shape: an icon, a bold title, the sentence, Reconnect.
    expect(banner()?.querySelector('svg.needs-you-icon')).not.toBeNull();
    expect(banner()?.querySelector('strong.more-alert-title')?.textContent).toBe(
      'Claude Code is logged out',
    );
    expect(banner()?.querySelector('.needs-you-message')?.textContent?.trim()).toBe(
      'New runs will fail at the login wall until you reconnect.',
    );
    const reconnect = banner()?.querySelector('a.needs-you-action');
    expect(reconnect?.textContent).toBe('Reconnect');
    expect(reconnect?.getAttribute('href')).toBe('/credentials');
    // The banner sits above the rows.
    expect(
      banner()!.compareDocumentPosition(container.querySelector('.more-list')!) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(row('Credentials')?.querySelector('.chip.status-error')?.textContent).toBe('logged out');
    expect(hint('Credentials')).toBeNull();
  });

  it('shows neither banner nor chip while logged in', async () => {
    await mountMore();
    expect(banner()).toBeNull();
    expect(container.querySelector('.more-list .chip')).toBeNull();
  });

  it('shows no Credentials state while the login status is unknown', async () => {
    loggedIn = null;
    await mountMore();
    expect(banner()).toBeNull();
    expect(row('Credentials')?.querySelector('.more-row-hint, .chip')).toBeNull();
  });

  it('shows the username and the live dot in the account block', async () => {
    await mountMore();
    const account = container.querySelector<HTMLElement>('.more-account')!;
    expect(account.querySelector('.more-account-name')?.textContent).toBe('dominik');
    const dot = account.querySelector<HTMLElement>('.live-dot')!;
    expect(dot.classList.contains('on')).toBe(false);
    expect(dot.getAttribute('aria-label')).toBe('Reconnecting');

    FakeEventSource.instances.at(-1)?.onopen?.();
    await settle();
    expect(dot.classList.contains('on')).toBe(true);
    expect(dot.getAttribute('aria-label')).toBe('Live');
  });

  it('Log out ends the session and refreshes the auth state', async () => {
    await mountMore();
    const before = authStateRequests;
    const button = container.querySelector<HTMLButtonElement>('button.more-logout')!;
    expect(button.textContent).toBe('Log out');

    button.click();
    expect(button.textContent).toBe('Logging out…');
    await settle();

    expect(logoutRequests).toBe(1);
    expect(authStateRequests).toBe(before + 1);
    // The refreshed state is unauthenticated: the guard bounced to /login.
    expect(container.querySelector('.login-stub')).not.toBeNull();
  });

  it('redirects to /settings from 1024px', async () => {
    desktopViewport();
    await mountMore();
    expect(page()).toBeNull();
    expect(container.querySelector('.settings-stub')).not.toBeNull();
  });
});
