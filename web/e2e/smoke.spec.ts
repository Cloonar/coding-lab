// The one Playwright smoke (brief §13): against a fresh state dir, walk
// first-run setup, round-trip the credentials through logout/login, and land on
// the authenticated Home page, which is Runs (issue #76) — its empty state
// links to the composer at /new. The app shell wraps every authenticated page:
// at a desktop viewport the side rail is persistent, so its "Log out" button is
// on-screen; at a phone viewport the bottom tab bar replaces the rail and Log
// out lives on the More page. The first-run walk runs at desktop width, and
// one phone-viewport test covers the tab bar.

import { expect, test, type Page, type Request } from '@playwright/test';
import { createGitRemote } from './gitRemote';

const username = 'admin';
const password = 'smoke-test-password';

// Desktop viewport → the side rail is persistent (>=1024px) and the tab bar is
// not shown, so "Log out" (which lives in the rail) is visible throughout.
test.use({ viewport: { width: 1280, height: 800 } });

// The first test provisions the admin account against the shared throwaway
// state dir (only one first-run per webServer instance); every test after it
// depends on that account existing, so the file must run in declaration
// order and stop on a failure rather than each test racing setup separately.
test.describe.configure({ mode: 'serial' });

/** Log in with the already-provisioned admin account and land on Home (Runs). Each
 *  test gets a fresh, unauthenticated browser context, so this is how tests
 *  after the first one reach an authenticated page. */
async function login(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Log in' }).click();
  await expect(page).toHaveURL('/');
}

/** Home is Runs; a fresh instance has no live run, and the empty state links
 *  to the composer. */
async function expectEmptyRuns(page: Page): Promise<void> {
  // Inside the page itself: the desktop rail's run list says "No live runs." too.
  const runs = page.getByRole('main');
  await expect(runs.getByText('No live runs', { exact: true })).toBeVisible();
  await expect(runs.getByRole('link', { name: 'New run' })).toHaveAttribute('href', '/new');
}

/** Wait for the SW registration to become active. Never rejects on its own —
 *  if registration never happens (e.g. a non-PROD build) this hangs, which is
 *  the point: it should fail the test via timeout, not resolve falsely. */
async function swReady(page: Page): Promise<void> {
  await page.evaluate(() => navigator.serviceWorker.ready);
}

test('first-run setup, login, empty Runs home', async ({ page }) => {
  // Fresh state dir → /auth/state reports setup_required → guard lands on /setup.
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole('heading', { name: 'First-run setup' })).toBeVisible();

  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByLabel('Confirm password').fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();

  // Setup starts a session; the guard drops us on the authenticated Home page,
  // Runs, with its empty state.
  await expect(page).toHaveURL('/');
  await expectEmptyRuns(page);

  // The composer lives at /new now; with no repository it says so.
  await page.goto('/new');
  await expect(page.getByText('No repositories yet')).toBeVisible();

  // Round-trip the admin credentials: log out from the rail (guard bounces to
  // /login), then log back in with the password set during setup.
  await page.getByRole('button', { name: 'Log out' }).click();
  await expect(page).toHaveURL(/\/login$/);

  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Log in' }).click();

  await expect(page).toHaveURL('/');
  await expectEmptyRuns(page);
});

// --- Phone viewport: the bottom tab bar (issue #76) ---
//
// Below 1024px the rail is not shown; the tab bar is the navigation and the
// More page holds Log out.

