// General section suite (issues #198, #61): the section's fields on the
// one-page settings — editing one Identity/Incogni field puts exactly that
// field in the page's one PATCH — plus the empty-name check, which shows
// under the field, and the git author fields' inheritance: what the repo
// inherits as the placeholder, "Default: …" and Reset once it is set here.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  baseRepo,
  container,
  fieldChanged,
  fieldDefault,
  fieldError,
  fieldState,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  resetButton,
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

describe('repo-settings General: the git author identity inherits', () => {
  it('shows the inherited author as the placeholder of a blank field', async () => {
    h.settingsOnServer = {
      ...h.settingsOnServer,
      git_author_name: 'lab-bot',
      git_author_email: 'lab-bot@example.com',
    };
    await mountGeneral();
    await waitFor(
      () => (input('git_author_name').placeholder !== '' ? true : null),
      'inherited values',
    );

    expect(input('git_author_name').value).toBe('');
    expect(input('git_author_name').placeholder).toBe('lab-bot');
    expect(input('git_author_email').placeholder).toBe('lab-bot@example.com');
    expect(fieldState('git_author_name')).toBe('inherited');
    expect(fieldState('git_author_email')).toBe('inherited');
    // The name itself cannot inherit.
    expect(fieldState('name')).toBeNull();
    expect(fieldState('incogni')).toBeNull();
  });

  it('says so when nothing is set below the repo', async () => {
    await mountGeneral(); // no global author in the fixture
    await waitFor(
      () => (input('git_author_name').placeholder !== '' ? true : null),
      'inherited values',
    );
    expect(input('git_author_name').placeholder).toBe('none set');
  });

  it('typing an author sets it here; Reset returns it to inherited and PATCHes null', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, git_author_name: 'lab-bot' };
    h.repoOnServer = { ...baseRepo(), git_author_name: 'Dominik' };
    await mountGeneral();
    await waitFor(() => resetButton('git_author_name'), 'Reset');

    expect(fieldState('git_author_name')).toBe('set here');
    expect(fieldDefault('git_author_name')).toBe('Default: lab-bot');
    expect(resetButton('git_author_name')?.getAttribute('aria-label')).toBe(
      'Reset Git author name to inherited',
    );

    resetButton('git_author_name')?.click();
    await settle();

    expect(input('git_author_name').value).toBe('');
    expect(input('git_author_name').placeholder).toBe('lab-bot');
    expect(fieldState('git_author_name')).toBe('inherited');
    expect(fieldChanged('git_author_name')).toBe(true);
    // Reset is gone; the field took the focus.
    expect(resetButton('git_author_name')).toBeNull();
    expect(document.activeElement).toBe(input('git_author_name'));
    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();

    expect(h.patchBodies).toEqual([{ git_author_name: null }]);
    expect(h.repoOnServer.git_author_name).toBeNull();
  });
});
