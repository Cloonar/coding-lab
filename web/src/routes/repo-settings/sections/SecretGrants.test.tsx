// Credential-gateway grant picker suite (issue #25 / ADR-0067) on
// /repos/:id/settings/secrets, where the picker renders above the legacy
// per-repo Secrets card. Modeled on Secrets.test.tsx and Imports.test.tsx: the
// section is device-local/immediate, so there is no form to submit, no Save
// button and no leave guard — every assertion is either on the exact request
// the toggle issued or on what the section renders after it.
//
// The properties worth pinning here: the picker is not optimistic (a failed
// attach leaves the row exactly as the server last described it), the four
// read states are visibly different from one another and none of them is an
// endless loading line, the dashboard link-out comes from the exposure
// endpoint or does not exist at all, and nothing about a repo's grant set is
// cached lab-side across a remount.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  container,
  grantsSection,
  h,
  installRepoSettingsHooks,
  mountSettings,
  settle,
  unmount,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountGrants = () => mountSettings(`/repos/${REPO_ID}/settings/secrets`);

/** A grant chip-toggle by pool kind + resource id (its form name). */
const toggle = (kind: string, id: string): HTMLButtonElement => {
  const el = container.querySelector<HTMLButtonElement>(`button[name="grant-${kind}-${id}"]`);
  if (!el) throw new Error(`missing chip-toggle button[name="grant-${kind}-${id}"]`);
  return el;
};

/** Every grant toggle in the picker, in render order. */
const toggles = (): HTMLButtonElement[] =>
  Array.from(grantsSection().querySelectorAll<HTMLButtonElement>('button[aria-pressed]'));

const waitForPicker = () =>
  waitFor(() => (toggles().length > 0 ? true : null), 'grant picker rows');

