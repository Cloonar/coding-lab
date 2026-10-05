// The one save rule of the repo settings (issue #61), through the real page:
// fields wait for Save; changing fields in several sections shows ONE save
// bar naming those sections and the total count; Save sends exactly one PATCH
// with only the changed fields, then the bar disappears and every mark clears
// at once; a problem the browser finds sends nothing and shows under its
// field; a refusal from the server lands under the field it names, or in the
// bar when it names none; Discard restores every field and Undo brings the
// edits back; and a refresh of the repo updates untouched fields but never a
// dirty one.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  chooseFromSelect,
  container,
  discard,
  emitRepoChanged,
  fieldChanged,
  fieldError,
  flush,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  routerHistory,
  save,
  saveBar,
  saveBarSections,
  saveBarTitle,
  segment,
  selectedLabel,
  setSwitch,
  settle,
  switchOn,
  textarea,
  toastText,
  typeInto,
  waitFor,
} from './harness';

installRepoSettingsHooks();
// A repo on the container Runner with Autoland on: nothing is folded away, so
// every field of the page is there to edit (the folding rules have their own
// suites in sections/).
beforeEach(() => {
  h.repoOnServer = { ...h.repoOnServer, runner: 'container', autoland_enabled: true };
});

const BASE = `/repos/${REPO_ID}/settings`;
const waitForPage = () =>
  waitFor(() => container.querySelector('input[name="afk_options.ultracode"]'), 'the page');
const mount = async (): Promise<void> => {
  await mountSettings(BASE);
  await waitForPage();
};

const saveButton = (): HTMLButtonElement => {
  const el = Array.from(saveBar()?.querySelectorAll('button') ?? []).find((b) =>
    b.classList.contains('primary'),
  );
  if (!el) throw new Error('missing Save button');
  return el;
};

