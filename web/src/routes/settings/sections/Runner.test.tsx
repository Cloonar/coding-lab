// Global settings › Runner coverage (issue #55): the fourth section, mirroring
// the repo-settings Runner section — Runner, Dev image and Container limits
// cards in one form, each saved through the settings PATCH as a
// dirty-fields-only body, server 400s in the section banner. The Runner card's
// inheriting-repo count and its one switch-to-host confirmation, and the Dev
// image card's blank-field fallback hint, are the section's own behavior.
// Mounted at /settings/runner.

import { describe, expect, it, vi } from 'vitest';
import {
  cardByHeading,
  chooseFromSelect,
  container,
  h,
  input,
  installSettingsHooks,
  mountAt,
  optionRows,
  repoWithRunner,
  selectTrigger,
  selectedLabel,
  settle,
  submitForm,
  typeInto,
  waitFor,
} from '../harness';

installSettingsHooks();

const HOST_LABEL = 'Host — unsandboxed, full host access';
const CONTAINER_LABEL = 'Container — rootless podman';
const FALLBACK = 'ghcr.io/cloonar/dev:1.4@sha256:0123456789abcdef';

/** The stored settings of a fresh install after the Runner slice: host default,
 *  no global dev image, the three seeded limits, and the read-only fallback. */
function seed(over: Record<string, unknown> = {}): void {
  h.settingsOnServer = {
    runner_default: 'host',
    dev_image_default: '',
    dev_image_fallback: '',
    container_memory: '8g',
    container_pids: 4096,
    container_nofile: 16384,
    ...over,
  };
}

async function mountRunner(): Promise<void> {
  await mountAt('/settings/runner');
  await waitFor(() => container.querySelector('button[name="runner_default"]'), 'runner select');
}

const text = () => container.textContent ?? '';

/** The Dev image card's field hint — the `.hint` directly under the input. */
const imageHint = () =>
  container.querySelector<HTMLElement>('input[name="dev_image_default"] + small.hint')
    ?.textContent ?? '';

describe('Settings runner section — structure', () => {
  it('renders the Runner, Dev image and Container limits cards in that order with one Save button', async () => {
    seed();
    await mountRunner();

    const headings = Array.from(container.querySelectorAll('section.card h2')).map(
      (el) => el.textContent,
    );
    expect(headings).toEqual(['Runner', 'Dev image', 'Container limits']);

    // One form, one submit button.
    expect(container.querySelectorAll('form')).toHaveLength(1);
    const submits = Array.from(container.querySelectorAll('button[type="submit"]'));
    expect(submits.map((b) => b.textContent?.trim())).toEqual(['Save settings']);

    // Each field lives in its own card.
    expect(cardByHeading('Runner').querySelector('button[name="runner_default"]')).not.toBeNull();
    expect(
      cardByHeading('Dev image').querySelector('input[name="dev_image_default"]'),
    ).not.toBeNull();
    const limits = cardByHeading('Container limits');
    for (const name of ['container_memory', 'container_pids', 'container_nofile']) {
      expect(limits.querySelector(`input[name="${name}"]`)).not.toBeNull();
    }
  });

  it('seeds every field from the stored settings', async () => {
    seed({
      runner_default: 'container',
      dev_image_default: 'docker.io/library/debian:bookworm@sha256:abc',
      container_memory: '4g',
      container_pids: 2048,
      container_nofile: 8192,
    });
    await mountRunner();

    expect(selectedLabel('runner_default')).toBe(CONTAINER_LABEL);
    expect(input('dev_image_default').value).toBe('docker.io/library/debian:bookworm@sha256:abc');
    expect(input('container_memory').value).toBe('4g');
    expect(input('container_pids').value).toBe('2048');
    expect(input('container_nofile').value).toBe('8192');
  });

  it('a clean submit notes "Nothing to save." and never PATCHes', async () => {
    seed();
    await mountRunner();

    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([]);
    expect(text()).toContain('Nothing to save.');
  });
});

