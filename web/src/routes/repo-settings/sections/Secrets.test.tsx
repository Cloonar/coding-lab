// Secrets section suite (issue #198): the RepoSettings.test.tsx secrets
// describe on /repos/:id/settings/secrets, unchanged in substance — the
// section is device-local/immediate (no useSettingsForm, no leave guard).
//
// Repo secrets (issue #104): write-only per-repo secrets — the API mock only
// ever hands back metadata, so "no value ever rendered" is pinned by
// construction; these tests additionally assert the value INPUT is
// password-typed and that requests carry only what the contract calls for.

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  button,
  container,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  secretsSection,
  settle,
  submitFormWithin,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountSecrets = () => mountSettings(`/repos/${REPO_ID}/settings/secrets`);

describe('RepoSettings secrets section', () => {
  it('renders listed secrets with name, description and updated date', async () => {
    h.secretsOnServer = [
      {
        id: 'sec_1',
        name: 'API_KEY',
        description: 'third-party api',
        created_at: '2026-07-01T00:00:00.000Z',
        updated_at: '2026-07-02T00:00:00.000Z',
        exposed_run_id: null,
        exposed_at: null,
      },
    ];
    await mountSecrets();
    await waitFor(() => container.querySelector('section h2') && secretsSection(), 'secrets section');

    const section = secretsSection();
    expect(section.textContent).toContain('API_KEY');
    expect(section.textContent).toContain('third-party api');
    // The secret name renders in a monospace element, per the design.
    const nameEl = section.querySelector('.card-title.mono');
    expect(nameEl?.textContent).toBe('API_KEY');

    // No value ever appears anywhere on the page — the mock only ever hands
    // back metadata, same as the real write-only API.
    expect(container.textContent).not.toContain('sekrit');
  });

  it('renders the empty state when the repo has no secrets', async () => {
    await mountSecrets();
    await waitFor(
      () => (secretsSection().textContent?.includes('No secrets yet') ? true : null),
      'empty state',
    );
    expect(secretsSection().textContent).toContain('No secrets yet');
  });

  it('add-secret form submits name, description and value, then refreshes the list', async () => {
    await mountSecrets();
    await waitFor(() => secretsSection(), 'secrets section');

    button('+ Add secret').click();
    await settle();

    const nameField = input('secret-name');
    const valueField = input('secret-value');
    expect(valueField.type).toBe('password');

    typeInto(nameField, 'DEPLOY_TOKEN');
    typeInto(input('secret-description'), 'deploy pipeline token');
    typeInto(valueField, 'a-fresh-secret-value');

    submitFormWithin(secretsSection());
    await settle();

    expect(h.secretRequestBodies).toEqual([
      { name: 'DEPLOY_TOKEN', description: 'deploy pipeline token', value: 'a-fresh-secret-value' },
    ]);
    // The create form closes and the list reflects the new secret's metadata
    // only — never the value that was just submitted.
    expect(secretsSection().textContent).toContain('DEPLOY_TOKEN');
    expect(secretsSection().textContent).not.toContain('a-fresh-secret-value');
  });

  it('rotate submits only the new value, never the name or id, and clears an exposure badge', async () => {
    h.secretsOnServer = [
      {
        id: 'sec_1',
        name: 'API_KEY',
        description: '',
        created_at: '2026-07-01T00:00:00.000Z',
        updated_at: '2026-07-01T00:00:00.000Z',
        // Exposed (issue #108): rotating is the remediation, so the refetch
        // after a successful rotate should clear the badge below.
        exposed_run_id: 'run_leaker',
        exposed_at: '2026-07-05T00:00:00.000Z',
      },
    ];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.card-title.mono'), 'secret row');

    expect(secretsSection().querySelector('.chip.exposed')).not.toBeNull();
    expect(secretsSection().textContent).toContain('Exposed in run');
    expect(
      secretsSection().querySelector<HTMLAnchorElement>('a[href="/runs/run_leaker"]'),
    ).not.toBeNull();

    button('Rotate').click();
    await settle();

    const rotateField = input('secret-rotate-value');
    expect(rotateField.type).toBe('password');
    typeInto(rotateField, 'rotated-secret-value');

    submitFormWithin(secretsSection());
    await settle();

    expect(h.secretRequestBodies).toEqual([{ value: 'rotated-secret-value' }]);
    // The row collapses back out of rotate mode and the list refreshes.
    expect(secretsSection().querySelector('input[name="secret-rotate-value"]')).toBeNull();
    expect(secretsSection().textContent).not.toContain('rotated-secret-value');
    // The refetched row has null exposure fields (the mock's rotate handler
    // mirrors RotateRepoSecret's clear-on-rotate) — the badge is gone.
    expect(secretsSection().querySelector('.chip.exposed')).toBeNull();
    expect(secretsSection().textContent).not.toContain('Exposed in run');
  });

  it('delete asks for confirmation before removing the secret', async () => {
    h.secretsOnServer = [
      {
        id: 'sec_1',
        name: 'API_KEY',
        description: '',
        created_at: '2026-07-01T00:00:00.000Z',
        updated_at: '2026-07-01T00:00:00.000Z',
        exposed_run_id: null,
        exposed_at: null,
      },
    ];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.card-title.mono'), 'secret row');

    // Declining the confirm leaves the secret in place.
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValueOnce(false);
    button('Delete').click();
    await settle();
    expect(confirmSpy).toHaveBeenCalledWith('Delete secret "API_KEY"?');
    expect(secretsSection().textContent).toContain('API_KEY');

    // Confirming removes it.
    confirmSpy.mockReturnValueOnce(true);
    button('Delete').click();
    await settle();
    expect(h.secretsOnServer).toHaveLength(0);
    expect(secretsSection().textContent).toContain('No secrets yet');
  });
});