describe('the save bar across sections', () => {
  it('is absent while nothing differs from the saved repo', async () => {
    await mount();
    expect(saveBar()).toBeNull();
    expect(container.querySelector('.sfield.changed')).toBeNull();
  });

  it('names the changed sections and the total count; Save sends ONE patch of the changed fields', async () => {
    await mount();

    await chooseFromSelect('model_default', 'Sonnet');
    expect(saveBarTitle()).toBe('1 unsaved change');
    expect(saveBarSections()).toEqual(['Agents']);

    typeInto(input('budget_minutes'), '90');
    typeInto(input('afk_branch_pattern'), 'issue-<N>');
    await settle();

    expect(saveBarTitle()).toBe('3 unsaved changes');
    expect(saveBarSections()).toEqual(['Agents', 'Branches']);
    // Each changed field is marked at its label; the others are not.
    expect(fieldChanged('model_default')).toBe(true);
    expect(fieldChanged('budget_minutes')).toBe(true);
    expect(fieldChanged('afk_branch_pattern')).toBe(true);
    expect(fieldChanged('effort_default')).toBe(false);
    expect(fieldChanged('default_branch')).toBe(false);
    expect(container.querySelectorAll('.sfield.changed')).toHaveLength(3);
    // Nothing was sent yet: fields wait for Save.
    expect(h.patchBodies).toEqual([]);

    await save();

    expect(h.patchBodies).toEqual([
      { model_default: 'sonnet', budget_minutes: 90, afk_branch_pattern: 'issue-<N>' },
    ]);
    // Saved: the bar is gone and every mark cleared, and a toast confirms.
    expect(saveBar()).toBeNull();
    expect(container.querySelector('.sfield.changed')).toBeNull();
    expect(toastText()).toBe('Saved 3 changes to coding-lab');
    // The fields show what was saved.
    expect(selectedLabel('model_default')).toBe('Sonnet');
    expect(input('budget_minutes').value).toBe('90');
    expect(input('afk_branch_pattern').value).toBe('issue-<N>');
  });

  it('counts one change as one, in words too', async () => {
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();

    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();
    expect(toastText()).toBe('Saved 1 change to coding-lab');
  });

  it('a section link in the bar jumps to that section', async () => {
    await mount();
    typeInto(input('budget_minutes'), '90');
    typeInto(input('afk_branch_pattern'), 'issue-<N>');
    await settle();

    const link = Array.from(saveBar()?.querySelectorAll('a') ?? []).find(
      (a) => a.textContent === 'Branches',
    );
    expect(link?.getAttribute('href')).toBe(`${BASE}/branches`);
    link?.click();
    await settle();

    expect(h.scrolls.at(-1)).toMatchObject({ target: 'settings-branches', smooth: true });
    expect(routerHistory.get()).toBe(`${BASE}/branches`);
  });

  it('typing a field back to its saved value takes it out of the count', async () => {
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    typeInto(input('budget_minutes'), '90');
    await settle();
    expect(saveBarTitle()).toBe('2 unsaved changes');

    typeInto(input('budget_minutes'), '');
    await settle();
    expect(saveBarTitle()).toBe('1 unsaved change');
    expect(fieldChanged('budget_minutes')).toBe(false);

    typeInto(input('git_author_name'), '');
    await settle();
    expect(saveBar()).toBeNull();
  });

  it('whitespace around an unchanged value is no change', async () => {
    await mount();
    typeInto(input('name'), 'coding-lab ');
    typeInto(input('git_author_name'), '   ');
    await settle();

    expect(saveBar()).toBeNull();
    expect(fieldChanged('name')).toBe(false);
  });

  it('a field that was touched but not changed stays out of the PATCH', async () => {
    await mount();
    typeInto(input('name'), 'coding-lab '); // touched: the same name, a stray space
    typeInto(input('default_branch'), ' main');
    typeInto(input('git_author_name'), 'Dominik'); // the one real change
    await settle();
    expect(saveBarTitle()).toBe('1 unsaved change');

    await save();

    expect(h.patchBodies).toEqual([{ git_author_name: 'Dominik' }]);
    expect(saveBar()).toBeNull();
  });

  it('shows Saving… and holds both buttons while the request is in flight', async () => {
    let release = (): void => {};
    h.patchHold = new Promise<void>((resolve) => (release = resolve));
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();

    saveButton().click();
    await settle();
    expect(saveButton().textContent).toBe('Saving…');
    expect(saveButton().disabled).toBe(true);
    expect(Array.from(saveBar()?.querySelectorAll('button') ?? []).every((b) => b.disabled)).toBe(
      true,
    );

    // An edit typed while the save is in flight is newer than what was sent.
    typeInto(input('git_author_email'), 'dominik@example.com');
    release();
    await settle();

    expect(h.patchBodies).toEqual([{ git_author_name: 'Dominik' }]);
    expect(saveBarTitle()).toBe('1 unsaved change');
    expect(fieldChanged('git_author_name')).toBe(false);
    expect(fieldChanged('git_author_email')).toBe(true);
    expect(saveButton().disabled).toBe(false);
  });

  it('shows the saved value as the server normalised it', async () => {
    await mount();
    // The server digest-pins an image ref on save: the answer differs from
    // what was typed, and it is the answer that is saved.
    const original = globalThis.fetch;
    vi.stubGlobal(
      'fetch',
      vi.fn((url: unknown, init?: RequestInit) => {
        if (String(url) === `/api/v1/repos/${REPO_ID}` && init?.method === 'PATCH') {
          h.patchBodies.push(JSON.parse(String(init.body)) as Record<string, unknown>);
          h.repoOnServer = { ...h.repoOnServer, image_ref: 'debian:bookworm@sha256:abc' };
          const text = JSON.stringify(h.repoOnServer);
          return Promise.resolve({
            ok: true,
            status: 200,
            json: () => Promise.resolve(JSON.parse(text) as unknown),
            text: () => Promise.resolve(text),
          });
        }
        return original(url as RequestInfo, init);
      }),
    );

    typeInto(input('image_ref'), 'debian:bookworm');
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ image_ref: 'debian:bookworm' }]);
    expect(saveBar()).toBeNull();
    expect(input('image_ref').value).toBe('debian:bookworm@sha256:abc');
  });
});

