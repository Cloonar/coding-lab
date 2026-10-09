// Global settings › Runner (issues #55, #205, #85): the Runner pick (the repo
// page's two picks, no inherit segment) with the unsandboxed-host warning and
// the inheriting-repo count line; Dev image, whose hint names what a blank
// falls through to; Container limits. Nothing folds: Dev image and the limits
// show whatever the Runner is, and the note says they apply to container runs,
// including repos pinned to container.
//
// Switching the default TO host asks at Save, in an in-page dialog naming the
// inheriting repos: Cancel sends nothing and keeps every edit, Switch to host
// saves the whole pending patch. A switch to container never asks, and
// neither does any other Save — the leave dialog's "Save and leave" asks too.

import { describe, expect, it, vi } from 'vitest';
import {
  container,
  dialogButton,
  fieldError,
  fieldHint,
  h,
  history,
  input,
  installSettingsHooks,
  leaveToOther,
  mountPage,
  openDialog,
  pageSection,
  pick,
  repoWithRunner,
  save,
  saveBar,
  saveBarTitle,
  segmentLabels,
  segmentValue,
  settle,
  toastText,
  typeField,
} from '../harness';

installSettingsHooks();

const runnerText = (): string => pageSection('runner').textContent ?? '';
const hostWarning = (): Element | null =>
  pageSection('runner').querySelector('.banner') as Element | null;

function inheriting(count: number): void {
  h.reposOnServer = [
    ...Array.from({ length: count }, (_, i) => repoWithRunner(`inherits-${i}`, null)),
    repoWithRunner('pinned', 'container'),
  ];
}

describe('Runner: the pick', () => {
  it("offers the repo page's two picks, with no inherit segment", async () => {
    await mountPage();
    expect(segmentLabels('runner_default')).toEqual(['Container', 'Host']);
    expect(segmentValue('runner_default')).toBe('host');
    expect(fieldHint('runner_default')).toBe(
      'Where sessions run for every repo that inherits the default. A repo can pin its own.',
    );
  });

  it('warns about host runs only while Host is drafted', async () => {
    await mountPage();
    expect(hostWarning()?.textContent).toContain('Host runs are unsandboxed');

    await pick('runner_default', 'container');
    expect(hostWarning()).toBeNull();
    await pick('runner_default', 'host');
    expect(hostWarning()).not.toBeNull();
  });

  it('says how many repos inherit the default — plural, singular, none', async () => {
    inheriting(3);
    await mountPage();
    expect(runnerText()).toContain('3 repos inherit this default.');
  });

  it('says it in the singular for one repo', async () => {
    inheriting(1);
    await mountPage();
    expect(runnerText()).toContain('1 repo inherits this default.');
  });

  it('says so when no repo inherits the default', async () => {
    inheriting(0);
    await mountPage();
    expect(runnerText()).toContain('No repos inherit this default.');
  });

  it('leaves the line out when the repo list cannot be loaded', async () => {
    h.reposError = true;
    await mountPage();
    expect(runnerText()).not.toContain('inherit this default');
    expect(runnerText()).not.toContain('inherits this default');
  });
});

describe('Runner: dev image and container limits never fold', () => {
  it('shows them with Host as with Container, saying whom they apply to', async () => {
    await mountPage();
    for (const runner of ['host', 'container']) {
      await pick('runner_default', runner);
      for (const key of [
        'dev_image_default',
        'container_memory',
        'container_pids',
        'container_nofile',
      ]) {
        expect(container.querySelector(`[data-field="${key}"]`)).not.toBeNull();
      }
      expect(runnerText()).toContain(
        'The dev image and the container limits apply to container runs — including repos pinned to container while the default is Host.',
      );
    }
    expect(runnerText()).not.toContain('Show anyway');
  });

  it('seeds the limits from the stored settings', async () => {
    await mountPage();
    expect(input('container_memory').value).toBe('8g');
    expect(input('container_pids').value).toBe('4096');
    expect(input('container_nofile').value).toBe('16384');
  });

  it('names the deployed image while the dev image is blank, and the pinning once one is typed', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, dev_image_fallback: 'ghcr.io/lab/dev:full' };
    await mountPage();
    expect(fieldHint('dev_image_default')).toBe(
      'Blank uses the deployed image ghcr.io/lab/dev:full.',
    );
    expect(container.querySelector('[data-field="dev_image_fallback"]')).toBeNull();

    await typeField('dev_image_default', 'ghcr.io/acme/dev:1');
    expect(fieldHint('dev_image_default')).toBe('Resolved and pinned to a digest on save.');

    await typeField('dev_image_default', '');
    expect(fieldHint('dev_image_default')).toContain('Blank uses the deployed image');
  });

  it('says no image is configured when there is no fallback either', async () => {
    await mountPage();
    expect(fieldHint('dev_image_default')).toBe(
      'No dev image is configured — container spawns are refused until one is set here, per repo, or on the server.',
    );
  });

  it('a typed image ref is sent trimmed; clearing a stored one sends ""', async () => {
    await mountPage();
    await typeField('dev_image_default', '  ghcr.io/acme/dev:1  ');
    await save();
    expect(h.patchBodies).toEqual([{ dev_image_default: 'ghcr.io/acme/dev:1' }]);

    await typeField('dev_image_default', '');
    await save();
    expect(h.patchBodies.at(-1)).toEqual({ dev_image_default: '' });
  });

  it('edits all three limits in one PATCH', async () => {
    await mountPage();
    await typeField('container_memory', '4g');
    await typeField('container_pids', '512');
    await typeField('container_nofile', '2048');
    await save();
    expect(h.patchBodies).toEqual([
      { container_memory: '4g', container_pids: 512, container_nofile: 2048 },
    ]);
  });

  it('a blank memory limit and a zero process limit are problems at their fields', async () => {
    await mountPage();
    await typeField('container_memory', ' ');
    await typeField('container_pids', '0');
    await save();

    expect(h.patchBodies).toEqual([]);
    expect(fieldError('container_memory')).toBe('Enter a memory limit, for example 8g.');
    expect(fieldError('container_pids')).toBe('Use a whole number, 1 or more.');
    expect(saveBarTitle()).toBe('2 problems to fix');
  });

  it("a bad memory grammar is the server's call, shown at the field", async () => {
    await mountPage();
    await typeField('container_memory', 'lots');
    h.patchRefusal = {
      error: 'container_memory must look like a podman --memory value, e.g. "8g"',
      field: 'container_memory',
    };
    await save();

    expect(fieldError('container_memory')).toContain('podman --memory');
    expect(input('container_memory').value).toBe('lots');
  });
});

