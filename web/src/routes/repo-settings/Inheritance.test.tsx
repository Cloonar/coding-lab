// Inheritance on the one-page repo settings (issue #61 §6), through the real
// page: every overridable field says "inherited" or "set here" at its label;
// an inherited field shows what it resolves to (the first pick of a select or
// a three-way control, the placeholder of a text or number field); a field
// set here shows "Default: <what it would inherit>" and Reset, which returns
// it to inherited as a change saved as null.
//
// The values come from the server alone (POST /repos/:id/inherited — here the
// harness's fake of it): asked when the Settings tab mounts, again when the
// saved repo changes and, debounced, when a draft that other fields' chains
// read changes — with exactly the edited chain drafts. An answer that is not
// the latest request's is dropped; a failed request leaves every field
// editable and saveable, with its state but no value.
//
// The fields are driven from the field table, so a new overridable field
// cannot be forgotten here.

import { beforeEach, describe, expect, it } from 'vitest';
import type { Repo } from '../../api';
import { OVERRIDABLE_FIELD_KEYS, REPO_FIELD_KEYS, isOverridable, repoField } from './fields';
import {
  CODEX,
  REPO_ID,
  baseProviders,
  baseRepo,
  baseSchedule,
  chooseFromSelect,
  container,
  emitRepoChanged,
  fieldChanged,
  fieldDefault,
  fieldState,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  optionRows,
  resetButton,
  save,
  saveBar,
  saveBarTitle,
  scheduleEditor,
  segment,
  segmentLabels,
  segmentValue,
  selectTrigger,
  selectedLabel,
  settle,
  settleInherited,
  textarea,
  typeInto,
  waitFor,
} from './harness';

installRepoSettingsHooks();
// A repo on the container Runner with Autoland on: nothing is folded away, so
// every overridable field is on the page.
beforeEach(() => {
  h.repoOnServer = { ...baseRepo(), runner: null, autoland_enabled: true };
  h.settingsOnServer = { ...h.settingsOnServer, runner_default: 'container' };
});

const BASE = `/repos/${REPO_ID}/settings`;
/** The page's notice about the inherited values (not an error banner). */
const notice = (): HTMLElement | null =>
  container.querySelector<HTMLElement>('.settings-page .banner.notice');
const tryAgain = (root: HTMLElement | null): HTMLButtonElement | undefined =>
  Array.from(root?.querySelectorAll('button') ?? []).find((b) => b.textContent === 'Try again');
const mount = async (): Promise<void> => {
  await mountSettings(BASE);
  await waitFor(() => container.querySelector('input[name="afk_options.ultracode"]'), 'the page');
};

// How each overridable field shows what it inherits.
const SELECTS = [
  'provider',
  'model_default',
  'effort_default',
  'afk_provider_default',
  'afk_model_default',
  'afk_effort_default',
  'lander_provider',
  'lander_model',
  'lander_effort',
];
const THREE_WAY = ['remote_default', 'afk_remote_default', 'runner'];
const INPUTS = [
  'budget_minutes',
  'max_instances_override',
  'image_ref',
  'container_memory',
  'container_pids',
  'container_nofile',
  'git_author_name',
  'git_author_email',
];
const OTHER = ['afk_options', 'afk_prompt'];

/** What the page shows as the value `key` inherits ('' = none shown). */
function shownInherited(key: string): string {
  if (SELECTS.includes(key)) return selectedLabel(key);
  if (THREE_WAY.includes(key)) return segmentLabels(key)[0] ?? '';
  if (INPUTS.includes(key)) return input(key).placeholder;
  if (key === 'afk_prompt') return textarea(key).placeholder;
  throw new Error(`no inherited display known for ${key}`);
}