describe('Settings runner section — Runner card', () => {
  it('offers the same two options and labels as the repo page, with no inherit row', async () => {
    seed();
    await mountRunner();

    selectTrigger('runner_default').click();
    await settle();

    expect(optionRows().map((r) => r.querySelector('.select-option-label')?.textContent)).toEqual([
      CONTAINER_LABEL,
      HOST_LABEL,
    ]);
  });

  it('shows the unsandboxed hint only while Host is selected', async () => {
    seed({ runner_default: 'container' });
    await mountRunner();
    expect(text()).not.toContain('Host runs are unsandboxed');

    await chooseFromSelect('runner_default', HOST_LABEL);
    expect(text()).toContain(
      'Host runs are unsandboxed — the agent has full host access to the server.',
    );

    await chooseFromSelect('runner_default', CONTAINER_LABEL);
    expect(text()).not.toContain('Host runs are unsandboxed');
  });

  it('states how many repos inherit the default (plural)', async () => {
    seed();
    h.reposOnServer = [
      repoWithRunner('a', null),
      repoWithRunner('b', null),
      repoWithRunner('c', null),
      repoWithRunner('d', 'host'),
      repoWithRunner('e', 'container'),
    ];
    await mountRunner();

    expect(text()).toContain('3 repos inherit this default.');
  });

  it('states the singular for one inheriting repo', async () => {
    seed();
    h.reposOnServer = [repoWithRunner('a', null), repoWithRunner('b', 'host')];
    await mountRunner();

    expect(text()).toContain('1 repo inherits this default.');
  });

  it('says so when no repo inherits the default', async () => {
    seed();
    h.reposOnServer = [repoWithRunner('a', 'host'), repoWithRunner('b', 'container')];
    await mountRunner();

    expect(text()).toContain('No repos inherit this default.');
  });

  it('omits the count line and still saves when the repo list cannot be fetched', async () => {
    seed({ runner_default: 'host' });
    h.reposError = true;
    await mountRunner();

    expect(text()).not.toContain('inherit this default');
    expect(text()).not.toContain('inherits this default');

    await chooseFromSelect('runner_default', CONTAINER_LABEL);
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ runner_default: 'container' }]);
  });

  it('switching to Host asks one confirmation naming the inheriting repo count, then saves', async () => {
    seed({ runner_default: 'container' });
    h.reposOnServer = [
      repoWithRunner('a', null),
      repoWithRunner('b', null),
      repoWithRunner('c', null),
      repoWithRunner('d', 'container'),
    ];
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
    await mountRunner();

    await chooseFromSelect('runner_default', HOST_LABEL);
    // Picking is not saving: no prompt yet.
    expect(confirm).not.toHaveBeenCalled();

    submitForm();
    await settle();

    expect(confirm).toHaveBeenCalledTimes(1);
    expect(confirm).toHaveBeenCalledWith(
      'Switch the global runner default to Host? 3 repos inherit it and their next sessions will run unsandboxed with full host access.',
    );
    expect(h.patchBodies).toEqual([{ runner_default: 'host' }]);
    expect(h.settingsOnServer.runner_default).toBe('host');
  });

  it('names a single inheriting repo in the singular', async () => {
    seed({ runner_default: 'container' });
    h.reposOnServer = [repoWithRunner('a', null)];
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
    await mountRunner();

    await chooseFromSelect('runner_default', HOST_LABEL);
    submitForm();
    await settle();

    expect(confirm).toHaveBeenCalledWith(
      'Switch the global runner default to Host? 1 repo inherits it and its next session will run unsandboxed with full host access.',
    );
  });

  it('still asks, without a number, when the repo list could not be fetched', async () => {
    seed({ runner_default: 'container' });
    h.reposError = true;
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
    await mountRunner();

    await chooseFromSelect('runner_default', HOST_LABEL);
    submitForm();
    await settle();

    expect(confirm).toHaveBeenCalledTimes(1);
    expect(confirm).toHaveBeenCalledWith(
      'Switch the global runner default to Host? Repos that inherit it will run their next sessions unsandboxed with full host access.',
    );
    expect(h.patchBodies).toEqual([{ runner_default: 'host' }]);
  });

  it('cancelling the confirmation saves nothing — not even other dirty fields — and keeps the drafts', async () => {
    seed({ runner_default: 'container' });
    h.reposOnServer = [repoWithRunner('a', null)];
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    await mountRunner();

    await chooseFromSelect('runner_default', HOST_LABEL);
    typeInto(input('container_memory'), '4g');
    submitForm();
    await settle();

    expect(confirm).toHaveBeenCalledTimes(1);
    expect(h.patchBodies).toEqual([]);
    expect(text()).not.toContain('Saved.');
    // Nothing was refetched/remounted: the operator's edits are still there.
    expect(selectedLabel('runner_default')).toBe(HOST_LABEL);
    expect(input('container_memory').value).toBe('4g');
  });

  it('switching to Container never asks for confirmation', async () => {
    seed({ runner_default: 'host' });
    h.reposOnServer = [repoWithRunner('a', null), repoWithRunner('b', null)];
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    await mountRunner();

    await chooseFromSelect('runner_default', CONTAINER_LABEL);
    submitForm();
    await settle();

    expect(confirm).not.toHaveBeenCalled();
    expect(h.patchBodies).toEqual([{ runner_default: 'container' }]);
  });

  it('saving another field while the default is already Host does not ask', async () => {
    seed({ runner_default: 'host' });
    h.reposOnServer = [repoWithRunner('a', null)];
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    await mountRunner();

    typeInto(input('container_memory'), '4g');
    submitForm();
    await settle();

    expect(confirm).not.toHaveBeenCalled();
    expect(h.patchBodies).toEqual([{ container_memory: '4g' }]);
  });

  it('a server 400 on the runner default shows in the section banner', async () => {
    seed({ runner_default: 'container' });
    h.patchError = 'runner_default must be "host" or "container"';
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    await mountRunner();

    await chooseFromSelect('runner_default', HOST_LABEL);
    submitForm();
    await settle();

    expect(container.querySelector('.banner')?.textContent).toContain(
      'runner_default must be "host" or "container"',
    );
    expect(selectedLabel('runner_default')).toBe(HOST_LABEL);
  });
});