describe('Runner: switching the default to host', () => {
  async function mountOnContainer(count = 2): Promise<void> {
    inheriting(count);
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountPage();
  }

  it('Save asks in an in-page dialog naming the inheriting repos, before sending anything', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    await mountOnContainer(2);
    await pick('runner_default', 'host');

    await save();

    const dialog = openDialog();
    expect(dialog?.getAttribute('role')).toBe('alertdialog');
    expect(dialog?.querySelector('.dialog-title')?.textContent).toBe(
      'Switch the global runner default to Host?',
    );
    expect(dialog?.textContent).toContain(
      '2 repos inherit it and their next sessions will run unsandboxed with full host access.',
    );
    expect(Array.from(dialog?.querySelectorAll('button') ?? []).map((b) => b.textContent)).toEqual([
      'Cancel',
      'Switch to host',
    ]);
    expect(document.activeElement).toBe(dialogButton('Cancel'));
    expect(h.patchBodies).toEqual([]);
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('names one inheriting repo in the singular', async () => {
    await mountOnContainer(1);
    await pick('runner_default', 'host');
    await save();
    expect(openDialog()?.textContent).toContain(
      '1 repo inherits it and its next session will run unsandboxed with full host access.',
    );
  });

  it('still asks, without a number, when the repo list could not be loaded', async () => {
    h.reposError = true;
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountPage();
    await pick('runner_default', 'host');
    await save();
    expect(openDialog()?.textContent).toContain(
      'Repos that inherit it will run their next sessions unsandboxed with full host access.',
    );
  });

  it('Cancel sends nothing — not even the other pending changes — and keeps every edit', async () => {
    await mountOnContainer();
    await pick('runner_default', 'host');
    await typeField('git_author_name', 'Dominik');
    await save();

    dialogButton('Cancel').click();
    await settle();

    expect(openDialog()).toBeNull();
    expect(h.patchBodies).toEqual([]);
    expect(segmentValue('runner_default')).toBe('host');
    expect(input('git_author_name').value).toBe('Dominik');
    expect(saveBarTitle()).toBe('2 unsaved changes');
    expect(toastText()).toBe('');
  });

  it('Switch to host saves the whole pending patch', async () => {
    await mountOnContainer();
    await pick('runner_default', 'host');
    await typeField('git_author_name', 'Dominik');
    await save();

    dialogButton('Switch to host').click();
    await settle();

    expect(h.patchBodies).toEqual([{ runner_default: 'host', git_author_name: 'Dominik' }]);
    expect(toastText()).toBe('Saved 2 changes');
    expect(saveBar()).toBeNull();
  });

  it('switching to container never asks', async () => {
    await mountPage();
    await pick('runner_default', 'container');
    await save();
    expect(openDialog()).toBeNull();
    expect(h.patchBodies).toEqual([{ runner_default: 'container' }]);
  });

  it('saving another field while the default already is host does not ask', async () => {
    await mountPage();
    await typeField('container_pids', '512');
    await save();
    expect(openDialog()).toBeNull();
    expect(h.patchBodies).toEqual([{ container_pids: 512 }]);
  });

  it("the leave dialog's Save and leave asks too; Cancel stays with every edit", async () => {
    await mountOnContainer();
    await pick('runner_default', 'host');
    await leaveToOther();

    dialogButton('Save and leave').click();
    await settle();
    expect(openDialog()?.querySelector('.dialog-title')?.textContent).toBe(
      'Switch the global runner default to Host?',
    );

    dialogButton('Cancel').click();
    await settle();
    expect(h.patchBodies).toEqual([]);
    expect(history.get()).not.toBe('/other');
    expect(segmentValue('runner_default')).toBe('host');
  });

  it("the leave dialog's Save and leave goes once the switch is confirmed", async () => {
    await mountOnContainer();
    await pick('runner_default', 'host');
    await leaveToOther();

    dialogButton('Save and leave').click();
    await settle();
    dialogButton('Switch to host').click();
    await settle();

    expect(h.patchBodies).toEqual([{ runner_default: 'host' }]);
    expect(history.get()).toBe('/other');
  });
});
