// Global settings › Runner (issues #55, #205, #85, #115): the Runner pick (the repo
// page's two picks, no inherit segment) with the unsandboxed-host warning and
// the inheriting-repo count line; Dev image, whose hint names what a blank
// falls through to; Container limits. Nothing folds: Dev image and the limits
// show whatever the Runner is, and the note says they apply to container runs,
// including repos pinned to container. The Nested containers default is an
// on/off switch with the repo page's copy, and the nested image cache a
// textarea of image refs, one per line, sent as the server stores it.
//
// Switching the default TO host asks at Save, in an in-page dialog naming the
// inheriting repos: Cancel sends nothing and keeps every edit, Switch to host
// saves the whole pending patch. A switch to container never asks, and
// neither does any other Save — the leave dialog's "Save and leave" asks too.

import { describe, expect, it, vi } from 'vitest';
import {
  container,
  dialogButton,
  fieldChanged,
  fieldError,
  fieldHint,
  fieldLabel,
  fieldState,
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
  switchButton,
  switchOn,
  textarea,
  toastText,
  typeField,
  typeInto,
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

const NESTED_HINT =
  "Lets a run start its own rootless podman containers (ddev, docker compose, buildx). Widens the run's kernel surface and adds several gigabytes of per-run storage.";

describe('Runner: nested containers default', () => {
  it('renders the switch off from the seed, with the repo page copy', async () => {
    await mountPage();
    expect(fieldLabel('nested_containers_default')).toBe('Nested containers');
    expect(switchOn('nested_containers_default')).toBe(false);
    expect(fieldHint('nested_containers_default')).toBe(
      `${NESTED_HINT} The default for every repo that inherits it; a repo can pin its own. Ignored under the host runner.`,
    );
    // The bottom layer: nothing to inherit, no state chip.
    expect(fieldState('nested_containers_default')).toBeNull();
  });

  it('seeds on from a stored true', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, nested_containers_default: true };
    await mountPage();
    expect(switchOn('nested_containers_default')).toBe(true);
  });

  it('turning it on saves exactly nested_containers_default: true, and back off false', async () => {
    await mountPage();
    switchButton('nested_containers_default').click();
    await settle();
    expect(fieldChanged('nested_containers_default')).toBe(true);
    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();
    expect(h.patchBodies).toEqual([{ nested_containers_default: true }]);
    expect(h.settingsOnServer.nested_containers_default).toBe(true);
    expect(saveBar()).toBeNull();

    switchButton('nested_containers_default').click();
    await settle();
    await save();
    expect(h.patchBodies.at(-1)).toEqual({ nested_containers_default: false });
  });

  it('never folds: it shows with Host as with Container', async () => {
    await mountPage();
    for (const runner of ['host', 'container']) {
      await pick('runner_default', runner);
      expect(container.querySelector('[data-field="nested_containers_default"]')).not.toBeNull();
      expect(container.querySelector('[data-field="nested_image_cache"]')).not.toBeNull();
    }
  });
});

describe('Runner: nested image cache', () => {
  const REFS = 'docker.io/ddev/ddev-webserver:v1.25.5\ndocker.io/ddev/ddev-traefik-router:v1.25.5';

  it('is a textarea seeded from the stored list, one ref per line, with its help', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, nested_image_cache: REFS };
    await mountPage();
    expect(fieldLabel('nested_image_cache')).toBe('Nested image cache');
    expect(textarea('nested_image_cache').value).toBe(REFS);
    const hint = fieldHint('nested_image_cache');
    expect(hint).toContain('One image ref per line.');
    expect(hint).toContain('shared read-only store that nested runs use');
    expect(hint).toContain('Saving the list starts a pull.');
    expect(hint).toContain('a failed pull is logged and never blocks a run');
    // The hint describes the control.
    expect(textarea('nested_image_cache').getAttribute('aria-describedby')).toBe(
      'rs-nested_image_cache-hint',
    );
  });

  it('round-trips: sent as the server stores it, and the answer is applied', async () => {
    await mountPage();
    typeInto(
      textarea('nested_image_cache'),
      '  docker.io/ddev/ddev-webserver:v1.25.5  \n\n docker.io/ddev/ddev-traefik-router:v1.25.5\n',
    );
    await settle();
    expect(fieldChanged('nested_image_cache')).toBe(true);
    await save();

    expect(h.patchBodies).toEqual([{ nested_image_cache: REFS }]);
    expect(h.settingsOnServer.nested_image_cache).toBe(REFS);
    expect(textarea('nested_image_cache').value).toBe(REFS);
    expect(saveBar()).toBeNull();
  });

  it('blank lines and indentation alone are no change', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, nested_image_cache: REFS };
    await mountPage();
    typeInto(textarea('nested_image_cache'), `\n${REFS}\n\n`);
    await settle();
    expect(fieldChanged('nested_image_cache')).toBe(false);
    expect(saveBar()).toBeNull();
  });

  it('clearing the list sends ""', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, nested_image_cache: REFS };
    await mountPage();
    typeInto(textarea('nested_image_cache'), '');
    await settle();
    await save();
    expect(h.patchBodies).toEqual([{ nested_image_cache: '' }]);
  });

  it('a line with a space is a problem at the field, and nothing is sent', async () => {
    await mountPage();
    typeInto(textarea('nested_image_cache'), 'docker.io/library/debian:trixie\nddev webserver');
    await settle();
    await save();

    expect(h.patchBodies).toEqual([]);
    expect(fieldError('nested_image_cache')).toBe(
      'Line 2 contains a space. Put one image ref on each line.',
    );
    expect(textarea('nested_image_cache').getAttribute('aria-invalid')).toBe('true');
    expect(saveBarTitle()).toBe('1 problem to fix');
  });

  it('a refusal naming the field is pinned under the textarea, and the list stays', async () => {
    await mountPage();
    typeInto(textarea('nested_image_cache'), 'docker.io/library/debian:trixie');
    await settle();
    h.patchRefusal = {
      error: 'nested_image_cache: line 1 is not an image reference',
      field: 'nested_image_cache',
    };
    await save();

    expect(fieldError('nested_image_cache')).toBe(
      'nested_image_cache: line 1 is not an image reference',
    );
    expect(textarea('nested_image_cache').getAttribute('aria-invalid')).toBe('true');
    expect(document.activeElement).toBe(textarea('nested_image_cache'));
    expect(textarea('nested_image_cache').value).toBe('docker.io/library/debian:trixie');

    // Editing it clears the refusal; the next Save goes through.
    typeInto(textarea('nested_image_cache'), 'docker.io/library/debian:bookworm');
    await settle();
    expect(fieldError('nested_image_cache')).toBeNull();
    await save();
    expect(h.patchBodies.at(-1)).toEqual({
      nested_image_cache: 'docker.io/library/debian:bookworm',
    });
  });

  it('saves with the switch in one PATCH', async () => {
    await mountPage();
    switchButton('nested_containers_default').click();
    typeInto(textarea('nested_image_cache'), 'docker.io/library/debian:trixie');
    await settle();
    await save();
    expect(h.patchBodies).toEqual([
      { nested_containers_default: true, nested_image_cache: 'docker.io/library/debian:trixie' },
    ]);
  });
});