/** A repo with a value of its own in every overridable field. */
function pinnedRepo(): Repo {
  return {
    ...baseRepo(),
    autoland_enabled: true,
    provider: 'claude-code',
    model_default: 'sonnet',
    effort_default: 'high',
    remote_default: true,
    afk_provider_default: 'claude-code',
    afk_model_default: 'sonnet',
    afk_effort_default: 'high',
    afk_remote_default: false,
    afk_options: { ultracode: 'true' },
    afk_prompt: 'Open a PR when done.',
    budget_minutes: 45,
    max_instances_override: 3,
    runner: 'container',
    image_ref: 'docker.io/library/debian:bookworm@sha256:abc',
    container_memory: '4g',
    container_pids: 2048,
    container_nofile: 8192,
    lander_provider: 'claude-code',
    lander_model: 'sonnet',
    lander_effort: 'high',
    git_author_name: 'Dominik',
    git_author_email: 'dominik@example.com',
  };
}

describe('every overridable field says inherited or set here', () => {
  it('this suite knows how each overridable field shows what it inherits', () => {
    expect([...SELECTS, ...THREE_WAY, ...INPUTS, ...OTHER].sort()).toEqual(
      [...OVERRIDABLE_FIELD_KEYS].sort(),
    );
  });

  it('reads "inherited" on a repo that sets nothing itself — and nothing on any other field', async () => {
    await mount();

    for (const key of REPO_FIELD_KEYS) {
      expect([key, fieldState(key)]).toEqual([key, isOverridable(key) ? 'inherited' : null]);
    }
    // Inherited: nothing to reset, nothing pending.
    expect(container.querySelector('.sfield-default')).toBeNull();
    expect(saveBar()).toBeNull();
  });

  it('reads "set here" with Default and Reset on a repo that pins everything', async () => {
    h.repoOnServer = pinnedRepo();
    await mount();

    for (const key of OVERRIDABLE_FIELD_KEYS) {
      expect([key, fieldState(key)]).toEqual([key, 'set here']);
      const reset = resetButton(key);
      expect(reset?.textContent).toBe('Reset');
      // Its accessible name says which field it resets.
      expect(reset?.getAttribute('aria-label')).toContain(`Reset ${repoField(key).label}`);
      expect(reset?.getAttribute('aria-label')).toMatch(/ to inherited$/);
      expect(fieldDefault(key)).toMatch(/^Default: .+/);
    }
    expect(saveBar()).toBeNull();
  });

  it('the state is in words, and describes the control', async () => {
    await mount();

    const chip = container.querySelector('#rs-model_default-state');
    expect(chip?.textContent).toBe('inherited');
    expect(selectTrigger('model_default').getAttribute('aria-describedby')).toContain(
      'rs-model_default-state',
    );
    expect(input('budget_minutes').getAttribute('aria-describedby')).toContain(
      'rs-budget_minutes-state',
    );
  });
});

