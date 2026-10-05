// General section suite (issues #198, #61): the section's fields on the
// one-page settings — editing one Identity/Incogni field puts exactly that
// field in the page's one PATCH — plus the empty-name check, which now shows
// under the field instead of in a section banner.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  container,
  fieldChanged,
  fieldError,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  save,
  saveBar,
  saveBarTitle,
  setSwitch,
  settle,
  switchOn,
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
      'general fields',
    );
    expect(input('name').value).toBe('coding-lab');

    typeInto(author, 'Dominik');
    await save();

    expect(h.patchBodies).toEqual([{ git_author_name: 'Dominik' }]);
    expect(h.repoOnServer.git_author_name).toBe('Dominik');
  });

  it('toggling Incogni PATCHes exactly that field', async () => {
    await mountGeneral();
    await waitFor(() => container.querySelector('button[name="incogni"]'), 'incogni switch');
    expect(switchOn('incogni')).toBe(false);

    setSwitch('incogni', true);
    await settle();
    expect(fieldChanged('incogni')).toBe(true);
    await save();

    expect(h.patchBodies).toEqual([{ incogni: true }]);
    expect(h.repoOnServer.incogni).toBe(true);
    expect(switchOn('incogni')).toBe(true);
    expect(fieldChanged('incogni')).toBe(false);
  });

  it('a renamed repo PATCHes the trimmed name', async () => {
    await mountGeneral();
    const name = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="name"]'),
      'name field',
    );

    typeInto(name, '  lab-core  ');
    await save();

    expect(h.patchBodies).toEqual([{ name: 'lab-core' }]);
  });

  it('rejects an empty name in the browser: no PATCH, the message under the field', async () => {
    await mountGeneral();
    const name = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="name"]'),
      'name field',
    );

    typeInto(name, '   ');
    await save();

    expect(h.patchBodies).toHaveLength(0);
    expect(fieldError('name')).toBe('Enter a name.');
    expect(name.getAttribute('aria-invalid')).toBe('true');
    expect(name.getAttribute('aria-describedby')).toContain('rs-name-error');
    expect(document.activeElement).toBe(name);
    expect(saveBarTitle()).toBe('1 problem to fix');

    // The problem clears as soon as the field is valid again.
    typeInto(name, 'lab-core');
    await settle();
    expect(fieldError('name')).toBeNull();
    expect(name.getAttribute('aria-invalid')).toBeNull();
    expect(saveBar()).not.toBeNull();
    expect(saveBarTitle()).toBe('1 unsaved change');
  });
});
