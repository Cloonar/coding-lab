// Runner section suite (issues #205, #55, #61): the section's fields on the
// one-page settings, saved through the page's one Save.
//
// The Runner is a three-way pick — inherited (naming the Runner that resolves
// to), Container, Host — and every field of the section says "inherited" or
// "set here". What a field inherits is the (fake) server's answer: the Runner
// from the global runner default, the dev image from the global default dev
// image or the server's flag, the limits from the global limits.
//
// Only what applies (issue #61 §7): while the EFFECTIVE Runner is host — set
// here or inherited — the unsandboxed warning shows and the dev image and the
// container limits fold into one note. "Show anyway" unfolds them with their
// values intact, and a folded field unfolds by itself when it is needed.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  baseRepo,
  container,
  fieldChanged,
  fieldDefault,
  fieldError,
  fieldHint,
  fieldState,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  pageSection,
  resetButton,
  save,
  saveBarTitle,
  segment,
  segmentLabels,
  segmentValue,
  settle,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const BASE = `/repos/${REPO_ID}/settings`;
const mountRunner = async (path = `${BASE}/runner`): Promise<void> => {
  await mountSettings(path);
  await waitFor(
    () => container.querySelector('button[role="radio"][name="runner"]'),
    'runner pick',
  );
};

const HOST_HINT =
  'Host runs are unsandboxed — the agent has full host access to the server. Break-glass only; use the container runner once available.';
const FOLD_NOTE = 'Dev image and container limits apply to container runs only.';
const IMAGE_ID = 'ghcr.io/cloonar/dev:1.4@sha256:0123456789abcdef';
const OTHER_IMAGE = 'docker.io/library/debian:bookworm@sha256:abc123';
const CONTAINER_ONLY = ['image_ref', 'container_memory', 'container_pids', 'container_nofile'];

const runnerText = () => pageSection('runner').textContent ?? '';
const rendered = (name: string) => container.querySelector(`input[name="${name}"]`) !== null;
const showAnyway = (): HTMLButtonElement => {
  const el = Array.from(pageSection('runner').querySelectorAll('button')).find(
    (b) => b.textContent === 'Show anyway',
  );
  if (!el) throw new Error('missing Show anyway');
  return el;
};
/** A repo that runs in a container, so the whole section shows. */
const containerRepo = () => ({ ...baseRepo(), runner: 'container' as const });

describe('RepoSettings Runner pick', () => {
  it('is a three-way pick: inherited (naming what it resolves to), Container, Host', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountRunner();

    expect(segmentLabels('runner')).toEqual(['Inherited · Container', 'Container', 'Host']);
    // baseRepo() pins host: set here, with what it would inherit and Reset.
    expect(segmentValue('runner')).toBe('host');
    expect(fieldState('runner')).toBe('set here');
    expect(fieldDefault('runner')).toBe('Default: Container');
    expect(resetButton('runner')?.getAttribute('aria-label')).toBe('Reset Runner to inherited');
  });

  it('names an inherited host as such', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();

    expect(segmentLabels('runner')[0]).toBe('Inherited · Host');
    expect(segmentValue('runner')).toBe('');
    expect(fieldState('runner')).toBe('inherited');
    expect(resetButton('runner')).toBeNull();
  });

  it('shows the state without a value when the default cannot be resolved', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    delete h.settingsOnServer.runner_default; // the server answers null
    await mountRunner();

    expect(segmentLabels('runner')).toEqual(['Inherited', 'Container', 'Host']);
    expect(fieldState('runner')).toBe('inherited');
    // Not known to be host: no warning, nothing folded.
    expect(runnerText()).not.toContain('Host runs are unsandboxed');
    expect(rendered('image_ref')).toBe(true);
  });

  it('choosing Container PATCHes runner and hides the host warning', async () => {
    await mountRunner();
    expect(runnerText()).toContain(HOST_HINT);

    segment('runner', 'container').click();
    await settle();
    expect(runnerText()).not.toContain('Host runs are unsandboxed');
    await save();

    expect(h.patchBodies).toEqual([{ runner: 'container' }]);
    expect(h.repoOnServer.runner).toBe('container');
  });

  it('picking Host on an inheriting repo PATCHes "host"', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountRunner();

    segment('runner', 'host').click();
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ runner: 'host' }]);
    expect(h.repoOnServer.runner).toBe('host');
  });

  it('Reset on a pinned repo PATCHes runner null, and the saved state reads inherited', async () => {
    await mountRunner();

    resetButton('runner')?.click();
    await settle();
    expect(segmentValue('runner')).toBe('');
    expect(fieldState('runner')).toBe('inherited');
    expect(fieldChanged('runner')).toBe(true);
    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();

    expect(h.patchBodies).toEqual([{ runner: null }]);
    expect(h.repoOnServer.runner).toBeNull();
    expect(segmentValue('runner')).toBe('');
    expect(segmentLabels('runner')[0]).toBe('Inherited · Host');
  });

  it('inherit → pin → inherit round-trips through the repo PATCH', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    await mountRunner();

    segment('runner', 'container').click();
    await settle();
    await save();
    segment('runner', '').click();
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ runner: 'container' }, { runner: null }]);
    expect(h.repoOnServer.runner).toBeNull();
  });

  it('an untouched inherit is not re-sent when saving another field', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountRunner();

    typeInto(input('container_memory'), '512m');
    await save();

    expect(h.patchBodies).toEqual([{ container_memory: '512m' }]);
    expect(h.repoOnServer.runner).toBeNull();
  });
});

