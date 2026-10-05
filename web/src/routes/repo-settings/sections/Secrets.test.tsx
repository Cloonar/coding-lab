// Secrets section suite (issue #61 §9, issue #104) on
// /repos/:id/settings/secrets: write-only per-repo secrets as rows that open
// in place to rotate or delete, with "+ New secret" as the last row. Every
// action is immediate (no form store, no leave guard).
//
// The API mock only ever hands back metadata, so "no value ever rendered" is
// pinned by construction; these tests additionally assert the value INPUTS
// are password-typed and that requests carry only what the contract calls
// for.

import { describe, expect, it } from 'vitest';
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
  toastText,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountSecrets = () => mountSettings(`/repos/${REPO_ID}/settings/secrets`);

const apiKey = () => ({
  id: 'sec_1',
  name: 'API_KEY',
  description: 'third-party api',
  created_at: '2026-07-01T00:00:00.000Z',
  updated_at: '2026-07-02T00:00:00.000Z',
  exposed_run_id: null,
  exposed_at: null,
});

const rowHeads = (): HTMLButtonElement[] =>
  Array.from(secretsSection().querySelectorAll<HTMLButtonElement>('button.secret-row-head'));
const rowHead = (name: string): HTMLButtonElement => {
  const head = rowHeads().find((el) => el.querySelector('.secret-row-name')?.textContent === name);
  if (!head) throw new Error(`missing secret row "${name}"`);
  return head;
};
const submitRowForm = (): void => {
  const form = secretsSection().querySelector('form');
  if (!form) throw new Error('no open secret row');
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
};
const errorUnder = (name: string): string | null =>
  input(name).closest('.sfield')?.querySelector('.sfield-error')?.textContent ?? null;