describe('Settings runner section — Dev image card', () => {
  it('names the deployed image in the hint while the field is blank', async () => {
    seed({ dev_image_default: '', dev_image_fallback: FALLBACK });
    await mountRunner();

    expect(imageHint()).toContain(`Blank uses the deployed image ${FALLBACK}`);
    // The ref is rendered as code, not prose.
    expect(
      container.querySelector('input[name="dev_image_default"] + small.hint code')?.textContent,
    ).toBe(FALLBACK);
  });

  it('says no image is configured when the fallback is empty too', async () => {
    seed({ dev_image_default: '', dev_image_fallback: '' });
    await mountRunner();

    expect(imageHint()).toContain(
      'No dev image is configured — container spawns are refused until one is set here, per repo, or on the server.',
    );
    expect(imageHint()).not.toContain('Blank uses the deployed image');
  });

  it('treats an absent dev_image_default like a blank one', async () => {
    h.settingsOnServer = { runner_default: 'host', dev_image_fallback: FALLBACK };
    await mountRunner();

    expect(input('dev_image_default').value).toBe('');
    expect(imageHint()).toContain(`Blank uses the deployed image ${FALLBACK}`);
  });

  it('drops the fallback hint while the field has a value and brings it back when cleared', async () => {
    seed({ dev_image_fallback: FALLBACK });
    await mountRunner();
    expect(imageHint()).toContain('Blank uses the deployed image');

    typeInto(input('dev_image_default'), 'docker.io/library/debian:bookworm');
    await settle();
    expect(imageHint()).not.toContain('Blank uses the deployed image');
    expect(imageHint()).not.toContain(FALLBACK);
    expect(imageHint()).toContain('Resolved and pinned to a digest on save.');

    // Whitespace alone is still blank.
    typeInto(input('dev_image_default'), '   ');
    await settle();
    expect(imageHint()).toContain(`Blank uses the deployed image ${FALLBACK}`);
  });

  it('shows no fallback hint when the stored default is set', async () => {
    seed({
      dev_image_default: 'docker.io/library/debian:bookworm@sha256:abc',
      dev_image_fallback: FALLBACK,
    });
    await mountRunner();

    expect(imageHint()).not.toContain('Blank uses the deployed image');
    expect(imageHint()).not.toContain('No dev image is configured');
  });

  it('never shows the fallback as its own field or row', async () => {
    seed({ dev_image_fallback: FALLBACK });
    await mountRunner();

    expect(container.querySelector('input[name="dev_image_fallback"]')).toBeNull();
    const values = Array.from(container.querySelectorAll('input')).map((el) => el.value);
    expect(values).not.toContain(FALLBACK);
    expect(container.querySelectorAll('section.card')).toHaveLength(3);
  });

  it('typing an image ref PATCHes dev_image_default alone, trimmed', async () => {
    seed({ dev_image_fallback: FALLBACK });
    await mountRunner();

    typeInto(input('dev_image_default'), '  docker.io/library/debian:bookworm ');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ dev_image_default: 'docker.io/library/debian:bookworm' }]);
  });

  it('clearing a stored image PATCHes an empty string', async () => {
    seed({ dev_image_default: 'docker.io/library/debian:bookworm@sha256:abc' });
    await mountRunner();

    typeInto(input('dev_image_default'), '');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ dev_image_default: '' }]);
    expect(h.settingsOnServer.dev_image_default).toBe('');
  });

  it('never sends dev_image_fallback in a PATCH, whatever else is saved', async () => {
    seed({ dev_image_fallback: FALLBACK, runner_default: 'container' });
    h.reposOnServer = [repoWithRunner('a', null)];
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    await mountRunner();

    // Dirty every field of the section at once.
    await chooseFromSelect('runner_default', HOST_LABEL);
    typeInto(input('dev_image_default'), 'docker.io/library/debian:bookworm');
    typeInto(input('container_memory'), '4g');
    typeInto(input('container_pids'), '2048');
    typeInto(input('container_nofile'), '8192');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([
      {
        runner_default: 'host',
        dev_image_default: 'docker.io/library/debian:bookworm',
        container_memory: '4g',
        container_pids: 2048,
        container_nofile: 8192,
      },
    ]);
    for (const body of h.patchBodies) expect(body).not.toHaveProperty('dev_image_fallback');
    // And the server stub (which 400s a read-only key) accepted the save.
    expect(text()).not.toContain('read-only');
  });

  it('a server 400 on the image ref shows in the section banner and keeps the draft', async () => {
    seed();
    h.patchError = 'resolve docker.io/nope/nothing:1: manifest unknown';
    await mountRunner();

    typeInto(input('dev_image_default'), 'docker.io/nope/nothing:1');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ dev_image_default: 'docker.io/nope/nothing:1' }]);
    expect(container.querySelector('.banner')?.textContent).toContain(
      'resolve docker.io/nope/nothing:1: manifest unknown',
    );
    expect(input('dev_image_default').value).toBe('docker.io/nope/nothing:1');
  });
});

