// The one save rule of global Settings (issue #85): fields wait for Save; ONE
// save bar for the whole page counts the changed fields and links their
// sections; Save sends ONE PATCH /settings with exactly the changed keys,
// applies the response at once and confirms in the toast ("Saved N
// changes"); Discard restores the saved values and offers Undo. The browser
// checks the whole-number fields before anything is sent — a problem shows
// under its field, is counted in the bar, and Save scrolls to and focuses the
// first one. A server refusal that names a key lands at that field; one that
// names none, and a network error, show in the bar. Edits survive every
// failure.

import { describe, expect, it } from 'vitest';
import {
  PINNED,
  button,
  container,
  discard,
  fieldChanged,
  fieldError,
  h,
  input,
  installSettingsHooks,
  mountPage,
  save,
  saveBar,
  saveBarError,
  saveBarSections,
  saveBarTitle,
  settle,
  toastText,
  typeField,
} from './harness';

installSettingsHooks();

const chip = (title: string): HTMLAnchorElement | undefined =>
  Array.from(container.querySelectorAll<HTMLAnchorElement>('a.settings-chip')).find((a) =>
    a.textContent?.startsWith(title),
  );

describe('one save bar for the page', () => {
  it('shows nothing while nothing differs from the saved settings', async () => {
    await mountPage();
    expect(saveBar()).toBeNull();
    // Typing the saved value back is no change.
    await typeField('max_instances', '5');
    await typeField('max_instances', '4');
    expect(saveBar()).toBeNull();
    expect(fieldChanged('max_instances')).toBe(false);
  });

  it('edits in two sections: one bar with the count and both sections, marked at field and chip', async () => {
    await mountPage();
    await typeField('max_instances', '8');
    await typeField('git_author_name', 'Dominik');

    expect(saveBarTitle()).toBe('2 unsaved changes');
    expect(saveBarSections()).toEqual(['Agents', 'General']);
    expect(fieldChanged('max_instances')).toBe(true);
    expect(fieldChanged('git_author_name')).toBe(true);
    expect(fieldChanged('git_author_email')).toBe(false);
    expect(chip('Agents')?.textContent).toContain('(unsaved changes)');
    expect(chip('General')?.textContent).toContain('(unsaved changes)');
    expect(chip('Runner')?.textContent).not.toContain('(unsaved changes)');
    expect(h.patchBodies).toEqual([]);
  });

  it('Save sends one PATCH with exactly the changed keys, applies it and confirms in the toast', async () => {
    await mountPage();
    await typeField('max_instances', ' 8 ');
    await typeField('git_author_name', '  Dominik ');

    await save();

    expect(h.patchBodies).toEqual([{ max_instances: 8, git_author_name: 'Dominik' }]);
    expect(toastText()).toBe('Saved 2 changes');
    expect(saveBar()).toBeNull();
    expect(fieldChanged('max_instances')).toBe(false);
    expect(chip('Agents')?.textContent).not.toContain('(unsaved changes)');
    // Focus goes to the page heading, not the top of the document.
    expect(document.activeElement?.textContent).toBe('Settings');
  });

  it('applies the response at once: the dev image shows the ref the server pinned', async () => {
    await mountPage();
    await typeField('dev_image_default', 'ghcr.io/acme/dev:1');

    await save();

    expect(h.patchBodies).toEqual([{ dev_image_default: 'ghcr.io/acme/dev:1' }]);
    expect(toastText()).toBe('Saved 1 change');
    expect(input('dev_image_default').value).toBe(`ghcr.io/acme/dev:1${PINNED}`);
    expect(saveBar()).toBeNull();
  });

  it('Discard restores the saved values; Undo in the toast brings the edits back', async () => {
    await mountPage();
    await typeField('max_instances', '8');
    await typeField('git_author_name', 'Dominik');

    await discard();

    expect(input('max_instances').value).toBe('4');
    expect(input('git_author_name').value).toBe('');
    expect(saveBar()).toBeNull();
    expect(toastText()).toContain('Changes discarded');

    button('Undo').click();
    await settle();

    expect(input('max_instances').value).toBe('8');
    expect(input('git_author_name').value).toBe('Dominik');
    expect(saveBarTitle()).toBe('2 unsaved changes');
    expect(h.patchBodies).toEqual([]);
  });
});