describe('an inherited field shows the value it resolves to', () => {
  it('as the first pick of a select, the inherit segment, or the placeholder', async () => {
    h.settingsOnServer = {
      ...h.settingsOnServer,
      spawn_remote_default: true,
      git_author_name: 'lab-bot',
      git_author_email: 'lab-bot@example.com',
      dev_image_default: 'ghcr.io/cloonar/dev:1.4@sha256:0123',
      afk_budget_minutes: 90,
      max_instances: 4,
    };
    await mount();
    await waitFor(() => (input('budget_minutes').placeholder === '90' ? true : null), 'values');

    const shown = Object.fromEntries(
      OVERRIDABLE_FIELD_KEYS.filter((key) => key !== 'afk_options').map((key) => [
        key,
        shownInherited(key),
      ]),
    );
    expect(shown).toEqual({
      provider: 'Inherited · Claude Code',
      model_default: 'Inherited · Opus (1M)',
      effort_default: 'Inherited · high',
      remote_default: 'Inherited · on',
      afk_provider_default: 'Inherited · Claude Code',
      afk_model_default: 'Inherited · Opus (1M)',
      afk_effort_default: 'Inherited · high',
      afk_remote_default: 'Inherited · on',
      afk_prompt: h.repoOnServer.afk_prompt_effective,
      budget_minutes: '90',
      max_instances_override: '4',
      runner: 'Inherited · Container',
      image_ref: 'ghcr.io/cloonar/dev:1.4@sha256:0123',
      container_memory: '8g',
      container_pids: '4096',
      container_nofile: '16384',
      lander_provider: 'Inherited · Claude Code',
      lander_model: 'Inherited · Opus (1M)',
      lander_effort: 'Inherited · high',
      git_author_name: 'lab-bot',
      git_author_email: 'lab-bot@example.com',
    });
    // The inherit entry is the FIRST pick of the open select, and selected.
    selectTrigger('model_default').click();
    await settle();
    expect(optionRows()[0]?.textContent).toBe('Inherited · Opus (1M)');
    expect(optionRows()[0]?.getAttribute('aria-selected')).toBe('true');
    // The three-way picks rest on their inherit segment.
    for (const key of THREE_WAY) expect(segmentValue(key)).toBe('');
  });

  it("words an empty model as the agent's own default", async () => {
    h.inheritedPatch = { model_default: '', effort_default: '' };
    await mount();

    expect(selectedLabel('model_default')).toBe('Inherited · agent default');
    expect(selectedLabel('effort_default')).toBe('Inherited · agent default');
  });

  it('shows an id the catalog does not carry as it is', async () => {
    h.inheritedPatch = { model_default: 'brand-new-model' };
    await mount();

    expect(selectedLabel('model_default')).toBe('Inherited · brand-new-model');
  });

  it('shows the state without a value for an entry the server could not resolve', async () => {
    h.inheritedPatch = {
      model_default: null,
      remote_default: null,
      budget_minutes: null,
      runner: null,
    };
    await mount();

    expect(selectedLabel('model_default')).toBe('Inherited');
    expect(segmentLabels('remote_default')).toEqual(['Inherited', 'On', 'Off']);
    expect(segmentLabels('runner')[0]).toBe('Inherited');
    expect(input('budget_minutes').placeholder).toBe('');
    for (const key of ['model_default', 'remote_default', 'budget_minutes', 'runner']) {
      expect(fieldState(key)).toBe('inherited');
    }
    // The fields that did resolve are unaffected.
    expect(selectedLabel('effort_default')).toBe('Inherited · high');
  });

  it('offers no catalog for a run class whose agent could not be resolved', async () => {
    h.inheritedPatch = { afk_provider_default: null };
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('button[name="afk_model_default"]'), 'the page');
    await settle();

    expect(selectedLabel('afk_provider_default')).toBe('Inherited');
    // No provider to ask for models or options: only the inherit entry, and
    // no option bag. Nothing is guessed from the other run classes.
    selectTrigger('afk_model_default').click();
    await settle();
    expect(optionRows()).toHaveLength(1);
    selectTrigger('afk_model_default').click();
    expect(container.querySelector('input[name="afk_options.ultracode"]')).toBeNull();
    // Runs you start resolve as ever.
    selectTrigger('model_default').click();
    await settle();
    expect(optionRows().map((row) => row.textContent)).toContain('Sonnet');
  });
});