describe('RepoSettings Runner: the host warning follows the effective Runner', () => {
  it('shows for a pinned host', async () => {
    await mountRunner();
    expect(runnerText()).toContain(HOST_HINT);
  });

  it('shows for an inherited host', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();
    expect(runnerText()).toContain(HOST_HINT);
  });

  it('is absent for an inherited container', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
    await mountRunner();
    expect(runnerText()).not.toContain('Host runs are unsandboxed');
  });

  it('is absent for a pinned container even while the global default is host', async () => {
    h.repoOnServer = containerRepo();
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();
    expect(runnerText()).not.toContain('Host runs are unsandboxed');
  });

  it('tracks the draft: inherit under a host default shows it, Container hides it', async () => {
    h.repoOnServer = containerRepo();
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();
    expect(runnerText()).not.toContain('Host runs are unsandboxed');

    segment('runner', '').click();
    await settle();
    expect(runnerText()).toContain(HOST_HINT);

    segment('runner', 'container').click();
    await settle();
    expect(runnerText()).not.toContain('Host runs are unsandboxed');
  });
});

describe('RepoSettings Runner: on host, the container-only fields fold', () => {
  it('folds the dev image and the limits into one note', async () => {
    await mountRunner();

    for (const name of CONTAINER_ONLY) expect(rendered(name)).toBe(false);
    expect(runnerText()).toContain(FOLD_NOTE);
    expect(showAnyway().textContent).toBe('Show anyway');
    // Nothing is pending because of it.
    expect(container.querySelector('.settings-savebar')).toBeNull();
  });

  it('folds for an inherited host too', async () => {
    h.repoOnServer = { ...baseRepo(), runner: null };
    h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'host' };
    await mountRunner();

    for (const name of CONTAINER_ONLY) expect(rendered(name)).toBe(false);
    expect(runnerText()).toContain(FOLD_NOTE);
  });

  it('does not fold on the container Runner', async () => {
    h.repoOnServer = containerRepo();
    await mountRunner();

    for (const name of CONTAINER_ONLY) expect(rendered(name)).toBe(true);
    expect(runnerText()).not.toContain(FOLD_NOTE);
  });

  it('Show anyway reveals them with their values intact, and takes the focus there', async () => {
    h.repoOnServer = {
      ...baseRepo(),
      image_ref: OTHER_IMAGE,
      container_memory: '4g',
      container_pids: 2048,
    };
    await mountRunner();
    expect(rendered('image_ref')).toBe(false);

    showAnyway().click();
    await settle();

    expect(input('image_ref').value).toBe(OTHER_IMAGE);
    expect(input('container_memory').value).toBe('4g');
    expect(input('container_pids').value).toBe('2048');
    expect(input('container_nofile').value).toBe('');
    expect(fieldState('image_ref')).toBe('set here');
    expect(fieldState('container_nofile')).toBe('inherited');
    // The action is gone; the note that they do not apply stays.
    expect(pageSection('runner').querySelector('.settings-na')).toBeNull();
    expect(runnerText()).toContain(FOLD_NOTE);
    expect(document.activeElement).toBe(input('image_ref'));
    // Unfolding changed nothing.
    expect(container.querySelector('.settings-savebar')).toBeNull();
  });

  it('staging a value on host is legal: it is saved, and the field stays in reach', async () => {
    await mountRunner();
    showAnyway().click();
    await settle();

    typeInto(input('container_memory'), '512m');
    await save();

    expect(h.patchBodies).toEqual([{ container_memory: '512m' }]);
    expect(h.repoOnServer.runner).toBe('host');
    expect(input('container_memory').value).toBe('512m');
  });

  it('switching to host folds the fields without touching their values or the PATCH', async () => {
    h.repoOnServer = { ...containerRepo(), container_memory: '4g', image_ref: OTHER_IMAGE };
    await mountRunner();
    expect(input('container_memory').value).toBe('4g');

    segment('runner', 'host').click();
    await settle();
    expect(rendered('container_memory')).toBe(false);
    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();

    // Only the Runner: folding cleared nothing.
    expect(h.patchBodies).toEqual([{ runner: 'host' }]);
    expect(h.repoOnServer.container_memory).toBe('4g');
    expect(h.repoOnServer.image_ref).toBe(OTHER_IMAGE);
  });

  it('a field with a pending change is never folded away', async () => {
    h.repoOnServer = containerRepo();
    await mountRunner();

    typeInto(input('container_pids'), '2048');
    segment('runner', 'host').click();
    await settle();

    // Host now, but the pending limit stays on the page — and in the count.
    expect(input('container_pids').value).toBe('2048');
    expect(fieldChanged('container_pids')).toBe(true);
    expect(saveBarTitle()).toBe('2 unsaved changes');
    await save();
    expect(h.patchBodies).toEqual([{ runner: 'host', container_pids: 2048 }]);
  });

  it('a problem in a folded field still blocks Save, which unfolds and focuses it', async () => {
    h.repoOnServer = containerRepo();
    await mountRunner();

    typeInto(input('container_nofile'), '0');
    segment('runner', 'host').click();
    await settle();
    await save();

    expect(h.patchBodies).toEqual([]);
    expect(fieldError('container_nofile')).toBe(
      'Use a whole number, 1 or more, or leave it empty.',
    );
    expect(document.activeElement).toBe(input('container_nofile'));
  });

  it('a refusal that names a container-only field on host lands at that field', async () => {
    await mountRunner();
    showAnyway().click();
    await settle();
    typeInto(input('image_ref'), 'nope:latest');
    await settle();
    h.patchRefusal = { error: 'image_ref: manifest unknown', field: 'image_ref' };
    await save();

    expect(fieldError('image_ref')).toBe('image_ref: manifest unknown');
    expect(document.activeElement).toBe(input('image_ref'));
  });

  it('?field=image_ref on a host repo unfolds the field and focuses it', async () => {
    await mountRunner(`${BASE}/runner?field=image_ref`);
    await waitFor(() => container.querySelector('input[name="image_ref"]'), 'the dev image');
    await settle();

    expect(document.activeElement).toBe(input('image_ref'));
    expect(h.scrolls.at(-1)?.target).toBe('image_ref');
    // The rest of the fold came along, and nothing changed.
    expect(rendered('container_memory')).toBe(true);
    expect(container.querySelector('.settings-savebar')).toBeNull();
  });
});