describe('problems found in the browser', () => {
  it('a non-integer in a capacity field shows under it, counts in the bar, and sends nothing', async () => {
    await mountPage();
    await typeField('max_instances', 'four');

    await save();

    expect(h.patchBodies).toEqual([]);
    expect(fieldError('max_instances')).toBe('Use a whole number, 1 or more.');
    expect(saveBarTitle()).toBe('1 problem to fix');
    expect(saveBarSections()).toEqual(['Agents']);
    expect(document.activeElement).toBe(input('max_instances'));
    expect(h.scrolls.at(-1)).toMatchObject({ target: 'max_instances' });
  });

  it('0 where the floor is 1, and 400 days of retention, are problems too — Save goes to the first', async () => {
    await mountPage();
    await typeField('afk_budget_minutes', '0');
    await typeField('transcript_retention_days', '400');
    await typeField('git_author_email', 'me@example.com');

    await save();

    expect(h.patchBodies).toEqual([]);
    expect(fieldError('afk_budget_minutes')).toBe('Use a whole number, 1 or more.');
    expect(fieldError('transcript_retention_days')).toBe('Use a whole number from 0 to 365.');
    expect(fieldError('git_author_email')).toBeNull();
    expect(saveBarTitle()).toBe('2 problems to fix');
    expect(saveBarSections()).toEqual(['Agents', 'General']);
    expect(document.activeElement).toBe(input('afk_budget_minutes'));
  });

  it('a problem goes once its field is valid, and the next Save sends', async () => {
    await mountPage();
    await typeField('max_instances', 'four');
    await save();
    expect(fieldError('max_instances')).not.toBeNull();

    await typeField('max_instances', '6');
    expect(fieldError('max_instances')).toBeNull();
    expect(saveBarTitle()).toBe('1 unsaved change');

    await save();
    expect(h.patchBodies).toEqual([{ max_instances: 6 }]);
  });

  it('the server floors hold for the ticks: below 5 seconds is a problem', async () => {
    await mountPage();
    await typeField('afk_tick_seconds', '2');
    await save();
    expect(fieldError('afk_tick_seconds')).toBe('Use a whole number, 5 or more.');
    expect(h.patchBodies).toEqual([]);
  });
});

describe('refusals and failures', () => {
  it('a refusal that names a key lands at that field; the edits survive', async () => {
    await mountPage();
    await typeField('dev_image_default', 'ghcr.io/acme/nope:1');
    await typeField('git_author_name', 'Dominik');
    h.patchRefusal = {
      error: 'cannot resolve ghcr.io/acme/nope:1',
      field: 'dev_image_default',
    };

    await save();

    expect(h.patchBodies).toHaveLength(1);
    expect(fieldError('dev_image_default')).toBe('cannot resolve ghcr.io/acme/nope:1');
    expect(saveBarError()).toBe('');
    expect(saveBarTitle()).toBe('1 problem to fix');
    expect(input('dev_image_default').value).toBe('ghcr.io/acme/nope:1');
    expect(input('git_author_name').value).toBe('Dominik');
    expect(document.activeElement).toBe(input('dev_image_default'));
    expect(toastText()).toBe('');
  });

  it('a refusal that names no key shows in the bar; the edits survive', async () => {
    await mountPage();
    await typeField('git_author_name', 'Dominik');
    h.patchRefusal = { error: 'invalid JSON body' };

    await save();

    expect(saveBarError()).toBe('Not saved. invalid JSON body');
    expect(fieldError('git_author_name')).toBeNull();
    expect(input('git_author_name').value).toBe('Dominik');
    expect(saveBarTitle()).toBe('1 unsaved change');
  });

  it('a network error shows in the bar; the edits survive', async () => {
    await mountPage();
    await typeField('git_author_name', 'Dominik');
    h.patchOffline = true;

    await save();

    expect(saveBarError()).toContain('Not saved.');
    expect(input('git_author_name').value).toBe('Dominik');
    expect(saveBar()).not.toBeNull();
  });

  it('never sends a read-only key, nor a field the operator did not touch', async () => {
    h.settingsOnServer = {
      ...h.settingsOnServer,
      dev_image_fallback: 'ghcr.io/lab/dev:full',
      afk_prompt_default: 'built in',
    };
    await mountPage();
    await typeField('container_pids', '512');

    await save();

    expect(h.patchBodies).toEqual([{ container_pids: 512 }]);
  });
});