test.describe('phone viewport', () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test('the tab bar navigates and More logs out', async ({ page }) => {
    await login(page);
    const tabs = page.getByRole('navigation', { name: 'Tabs' });
    await expect(tabs).toBeVisible();
    // No rail on the phone, and Runs is lit on the home page.
    await expect(page.getByRole('button', { name: 'Log out' })).toBeHidden();
    await expect(tabs.getByRole('link', { name: 'Runs' })).toHaveAttribute('aria-current', 'page');
    await expectEmptyRuns(page);

    await tabs.getByRole('link', { name: 'New' }).click();
    await expect(page).toHaveURL('/new');
    await expect(tabs.getByRole('link', { name: 'New' })).toHaveAttribute('aria-current', 'page');
    await expect(page.getByRole('heading', { name: 'New run' })).toBeVisible();
    await expect(page.getByText('No repositories yet')).toBeVisible();

    await tabs.getByRole('link', { name: 'More' }).click();
    await expect(page).toHaveURL('/more');
    await expect(tabs.getByRole('link', { name: 'More' })).toHaveAttribute('aria-current', 'page');
    await expect(page.getByRole('heading', { name: 'More' })).toBeVisible();

    await page.getByRole('button', { name: 'Log out' }).click();
    await expect(page).toHaveURL(/\/login$/);
  });
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

  // Deep link to the Notifications category (issue #198): the bare /settings
  // path redirects to the General section on a desktop-sized viewport.
  await page.goto('/settings/notifications');
  // The "ready" env in the Notifications section's detectPushEnv() only resolves once
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

// --- Repositories and repo settings (issue #61) ---
//
// Against the real server, with a local bare repository as the remote (the
// clone needs no network): the empty list, Add repository, the repo home with
// its readiness block and tabs, the one-page settings with its save bar and a
// browser-side refusal, a Schedule through the editor route, and the delete
// dialog. Nothing here starts a run — Run now is only checked for presence.

test('repositories: add, repo home tabs, one-page settings, schedule, delete', async ({ page }) => {
  test.setTimeout(120_000);
  const remote = await createGitRemote('smoke-remote');
  try {
    await login(page);
    await walkRepositories(page, remote.url);
  } finally {
    remote.dispose();
  }
});

async function walkRepositories(page: Page, remoteURL: string): Promise<void> {
  const tabs = page.getByRole('navigation', { name: 'Repository' });
  const saveBar = page.getByRole('region', { name: 'Unsaved changes' });
  let repoID = '';

  await test.step('the empty list opens Add repository', async () => {
    await page.goto('/repos');
    await expect(page.getByRole('heading', { name: 'Repositories', level: 1 })).toBeVisible();
    await expect(page.getByText('No repositories yet')).toBeVisible();
    await page.getByRole('link', { name: 'Add repository' }).click();
    await expect(page).toHaveURL('/repos/new');
    await expect(page.getByRole('heading', { name: 'Add repository' })).toBeVisible();
  });

  await test.step('Add repository lands on the repo home, which finishes cloning', async () => {
    await page.getByLabel('Remote URL').fill(remoteURL);
    // The name follows the URL: its basename without .git.
    await expect(page.getByLabel('Name')).toHaveValue('smoke-remote');
    await expect(page.getByRole('radio', { name: 'Auto' })).toBeChecked();
    await page.getByRole('button', { name: 'Add and start cloning' }).click();

    // The new repo's home: /repos/<id>, not the form's own /repos/new.
    await expect(page).toHaveURL(/\/repos\/(?!new$)[^/]+$/);
    repoID = new URL(page.url()).pathname.split('/')[2] ?? '';
    await expect(page.getByRole('heading', { name: 'smoke-remote', level: 1 })).toBeVisible();
    const readiness = page.getByRole('region', { name: 'Readiness' });
    await expect(readiness).toBeVisible();

    // A local clone is quick; the header's chip goes once it is done.
    await expect(page.getByText('cloning', { exact: true })).toBeHidden({ timeout: 30_000 });
    // The clone check passes. The block is collapsed when every check passes
    // (an agent login on the host can make that so) and open otherwise; the
    // report may still move while the clone settles, hence the retry.
    const toggle = readiness.getByRole('heading').getByRole('button');
    await expect(async () => {
      if ((await toggle.getAttribute('aria-expanded')) === 'false') await toggle.click();
      await expect(readiness.getByText('Passing: Clone', { exact: true })).toBeVisible({
        timeout: 1_000,
      });
    }).toPass({ timeout: 15_000 });
  });

  await test.step('each tab has its own URL inside the frame', async () => {
    const base = `/repos/${repoID}`;
    await expect(tabs.getByRole('link', { name: 'Overview' })).toHaveAttribute(
      'aria-current',
      'page',
    );

    // The open count is known once the summary is: none on a fresh repo.
    const issuesTab = tabs.getByRole('link', { name: 'Issues (0 open)' });
    await issuesTab.click();
    await expect(page).toHaveURL(`${base}/issues`);
    await expect(issuesTab).toHaveAttribute('aria-current', 'page');
    await expect(page.getByRole('heading', { name: 'Issues', level: 2 })).toBeVisible();
    await expect(page.getByText('No open issues.')).toBeVisible();

    // Builtin-bound (Auto without a forge credential), so CRs is a tab.
    const crsTab = tabs.getByRole('link', { name: 'CRs' });
    await crsTab.click();
    await expect(page).toHaveURL(`${base}/crs`);
    await expect(crsTab).toHaveAttribute('aria-current', 'page');
    await expect(page.getByRole('heading', { name: 'Change requests' })).toBeVisible();

    const settingsTab = tabs.getByRole('link', { name: 'Settings' });
    await settingsTab.click();
    await expect(page).toHaveURL(`${base}/settings`);
    await expect(settingsTab).toHaveAttribute('aria-current', 'page');
    // One page: every section is here, and the desktop outline lists them
    // under their group labels.
    const outline = page.getByRole('navigation', { name: 'Settings sections' });
    for (const group of ['Runs', 'Automation', 'Access', 'Setup']) {
      await expect(outline.getByRole('group', { name: group })).toBeVisible();
    }
    for (const section of [
      'Agents',
      'Runner',
      'Autoland',
      'Schedules',
      'Secrets',
      'Imports',
      'General',
      'Integrations',
      'Branches',
      'Danger zone',
    ]) {
      await expect(outline.getByRole('link', { name: section, exact: true })).toBeVisible();
      await expect(
        page.getByRole('heading', { name: section, level: 2, exact: true }),
      ).toBeAttached();
    }

    // A section's own URL opens the page scrolled to it.
    await page.goto(`${base}/settings/branches`);
    await expect(page.getByRole('heading', { name: 'Branches', level: 2 })).toBeInViewport();
    await expect(outline.getByRole('link', { name: 'Branches', exact: true })).toHaveAttribute(
      'aria-current',
      'location',
    );
  });

  await test.step('one save bar saves two sections in one request', async () => {
    const general = page.getByRole('region', { name: 'General' });
    const branches = page.getByRole('region', { name: 'Branches' });
    // The changed mark joins the label ("Name (unsaved change)"), so match its start.
    const name = general.getByRole('textbox', { name: /^Name/ });
    const prefix = branches.getByRole('textbox', { name: /^Manual branch prefix/ });

    await expect(saveBar).toBeHidden();
    await name.fill('smoke-renamed');
    await prefix.fill('wip/');
    await expect(saveBar.getByText('2 unsaved changes')).toBeVisible();
    await expect(saveBar.getByRole('link', { name: 'General' })).toBeVisible();
    await expect(saveBar.getByRole('link', { name: 'Branches' })).toBeVisible();

    const patched = page.waitForResponse(
      (response) =>
        response.request().method() === 'PATCH' &&
        new URL(response.url()).pathname === `/api/v1/repos/${repoID}`,
    );
    await saveBar.getByRole('button', { name: 'Save' }).click();
    const response = await patched;
    expect(response.ok()).toBe(true);
    // Only the changed fields travel.
    expect(response.request().postDataJSON()).toEqual({
      name: 'smoke-renamed',
      manual_branch_prefix: 'wip/',
    });
    await expect(saveBar).toBeHidden();
    await expect(page.getByText('Saved 2 changes to smoke-renamed')).toBeVisible();

    await page.reload();
    await expect(page.getByRole('heading', { name: 'smoke-renamed', level: 1 })).toBeVisible();
    await expect(name).toHaveValue('smoke-renamed');
    await expect(prefix).toHaveValue('wip/');
  });

  await test.step('an AFK branch pattern without <N> is refused before any request', async () => {
    const pattern = page
      .getByRole('region', { name: 'Branches' })
      .getByRole('textbox', { name: /^AFK branch pattern/ });
    const saved = await pattern.inputValue();
    const patches: string[] = [];
    const onRequest = (request: Request) => {
      if (request.method() === 'PATCH') patches.push(request.url());
    };
    page.on('request', onRequest);
    try {
      await pattern.fill('afk/no-number');
      await expect(saveBar.getByText('1 unsaved change')).toBeVisible();
      await saveBar.getByRole('button', { name: 'Save' }).click();

      await expect(saveBar.getByText('1 problem to fix')).toBeVisible();
      await expect(saveBar.getByRole('link', { name: 'Branches' })).toBeVisible();
      await expect(pattern).toBeFocused();
      await expect(pattern).toHaveAttribute('aria-invalid', 'true');
      await expect(
        page.getByRole('alert').filter({ hasText: 'The pattern needs <N> exactly once.' }),
      ).toBeVisible();
      expect(patches).toEqual([]);
    } finally {
      page.off('request', onRequest);
    }

    // Back to the saved value: nothing is pending, so the bar goes.
    await pattern.fill(saved);
    await expect(saveBar).toBeHidden();
    await expect(pattern).not.toHaveAttribute('aria-invalid', 'true');
  });

  await test.step('a Schedule is added through the editor route', async () => {
    // Twelve hours away, so the cadence cannot fire while the smoke runs.
    const at = new Date(Date.now() + 12 * 60 * 60 * 1000);
    const time = `${String(at.getHours()).padStart(2, '0')}:${String(at.getMinutes()).padStart(2, '0')}`;

    await page.getByRole('link', { name: 'New schedule' }).click();
    await expect(page).toHaveURL(`/repos/${repoID}/settings/schedules/new`);
    const editor = page.getByRole('dialog', { name: 'New schedule' });
    await expect(editor).toBeVisible();
    // A Schedule never saved has nothing to run.
    await expect(editor.getByRole('button', { name: 'Run now' })).toHaveCount(0);

    await editor.getByLabel('Name').fill('Nightly smoke');
    await editor.getByLabel('Prompt').fill('List the files at the repository root.');
    await editor.getByRole('radio', { name: 'Daily' }).click();
    await expect(editor.getByRole('radio', { name: 'Daily' })).toBeChecked();
    await editor.getByLabel('Time, server-local').fill(time);
    await editor.getByRole('button', { name: 'Save schedule' }).click();

    await expect(editor).toBeHidden();
    await expect(page.getByText('Saved "Nightly smoke"')).toBeVisible();
    const row = page.getByRole('link', { name: /Nightly smoke/ });
    await expect(row).toContainText(`Daily at ${time}`);
    // The next run is the server's rendering of the cadence.
    await expect(row).toContainText(
      new RegExp(`Next \\w{3} \\d{4}-\\d{2}-\\d{2} ${time} · last run never ran`),
    );

    // Opened again, the saved Schedule offers Run now (not pressed: no run
    // starts in the smoke).
    await row.click();
    await expect(page).toHaveURL(new RegExp(`/repos/${repoID}/settings/schedules/(?!new$)[^/]+$`));
    const edit = page.getByRole('dialog', { name: 'Edit schedule' });
    await expect(edit.getByRole('button', { name: 'Run now' })).toBeEnabled();
    await expect(edit.getByLabel('Name')).toHaveValue('Nightly smoke');
    await edit.getByRole('button', { name: 'Back to schedules' }).click();
    await expect(edit).toBeHidden();
  });

  await test.step('Delete repository states its consequences and returns to the list', async () => {
    await page
      .getByRole('region', { name: 'Danger zone' })
      .getByRole('button', { name: 'Delete repository' })
      .click();
    const dialog = page.getByRole('alertdialog', { name: 'Delete smoke-renamed?' });
    await expect(dialog).toBeVisible();
    // Only what applies to this repo: no live runs, no clone in flight, no
    // parked work, no secrets — one Schedule and the built-in tracker.
    await expect(dialog.getByRole('listitem')).toHaveText([
      'Deletes 1 Schedule.',
      "Deletes the issues and change requests kept in lab's built-in tracker.",
      "Removes lab's clone, this repository's settings and its run history.",
    ]);
    const confirm = dialog.getByRole('button', { name: 'Delete repository' });
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel('Type smoke-renamed to confirm').fill('smoke-renamed');
    await expect(confirm).toBeEnabled();
    await confirm.click();

    await expect(page).toHaveURL('/repos');
    await expect(page.getByText('Deleted smoke-renamed from lab')).toBeVisible();
    await expect(page.getByText('No repositories yet')).toBeVisible();
  });
}
