// General section suite (issue #198, new coverage): per-section dirty-only
// PATCH — editing one Identity/Incogni field puts exactly that field on the
// wire — plus the empty-name validation string.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  container,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  settle,
  submitForm,
  toggleCheckbox,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountGeneral = () => mountSettings(`/repos/${REPO_ID}/settings/general`);

describe('repo-settings General section', () => {
  it('editing the git author name PATCHes exactly that field', async () => {
    await mountGeneral();
    const author = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="git_author_name"]'),
      'general form',
    );
    expect(input('name').value).toBe('coding-lab');

    typeInto(author, 'Dominik');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ git_author_name: 'Dominik' }]);
    expect(h.repoOnServer.git_author_name).toBe('Dominik');
  });

  it('toggling Incogni PATCHes exactly that field', async () => {
    await mountGeneral();
    const incogni = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="incogni"]'),
      'incogni checkbox',
    );
    expect(incogni.checked).toBe(false);

    toggleCheckbox(incogni, true);
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ incogni: true }]);
    expect(h.repoOnServer.incogni).toBe(true);
  });

  it('rejects an empty name client-side without PATCHing', async () => {
    await mountGeneral();
    const name = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="name"]'),
      'name field',
    );

    typeInto(name, '   ');
    submitForm();
    await settle();

    expect(h.patchBodies).toHaveLength(0);
    expect(container.textContent).toContain('Name must not be empty.');
  });
});
