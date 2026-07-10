// lab offline shell (PWA groundwork, D5): cache the app shell — index.html
// plus the content-hashed built assets it references — so the SPA opens
// offline. The shell HTML is parsed for its /assets/ URLs on install and on
// every fresh navigation: the hashed bundles are precached (an offline open
// needs the JS/CSS, not just the HTML), and bundles a newer shell no longer
// references are pruned, so superseded deploys never accumulate.
//
// /api and /agent are NEVER cached or intercepted: operational state must
// always hit the network; a stale answer is worse than an offline error.

const CACHE = 'lab-shell-v1';

// Paths the worker must never touch. Not calling respondWith leaves the
// request entirely to the network (including the /api/v1/events SSE stream).
const NETWORK_ONLY = /^\/(?:api|agent)\/|^\/(?:healthz|readyz|metrics)$/;

// Content-hashed bundles — the only paths safe to serve cache-first forever.
// Everything else static (manifest, icons) revalidates network-first so a
// changed file is picked up without a cache-name bump.
const HASHED = /^\/assets\//;

// The same-origin /assets/ URLs a shell HTML references (Vite emits them as
// absolute /assets/<name>-<hash>.<ext> script/link targets).
const shellAssets = (html) => [...new Set(html.match(/\/assets\/[A-Za-z0-9._-]+/g) ?? [])];

// Precache the assets `html` references (skipping ones already cached — the
// hash in the name makes them immutable) and prune hashed entries no shell
// references anymore, keeping storage bounded to one deploy's bundle set.
const syncAssets = async (cache, html) => {
  const assets = shellAssets(html);
  const keep = new Set(assets.map((p) => new URL(p, self.location.origin).href));
  await Promise.all(
    assets.map(async (p) => {
      if (await cache.match(p)) return;
      const response = await fetch(p);
      if (response.ok) await cache.put(p, response);
    }),
  );
  const keys = await cache.keys();
  await Promise.all(
    keys
      .filter((key) => HASHED.test(new URL(key.url).pathname) && !keep.has(key.url))
      .map((key) => cache.delete(key)),
  );
};

// Cache `response` as the offline shell and sync its asset set — only ever
// called with an OK text/html response (the shell is the only HTML the
// server serves; an asset navigation must not overwrite the '/' key).
const cacheShell = async (response) => {
  const cache = await caches.open(CACHE);
  const html = await response.clone().text();
  await cache.put('/', response);
  await syncAssets(cache, html);
};

const isShell = (response) =>
  response.ok && (response.headers.get('content-type') ?? '').includes('text/html');

self.addEventListener('install', (event) => {
  event.waitUntil(
    (async () => {
      const response = await fetch('/');
      if (isShell(response)) await cacheShell(response);
      const cache = await caches.open(CACHE);
      await cache.add('/manifest.webmanifest');
      await self.skipWaiting();
    })(),
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(keys.filter((key) => key !== CACHE).map((key) => caches.delete(key))),
      )
      .then(() => self.clients.claim()),
  );
});

const putInCache = (request, response) => {
  const copy = response.clone();
  caches
    .open(CACHE)
    .then((cache) => cache.put(request, copy))
    .catch(() => {});
};

self.addEventListener('fetch', (event) => {
  const { request } = event;
  if (request.method !== 'GET') return;
  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;
  if (NETWORK_ONLY.test(url.pathname)) return;

  if (request.mode === 'navigate') {
    // Navigations are network-first: a fresh shell when online, the cached
    // shell as the offline fallback. Every SPA route serves the same shell,
    // so it is cached under the single '/' key — but only a real HTML
    // response is: navigating straight to an image or download must not
    // replace the shell.
    event.respondWith(
      (async () => {
        try {
          const response = await fetch(request);
          if (isShell(response)) {
            const forCache = response.clone();
            event.waitUntil(cacheShell(forCache).catch(() => {}));
          }
          return response;
        } catch {
          return (await caches.match('/')) ?? Response.error();
        }
      })(),
    );
    return;
  }

  event.respondWith(
    (async () => {
      const cached = await caches.match(request);
      if (HASHED.test(url.pathname)) {
        // Hashed bundles are immutable: cache-first, misses fill from the
        // network. Never cache a non-OK or HTML answer under an asset key —
        // a stale hash miss must stay a miss, not a poisoned entry.
        if (cached) return cached;
        const response = await fetch(request);
        const ct = response.headers.get('content-type') ?? '';
        if (response.ok && !ct.includes('text/html')) putInCache(request, response);
        return response;
      }
      // Mutable-named statics (manifest, icons): network-first so a changed
      // file is picked up without a cache bump, cached copy as the offline
      // fallback.
      try {
        const response = await fetch(request);
        if (response.ok) putInCache(request, response);
        return response;
      } catch {
        return cached ?? Response.error();
      }
    })(),
  );
});

// Web Push (issue #98). The payload is JSON {title, body, tag, route}; `route`
// is a PWA-internal path (e.g. "/runs/abc") the click handler navigates to.
//
// WHY we always show a notification, even for absent or malformed data: iOS
// Safari revokes the push subscription of a service worker that receives a
// push without showing one, so a garbage payload still shows a minimal notice
// rather than silently losing the device's subscription.
self.addEventListener('push', (event) => {
  let payload = {};
  try {
    if (event.data) payload = event.data.json();
  } catch {
    // Non-JSON / truncated body — fall through to the minimal fallback below.
    payload = {};
  }
  const title = typeof payload.title === 'string' && payload.title !== '' ? payload.title : 'lab';
  const options = { body: typeof payload.body === 'string' ? payload.body : '' };
  // A tag coalesces notifications per run on the lock screen — pass it only
  // when non-empty; an empty tag would collapse unrelated notifications into
  // one and drop all but the last.
  if (typeof payload.tag === 'string' && payload.tag !== '') options.tag = payload.tag;
  // data.route is the only channel to the notificationclick handler (the
  // Notification survives the SW being killed and respawned between the two).
  options.data = { route: typeof payload.route === 'string' ? payload.route : '/' };
  event.waitUntil(self.registration.showNotification(title, options));
});

// A tapped notification focuses the existing app window (navigated to the
// payload route) or opens a new one — so a push is a one-tap deep link.
self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const route = event.notification.data?.route || '/';
  event.waitUntil(
    (async () => {
      const windowClients = await self.clients.matchAll({
        type: 'window',
        includeUncontrolled: true,
      });
      const client = windowClients[0];
      if (client) {
        // Guard both: navigate() rejects on a cross-origin or dead client, and
        // we still want to focus whatever window we have if navigate fails.
        await client.navigate(route).catch(() => {});
        await client.focus().catch(() => {});
        return;
      }
      await self.clients.openWindow(route);
    })(),
  );
});