describe('validation in the browser, on Save', () => {
  it('a wrong number sends nothing, shows under its field, and is counted in the bar', async () => {
    await mount();
    typeInto(input('budget_minutes'), '0');
    typeInto(input('max_instances_override'), '2.5');
    typeInto(input('container_pids'), '-1');
    typeInto(input('container_nofile'), '0');
    typeInto(input('git_author_name'), 'Dominik'); // a valid change alongside
    await settle();
    // Not checked before Save: no problem shown while typing.
    expect(fieldError('budget_minutes')).toBeNull();
    expect(saveBarTitle()).toBe('5 unsaved changes');

    await save();

    expect(h.patchBodies).toEqual([]);
    const message = 'Use a whole number, 1 or more, or leave it empty.';
    expect(fieldError('budget_minutes')).toBe(message);
    expect(fieldError('max_instances_override')).toBe(message);
    expect(fieldError('container_pids')).toBe(message);
    expect(fieldError('container_nofile')).toBe(message);
    expect(fieldError('git_author_name')).toBeNull();
    expect(saveBarTitle()).toBe('4 problems to fix');
    expect(saveBarSections()).toEqual(['Agents', 'Runner']);
    expect(saveBar()?.classList.contains('bad')).toBe(true);
    // Save went to the first problem in page order and focused it.
    expect(h.scrolls.at(-1)).toMatchObject({ target: 'budget_minutes', smooth: true });
    expect(document.activeElement).toBe(input('budget_minutes'));
    expect(input('budget_minutes').getAttribute('aria-invalid')).toBe('true');
    expect(input('budget_minutes').getAttribute('aria-describedby')).toBe(
      'rs-budget_minutes-state rs-budget_minutes-error',
    );
    // The edits are kept.
    expect(input('budget_minutes').value).toBe('0');
    expect(input('git_author_name').value).toBe('Dominik');
  });

  it('a problem clears when its field becomes valid; the last one brings the count back', async () => {
    await mount();
    typeInto(input('budget_minutes'), '0');
    typeInto(input('container_pids'), '0');
    await settle();
    await save();
    expect(saveBarTitle()).toBe('2 problems to fix');

    typeInto(input('budget_minutes'), '45');
    await settle();
    expect(fieldError('budget_minutes')).toBeNull();
    expect(saveBarTitle()).toBe('1 problem to fix');
    expect(saveBarSections()).toEqual(['Runner']);

    // Still wrong: the message stays while the operator types.
    typeInto(input('container_pids'), '-5');
    await settle();
    expect(fieldError('container_pids')).not.toBeNull();

    typeInto(input('container_pids'), '');
    await settle();
    expect(fieldError('container_pids')).toBeNull();
    expect(saveBarTitle()).toBe('1 unsaved change');
    expect(saveBar()?.classList.contains('bad')).toBe(false);

    await save();
    expect(h.patchBodies).toEqual([{ budget_minutes: 45 }]);
  });

  it("a problem section's link in the bar goes to the first problem in it", async () => {
    await mount();
    typeInto(input('budget_minutes'), '0');
    typeInto(input('container_nofile'), '0');
    await settle();
    await save();
    expect(document.activeElement).toBe(input('budget_minutes'));

    const link = Array.from(saveBar()?.querySelectorAll('a') ?? []).find(
      (a) => a.textContent === 'Runner',
    );
    link?.click();
    await settle();

    expect(h.scrolls.at(-1)).toMatchObject({ target: 'container_nofile' });
    expect(document.activeElement).toBe(input('container_nofile'));
  });

  it('checks only the changed fields: a stored oddity never blocks an unrelated save', async () => {
    h.repoOnServer = { ...h.repoOnServer, afk_branch_pattern: 'legacy-pattern' };
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ git_author_name: 'Dominik' }]);
    expect(fieldError('afk_branch_pattern')).toBeNull();
  });
});