describe('RepoSettings Runner: container limits', () => {
  it('shows the limits a blank field inherits as placeholders', async () => {
    h.repoOnServer = containerRepo();
    await mountRunner();

    expect(input('container_memory').value).toBe('');
    expect(input('container_memory').placeholder).toBe('8g');
    expect(input('container_pids').placeholder).toBe('4096');
    expect(input('container_nofile').placeholder).toBe('16384');
    for (const name of ['container_memory', 'container_pids', 'container_nofile']) {
      expect(fieldState(name)).toBe('inherited');
    }
  });

  it('setting container limits PATCHes all three fields', async () => {
    h.repoOnServer = containerRepo();
    await mountRunner();

    typeInto(input('container_memory'), '512m');
    typeInto(input('container_pids'), '2048');
    typeInto(input('container_nofile'), '8192');
    await settle();
    expect(fieldState('container_memory')).toBe('set here');
    expect(fieldDefault('container_memory')).toBe('Default: 8g');
    await save();

    expect(h.patchBodies).toEqual([
      { container_memory: '512m', container_pids: 2048, container_nofile: 8192 },
    ]);
    expect(h.repoOnServer.container_memory).toBe('512m');
    expect(h.repoOnServer.container_pids).toBe(2048);
    expect(h.repoOnServer.container_nofile).toBe(8192);
  });

  it('clearing a stored limit back to inherit PATCHes null', async () => {
    h.repoOnServer = {
      ...containerRepo(),
      container_memory: '4g',
      container_pids: 2048,
      container_nofile: 8192,
    };
    await mountRunner();

    expect(input('container_memory').value).toBe('4g');
    expect(input('container_pids').value).toBe('2048');
    expect(input('container_nofile').value).toBe('8192');

    typeInto(input('container_memory'), '');
    await save();

    expect(h.patchBodies).toEqual([{ container_memory: null }]);
    expect(h.repoOnServer.container_memory).toBeNull();
    // The untouched overrides are not re-sent.
    expect(h.repoOnServer.container_pids).toBe(2048);
    expect(h.repoOnServer.container_nofile).toBe(8192);
  });

  it('Reset on the pids and nofile limits PATCHes both back to null', async () => {
    h.repoOnServer = { ...containerRepo(), container_pids: 2048, container_nofile: 8192 };
    await mountRunner();
    expect(resetButton('container_pids')?.getAttribute('aria-label')).toBe(
      'Reset Processes (Container limits) to inherited',
    );

    resetButton('container_pids')?.click();
    resetButton('container_nofile')?.click();
    await settle();
    expect(input('container_pids').value).toBe('');
    expect(input('container_pids').placeholder).toBe('4096');
    await save();

    expect(h.patchBodies).toEqual([{ container_pids: null, container_nofile: null }]);
  });
});

