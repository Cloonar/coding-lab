// Claude auth card logout gate (issue #46): the "Log out" button shows only
// while logged in, and the machine-wide logout POST fires only after the
// confirm dialog — which names the running-instance count and warns that AFK
// auto stays on.

import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ClaudeAuthStatus } from '../api';
import { EventsProvider } from '../events';
import ClaudeAuthCard from './ClaudeAuthCard';

/** Minimal EventSource stand-in so EventsProvider can mount under jsdom. */
class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

const LOGGED_IN: ClaudeAuthStatus = {
  logged_in: true,
  email: 'op@example.invalid',
  method: 'claude.ai',
  checked_at: '2026-07-08T00:00:00.000Z',
};
const LOGGED_OUT: ClaudeAuthStatus = {
  logged_in: false,
  email: '',
  method: '',
  checked_at: '2026-07-08T00:00:01.000Z',
};

let authOnServer: ClaudeAuthStatus;
let logoutCalls: number;
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
      if (url.startsWith('/api/v1/providers/claude/auth/status') && method === 'GET') {
        return Promise.resolve(jsonResponse(200, authOnServer));
      }
      if (url === '/api/v1/providers/claude/auth/logout' && method === 'POST') {
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

async function mountCard(activeRuns = 0): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(
    () => (
      <EventsProvider>
        <ClaudeAuthCard activeRuns={activeRuns} />
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

beforeEach(() => {
  authOnServer = LOGGED_IN;
  logoutCalls = 0;
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('ClaudeAuthCard logout gate', () => {
  it('shows the Log out button only while logged in', async () => {
    await mountCard();
    expect(maybe('button.claude-logout')).not.toBeNull();

    dispose?.();
    container.remove();

    // A logged-out card offers login instead, never a Log out button.
    authOnServer = LOGGED_OUT;
    await mountCard();
    expect(maybe('button.claude-logout')).toBeNull();
    expect(container.textContent).toContain('Log in to Claude');
  });

  it('gates the logout POST behind the confirm dialog', async () => {
    await mountCard();

    // Clicking Log out reveals the confirm — no POST yet.
    query<HTMLButtonElement>('button.claude-logout').click();
    await settle();
    expect(maybe('[role="alertdialog"]')).not.toBeNull();
    expect(logoutCalls).toBe(0);

    // Cancel dismisses it without logging out.
    const cancel = Array.from(container.querySelectorAll('button')).find(
      (b) => b.textContent === 'Cancel',
    );
    cancel?.click();
    await settle();
    expect(maybe('[role="alertdialog"]')).toBeNull();
    expect(logoutCalls).toBe(0);

    // Re-open and confirm: the POST fires and the card flips to logged-out.
    query<HTMLButtonElement>('button.claude-logout').click();
    await settle();
    query<HTMLButtonElement>('button.claude-logout-confirm').click();
    await settle();

    expect(logoutCalls).toBe(1);
    expect(maybe('button.claude-logout')).toBeNull();
    expect(container.textContent).toContain('Log in to Claude');
  });

  it('names the running-instance count and the AFK warning in the confirm', async () => {
    await mountCard(3);

    query<HTMLButtonElement>('button.claude-logout').click();
    await settle();

    const dialog = query('[role="alertdialog"]');
    expect(dialog.textContent).toContain('3 running instance');
    expect(dialog.textContent).toContain('AFK auto stays on');
  });
});
