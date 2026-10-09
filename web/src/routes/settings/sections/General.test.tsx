// Global settings › General (issues #198, #81, #85): Git author and
// Transcripts as fields that wait for the save bar, then the read-only
// Credential gateway and SSH bastion status cards at the end of the section,
// outside the saved fields. Transcript retention is a whole number from 0
// (keep none) to 365, checked at its field before anything is sent.

import { describe, expect, it } from 'vitest';
import {
  fieldError,
  fieldHint,
  fieldLabel,
  h,
  input,
  installSettingsHooks,
  mountPage,
  pageSection,
  save,
  saveBarTitle,
  settle,
  switchButton,
  switchOn,
  typeField,
} from '../harness';

installSettingsHooks();

describe('General: git author', () => {
  it('editing only the name PATCHes exactly git_author_name, trimmed', async () => {
    await mountPage();
    expect(fieldLabel('git_author_name')).toBe('Git author name');
    expect(fieldHint('git_author_name')).toBe('Used for commits unless a repo overrides it.');

    await typeField('git_author_name', '  Dominik  ');
    await save();

    expect(h.patchBodies).toEqual([{ git_author_name: 'Dominik' }]);
  });

  it('seeds both fields from the stored settings', async () => {
    h.settingsOnServer = {
      ...h.settingsOnServer,
      git_author_name: 'Lab',
      git_author_email: 'lab@example.com',
    };
    await mountPage();
    expect(input('git_author_name').value).toBe('Lab');
    expect(input('git_author_email').value).toBe('lab@example.com');
  });
});

describe('General: transcript retention (issue #81)', () => {
  it('seeds from the server value and explains the range', async () => {
    await mountPage();
    expect(input('transcript_retention_days').value).toBe('30');
    expect(fieldHint('transcript_retention_days')).toBe(
      'Ended runs keep their transcript for this many days; 0 keeps none (max 365).',
    );
  });

  it('0 is the off switch, not a problem', async () => {
    await mountPage();
    await typeField('transcript_retention_days', '0');
    await save();
    expect(h.patchBodies).toEqual([{ transcript_retention_days: 0 }]);
  });

  it('the cap 365 saves', async () => {
    await mountPage();
    await typeField('transcript_retention_days', '365');
    await save();
    expect(h.patchBodies).toEqual([{ transcript_retention_days: 365 }]);
  });

  it('366 and a non-number are problems at the field, and nothing is sent', async () => {
    await mountPage();
    await typeField('transcript_retention_days', '366');
    await save();
    expect(fieldError('transcript_retention_days')).toBe('Use a whole number from 0 to 365.');
    expect(saveBarTitle()).toBe('1 problem to fix');

    await typeField('transcript_retention_days', 'forever');
    await save();
    expect(fieldError('transcript_retention_days')).toBe('Use a whole number from 0 to 365.');
    expect(h.patchBodies).toEqual([]);
  });
});

describe('General: merge deletes the head branch (issue #90)', () => {
  it('renders the switch on, with the one-line hint', async () => {
    await mountPage();
    expect(fieldLabel('merge_delete_head')).toBe('Delete head branch after merge');
    expect(fieldHint('merge_delete_head')).toBe(
      'Deletes the PR/CR head branch on origin after a merge; local branches are unaffected.',
    );
    expect(switchOn('merge_delete_head')).toBe(true);
  });

  it('seeds off from a stored false', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, merge_delete_head: false };
    await mountPage();
    expect(switchOn('merge_delete_head')).toBe(false);
  });

  it('turning it off saves exactly merge_delete_head: false', async () => {
    await mountPage();
    switchButton('merge_delete_head').click();
    await settle();
    await save();
    expect(h.patchBodies).toEqual([{ merge_delete_head: false }]);
  });
});

describe('General: status cards', () => {
  it('ends with the credential gateway and SSH bastion cards, outside the saved fields', async () => {
    await mountPage();
    const section = pageSection('general');
    const cards = Array.from(section.querySelectorAll(':scope > .card'));

    expect(cards).toHaveLength(3);
    expect(cards[0]?.querySelector('[data-field="git_author_name"]')).not.toBeNull();
    expect(cards[1]?.querySelector('h2')?.textContent).toBe('Credential gateway');
    expect(cards[2]?.querySelector('h2')?.textContent).toBe('SSH bastion');
    expect(cards[1]?.querySelector('[data-field]')).toBeNull();
    expect(cards[2]?.querySelector('[data-field]')).toBeNull();
  });
});
