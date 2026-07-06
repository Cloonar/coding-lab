// The one Playwright smoke (brief §13): against a fresh state dir, walk
// first-run setup, round-trip the credentials through logout/login, and land
// on the authenticated dashboard's empty state.

import { expect, test } from '@playwright/test';

const username = 'admin';
const password = 'smoke-test-password';

test('first-run setup, login, empty dashboard', async ({ page }) => {
  // Fresh state dir → /auth/state reports setup_required → guard lands on /setup.
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole('heading', { name: 'First-run setup' })).toBeVisible();

  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByLabel('Confirm password').fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();

  // Setup starts a session; the guard drops us on the authenticated
  // dashboard with the add-repo empty state.
  await expect(page).toHaveURL('/');
  await expect(page.getByText('No repositories yet')).toBeVisible();

  // Round-trip the admin credentials: log out (guard bounces to /login),
  // then log back in with the password set during setup.
  await page.getByRole('button', { name: 'Log out' }).click();
  await expect(page).toHaveURL(/\/login$/);

  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Log in' }).click();

  await expect(page).toHaveURL('/');
  await expect(page.getByText('No repositories yet')).toBeVisible();
});