describe('editing and Reset', () => {
  it('editing an inherited field flips it to "set here" with Default and Reset', async () => {
    await mount();

    await chooseFromSelect('model_default', 'Sonnet');
    typeInto(input('budget_minutes'), '45');
    segment('runner', 'host').click();
    await settle();

    expect(fieldState('model_default')).toBe('set here');
    expect(fieldDefault('model_default')).toBe('Default: Opus (1M)');
    expect(fieldState('budget_minutes')).toBe('set here');
    expect(fieldDefault('budget_minutes')).toBe('Default: 120');
    expect(fieldState('runner')).toBe('set here');
    expect(fieldDefault('runner')).toBe('Default: Container');
    for (const key of ['model_default', 'budget_minutes', 'runner']) {
      expect(resetButton(key)).not.toBeNull();
      expect(fieldChanged(key)).toBe(true);
    }
    // An untouched neighbour still inherits.
    expect(fieldState('effort_default')).toBe('inherited');
    expect(resetButton('effort_default')).toBeNull();
  });

  it('Reset on an edit that was never saved simply takes it back', async () => {
    await mount();
    await chooseFromSelect('model_default', 'Sonnet');
    expect(saveBarTitle()).toBe('1 unsaved change');

    resetButton('model_default')?.click();
    await settle();

    expect(fieldState('model_default')).toBe('inherited');
    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');
    expect(saveBar()).toBeNull();
  });

  it('Reset returns every pinned field to inherited: counted in the save bar, saved as null', async () => {
    h.repoOnServer = pinnedRepo();
    await mount();

    for (const key of OVERRIDABLE_FIELD_KEYS) {
      const reset = resetButton(key);
      if (reset === null) throw new Error(`no Reset for ${key}`);
      reset.click();
      await settle();
      expect([key, fieldState(key)]).toEqual([key, 'inherited']);
      expect([key, fieldChanged(key)]).toEqual([key, true]);
      expect([key, resetButton(key)]).toEqual([key, null]);
    }
    expect(saveBarTitle()).toBe(`${OVERRIDABLE_FIELD_KEYS.length} unsaved changes`);

    await save();

    // One PATCH: a null override for each of them, and nothing else.
    expect(h.patchBodies).toEqual([
      Object.fromEntries(OVERRIDABLE_FIELD_KEYS.map((key) => [key, null])),
    ]);
    expect(saveBar()).toBeNull();
    for (const key of OVERRIDABLE_FIELD_KEYS) {
      expect([key, fieldState(key)]).toEqual([key, 'inherited']);
    }
  });

  it('a field that is not overridable has no state and no Reset', async () => {
    await mount();
    typeInto(input('name'), 'lab-core');
    await settle();

    expect(fieldChanged('name')).toBe(true);
    expect(fieldState('name')).toBeNull();
    expect(resetButton('name')).toBeNull();
  });
});

