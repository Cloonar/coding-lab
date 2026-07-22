// Global settings › General coverage (issue #198), new for the split: the git
// author card saves a dirty-fields-only PATCH (an untouched field never rides
// along), and a clean submit notes 'Nothing to save.' without a PATCH. Mounted
// at /settings/general.

import { describe, expect, it } from 'vitest';
import {
  container,
  h,
  input,
  installSettingsHooks,
  mountAt,
  settle,
  submitForm,
  typeInto,
  waitFor,
} from '../harness';

installSettingsHooks();

const mountGeneral = () => mountAt('/settings/general');

describe('Settings general — git author (issue #198)', () => {
  it('editing only the name PATCHes exactly git_author_name', async () => {
    h.settingsOnServer = { git_author_name: 'Old Name', git_author_email: 'me@example.com' };
    await mountGeneral();
    await waitFor(
      () => container.querySelector('input[name="git_author_name"]'),
      'git author card',
    );

    typeInto(input('git_author_name'), 'New Name');
    submitForm();
    await settle();

    // Dirty-fields-only: the untouched email stays out of the patch.
    expect(h.patchBodies).toEqual([{ git_author_name: 'New Name' }]);
  });

  it('a clean submit notes "Nothing to save." and never PATCHes', async () => {
    h.settingsOnServer = { git_author_name: 'Dominik', git_author_email: 'd@example.com' };
    await mountGeneral();
    await waitFor(
      () => container.querySelector('input[name="git_author_name"]'),
      'git author card',
    );

    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([]);
    expect(container.textContent).toContain('Nothing to save.');
  });
});