describe('RepoSettings Runner: dev image', () => {
  it('typing a dev image ref and saving PATCHes it alone', async () => {
    h.repoOnServer = containerRepo();
    await mountRunner();

    typeInto(input('image_ref'), 'docker.io/library/debian:bookworm');
    await save();

    expect(h.patchBodies).toEqual([{ image_ref: 'docker.io/library/debian:bookworm' }]);
    expect(h.repoOnServer.image_ref).toBe('docker.io/library/debian:bookworm');
  });

  it('clearing a seeded dev image ref PATCHes null', async () => {
    h.repoOnServer = { ...containerRepo(), image_ref: OTHER_IMAGE };
    await mountRunner();
    expect(input('image_ref').value).toBe(OTHER_IMAGE);

    typeInto(input('image_ref'), '');
    await save();

    expect(h.patchBodies).toEqual([{ image_ref: null }]);
    expect(h.repoOnServer.image_ref).toBeNull();
  });

  it('an untouched dev image ref is not re-sent when saving a different field', async () => {
    h.repoOnServer = { ...containerRepo(), image_ref: 'docker.io/library/debian:bookworm' };
    await mountRunner();

    typeInto(input('container_memory'), '512m');
    await save();

    expect(h.patchBodies).toEqual([{ container_memory: '512m' }]);
    expect(h.repoOnServer.image_ref).toBe('docker.io/library/debian:bookworm');
  });

  it('names the global default dev image a blank field inherits', async () => {
    h.repoOnServer = containerRepo();
    h.settingsOnServer = {
      ...h.settingsOnServer,
      dev_image_default: IMAGE_ID,
      dev_image_fallback: OTHER_IMAGE,
    };
    await mountRunner();

    expect(input('image_ref').placeholder).toBe(IMAGE_ID);
    expect(fieldState('image_ref')).toBe('inherited');
    // In full under the field too: the placeholder cuts a pinned ref short.
    expect(fieldHint('image_ref')).toBe(
      `Resolved and pinned to a digest on save. Inherits ${IMAGE_ID}.`,
    );
    expect(container.querySelector('[data-field="image_ref"] .sfield-hint code')?.textContent).toBe(
      IMAGE_ID,
    );
    // The state and the hint describe the input.
    expect(input('image_ref').getAttribute('aria-describedby')).toBe(
      'rs-image_ref-state rs-image_ref-hint',
    );
  });

  it('falls back to the server image when no global default image is set', async () => {
    h.repoOnServer = containerRepo();
    h.settingsOnServer = {
      ...h.settingsOnServer,
      dev_image_default: '',
      dev_image_fallback: OTHER_IMAGE,
    };
    await mountRunner();

    expect(input('image_ref').placeholder).toBe(OTHER_IMAGE);
    expect(fieldHint('image_ref')).toBe(
      `Resolved and pinned to a digest on save. Inherits ${OTHER_IMAGE}.`,
    );
  });

  it('says none is configured when nothing below the repo sets one', async () => {
    h.repoOnServer = containerRepo();
    h.settingsOnServer = { ...h.settingsOnServer, dev_image_default: '', dev_image_fallback: '' };
    await mountRunner();

    expect(fieldHint('image_ref')).toContain(
      'No dev image is configured — container spawns are refused until one is set here, in global Settings, or on the server.',
    );
    expect(fieldHint('image_ref')).not.toContain('Inherits');
    expect(input('image_ref').placeholder).toBe('none configured');
  });

  it('a repo with its own image shows what it would inherit, and no warning', async () => {
    h.repoOnServer = { ...containerRepo(), image_ref: OTHER_IMAGE };
    h.settingsOnServer = { ...h.settingsOnServer, dev_image_default: '', dev_image_fallback: '' };
    await mountRunner();

    expect(fieldState('image_ref')).toBe('set here');
    expect(fieldDefault('image_ref')).toBe('Default: none configured');
    expect(fieldHint('image_ref')).toBe('Resolved and pinned to a digest on save.');
  });
});