describe('the inherited values come from the server', () => {
  it('asks when the Settings tab mounts, about the saved repo', async () => {
    await mount();

    expect(h.inheritedBodies).toEqual([{}]);
  });

  it('asks again when a saved value that chains read changes', async () => {
    await mount();
    expect(selectedLabel('afk_model_default')).toBe('Inherited · Opus (1M)');

    // The repo's model was changed elsewhere: the AFK model inherits it.
    h.repoOnServer = { ...h.repoOnServer, model_default: 'sonnet' };
    emitRepoChanged();
    await settle();

    expect(h.inheritedBodies).toEqual([{}, {}]);
    expect(selectedLabel('afk_model_default')).toBe('Inherited · Sonnet');
  });

  it('asks nothing for a refresh that changes no value a chain reads', async () => {
    await mount();

    // The frame refetches the repo for every run and issue event; a counter
    // moved, the Auto switch was flipped on Overview — no answer depends on it.
    emitRepoChanged();
    await settle();
    h.repoOnServer = { ...h.repoOnServer, afk_auto_enabled: true, budget_minutes: 45 };
    emitRepoChanged();
    await settle();

    expect(input('budget_minutes').value).toBe('45'); // the refresh did land
    expect(h.inheritedBodies).toEqual([{}]);
  });

  it('a drafted lander model is sent along, and the inherited lander effort follows it', async () => {
    // Sonnet has an effort list of its own; the provider-level one is the union.
    h.providersOnServer = [
      {
        ...baseProviders()[0]!,
        models: [
          { value: 'opus[1m]', label: 'Opus (1M)', efforts: [] },
          { value: 'sonnet', label: 'Sonnet', efforts: [{ value: 'low', label: 'low' }] },
        ],
        efforts: [
          { value: 'high', label: 'high' },
          { value: 'low', label: 'low' },
        ],
      },
    ];
    await mount();
    expect(selectedLabel('lander_effort')).toBe('Inherited · high');

    await chooseFromSelect('lander_model', 'Sonnet');
    await settleInherited();

    expect(h.inheritedBodies.at(-1)).toEqual({ lander_model: 'sonnet' });
    expect(selectedLabel('lander_effort')).toBe('Inherited · low');
    expect(h.patchBodies).toEqual([]);
  });

  it('a drafted Agent is sent along; the Model pick and its catalog follow the answer', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    await mount();
    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');

    await chooseFromSelect('provider', 'Codex');
    // The catalog is the drafted agent's at once…
    selectTrigger('model_default').click();
    await settle();
    expect(optionRows().map((row) => row.textContent)).toContain('GPT-5 Codex');
    expect(optionRows().map((row) => row.textContent)).not.toContain('Sonnet');
    selectTrigger('model_default').click();
    // …and nothing was asked yet: the request waits out the debounce.
    expect(h.inheritedBodies).toEqual([{}]);

    await settleInherited();

    expect(h.inheritedBodies).toEqual([{}, { provider: 'codex' }]);
    expect(selectedLabel('model_default')).toBe('Inherited · GPT-5 Codex');
    expect(selectedLabel('effort_default')).toBe('Inherited · medium');
    // Fields whose chains read the agent follow too: AFK and the lander
    // inherit the drafted agent.
    expect(selectedLabel('afk_provider_default')).toBe('Inherited · Codex');
    expect(selectedLabel('afk_model_default')).toBe('Inherited · GPT-5 Codex');
    expect(selectedLabel('lander_provider')).toBe('Inherited · Codex');
    // Nothing was saved for any of it.
    expect(h.patchBodies).toEqual([]);
  });

  it('sends only the chain drafts that are edited: a value, false, or null for a reset', async () => {
    h.repoOnServer = { ...h.repoOnServer, afk_model_default: 'sonnet', budget_minutes: 45 };
    await mount();

    await chooseFromSelect('effort_default', 'high'); // a value
    segment('remote_default', 'false').click(); // an explicit off
    resetButton('afk_model_default')?.click(); // back to inherit
    typeInto(input('budget_minutes'), '90'); // no other field's chain reads this
    await settleInherited();

    expect(h.inheritedBodies.at(-1)).toEqual({
      effort_default: 'high',
      remote_default: false,
      afk_model_default: null,
    });
  });

  it('a quick run of picks is one request, and an edit of an unrelated field is none', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    await mount();

    await chooseFromSelect('provider', 'Codex');
    await chooseFromSelect('afk_provider_default', 'Claude Code');
    await settleInherited();
    expect(h.inheritedBodies).toEqual([
      {},
      { provider: 'codex', afk_provider_default: 'claude-code' },
    ]);

    typeInto(input('git_author_name'), 'Dominik');
    typeInto(input('container_memory'), '4g');
    await settleInherited();
    expect(h.inheritedBodies).toHaveLength(2);
  });

  it('taking a chain edit back asks again, about the saved repo', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    await mount();
    await chooseFromSelect('provider', 'Codex');
    await settleInherited();
    expect(selectedLabel('model_default')).toBe('Inherited · GPT-5 Codex');

    resetButton('provider')?.click();
    await settleInherited();

    expect(h.inheritedBodies.at(-1)).toEqual({});
    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');
  });

  it('after a save the values are asked for the saved repo, with no drafts', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    await mount();
    await chooseFromSelect('provider', 'Codex');
    await settleInherited();

    await save();
    await settleInherited();

    expect(h.patchBodies).toEqual([{ provider: 'codex' }]);
    expect(h.inheritedBodies.at(-1)).toEqual({});
    expect(selectedLabel('model_default')).toBe('Inherited · GPT-5 Codex');
    expect(fieldDefault('provider')).toBe('Default: Claude Code');
  });
});

describe('a stale answer', () => {
  it('from an older request that finishes late never overwrites a newer one', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    await mount();

    // Request 2 (the Codex draft) is held; request 3 (back to inherit) is not.
    let releaseOld = (): void => {};
    h.inheritedGate = (request) =>
      request === 2 ? new Promise<void>((resolve) => (releaseOld = resolve)) : undefined;

    await chooseFromSelect('provider', 'Codex');
    await settleInherited();
    expect(h.inheritedBodies).toHaveLength(2);

    resetButton('provider')?.click();
    await settleInherited();
    expect(h.inheritedBodies).toHaveLength(3);
    expect(h.inheritedBodies[2]).toEqual({});
    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');

    // The older request answers at last — for a draft that is gone.
    releaseOld();
    await settle();

    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');
    expect(selectedLabel('afk_provider_default')).toBe('Inherited · Claude Code');
  });
});

