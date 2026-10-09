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

describe('Settings general — transcript retention (issue #81)', () => {
  const field = () =>
    waitFor(
      () => container.querySelector('input[name="transcript_retention_days"]'),
      'transcript retention field',
    );

  it('seeds from the server value and renders the help copy', async () => {
    h.settingsOnServer = { transcript_retention_days: 30 };
    await mountGeneral();
    await field();

    expect(input('transcript_retention_days').value).toBe('30');
    expect(container.textContent).toContain('Transcript retention (days)');
    expect(container.textContent).toContain(
      'Ended runs keep their transcript for this many days; 0 keeps none',
    );
  });

  it('editing it PATCHes exactly transcript_retention_days as a number', async () => {
    h.settingsOnServer = { git_author_name: 'Dominik', transcript_retention_days: 30 };
    await mountGeneral();
    await field();

    typeInto(input('transcript_retention_days'), '90');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ transcript_retention_days: 90 }]);
  });

  it('0 is the off switch, not rejected', async () => {
    h.settingsOnServer = { transcript_retention_days: 30 };
    await mountGeneral();
    await field();

    typeInto(input('transcript_retention_days'), '0');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ transcript_retention_days: 0 }]);
  });

  it('the cap 365 saves; 366 and a non-number block the save client-side', async () => {
    h.settingsOnServer = { transcript_retention_days: 30 };
    await mountGeneral();
    await field();

    typeInto(input('transcript_retention_days'), '366');
    submitForm();
    await settle();
    expect(h.patchBodies).toEqual([]);
    expect(container.textContent).toContain('Must be at most 365.');
    expect(input('transcript_retention_days').getAttribute('aria-invalid')).toBe('true');

    typeInto(input('transcript_retention_days'), '-1');
    submitForm();
    await settle();
    expect(h.patchBodies).toEqual([]);
    expect(container.textContent).toContain('Enter a whole number.');

    typeInto(input('transcript_retention_days'), '365');
    submitForm();
    await settle();
    expect(h.patchBodies).toEqual([{ transcript_retention_days: 365 }]);
  });
});