describe('a refusal from the server', () => {
  it('that names a field is shown at that field, scrolled to and focused; the edits stay', async () => {
    h.patchRefusal = {
      error: 'image_ref: resolving "nope:latest": manifest unknown',
      field: 'image_ref',
    };
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    typeInto(input('image_ref'), 'nope:latest');
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ image_ref: 'nope:latest', git_author_name: 'Dominik' }]);
    expect(fieldError('image_ref')).toBe('image_ref: resolving "nope:latest": manifest unknown');
    expect(input('image_ref').getAttribute('aria-invalid')).toBe('true');
    expect(document.activeElement).toBe(input('image_ref'));
    expect(h.scrolls.at(-1)).toMatchObject({ target: 'image_ref' });
    expect(saveBarTitle()).toBe('1 problem to fix');
    expect(saveBarSections()).toEqual(['Runner']);
    expect(saveBar()?.querySelector('.settings-savebar-error')).toBeNull();
    // Nothing was saved and nothing was lost.
    expect(input('image_ref').value).toBe('nope:latest');
    expect(input('git_author_name').value).toBe('Dominik');
    expect(toastText()).toBe('');

    // Editing the field clears the refusal; the next Save goes through.
    h.patchRefusal = null;
    typeInto(input('image_ref'), 'debian:bookworm');
    await settle();
    expect(fieldError('image_ref')).toBeNull();
    expect(saveBarTitle()).toBe('2 unsaved changes');
    await save();
    expect(h.patchBodies[1]).toEqual({ image_ref: 'debian:bookworm', git_author_name: 'Dominik' });
    expect(saveBar()).toBeNull();
  });

  it('on a control that is not a text field lands there too', async () => {
    h.patchRefusal = {
      error: 'tracker_binding: "forge" requires a forge_token credential',
      field: 'tracker_binding',
    };
    h.repoOnServer = { ...h.repoOnServer, tracker_binding: 'builtin', forge_kind: 'none' };
    await mount();
    segment('tracker_binding', 'forge').click();
    await settle();
    await save();

    expect(fieldError('tracker_binding')).toBe(
      'tracker_binding: "forge" requires a forge_token credential',
    );
    const group = segment('tracker_binding', 'forge').closest('[role="radiogroup"]');
    expect(group?.getAttribute('aria-invalid')).toBe('true');
    expect(group?.getAttribute('aria-describedby')).toContain('rs-tracker_binding-error');
    expect(document.activeElement).toBe(segment('tracker_binding', 'forge'));
  });

  it('that names no field is shown in the save bar', async () => {
    h.patchRefusal = { error: 'repo is being deleted', status: 409 };
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();
    await save();

    const alert = saveBar()?.querySelector('.settings-savebar-error');
    expect(alert?.textContent).toBe('Not saved. repo is being deleted');
    expect(alert?.getAttribute('role')).toBe('alert');
    expect(container.querySelector('.sfield-error')).toBeNull();
    expect(saveBarTitle()).toBe('1 unsaved change');
    expect(input('git_author_name').value).toBe('Dominik');

    // The next edit clears the message; the change is still pending.
    typeInto(input('git_author_name'), 'Dominik P');
    await settle();
    expect(saveBar()?.querySelector('.settings-savebar-error')).toBeNull();
    expect(saveBarTitle()).toBe('1 unsaved change');
  });

  it('that names a field the page does not have falls back to the save bar', async () => {
    h.patchRefusal = { error: 'unknown field "bogus"', field: 'bogus' };
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();
    await save();

    expect(saveBar()?.querySelector('.settings-savebar-error')?.textContent).toBe(
      'Not saved. unknown field "bogus"',
    );
  });

  it('a network error is shown in the save bar and keeps the edits', async () => {
    h.patchOffline = true;
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();
    await save();

    expect(saveBar()?.querySelector('.settings-savebar-error')?.textContent).toBe(
      'Not saved. Network error — is lab still running?',
    );
    expect(input('git_author_name').value).toBe('Dominik');

    h.patchOffline = false;
    await save();
    expect(saveBar()).toBeNull();
    expect(h.repoOnServer.git_author_name).toBe('Dominik');
  });
});

describe('Discard and Undo', () => {
  const undoButton = () => container.querySelector<HTMLButtonElement>('.toast .toast-action');

  it('Discard restores every field; Undo brings the edits back', async () => {
    await mount();
    await chooseFromSelect('model_default', 'Sonnet');
    typeInto(input('budget_minutes'), '90');
    setSwitch('incogni', true);
    segment('tracker_binding', 'builtin').click();
    typeInto(textarea('afk_prompt'), 'Open a PR when done.');
    input('afk_options.ultracode').click();
    await settle();
    expect(saveBarTitle()).toBe('6 unsaved changes');

    await discard();

    expect(saveBar()).toBeNull();
    expect(container.querySelector('.sfield.changed')).toBeNull();
    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');
    expect(input('budget_minutes').value).toBe('');
    expect(switchOn('incogni')).toBe(false);
    expect(segment('tracker_binding', 'forge').getAttribute('aria-checked')).toBe('true');
    expect(textarea('afk_prompt').value).toBe('');
    expect(input('afk_options.ultracode').checked).toBe(false);
    expect(h.patchBodies).toEqual([]);
    expect(toastText()).toContain('Changes discarded');
    expect(undoButton()?.textContent).toBe('Undo');

    undoButton()?.click();
    await settle();

    expect(saveBarTitle()).toBe('6 unsaved changes');
    expect(selectedLabel('model_default')).toBe('Sonnet');
    expect(input('budget_minutes').value).toBe('90');
    expect(switchOn('incogni')).toBe(true);
    expect(segment('tracker_binding', 'builtin').getAttribute('aria-checked')).toBe('true');
    expect(textarea('afk_prompt').value).toBe('Open a PR when done.');
    expect(input('afk_options.ultracode').checked).toBe(true);

    await save();
    expect(h.patchBodies).toEqual([
      {
        model_default: 'sonnet',
        afk_options: { ultracode: 'true' },
        afk_prompt: 'Open a PR when done.',
        budget_minutes: 90,
        incogni: true,
        tracker_binding: 'builtin',
      },
    ]);
  });

  it('Discard drops the problems too, and Undo brings them back with their fields', async () => {
    await mount();
    typeInto(input('afk_branch_pattern'), 'afk/');
    await settle();
    await save();
    expect(saveBarTitle()).toBe('1 problem to fix');

    await discard();
    expect(saveBar()).toBeNull();
    expect(container.querySelector('.sfield-error')).toBeNull();
    expect(input('afk_branch_pattern').value).toBe('afk/<N>');

    undoButton()?.click();
    await settle();
    expect(input('afk_branch_pattern').value).toBe('afk/');
    expect(fieldError('afk_branch_pattern')).not.toBeNull();
    expect(saveBarTitle()).toBe('1 problem to fix');
  });
});

