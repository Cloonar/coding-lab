// Service-worker push contract (issue #98). Evaluates the REAL web/public/sw.js
// source fresh per test against a stubbed ServiceWorkerGlobalScope, capturing
// the listeners it registers. Two things are load-bearing: a push ALWAYS shows
// a notification (iOS revokes a silent worker's subscription), and the added
// listeners must not perturb the network-only fetch rule — /api, /agent and the
// health endpoints are never intercepted.
//
// The source is loaded via Vite's ?raw (the real file, no copy) and run through
// `new Function` with `self` as its only injected binding: the worker needs
// nothing else at load time — it merely registers its event listeners — and a
// fresh evaluation per test is a clean-slate global scope without touching the
// jsdom one (dynamic import of a file under public/ resolves to an http module
// id the ESM loader rejects).

import swSource from '../public/sw.js?raw';
import { describe, expect, it, vi } from 'vitest';

const ORIGIN = 'https://lab.example';

interface Listeners {
  [type: string]: (event: unknown) => void;
}

interface FakeClient {
  navigate: ReturnType<typeof vi.fn>;
  focus: ReturnType<typeof vi.fn>;
}

interface Harness {
  /** Invokes a registered listener with a waitUntil-capturing event and awaits
   *  everything it deferred. */
  fire: (type: string, event: Record<string, unknown>) => Promise<void>;
  showNotification: ReturnType<typeof vi.fn>;
  matchAll: ReturnType<typeof vi.fn>;
  openWindow: ReturnType<typeof vi.fn>;
}

/** Evaluates a fresh sw.js against a stubbed scope; `windowClients` seeds
 *  clients.matchAll (the windows a notificationclick can focus). */
function loadSW(windowClients: FakeClient[] = []): Harness {
  const listeners: Listeners = {};
  const showNotification = vi.fn(() => Promise.resolve());
  const matchAll = vi.fn(() => Promise.resolve(windowClients));
  const openWindow = vi.fn(() => Promise.resolve(null));
  const self = {
    addEventListener: (type: string, cb: (event: unknown) => void) => {
      listeners[type] = cb;
    },
    registration: { showNotification },
    clients: { matchAll, openWindow, claim: () => Promise.resolve() },
    location: { origin: ORIGIN },
    skipWaiting: () => Promise.resolve(),
  };
  new Function('self', swSource)(self);

  const fire = async (type: string, event: Record<string, unknown>): Promise<void> => {
    const cb = listeners[type];
    if (!cb) throw new Error(`no ${type} listener registered`);
    const waited: unknown[] = [];
    cb({ ...event, waitUntil: (p: unknown) => waited.push(p) });
    await Promise.all(waited);
  };
  return { fire, showNotification, matchAll, openWindow };
}

function fakeClient(): FakeClient {
  return { navigate: vi.fn(() => Promise.resolve()), focus: vi.fn(() => Promise.resolve()) };
}

describe('service worker push', () => {
  it('shows a notification with the payload title, body, tag and route', async () => {
    const { fire, showNotification } = loadSW();
    await fire('push', {
      data: {
        json: () => ({
          title: 'Run needs you',
          body: 'coding-lab #98 is blocked',
          tag: 'run:abc',
          route: '/runs/abc',
        }),
      },
    });

    expect(showNotification).toHaveBeenCalledTimes(1);
    const [title, options] = showNotification.mock.calls[0] as [string, Record<string, unknown>];
    expect(title).toBe('Run needs you');
    expect(options.body).toBe('coding-lab #98 is blocked');
    expect(options.tag).toBe('run:abc');
    expect(options.data).toEqual({ route: '/runs/abc' });
  });

  it('still shows a notification for a malformed payload (fallback title "lab")', async () => {
    const { fire, showNotification } = loadSW();
    await fire('push', {
      data: {
        json: () => {
          throw new SyntaxError('not json');
        },
      },
    });

    expect(showNotification).toHaveBeenCalledTimes(1);
    const [title, options] = showNotification.mock.calls[0] as [string, Record<string, unknown>];
    expect(title).toBe('lab');
    expect(options.body).toBe('');
    // No tag when the payload carried none — an empty tag would coalesce
    // unrelated notifications on the lock screen.
    expect('tag' in options).toBe(false);
  });

  it('still shows a notification when the push carries no data at all', async () => {
    const { fire, showNotification } = loadSW();
    await fire('push', { data: null });

    expect(showNotification).toHaveBeenCalledTimes(1);
    expect(showNotification.mock.calls[0]?.[0]).toBe('lab');
  });
});

describe('service worker notificationclick', () => {
  it('focuses and navigates an existing window client to the route', async () => {
    const client = fakeClient();
    const { fire, openWindow } = loadSW([client]);
    const close = vi.fn();
    await fire('notificationclick', {
      notification: { data: { route: '/runs/abc' }, close },
    });

    expect(close).toHaveBeenCalledTimes(1);
    expect(client.navigate).toHaveBeenCalledWith('/runs/abc');
    expect(client.focus).toHaveBeenCalledTimes(1);
    expect(openWindow).not.toHaveBeenCalled();
  });

  it('opens a new window when no client exists', async () => {
    const { fire, openWindow } = loadSW([]);
    const close = vi.fn();
    await fire('notificationclick', {
      notification: { data: { route: '/runs/xyz' }, close },
    });

    expect(openWindow).toHaveBeenCalledWith('/runs/xyz');
  });

  it("falls back to '/' when the notification carries no route", async () => {
    const { fire, openWindow } = loadSW([]);
    await fire('notificationclick', {
      notification: { data: undefined, close: vi.fn() },
    });

    expect(openWindow).toHaveBeenCalledWith('/');
  });
});

describe('service worker fetch (network-only rule preserved)', () => {
  it('never intercepts /api, /agent or the health endpoints', async () => {
    const { fire } = loadSW();
    for (const path of ['/api/v1/x', '/agent/y', '/healthz', '/readyz', '/metrics']) {
      const respondWith = vi.fn();
      await fire('fetch', {
        request: { method: 'GET', url: `${ORIGIN}${path}`, mode: 'cors' },
        respondWith,
      });
      expect(respondWith, `must not intercept ${path}`).not.toHaveBeenCalled();
    }
  });
});
