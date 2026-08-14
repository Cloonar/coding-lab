// The OneCLI dashboard's login bounce (issue #26). Port mode's proxy answers
// an unauthenticated browser navigation with a 302 to
// /login?next=onecli-dashboard&path=<URL-encoded path+query>, and this route
// is the other half of it:
//
//   - `next` is a fixed KEYWORD, never a URL. The destination ORIGIN comes
//     from the authenticated GET /onecli/dashboard and nothing else, which is
//     what makes the bounce structurally incapable of being an open redirect;
//   - the bounce fires off the authenticated branch, so it covers a login that
//     just happened AND a bounce that arrived on an already-valid session;
//   - every miss (off, no URL, a failed request) falls back to the ordinary /
//     navigation — the operator is logged in either way;
//   - a login with no ?next at all behaves exactly as it always has, and never
//     touches the exposure endpoint.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MemoryHistory } from '@solidjs/router';
import type { OneCLIDashboardExposure } from '../api';
import App from '../App';
import Login, { safeDashboardPath } from './Login';

const DASHBOARD_URL = 'https://lab.example.test:8443';

/** Minimal EventSource stand-in so EventsProvider can mount under jsdom. */
class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

let authenticatedOnServer: boolean;
/** The exposure GET /onecli/dashboard answers with, or an Error to reject. */
let exposureOnServer: OneCLIDashboardExposure | Error;
let dashboardCalls: number;
let replace: ReturnType<typeof vi.fn>;
let dispose: (() => void) | undefined;
let container: HTMLDivElement;
let history: MemoryHistory;

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
        return Promise.resolve(
          jsonResponse(200, {
            setup_required: false,
            authenticated: authenticatedOnServer,
            username: 'dominik',
          }),
        );
      }
      if (url === '/api/v1/auth/login' && method === 'POST') {
        authenticatedOnServer = true;
        return Promise.resolve(jsonResponse(204));
      }
      if (url === '/api/v1/onecli/dashboard' && method === 'GET') {
        dashboardCalls += 1;
        return exposureOnServer instanceof Error
          ? Promise.reject(exposureOnServer)
          : Promise.resolve(jsonResponse(200, exposureOnServer));
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

/**
 * jsdom's window.location is unforgeable — its own properties cannot be
 * redefined, so vi.spyOn(window.location, 'replace') throws. Replacing the
 * whole global is the one approach that works, and vi.unstubAllGlobals() in
 * the shared afterEach puts the real one back.
 */
function stubLocation(): void {
  replace = vi.fn();
  vi.stubGlobal('location', {
    href: 'http://localhost/login',
    origin: 'http://localhost',
    replace,
  });
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountLogin(search = ''): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  history = createMemoryHistory();
  history.set({ value: `/login${search}` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/login" component={Login} />
        <Route path="/" component={() => <p class="home">home</p>} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function input(name: string): HTMLInputElement {
  const el = container.querySelector<HTMLInputElement>(`input[name="${name}"]`);
  if (!el) throw new Error(`missing input[name="${name}"]`);
  return el;
}

function typeInto(el: HTMLInputElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/** Fills the form and submits it, then lets the auth refresh settle. */
async function logIn(): Promise<void> {
  typeInto(input('username'), 'dominik');
  typeInto(input('password'), 'hunter22');
  const form = container.querySelector('form');
  if (!form) throw new Error('missing login form');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
  await settle();
}

beforeEach(() => {
  authenticatedOnServer = false;
  exposureOnServer = { mode: 'port', url: DASHBOARD_URL };
  dashboardCalls = 0;
  stubApi();
  stubLocation();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  // Optional: the safeDashboardPath cases never mount anything.
  container?.remove();
  vi.unstubAllGlobals();
});

describe('safeDashboardPath', () => {
  it('keeps an ordinary path, query and all', () => {
    expect(safeDashboardPath('/settings')).toBe('/settings');
    expect(safeDashboardPath('/settings?foo=bar')).toBe('/settings?foo=bar');
    expect(safeDashboardPath('/')).toBe('/');
  });

  it('refuses a protocol-relative path — the open redirect this design exists to prevent', () => {
    expect(safeDashboardPath('//evil.com')).toBe('/');
    expect(safeDashboardPath('//evil.com/settings')).toBe('/');
  });

  it('refuses a backslash separator, which a browser reads as the same thing', () => {
    expect(safeDashboardPath('/\\evil.com')).toBe('/');
  });

  it('refuses an absolute URL', () => {
    expect(safeDashboardPath('https://evil.com')).toBe('/');
    expect(safeDashboardPath('evil.com')).toBe('/');
  });

  it('refuses anything absent, empty or not a string', () => {
    expect(safeDashboardPath('')).toBe('/');
    expect(safeDashboardPath(undefined)).toBe('/');
    // A repeated ?path= arrives as an array, which is not a path either.
    expect(safeDashboardPath(['/settings', '//evil.com'])).toBe('/');
  });
});

describe('Login', () => {
  it('bounces to the dashboard origin + path after a successful login', async () => {
    await mountLogin('?next=onecli-dashboard&path=%2Fsettings');
    await logIn();

    expect(dashboardCalls).toBe(1);
    expect(replace).toHaveBeenCalledTimes(1);
    expect(replace).toHaveBeenCalledWith(`${DASHBOARD_URL}/settings`);
    // The bounce leaves the SPA outright; the router never navigates.
    expect(history.get()).toBe('/login?next=onecli-dashboard&path=%2Fsettings');
  });

  it('bounces a session that was already valid when the redirect arrived', async () => {
    authenticatedOnServer = true;

    await mountLogin('?next=onecli-dashboard&path=%2Fsettings%3Ffoo%3Dbar');

    expect(replace).toHaveBeenCalledWith(`${DASHBOARD_URL}/settings?foo=bar`);
  });

  it('bounces to subdomain mode the same way — the mode is lab’s business, not the SPA’s', async () => {
    authenticatedOnServer = true;
    exposureOnServer = { mode: 'subdomain', url: 'https://onecli.example.test' };

    await mountLogin('?next=onecli-dashboard');

    expect(replace).toHaveBeenCalledWith('https://onecli.example.test/');
  });

  it('drops a hostile ?path= on the floor and lands on the dashboard root', async () => {
    authenticatedOnServer = true;

    await mountLogin('?next=onecli-dashboard&path=%2F%2Fevil.com');

    expect(replace).toHaveBeenCalledWith(`${DASHBOARD_URL}/`);
  });

  it('falls back to / when the dashboard is off — never a dead end', async () => {
    authenticatedOnServer = true;
    exposureOnServer = { mode: 'off' };

    await mountLogin('?next=onecli-dashboard&path=%2Fsettings');

    expect(dashboardCalls).toBe(1);
    expect(replace).not.toHaveBeenCalled();
    expect(history.get()).toBe('/');
    expect(container.querySelector('.home')).not.toBeNull();
  });

  it('falls back to / when the exposure has a mode but no URL', async () => {
    authenticatedOnServer = true;
    exposureOnServer = { mode: 'port' };

    await mountLogin('?next=onecli-dashboard');

    expect(replace).not.toHaveBeenCalled();
    expect(history.get()).toBe('/');
  });

  it('falls back to / when the exposure request fails, with nothing left unhandled', async () => {
    authenticatedOnServer = true;
    exposureOnServer = new Error('network down');

    // vitest fails the run on an unhandled rejection, so reaching the
    // assertions at all is half of what this test proves.
    await mountLogin('?next=onecli-dashboard&path=%2Fsettings');

    expect(dashboardCalls).toBe(1);
    expect(replace).not.toHaveBeenCalled();
    expect(history.get()).toBe('/');
  });

  it('ignores a ?next= keyword it does not know', async () => {
    authenticatedOnServer = true;

    await mountLogin('?next=somewhere-else&path=%2Fsettings');

    expect(dashboardCalls).toBe(0);
    expect(replace).not.toHaveBeenCalled();
    expect(history.get()).toBe('/');
  });

  it('an ordinary login with no query params goes to / and never asks for the exposure', async () => {
    await mountLogin();
    expect(container.querySelector('form')).not.toBeNull();

    await logIn();

    expect(history.get()).toBe('/');
    expect(container.querySelector('.home')).not.toBeNull();
    expect(dashboardCalls).toBe(0);
    expect(replace).not.toHaveBeenCalled();
  });
});
