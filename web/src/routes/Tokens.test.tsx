// Token-shown-once contract: the secret from POST /tokens appears exactly
// once — in the reveal card's copy-able field with the store-it-now warning
// — and is gone for good after Done. The list only ever renders metadata
// (name, created, last used), never a secret.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ApiToken } from '../api';
import App from '../App';
import Tokens from './Tokens';

const SECRET = 'lab_pat_c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0';

class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

let tokensOnServer: ApiToken[];
let createBodies: Record<string, unknown>[];
let deletedIDs: string[];
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
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
        );
      }
      if (url === '/api/v1/tokens' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { tokens: tokensOnServer }));
      }
      if (url === '/api/v1/tokens' && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as { name: string };
        createBodies.push(body);
        // The metadata row exists from now on; the secret exists ONLY in
        // this one response.
        tokensOnServer = [
          ...tokensOnServer,
          {
            id: 'tok_new',
            name: body.name,
            created_at: '2026-07-06T00:00:00.000Z',
            last_used_at: null,
          },
        ];
        return Promise.resolve(
          jsonResponse(201, { id: 'tok_new', name: body.name, token: SECRET }),
        );
      }
      if (url.startsWith('/api/v1/tokens/') && method === 'DELETE') {
        const id = url.slice('/api/v1/tokens/'.length);
        deletedIDs.push(id);
        tokensOnServer = tokensOnServer.filter((t) => t.id !== id);
        return Promise.resolve(jsonResponse(204));
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

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountTokens(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/tokens' });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/tokens" component={Tokens} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function query<T extends Element>(selector: string): T {
  const el = container.querySelector<T>(selector);
  if (!el) throw new Error(`missing ${selector}`);
  return el;
}

function button(text: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'));
  const el = buttons.find((b) => b.textContent?.trim() === text);
  if (!el) throw new Error(`missing button ${JSON.stringify(text)}`);
  return el;
}

function typeInto(el: HTMLInputElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/** The secret must never leak outside the reveal input's value. */
function secretOccurrences(): number {
  let count = 0;
  for (const input of Array.from(container.querySelectorAll('input'))) {
    if (input.value === SECRET) count += 1;
  }
  if (container.textContent?.includes(SECRET)) count += 1;
  return count;
}

beforeEach(() => {
  tokensOnServer = [];
  createBodies = [];
  deletedIDs = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('Tokens page (token shown once)', () => {
  it('lists metadata with never-used and last-used rendering', async () => {
    tokensOnServer = [
      {
        id: 'tok_1',
        name: 'ci-deploy',
        created_at: '2026-07-01T00:00:00.000Z',
        last_used_at: null,
      },
      {
        id: 'tok_2',
        name: 'phone',
        created_at: '2026-07-01T00:00:00.000Z',
        last_used_at: '2026-07-05T10:00:00.000Z',
      },
    ];
    await mountTokens();

    expect(container.textContent).toContain('ci-deploy');
    expect(container.textContent).toContain('never used');
    expect(container.textContent).toContain('phone');
    expect(container.textContent).toContain('last used');
  });

  it('creates a token, reveals the secret exactly once, and Done destroys it', async () => {
    await mountTokens();
    expect(container.textContent).toContain('No API tokens yet');

    typeInto(query<HTMLInputElement>('input[name="token-name"]'), 'ci-deploy');
    button('Create token').click();
    await settle();

    expect(createBodies).toEqual([{ name: 'ci-deploy' }]);

    // The reveal card: warning + the secret in a copy-able field.
    expect(container.textContent).toContain('It will not be shown again.');
    expect(query<HTMLInputElement>('input[name="token-value"]').value).toBe(SECRET);
    expect(query<HTMLInputElement>('input[name="token-value"]').readOnly).toBe(true);
    expect(secretOccurrences()).toBe(1);

    // The refetched list shows the metadata row (no secret anywhere in it).
    expect(container.textContent).toContain('ci-deploy');

    button('Done').click();
    await settle();

    // Gone for good: no input holds the secret, no text node mentions it.
    expect(container.querySelector('input[name="token-value"]')).toBeNull();
    expect(secretOccurrences()).toBe(0);
    expect(container.textContent).toContain('ci-deploy'); // the row remains
  });

  it('deletes only after the confirm and refetches the list', async () => {
    tokensOnServer = [
      {
        id: 'tok_1',
        name: 'ci-deploy',
        created_at: '2026-07-01T00:00:00.000Z',
        last_used_at: null,
      },
    ];
    await mountTokens();

    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValueOnce(false);
    button('Delete').click();
    await settle();
    expect(deletedIDs).toEqual([]);

    confirmSpy.mockReturnValueOnce(true);
    button('Delete').click();
    await settle();

    expect(deletedIDs).toEqual(['tok_1']);
    expect(container.textContent).toContain('No API tokens yet');
    confirmSpy.mockRestore();
  });
});
