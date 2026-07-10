// The one Playwright smoke (brief §13): against a fresh state dir, walk
// first-run setup, round-trip the credentials through logout/login, and land on
// the authenticated Home (new-run) page's empty state. Post issue #41 the app
// shell wraps every authenticated page: at a desktop viewport the side rail is
// persistent, so its "Log out" button is on-screen without opening a drawer.

import { expect, test, type Page } from '@playwright/test';

const username = 'admin';
const password = 'smoke-test-password';

// Desktop viewport → the side rail is persistent (>=1024px), not an overlay
// drawer, so "Log out" (which lives in the rail now) is visible throughout.
test.use({ viewport: { width: 1280, height: 800 } });

// The first test provisions the admin account against the shared throwaway
// state dir (only one first-run per webServer instance); every test after it
// depends on that account existing, so the file must run in declaration
// order and stop on a failure rather than each test racing setup separately.
test.describe.configure({ mode: 'serial' });

/** Log in with the already-provisioned admin account and land on Home. Each
 *  test gets a fresh, unauthenticated browser context, so this is how tests
 *  after the first one reach an authenticated page. */
async function login(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Log in' }).click();
  await expect(page).toHaveURL('/');
}

/** Wait for the SW registration to become active. Never rejects on its own —
 *  if registration never happens (e.g. a non-PROD build) this hangs, which is
 *  the point: it should fail the test via timeout, not resolve falsely. */
async function swReady(page: Page): Promise<void> {
  await page.evaluate(() => navigator.serviceWorker.ready);
}

test('first-run setup, login, empty home', async ({ page }) => {
  // Fresh state dir → /auth/state reports setup_required → guard lands on /setup.
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole('heading', { name: 'First-run setup' })).toBeVisible();

  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByLabel('Confirm password').fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();

  // Setup starts a session; the guard drops us on the authenticated Home page
  // with the add-repo empty state.
  await expect(page).toHaveURL('/');
  await expect(page.getByText('No repositories yet')).toBeVisible();

  // Round-trip the admin credentials: log out from the rail (guard bounces to
  // /login), then log back in with the password set during setup.
  await page.getByRole('button', { name: 'Log out' }).click();
  await expect(page).toHaveURL(/\/login$/);

  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Log in' }).click();

  await expect(page).toHaveURL('/');
  await expect(page.getByText('No repositories yet')).toBeVisible();
});

// --- Service worker smoke (issue #98) ---
//
// serve.sh runs `npm run build` and embeds the real dist/ into the `lab`
// binary, so PROD-only registration (src/pwa.ts) fires and web/public/sw.js
// is the actual worker under test here — not a dev-server stand-in.

test('service worker registers and takes control of the page', async ({ page }) => {
  await login(page);
  await swReady(page);
  // A page's own first navigation can predate self.clients.claim() taking
  // effect for that exact client — ready resolving only promises an active
  // worker for the registration, not that THIS tab is controlled yet. One
  // reload always lands on a controlled page.
  await page.reload();
  await swReady(page);

  const controlled = await page.evaluate(() => navigator.serviceWorker.controller !== null);
  expect(controlled).toBe(true);
});

test('network-only paths bypass the cache while the app shell is precached', async ({ page }) => {
  await login(page);
  await swReady(page);
  await page.reload();
  await swReady(page);

  // /healthz and /api/v1/auth/state both match sw.js's NETWORK_ONLY regex —
  // fetched live here, then asserted absent from every cache. The shell is
  // cached under the '/' key by sw.js's cacheShell() on install and on every
  // navigation, so it must be present as the control for "the SW is active
  // and caching something, it just isn't caching THESE".
  const result = await page.evaluate(async () => {
    const [healthz, authState] = await Promise.all([
      fetch('/healthz'),
      fetch('/api/v1/auth/state'),
    ]);
    return {
      healthzOk: healthz.ok,
      authStateOk: authState.ok,
      healthzCached: (await caches.match('/healthz')) !== undefined,
      authStateCached: (await caches.match('/api/v1/auth/state')) !== undefined,
      shellCached: (await caches.match('/')) !== undefined,
    };
  });

  expect(result.healthzOk).toBe(true);
  expect(result.authStateOk).toBe(true);
  expect(result.healthzCached).toBe(false);
  expect(result.authStateCached).toBe(false);
  expect(result.shellCached).toBe(true);
});

test('settings notifications card reaches the ready state', async ({ page }) => {
  await login(page);
  await swReady(page);

  await page.goto('/settings');
  // The "ready" env in Settings.tsx's detectPushEnv() only resolves once
  // navigator.serviceWorker.getRegistration() finds a registration — so this
  // heading/button pair is also a UI-level assertion that the SW is
  // registered, not just that the card rendered.
  await expect(page.getByRole('heading', { name: 'Notifications' })).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Enable notifications on this device' }),
  ).toBeVisible();
});

// grantPermissions() is a Chromium-only CDP feature; playwright.config.ts
// pins this project to browserName: 'chromium', so no runtime skip is needed
// here — a different project would need one.
test('shows a real notification through the SW registration', async ({ page, context }) => {
  await context.grantPermissions(['notifications']);
  await login(page);
  await swReady(page);

  // This exercises the exact showNotification()/getNotifications() surface
  // sw.js's `push` handler drives, with a real permission grant — as close as
  // a browser test can get to a push without a live push service. Dispatching
  // an actual PushEvent from a page isn't possible in any browser API;
  // sw.js's payload parsing, fallback text, and tag/data plumbing are covered
  // hermetically against synthetic PushEvents in src/sw.test.ts instead.
  const count = await page.evaluate(async () => {
    const registration = await navigator.serviceWorker.ready;
    await registration.showNotification('smoke', { tag: 'smoke' });
    const notifications = await registration.getNotifications({ tag: 'smoke' });
    return notifications.length;
  });
  expect(count).toBe(1);

  await page.evaluate(async () => {
    const registration = await navigator.serviceWorker.ready;
    const notifications = await registration.getNotifications({ tag: 'smoke' });
    for (const notification of notifications) notification.close();
  });
});
