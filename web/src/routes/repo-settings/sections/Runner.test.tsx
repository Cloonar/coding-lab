// Runner section suite (issue #205), the Autoland.test.tsx harness style
// applied to /repos/:id/settings/runner: the default host pick shows the
// unsandboxed warning, picking container hides it and PATCHes runner, and the
// three container limit overrides set/clear independently. Issue #55 adds the
// inherit state: the picker's first row follows the global runner default, the
// host hint follows the EFFECTIVE runner, and the dev image hint names what a
// blank field inherits.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  baseRepo,
  chooseFromSelect,
  container,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  optionRows,
  selectTrigger,
  selectedLabel,
  settle,
  submitForm,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountRunner = () => mountSettings(`/repos/${REPO_ID}/settings/runner`);

describe('RepoSettings Runner', () => {
  it('renders the default: host selected, the unsandboxed warning visible, and the seeded global hints', async () => {
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    expect(selectedLabel('runner')).toBe('Host — unsandboxed, full host access');
    expect(container.textContent).toContain(
      'Host runs are unsandboxed — the agent has full host access to the server. Break-glass only; use the container runner once available.',
    );
    expect(container.textContent).toContain('Inherit global default — currently 8g');
    expect(container.textContent).toContain('Inherit global default — currently 4096');
    expect(container.textContent).toContain('Inherit global default — currently 16384');
    expect(input('container_memory').value).toBe('');
    expect(input('container_pids').value).toBe('');
    expect(input('container_nofile').value).toBe('');
  });

  it('choosing Container hides the warning and PATCHes runner', async () => {
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    await chooseFromSelect('runner', 'Container — rootless podman');
    expect(container.textContent).not.toContain('Host runs are unsandboxed');

    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ runner: 'container' }]);
    expect(h.repoOnServer.runner).toBe('container');
  });

  it('setting container limits PATCHes all three fields', async () => {
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    typeInto(input('container_memory'), '512m');
    typeInto(input('container_pids'), '2048');
    typeInto(input('container_nofile'), '8192');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([
      { container_memory: '512m', container_pids: 2048, container_nofile: 8192 },
    ]);
    expect(h.repoOnServer.container_memory).toBe('512m');
    expect(h.repoOnServer.container_pids).toBe(2048);
    expect(h.repoOnServer.container_nofile).toBe(8192);
  });

  it('clearing a stored limit back to inherit PATCHes null', async () => {
    h.repoOnServer = {
      ...baseRepo(),
      runner: 'container',
      container_memory: '4g',
      container_pids: 2048,
      container_nofile: 8192,
    };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    expect(input('container_memory').value).toBe('4g');
    expect(input('container_pids').value).toBe('2048');
    expect(input('container_nofile').value).toBe('8192');

    typeInto(input('container_memory'), '');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ container_memory: null }]);
    expect(h.repoOnServer.container_memory).toBeNull();
    // The untouched overrides are not re-sent.
    expect(h.repoOnServer.container_pids).toBe(2048);
    expect(h.repoOnServer.container_nofile).toBe(8192);
  });

  it('clearing the pids and nofile limits PATCHes both back to null', async () => {
    h.repoOnServer = {
      ...baseRepo(),
      runner: 'container',
      container_pids: 2048,
      container_nofile: 8192,
    };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    typeInto(input('container_pids'), '');
    typeInto(input('container_nofile'), '');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ container_pids: null, container_nofile: null }]);
  });

  it('typing a dev image ref and saving PATCHes it alone', async () => {
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    typeInto(input('image_ref'), 'docker.io/library/debian:bookworm');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ image_ref: 'docker.io/library/debian:bookworm' }]);
    expect(h.repoOnServer.image_ref).toBe('docker.io/library/debian:bookworm');
  });

  it('clearing a seeded dev image ref PATCHes null', async () => {
    h.repoOnServer = {
      ...baseRepo(),
      runner: 'container',
      image_ref: 'docker.io/library/debian:bookworm@sha256:abc123',
    };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    expect(input('image_ref').value).toBe('docker.io/library/debian:bookworm@sha256:abc123');

    typeInto(input('image_ref'), '');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ image_ref: null }]);
    expect(h.repoOnServer.image_ref).toBeNull();
  });

  it('an untouched dev image ref is not re-sent when saving a different field', async () => {
    h.repoOnServer = {
      ...baseRepo(),
      runner: 'container',
      image_ref: 'docker.io/library/debian:bookworm',
    };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    typeInto(input('container_memory'), '512m');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ container_memory: '512m' }]);
    expect(h.repoOnServer.image_ref).toBe('docker.io/library/debian:bookworm');
  });
});

const HOST_LABEL = 'Host — unsandboxed, full host access';
const CONTAINER_LABEL = 'Container — rootless podman';
const HOST_HINT = 'Host runs are unsandboxed — the agent has full host access to the server.';
const IMAGE_ID = 'ghcr.io/cloonar/dev:1.4@sha256:0123456789abcdef';
const OTHER_IMAGE = 'docker.io/library/debian:bookworm@sha256:abc123';

const text = () => container.textContent ?? '';

/** The Dev image field's hint — the `.hint` directly under the input. */
const imageHint = () =>
  container.querySelector<HTMLElement>('input[name="image_ref"] + small.hint')?.textContent ?? '';