describe('while the inherited values are not there', () => {
  const NO_VALUES: Record<string, string> = {
    model_default: 'Inherited',
    remote_default: 'Inherited',
    runner: 'Inherited',
    budget_minutes: '',
    container_memory: '',
    git_author_name: '',
  };

  it('loading: every field renders with its state but no value, and is editable', async () => {
    let release = (): void => {};
    h.inheritedGate = () => new Promise<void>((resolve) => (release = resolve));
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="budget_minutes"]'), 'the page');

    for (const [key, shown] of Object.entries(NO_VALUES)) {
      expect([key, shownInherited(key)]).toEqual([key, shown]);
      expect([key, fieldState(key)]).toEqual([key, 'inherited']);
    }
    typeInto(input('budget_minutes'), '45');
    await settle();
    expect(fieldState('budget_minutes')).toBe('set here');
    // Set here, with Reset — but no "Default:" it could only guess at.
    expect(fieldDefault('budget_minutes')).toBeNull();
    expect(resetButton('budget_minutes')).not.toBeNull();

    release();
    await settle();
    expect(fieldDefault('budget_minutes')).toBe('Default: 120');
    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');
  });

  it('a failed request leaves the fields editable and saveable, with no guessed value', async () => {
    h.inheritedError = 'inherited: store unavailable';
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="budget_minutes"]'), 'the page');
    await settle();

    for (const [key, shown] of Object.entries(NO_VALUES)) {
      expect([key, shownInherited(key)]).toEqual([key, shown]);
      expect([key, fieldState(key)]).toEqual([key, 'inherited']);
    }
    // The page says so, with a way to try again — as a notice: no error takes
    // over the page, and nothing is pending because of it.
    expect(notice()?.textContent).toContain(
      'The inherited values could not be loaded. inherited: store unavailable',
    );
    expect(notice()?.getAttribute('role')).toBe('status');
    expect(container.querySelector('.settings-page .banner.error')).toBeNull();
    expect(saveBar()).toBeNull();

    typeInto(input('budget_minutes'), '45');
    typeInto(input('git_author_name'), 'Dominik');
    segment('remote_default', 'false').click();
    await settle();
    expect(saveBarTitle()).toBe('3 unsaved changes');
    await save();

    expect(h.patchBodies).toEqual([
      { remote_default: false, budget_minutes: 45, git_author_name: 'Dominik' },
    ]);
    expect(saveBar()).toBeNull();
  });

  it('the next trigger asks again, and the values appear', async () => {
    h.inheritedError = 'inherited: store unavailable';
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="budget_minutes"]'), 'the page');
    await settle();
    expect(selectedLabel('model_default')).toBe('Inherited');
    const asked = h.inheritedBodies.length;

    // Nothing retries on its own…
    await settleInherited();
    expect(h.inheritedBodies).toHaveLength(asked);

    // …the next refresh of the repo does.
    h.inheritedError = null;
    emitRepoChanged();
    await settle();

    expect(h.inheritedBodies).toHaveLength(asked + 1);
    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');
    expect(input('budget_minutes').placeholder).toBe('120');
    expect(notice()).toBeNull();
  });

  it('Try again in the notice asks at once', async () => {
    h.inheritedError = 'inherited: store unavailable';
    await mountSettings(BASE);
    await waitFor(notice, 'the notice');
    const asked = h.inheritedBodies.length;

    h.inheritedError = null;
    tryAgain(notice())?.click();
    await settle();

    expect(h.inheritedBodies).toHaveLength(asked + 1);
    expect(notice()).toBeNull();
    expect(selectedLabel('model_default')).toBe('Inherited · Opus (1M)');
  });

  it('a later request that fails keeps what was there: the bag, the catalogs, the folds', async () => {
    await mount();
    input('afk_options.ultracode').click();
    await settle();
    expect(saveBarTitle()).toBe('1 unsaved change');
    expect(container.querySelector('[data-field="image_ref"]')).not.toBeNull();

    // A saved value some chain reads changed, and the question about it fails.
    h.inheritedError = 'inherited: store unavailable';
    h.repoOnServer = { ...h.repoOnServer, model_default: 'sonnet' };
    emitRepoChanged();
    await settle();

    // Nothing was withdrawn: the AFK agent is still known, so the bag's boxes
    // are still there with the pending change; the Runner did not unfold; the
    // model catalog is still offered.
    expect(input('afk_options.ultracode').checked).toBe(true);
    expect(input('afk_options.ultracode').disabled).toBe(false);
    expect(saveBarTitle()).toBe('1 unsaved change');
    expect(selectedLabel('afk_provider_default')).toBe('Inherited · Claude Code');
    expect(container.querySelector('[data-field="image_ref"]')).not.toBeNull();
    selectTrigger('afk_model_default').click();
    await settle();
    expect(optionRows().map((row) => row.textContent)).toContain('Sonnet');
    selectTrigger('afk_model_default').click();
    // The page says the values may be out of date, and offers to ask again.
    expect(notice()?.textContent).toContain(
      'The inherited values could not be refreshed, so the ones shown may be out of date. inherited: store unavailable',
    );

    await save();
    expect(h.patchBodies).toEqual([{ afk_options: { ultracode: 'true' } }]);

    h.inheritedError = null;
    tryAgain(notice())?.click();
    await settle();
    expect(notice()).toBeNull();
    expect(selectedLabel('afk_model_default')).toBe('Inherited · Sonnet');
  });

  it('the drafted agent still is the effective one while its question fails', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    await mount();
    expect(selectedLabel('afk_provider_default')).toBe('Inherited · Claude Code');

    // The values were for the saved agent; the question about the drafted one fails.
    h.inheritedError = 'inherited: store unavailable';
    await chooseFromSelect('provider', 'Codex');
    await settleInherited();

    // The last good answer stands, marked as possibly out of date…
    expect(selectedLabel('afk_provider_default')).toBe('Inherited · Claude Code');
    expect(notice()?.textContent).toContain('may be out of date');
    // …and the pick the operator made here decides its own catalog.
    selectTrigger('model_default').click();
    await settle();
    expect(optionRows().map((row) => row.textContent)).toContain('GPT-5 Codex');
  });
});

