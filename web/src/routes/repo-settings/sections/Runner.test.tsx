// Runner section suite (issue #205), the Autoland.test.tsx harness style
// applied to /repos/:id/settings/runner: the default host pick shows the
// unsandboxed warning, picking container hides it and PATCHes runner, and the
// three container limit overrides set/clear independently.

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