describe('a refresh of the repo (repo.changed)', () => {
  it('the Auto-spawn field follows a change made elsewhere while it is untouched', async () => {
    await mount();
    expect(switchOn('afk_auto_enabled')).toBe(false);

    // The Overview's Auto switch applies at once, on the server.
    h.repoOnServer = { ...h.repoOnServer, afk_auto_enabled: true };
    emitRepoChanged();
    await settle();

    expect(switchOn('afk_auto_enabled')).toBe(true);
    expect(saveBar()).toBeNull();
  });

  it('but not while the operator has an unsaved edit on it', async () => {
    h.repoOnServer = { ...h.repoOnServer, afk_auto_enabled: true };
    await mount();
    setSwitch('afk_auto_enabled', false); // the operator's unsaved edit: off
    await settle();
    expect(saveBarTitle()).toBe('1 unsaved change');

    // Other fields change on the server; so does nothing about Auto-spawn.
    h.repoOnServer = { ...h.repoOnServer, budget_minutes: 30, default_branch: 'master' };
    emitRepoChanged();
    await settle();

    expect(switchOn('afk_auto_enabled')).toBe(false); // the edit stands
    expect(fieldChanged('afk_auto_enabled')).toBe(true);
    expect(input('budget_minutes').value).toBe('30'); // untouched fields follow
    expect(input('default_branch').value).toBe('master');
    expect(saveBarTitle()).toBe('1 unsaved change');

    await save();
    expect(h.patchBodies).toEqual([{ afk_auto_enabled: false }]);
    expect(h.repoOnServer.budget_minutes).toBe(30);
  });

  it('an edit the server caught up with is no longer pending', async () => {
    await mount();
    setSwitch('afk_auto_enabled', true);
    await settle();
    expect(saveBarTitle()).toBe('1 unsaved change');

    h.repoOnServer = { ...h.repoOnServer, afk_auto_enabled: true };
    emitRepoChanged();
    await settle();

    expect(switchOn('afk_auto_enabled')).toBe(true);
    expect(saveBar()).toBeNull();
  });

  it('pending edits survive a refresh that fails', async () => {
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();

    // The refetch fails: the frame has no repo to show for a moment.
    const original = globalThis.fetch;
    let failing = true;
    vi.stubGlobal(
      'fetch',
      vi.fn((url: unknown, init?: RequestInit) =>
        failing && String(url) === `/api/v1/repos/${REPO_ID}` && (init?.method ?? 'GET') === 'GET'
          ? Promise.reject(new TypeError('Failed to fetch'))
          : original(url as RequestInfo, init),
      ),
    );
    emitRepoChanged();
    await settle();

    expect(input('git_author_name').value).toBe('Dominik');
    expect(saveBarTitle()).toBe('1 unsaved change');

    failing = false;
    await save();
    expect(h.patchBodies).toEqual([{ git_author_name: 'Dominik' }]);
    await flush();
    expect(saveBar()).toBeNull();
  });
});

describe('the form asks nothing through the browser', () => {
  it('no window.confirm for an edit, a save, a discard or a jump', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    await mount();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();
    await discard();
    typeInto(input('git_author_name'), 'Dominik');
    await settle();
    await save();

    expect(confirmSpy).not.toHaveBeenCalled();
  });
});
