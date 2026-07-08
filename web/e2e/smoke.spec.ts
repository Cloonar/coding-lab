// The one Playwright smoke (brief §13): against a fresh state dir, walk
// first-run setup, round-trip the credentials through logout/login, and land on
// the authenticated Home (new-run) page's empty state. Post issue #41 the app
// shell wraps every authenticated page: at a desktop viewport the side rail is
// persistent, so its "Log out" button is on-screen without opening a drawer.

import { expect, test } from '@playwright/test';

const username = 'admin';
const password = 'smoke-test-password';

// Desktop viewport → the side rail is persistent (>=1024px), not an overlay
// drawer, so "Log out" (which lives in the rail now) is visible throughout.
test.use({ viewport: { width: 1280, height: 800 } });

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