describe('the option bag never starts from a guess', () => {
  const HINT = 'The inherited options are not known yet, so they cannot be changed here.';
  const bagHint = (): string | null =>
    container.querySelector('[data-field="afk_options"] .sfield-hint')?.textContent ?? null;
  // The AFK agent is set HERE, so its options are known from the catalog even
  // while nothing is known about what the repo inherits.
  const setHere = (): void => {
    h.repoOnServer = { ...h.repoOnServer, afk_provider_default: 'claude-code' };
    h.settingsOnServer = { ...h.settingsOnServer, spawn_options_afk: { ultracode: 'true' } };
  };

  it('while the inherited bag is not known the boxes show no state and cannot be toggled', async () => {
    setHere();
    h.inheritedError = 'inherited: store unavailable';
    await mountSettings(BASE);
    const box = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_options.ultracode"]'),
      'the bag',
    );
    await settle();

    // Neither on nor off — it inherits ON, and "unchecked" would be a guess.
    expect(box.disabled).toBe(true);
    expect(box.indeterminate).toBe(true);
    expect(box.checked).toBe(false);
    expect(bagHint()).toBe(HINT);
    box.click();
    await settle();
    expect(saveBar()).toBeNull();
    expect(h.patchBodies).toEqual([]);

    // Once the answer is there the boxes show it, and a toggle starts from it.
    h.inheritedError = null;
    tryAgain(notice())?.click();
    await settle();
    expect(box.disabled).toBe(false);
    expect(box.indeterminate).toBe(false);
    expect(box.checked).toBe(true);
    expect(bagHint()).toBeNull();
  });

  it('the same while the first answer is still on its way', async () => {
    setHere();
    let release = (): void => {};
    h.inheritedGate = () => new Promise<void>((resolve) => (release = resolve));
    await mountSettings(BASE);
    const box = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_options.ultracode"]'),
      'the bag',
    );

    expect(box.disabled).toBe(true);
    expect(box.indeterminate).toBe(true);
    expect(bagHint()).toBe(HINT);

    release();
    await settle();
    expect(box.disabled).toBe(false);
    expect(box.checked).toBe(true);
  });

  it('a bag of its own is known whatever the server says about the inherited one', async () => {
    setHere();
    h.repoOnServer = { ...h.repoOnServer, afk_options: { ultracode: 'false' } };
    h.inheritedError = 'inherited: store unavailable';
    await mountSettings(BASE);
    const box = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_options.ultracode"]'),
      'the bag',
    );
    await settle();

    expect(box.disabled).toBe(false);
    expect(box.indeterminate).toBe(false);
    expect(box.checked).toBe(false);
    box.click();
    await settle();
    await save();
    expect(h.patchBodies).toEqual([{ afk_options: { ultracode: 'true' } }]);
  });
});