describe('RepoSettings secrets section', () => {
  it('renders listed secrets with name, description and updated date, closed', async () => {
    h.secretsOnServer = [apiKey()];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.secret-row-name.mono'), 'secret row');

    const section = secretsSection();
    expect(section.textContent).toContain('API_KEY');
    expect(section.textContent).toContain('third-party api');
    expect(section.textContent).toContain('updated');
    // The secret name renders in a monospace element, per the design.
    expect(section.querySelector('.secret-row-name.mono')?.textContent).toBe('API_KEY');
    // Rows start closed; the last row is the way to add one.
    const head = rowHead('API_KEY');
    expect(head.getAttribute('aria-expanded')).toBe('false');
    expect(section.querySelector('input[name="secret-rotate-value"]')).toBeNull();
    expect(rowHeads().at(-1)?.textContent).toContain('New secret');
    // No duplicate card title: the page section's heading is the one heading.
    expect(section.querySelector('h2')).toBeNull();

    // No value ever appears anywhere on the page — the mock only ever hands
    // back metadata, same as the real write-only API.
    expect(container.textContent).not.toContain('sekrit');
  });

  it('renders the empty state when the repo has no secrets, with the add row', async () => {
    await mountSecrets();
    await waitFor(
      () => (secretsSection().textContent?.includes('No secrets yet') ? true : null),
      'empty state',
    );
    expect(secretsSection().textContent).toContain('No secrets yet');
    expect(rowHeads().at(-1)?.textContent).toContain('New secret');
  });

  it('the new-secret row opens in place, checks its fields, then adds and says so', async () => {
    await mountSecrets();
    await waitFor(() => rowHeads().at(-1) ?? null, 'the add row');

    const head = rowHead('New secret');
    head.click();
    await settle();
    expect(head.getAttribute('aria-expanded')).toBe('true');

    // An empty name is refused before any request, at the field.
    submitRowForm();
    await settle();
    expect(h.secretRequestBodies).toHaveLength(0);
    expect(errorUnder('secret-name')).toBe('Give the secret a name.');
    expect(errorUnder('secret-value')).toBe('Paste the value first.');
    expect(document.activeElement).toBe(input('secret-name'));

    const valueField = input('secret-value');
    expect(valueField.type).toBe('password');
    typeInto(input('secret-name'), 'DEPLOY_TOKEN');
    typeInto(input('secret-description'), 'deploy pipeline token');
    typeInto(valueField, 'a-fresh-secret-value');
    submitRowForm();
    await settle();

    expect(h.secretRequestBodies).toEqual([
      { name: 'DEPLOY_TOKEN', description: 'deploy pipeline token', value: 'a-fresh-secret-value' },
    ]);
    expect(toastText()).toBe('Added DEPLOY_TOKEN');
    // The add row closes and the list reflects the new secret's metadata
    // only — never the value that was just submitted.
    expect(secretsSection().querySelector('input[name="secret-name"]')).toBeNull();
    expect(secretsSection().textContent).toContain('DEPLOY_TOKEN');
    expect(secretsSection().textContent).not.toContain('a-fresh-secret-value');
  });

  it('a row opens to rotate: an empty value is refused, a value PATCHes and clears an exposure', async () => {
    h.secretsOnServer = [
      {
        ...apiKey(),
        description: '',
        // Exposed (issue #108): rotating is the remediation, so the refetch
        // after a successful rotate should clear the badge below.
        exposed_run_id: 'run_leaker',
        exposed_at: '2026-07-05T00:00:00.000Z',
      },
    ];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.secret-row-name.mono'), 'secret row');

    expect(secretsSection().querySelector('.chip.exposed')).not.toBeNull();
    expect(secretsSection().textContent).toContain('Exposed in run');
    expect(
      secretsSection().querySelector<HTMLAnchorElement>('a[href="/runs/run_leaker"]'),
    ).not.toBeNull();

    rowHead('API_KEY').click();
    await settle();
    expect(rowHead('API_KEY').getAttribute('aria-expanded')).toBe('true');
    expect(secretsSection().textContent).toContain('The current value is never shown.');

    const rotateField = input('secret-rotate-value');
    expect(rotateField.type).toBe('password');
    button('Save new value').click();
    await settle();
    expect(h.secretRequestBodies).toHaveLength(0);
    expect(errorUnder('secret-rotate-value')).toBe('Paste the new value first.');

    typeInto(rotateField, 'rotated-secret-value');
    button('Save new value').click();
    await settle();

    // Only the new value rides the request — never the name or id.
    expect(h.secretRequestBodies).toEqual([{ value: 'rotated-secret-value' }]);
    expect(toastText()).toBe('API_KEY has a new value');
    // The row closes and the list refreshes.
    expect(secretsSection().querySelector('input[name="secret-rotate-value"]')).toBeNull();
    expect(secretsSection().textContent).not.toContain('rotated-secret-value');
    // The refetched row has null exposure fields (the mock's rotate handler
    // mirrors RotateRepoSecret's clear-on-rotate) — the badge is gone.
    expect(secretsSection().querySelector('.chip.exposed')).toBeNull();
    expect(secretsSection().textContent).not.toContain('Exposed in run');
  });

  it('deletes behind an inline confirmation, in place, and says so', async () => {
    h.secretsOnServer = [{ ...apiKey(), description: '' }];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.secret-row-name.mono'), 'secret row');

    rowHead('API_KEY').click();
    await settle();
    button('Delete').click();
    await settle();

    // Asked where the button was; nothing over the page, nothing gone yet.
    expect(secretsSection().textContent).toContain('Delete API_KEY?');
    expect(container.querySelector('.dialog')).toBeNull();
    expect(h.secretsOnServer).toHaveLength(1);

    button('Delete for good').click();
    await settle();

    expect(h.secretsOnServer).toHaveLength(0);
    expect(toastText()).toBe('Deleted API_KEY');
    expect(secretsSection().textContent).toContain('No secrets yet');
  });

  it('one row open at a time', async () => {
    h.secretsOnServer = [apiKey(), { ...apiKey(), id: 'sec_2', name: 'OTHER' }];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.secret-row-name.mono'), 'secret rows');

    rowHead('API_KEY').click();
    await settle();
    rowHead('OTHER').click();
    await settle();

    expect(rowHead('API_KEY').getAttribute('aria-expanded')).toBe('false');
    expect(rowHead('OTHER').getAttribute('aria-expanded')).toBe('true');
    expect(secretsSection().querySelectorAll('input[name="secret-rotate-value"]')).toHaveLength(1);
  });

  it('keeps a row across a reload, and hands the focus to its head after a new value', async () => {
    h.secretsOnServer = [apiKey(), { ...apiKey(), id: 'sec_2', name: 'OTHER' }];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.secret-row-name.mono'), 'secret rows');
    const head = rowHead('API_KEY');
    const other = rowHead('OTHER');

    head.click();
    await settle();
    typeInto(input('secret-rotate-value'), 'rotated-secret-value');
    const saveButton = button('Save new value');
    saveButton.focus();
    saveButton.click();
    await settle();

    // The row closed with the button that had the focus; its head takes it.
    expect(saveButton.isConnected).toBe(false);
    expect(rowHead('API_KEY')).toBe(head);
    expect(rowHead('OTHER')).toBe(other);
    expect(document.activeElement).toBe(head);
    expect(head.getAttribute('aria-expanded')).toBe('false');
  });

  it('after a delete the focus goes to the row that takes its place, else to New secret', async () => {
    h.secretsOnServer = [apiKey(), { ...apiKey(), id: 'sec_2', name: 'OTHER' }];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.secret-row-name.mono'), 'secret rows');

    rowHead('API_KEY').click();
    await settle();
    button('Delete').click();
    await settle();
    button('Delete for good').click();
    await settle();

    expect(h.secretsOnServer.map((secret) => secret.name)).toEqual(['OTHER']);
    expect(document.activeElement).toBe(rowHead('OTHER'));

    // The last one: focus goes to the way to add one.
    rowHead('OTHER').click();
    await settle();
    button('Delete').click();
    await settle();
    button('Delete for good').click();
    await settle();

    expect(h.secretsOnServer).toHaveLength(0);
    expect(document.activeElement).toBe(rowHead('New secret'));
  });

  it('a failed delete is said in the row it was asked for, and the row stays', async () => {
    h.secretsOnServer = [apiKey(), { ...apiKey(), id: 'sec_2', name: 'OTHER' }];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.secret-row-name.mono'), 'secret rows');
    rowHead('OTHER').click();
    await settle();
    h.secretRefusal = { error: 'secrets: store unavailable' };
    button('Delete').click();
    await settle();
    button('Delete for good').click();
    await settle();

    const row = rowHead('OTHER').closest('li');
    const alert = row?.querySelector('[role="alert"]');
    expect(alert?.textContent).toBe('OTHER was not deleted. secrets: store unavailable');
    expect(rowHead('API_KEY').closest('li')?.querySelector('[role="alert"]')).toBeNull();
    // Nothing above the list speaks for a single row.
    expect(secretsSection().querySelector('.banner.error')).toBeNull();
    expect(h.secretsOnServer).toHaveLength(2);
    expect(toastText()).toBe('');
  });

  it('a refused new value is said under its field, which keeps the focus', async () => {
    h.secretsOnServer = [apiKey()];
    await mountSecrets();
    await waitFor(() => secretsSection().querySelector('.secret-row-name.mono'), 'secret row');
    rowHead('API_KEY').click();
    await settle();
    h.secretRefusal = { error: 'value: must not be empty', field: 'value' };
    typeInto(input('secret-rotate-value'), ' ');
    button('Save new value').click();
    await settle();

    expect(errorUnder('secret-rotate-value')).toBe('value: must not be empty');
    expect(input('secret-rotate-value').getAttribute('aria-invalid')).toBe('true');
    expect(document.activeElement).toBe(input('secret-rotate-value'));
    expect(rowHead('API_KEY').getAttribute('aria-expanded')).toBe('true');
  });

  it('a refused name of a new secret is said under the name, a refusal without a field under the form', async () => {
    await mountSecrets();
    await waitFor(() => rowHeads().at(-1) ?? null, 'the add row');
    rowHead('New secret').click();
    await settle();
    typeInto(input('secret-name'), 'lower_case');
    typeInto(input('secret-value'), 'v');

    h.secretRefusal = {
      error: 'name: must be uppercase letters, digits and underscores',
      field: 'name',
    };
    submitRowForm();
    await settle();
    expect(errorUnder('secret-name')).toBe(
      'name: must be uppercase letters, digits and underscores',
    );
    expect(document.activeElement).toBe(input('secret-name'));
    expect(secretsSection().querySelector('.banner.error')).toBeNull();

    h.secretRefusal = { error: 'secrets: store unavailable' };
    submitRowForm();
    await settle();
    expect(errorUnder('secret-name')).toBeNull();
    expect(secretsSection().querySelector('form .banner.error')?.textContent).toContain(
      'secrets: store unavailable',
    );
  });

  it('after adding one the focus rests on New secret', async () => {
    await mountSecrets();
    await waitFor(() => rowHeads().at(-1) ?? null, 'the add row');
    rowHead('New secret').click();
    await settle();
    typeInto(input('secret-name'), 'DEPLOY_TOKEN');
    typeInto(input('secret-value'), 'v');
    const add = button('Add secret');
    add.focus();
    add.click();
    await settle();

    expect(add.isConnected).toBe(false);
    expect(document.activeElement).toBe(rowHead('New secret'));
    expect(secretsSection().textContent).toContain('DEPLOY_TOKEN');
  });
});