async function optionLabels(): Promise<(string | null | undefined)[]> {
  selectTrigger('runner').click();
  await settle();
  return optionRows().map((r) => r.querySelector('.select-option-label')?.textContent);
}

describe('RepoSettings Runner — inherit (issue #55)', () => {
  it('offers "Inherit global default — currently Container" first when the default is container', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    expect(await optionLabels()).toEqual([
      'Inherit global default — currently Container',
      CONTAINER_LABEL,
      HOST_LABEL,
    ]);
  });

  it('offers "Inherit global default — currently Host" first when the default is host', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    expect((await optionLabels())[0]).toBe('Inherit global default — currently Host');
  });

  it('drops the "currently …" suffix rather than guess when the default is not a runner', async () => {
    delete h.settingsOnServer.runner_default;
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    expect((await optionLabels())[0]).toBe('Inherit global default');
  });

  it('a repo with no pin shows the inherit row selected', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    expect(selectedLabel('runner')).toBe('Inherit global default — currently Container');
  });

  it('shows the host hint for an inherited host', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');
    expect(text()).toContain(HOST_HINT);
  });

  it('hides the host hint for an inherited container', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');
    expect(text()).not.toContain('Host runs are unsandboxed');
  });

  it('a pinned container hides the host hint even while the global default is host', async () => {
    h.repoOnServer = { ...baseRepo(), runner: 'container' };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');
    expect(text()).not.toContain('Host runs are unsandboxed');
  });

  it('the host hint tracks the draft: inherit under a host default shows it, Container hides it', async () => {
    h.repoOnServer = { ...baseRepo(), runner: 'container' };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');
    expect(text()).not.toContain('Host runs are unsandboxed');

    await chooseFromSelect('runner', 'Inherit global default — currently Host');
    expect(text()).toContain(HOST_HINT);

    await chooseFromSelect('runner', CONTAINER_LABEL);
    expect(text()).not.toContain('Host runs are unsandboxed');
  });

  it('picking inherit on a pinned repo PATCHes runner null', async () => {
    h.repoOnServer = { ...baseRepo(), runner: 'host' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    await chooseFromSelect('runner', 'Inherit global default — currently Host');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ runner: null }]);
    expect(h.repoOnServer.runner).toBeNull();
    // The saved state reads back as inherit.
    expect(selectedLabel('runner')).toBe('Inherit global default — currently Host');
  });

  it('picking Host on an inheriting repo PATCHes "host"', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    await chooseFromSelect('runner', HOST_LABEL);
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ runner: 'host' }]);
    expect(h.repoOnServer.runner).toBe('host');
  });

  it('picking Container on an inheriting repo PATCHes "container"', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    await chooseFromSelect('runner', CONTAINER_LABEL);
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ runner: 'container' }]);
    expect(h.repoOnServer.runner).toBe('container');
  });

  it('inherit → pin → inherit round-trips through the repo PATCH', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    await chooseFromSelect('runner', CONTAINER_LABEL);
    submitForm();
    await settle();
    await chooseFromSelect('runner', 'Inherit global default — currently Host');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ runner: 'container' }, { runner: null }]);
    expect(h.repoOnServer.runner).toBeNull();
  });

  it('an untouched inherit is not re-sent when saving another field', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    await mountRunner();
    await waitFor(() => container.querySelector('button[name="runner"]'), 'runner select');

    typeInto(input('container_memory'), '512m');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ container_memory: '512m' }]);
    expect(h.repoOnServer.runner).toBeNull();
  });
});

describe('RepoSettings Runner — dev image hint (issue #55)', () => {
  it('names the global default dev image a blank field inherits', async () => {
    h.settingsOnServer = {
      ...h.settingsOnServer,
      dev_image_default: IMAGE_ID,
      dev_image_fallback: OTHER_IMAGE,
    };
    await mountRunner();
    await waitFor(() => container.querySelector('input[name="image_ref"]'), 'image field');

    expect(imageHint()).toBe(
      `Resolved and pinned to a digest on save. Blank inherits ${IMAGE_ID}.`,
    );
    expect(container.querySelector('input[name="image_ref"] + small.hint code')?.textContent).toBe(
      IMAGE_ID,
    );
  });

  it('falls back to the server image when no global default image is set', async () => {
    h.settingsOnServer = {
      ...h.settingsOnServer,
      dev_image_default: '',
      dev_image_fallback: OTHER_IMAGE,
    };
    await mountRunner();
    await waitFor(() => container.querySelector('input[name="image_ref"]'), 'image field');

    expect(imageHint()).toBe(
      `Resolved and pinned to a digest on save. Blank inherits ${OTHER_IMAGE}.`,
    );
  });

  it('says none is configured when both are empty', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, dev_image_default: '', dev_image_fallback: '' };
    await mountRunner();
    await waitFor(() => container.querySelector('input[name="image_ref"]'), 'image field');

    expect(imageHint()).toContain(
      'No dev image is configured — container spawns are refused until one is set here, in global Settings, or on the server.',
    );
    expect(imageHint()).not.toContain('Blank inherits');
  });

  it('no longer claims a blank field inherits "the server\'s default image"', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, dev_image_fallback: OTHER_IMAGE };
    await mountRunner();
    await waitFor(() => container.querySelector('input[name="image_ref"]'), 'image field');

    expect(text()).not.toContain("Blank inherits the server's default image.");
  });
});