describe('RepoSettings credential-gateway grant picker', () => {
  it('renders the whole pool — secrets first, then connections — with what each unlocks', async () => {
    await mountGrants();
    await waitForPicker();

    const section = grantsSection();
    expect(section.textContent).toContain('ANTHROPIC_API_KEY');
    expect(section.textContent).toContain('Secret · unlocks anthropic');
    expect(section.textContent).toContain('GitHub app');
    expect(section.textContent).toContain('Connection · unlocks github');
    // Pool order: the secrets half precedes the connections half.
    expect(toggles().map((t) => t.getAttribute('name'))).toEqual([
      'grant-secrets-sec_pool_1',
      'grant-connections-conn_pool_1',
    ]);
  });

  it('reflects the repo current grants, matching on kind AND id', async () => {
    // Same id in both halves of the pool: only the granted KIND may light up.
    h.poolOnServer = {
      configured: true,
      secrets: [{ id: 'shared_id', name: 'SHARED_SECRET', provider: 'anthropic' }],
      connections: [{ id: 'shared_id', name: 'Shared connection', provider: 'github' }],
    };
    h.grantsOnServer = [{ kind: 'secrets', id: 'shared_id', name: 'SHARED_SECRET' }];
    await mountGrants();
    await waitForPicker();

    expect(toggle('secrets', 'shared_id').getAttribute('aria-pressed')).toBe('true');
    expect(toggle('connections', 'shared_id').getAttribute('aria-pressed')).toBe('false');
  });

  it('toggling an ungranted entry PUTs the grant and the row then reads as granted', async () => {
    await mountGrants();
    await waitForPicker();
    expect(toggle('secrets', 'sec_pool_1').getAttribute('aria-pressed')).toBe('false');

    toggle('secrets', 'sec_pool_1').click();
    await settle();

    expect(h.grantRequests).toEqual([
      `PUT /api/v1/repos/${REPO_ID}/onecli/grants/secrets/sec_pool_1`,
    ]);
    expect(h.grantsOnServer).toEqual([
      { kind: 'secrets', id: 'sec_pool_1', name: 'ANTHROPIC_API_KEY' },
    ]);
    // Rendered from the refetched grant set, not from a local flip.
    expect(toggle('secrets', 'sec_pool_1').getAttribute('aria-pressed')).toBe('true');
    expect(toggle('secrets', 'sec_pool_1').classList.contains('on')).toBe(true);
    // The other half of the pool is untouched.
    expect(toggle('connections', 'conn_pool_1').getAttribute('aria-pressed')).toBe('false');
  });

  it('toggling a granted entry DELETEs the grant and the row then reads as not granted', async () => {
    h.grantsOnServer = [{ kind: 'connections', id: 'conn_pool_1', name: 'GitHub app' }];
    await mountGrants();
    await waitForPicker();
    expect(toggle('connections', 'conn_pool_1').getAttribute('aria-pressed')).toBe('true');

    toggle('connections', 'conn_pool_1').click();
    await settle();

    expect(h.grantRequests).toEqual([
      `DELETE /api/v1/repos/${REPO_ID}/onecli/grants/connections/conn_pool_1`,
    ]);
    expect(h.grantsOnServer).toEqual([]);
    expect(toggle('connections', 'conn_pool_1').getAttribute('aria-pressed')).toBe('false');
    expect(toggle('connections', 'conn_pool_1').classList.contains('on')).toBe(false);
  });

  it('a failed attach surfaces the server message and leaves the row unchanged', async () => {
    h.grantWriteError = 'onecli: grant refused — the pool resource was deleted';
    await mountGrants();
    await waitForPicker();

    toggle('secrets', 'sec_pool_1').click();
    await settle();

    // The call went out, the server refused, and NOTHING flipped: this is the
    // proof the picker renders server truth rather than an optimistic guess.
    expect(h.grantRequests).toEqual([
      `PUT /api/v1/repos/${REPO_ID}/onecli/grants/secrets/sec_pool_1`,
    ]);
    expect(grantsSection().textContent).toContain(
      'onecli: grant refused — the pool resource was deleted',
    );
    expect(toggle('secrets', 'sec_pool_1').getAttribute('aria-pressed')).toBe('false');
    expect(h.grantsOnServer).toEqual([]);
  });

  it('an unconfigured integration explains itself and offers no toggles', async () => {
    h.poolOnServer = { configured: false, secrets: [], connections: [] };
    await mountGrants();
    await waitFor(
      () => (grantsSection().textContent?.includes('Not configured') ? true : null),
      'unconfigured copy',
    );

    const section = grantsSection();
    expect(section.textContent).toContain(
      'Not configured — the credential gateway integration is off in this lab',
    );
    expect(section.querySelector('a[href*="onecli-credential-gateway"]')).not.toBeNull();
    // Normal and healthy, never an error — and nothing to toggle.
    expect(section.querySelector('.banner.error')).toBeNull();
    expect(toggles()).toHaveLength(0);
  });

  it('a failed read shows an error banner with a Retry that refetches and succeeds', async () => {
    h.gatewayReadError = 'onecli: pool: dial tcp 127.0.0.1:10254: connection refused';
    await mountGrants();
    await waitFor(
      () => grantsSection().querySelector('.banner.error'),
      'gateway read error banner',
    );

    expect(grantsSection().textContent).toContain(
      'onecli: pool: dial tcp 127.0.0.1:10254: connection refused',
    );
    // The failure state is a banner, not a loading line that never resolves.
    expect(grantsSection().textContent).not.toContain('Loading the OneCLI project pool');
    expect(toggles()).toHaveLength(0);

    const retry = Array.from(grantsSection().querySelectorAll('button')).find(
      (b) => b.textContent?.trim() === 'Retry',
    );
    expect(retry).toBeDefined();

    h.gatewayReadError = null;
    retry?.click();
    await settle();

    expect(grantsSection().querySelector('.banner.error')).toBeNull();
    expect(toggles()).toHaveLength(2);
    expect(grantsSection().textContent).toContain('ANTHROPIC_API_KEY');
  });

  it('a reachable but empty pool points the operator at the OneCLI dashboard', async () => {
    h.poolOnServer = { configured: true, secrets: [], connections: [] };
    await mountGrants();
    await waitFor(() => grantsSection().querySelector('.empty'), 'empty-pool state');

    expect(grantsSection().querySelector('.empty')?.textContent).toContain(
      'The OneCLI project pool is empty — create the first secret in the OneCLI dashboard',
    );
    expect(grantsSection().querySelector('.banner.error')).toBeNull();
    expect(toggles()).toHaveLength(0);
  });

  it('links out to the dashboard from the exposure endpoint, and nowhere else', async () => {
    await mountGrants();
    await waitForPicker();

    const link = grantsSection().querySelector<HTMLAnchorElement>(
      'a[href="https://lab.example.com:8443"]',
    );
    expect(link).not.toBeNull();
    expect(link?.getAttribute('target')).toBe('_blank');
    expect(link?.getAttribute('rel')).toBe('noopener noreferrer');
    expect(link?.textContent).toContain(
      'secret values are created, rotated and deleted there, and only there',
    );
    expect(grantsSection().textContent).not.toContain('The OneCLI dashboard is not exposed');
  });

  it('renders no link but an annotation when the dashboard is not exposed', async () => {
    h.dashboardExposure = { mode: 'off' };
    await mountGrants();
    await waitForPicker();

    expect(grantsSection().querySelector('a[href^="https://lab.example.com"]')).toBeNull();
    expect(grantsSection().textContent).toContain(
      'The OneCLI dashboard is not exposed by this lab',
    );
    // The picker itself is unaffected.
    expect(toggles()).toHaveLength(2);
  });

  it('renders no link and still picks when the exposure read itself fails', async () => {
    h.dashboardExposure = null;
    await mountGrants();
    await waitForPicker();

    expect(grantsSection().querySelector('a[href^="https://lab.example.com"]')).toBeNull();
    expect(grantsSection().textContent).toContain(
      'The OneCLI dashboard is not exposed by this lab',
    );
    expect(grantsSection().querySelector('.banner.error')).toBeNull();
    expect(toggles()).toHaveLength(2);
  });

  it('reads the grant set back from the server on a remount — nothing is cached lab-side', async () => {
    await mountGrants();
    await waitForPicker();

    toggle('secrets', 'sec_pool_1').click();
    await settle();
    expect(toggle('secrets', 'sec_pool_1').getAttribute('aria-pressed')).toBe('true');

    unmount();
    await mountGrants();
    await waitForPicker();

    // Fresh resources, fresh GET: the row is granted because the server says
    // so, and the toggle's own call is the only thing that ever changed it.
    expect(toggle('secrets', 'sec_pool_1').getAttribute('aria-pressed')).toBe('true');
    expect(toggle('connections', 'conn_pool_1').getAttribute('aria-pressed')).toBe('false');
    expect(h.grantRequests).toEqual([
      `PUT /api/v1/repos/${REPO_ID}/onecli/grants/secrets/sec_pool_1`,
    ]);
  });
});