describe('Settings runner section — Container limits card', () => {
  it('edits all three limits in one PATCH', async () => {
    seed();
    await mountRunner();

    typeInto(input('container_memory'), '512m');
    typeInto(input('container_pids'), '2048');
    typeInto(input('container_nofile'), '8192');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([
      { container_memory: '512m', container_pids: 2048, container_nofile: 8192 },
    ]);
    expect(h.settingsOnServer.container_memory).toBe('512m');
    expect(h.settingsOnServer.container_pids).toBe(2048);
    expect(h.settingsOnServer.container_nofile).toBe(8192);
  });

  it('sends only the limits that were edited', async () => {
    seed();
    await mountRunner();

    typeInto(input('container_pids'), '1024');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ container_pids: 1024 }]);
  });

  it('shows concrete values, not an inherit hint', async () => {
    seed();
    await mountRunner();

    expect(cardByHeading('Container limits').textContent).not.toContain('Inherit global default');
  });

  it('a blank whole-number limit is a client-side error and sends nothing', async () => {
    seed();
    await mountRunner();

    typeInto(input('container_pids'), '');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([]);
    expect(container.querySelector('.banner')?.textContent).toContain(
      'Container PID limit must be a whole number.',
    );

    typeInto(input('container_pids'), '4096');
    typeInto(input('container_nofile'), '');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([]);
    expect(container.querySelector('.banner')?.textContent).toContain(
      'Container open-files limit must be a whole number.',
    );
  });

  it('a blank memory limit is a client-side error and sends nothing', async () => {
    seed();
    await mountRunner();

    typeInto(input('container_memory'), '  ');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([]);
    expect(container.querySelector('.banner')?.textContent).toContain(
      'Container memory limit must not be blank.',
    );
  });

  it('leaves grammar and minimums to the server and shows its 400 in the banner', async () => {
    seed();
    h.patchError = 'container_memory: invalid memory "lots"';
    await mountRunner();

    typeInto(input('container_memory'), 'lots');
    typeInto(input('container_pids'), '0');
    submitForm();
    await settle();

    // The client sent the values as typed...
    expect(h.patchBodies).toEqual([{ container_memory: 'lots', container_pids: 0 }]);
    // ...and surfaced the server's refusal, keeping the drafts for a retry.
    expect(container.querySelector('.banner')?.textContent).toContain(
      'container_memory: invalid memory "lots"',
    );
    expect(input('container_memory').value).toBe('lots');
  });
});