describe('what acts at once reads the saved repo, never the drafts', () => {
  const editorAgent = (): string =>
    container.querySelector('button[name="schedule_provider"] .select-field-label')?.textContent ??
    '';
  const openSchedule = async (): Promise<void> => {
    container.querySelector<HTMLAnchorElement>('.schedules-list a.schedule-row-main')?.click();
    await settle();
    await waitFor(scheduleEditor, 'the schedule editor');
  };

  it('a drafted Agent does not change what a Schedule inherits', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    h.schedules = [baseSchedule()];
    await mount();
    await chooseFromSelect('provider', 'Codex');
    await settleInherited();
    // The page's own fields follow the draft…
    expect(selectedLabel('afk_provider_default')).toBe('Inherited · Codex');

    await openSchedule();

    // …a Schedule, which applies at once, does not: the saved repo's AFK
    // runs still resolve to the saved agent, with that agent's models.
    expect(editorAgent()).toBe('Inherited · Claude Code');
    selectTrigger('schedule_model').click();
    await settle();
    const models = optionRows().map((row) => row.textContent);
    expect(models).toContain('Sonnet');
    expect(models).not.toContain('GPT-5 Codex');
    // The saved repo's answer was already known: nothing more was asked.
    expect(h.inheritedBodies).toEqual([{}, { provider: 'codex' }]);
  });

  it('once the Agent is saved the Schedule follows it', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    h.schedules = [baseSchedule()];
    await mount();
    await chooseFromSelect('provider', 'Codex');
    await settleInherited();
    await save();
    await settleInherited();

    await openSchedule();

    expect(editorAgent()).toBe('Inherited · Codex');
  });

  it('asks about the saved repo on its own when it changes under pending drafts', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    h.schedules = [baseSchedule()];
    await mount();
    await chooseFromSelect('model_default', 'Sonnet'); // a chain draft, pending
    await settleInherited();
    const asked = h.inheritedBodies.length;

    // The saved agent changes elsewhere while that draft is pending.
    h.repoOnServer = { ...h.repoOnServer, provider: 'codex' };
    emitRepoChanged();
    await settle();

    // One question for the page's drafts, one for the saved repo alone.
    expect(h.inheritedBodies.slice(asked)).toEqual([{ model_default: 'sonnet' }, {}]);
    await openSchedule();
    expect(editorAgent()).toBe('Inherited · Codex');
  });
});

describe('the browser resolves nothing itself', () => {
  it('no repo settings source imports the spawn chain helpers', () => {
    const sources = import.meta.glob<string>(
      ['./**/*.ts', './**/*.tsx', '!./**/*.test.ts*', '!./harness.tsx'],
      { eager: true, query: '?raw', import: 'default' },
    );
    const files = Object.keys(sources);
    // Sanity: the glob sees the page, the store and the sections.
    expect(files).toContain('./form.tsx');
    expect(files).toContain('./sections/Agents.tsx');
    expect(files).toContain('./sections/Schedules.tsx');

    const offenders = files.filter((file) =>
      /lib\/spawn|providerFor\(|resolveRemote\(|resolveSpawnOption\(|getSettings\(/.test(
        sources[file] ?? '',
      ),
    );
    expect(offenders).toEqual([]);
  });
});
