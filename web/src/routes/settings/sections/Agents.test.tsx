// Global settings › Agents (issues #198, #85): four groups — Runs you start,
// AFK runs, Lander, Capacity — with every field the section had, in the repo
// page's words where the fields match. The agent of runs you start is the
// root (no inherit entry; an unseeded store shows the first provider) and the
// catalogs follow the DRAFTED agents. The AFK and lander overrides speak the
// repo page's inheritance vocabulary, derived in the browser from the drafted
// base field, live: "inherited" + "Inherited · <base>" while blank; "set
// here" + "Default: <base>" + Reset once set — Reset is a change, saved as ""
// (text) or null (the AFK remote control). The option bag and the seed
// prompt's Customize action and placeholder behave as before.

import { describe, expect, it } from 'vitest';
import {
  CODEX,
  chooseFromSelect,
  container,
  fieldDefault,
  fieldError,
  fieldHint,
  fieldLabel,
  fieldState,
  h,
  input,
  installSettingsHooks,
  mountPage,
  optionLabels,
  pageSection,
  pick,
  resetButton,
  save,
  saveBar,
  saveBarTitle,
  segment,
  segmentLabels,
  segmentValue,
  selectedLabel,
  settle,
  switchButton,
  switchOn,
  textarea,
  toggleCheckbox,
  typeField,
  typeInto,
} from '../harness';

installSettingsHooks();

/** The groups of the Agents card, each with the fields it holds, in order. */
function groups(): { title: string; fields: string[] }[] {
  return Array.from(pageSection('agents').querySelectorAll('.settings-group')).map((group) => ({
    title: group.querySelector('h3.settings-sub')?.textContent ?? '',
    fields: Array.from(group.querySelectorAll('[data-field]')).map(
      (field) => field.getAttribute('data-field') ?? '',
    ),
  }));
}

describe('Agents: groups and fields', () => {
  it('renders Runs you start, AFK runs, Lander and Capacity with every field', async () => {
    await mountPage();

    expect(groups()).toEqual([
      {
        title: 'Runs you start',
        fields: [
          'provider_default',
          'spawn_model_default',
          'spawn_effort_default',
          'spawn_remote_default',
          'dialog_timeout_minutes',
        ],
      },
      {
        title: 'AFK runs',
        fields: [
          'spawn_provider_default_afk',
          'spawn_model_default_afk',
          'spawn_effort_default_afk',
          'spawn_remote_default_afk',
          'spawn_options_afk',
          'afk_prompt',
        ],
      },
      { title: 'Lander', fields: ['spawn_model_default_lander', 'spawn_effort_default_lander'] },
      {
        title: 'Capacity',
        fields: [
          'max_instances',
          'afk_budget_minutes',
          'afk_tick_seconds',
          'afk_schedule_seconds',
          'sweep_interval_minutes',
        ],
      },
    ]);
  });

  it("uses the repo page's words for the fields both pages have", async () => {
    await mountPage();

    for (const key of ['provider_default', 'spawn_provider_default_afk']) {
      expect(fieldLabel(key)).toBe('Agent');
    }
    for (const key of [
      'spawn_model_default',
      'spawn_model_default_afk',
      'spawn_model_default_lander',
    ]) {
      expect(fieldLabel(key)).toBe('Model');
    }
    for (const key of [
      'spawn_effort_default',
      'spawn_effort_default_afk',
      'spawn_effort_default_lander',
    ]) {
      expect(fieldLabel(key)).toBe('Effort');
    }
    expect(fieldLabel('spawn_remote_default')).toBe('Remote control');
    expect(fieldLabel('spawn_remote_default_afk')).toBe('Remote control');
    expect(fieldLabel('spawn_options_afk')).toBe('Options');
    expect(fieldLabel('afk_prompt')).toBe('Seed prompt');
    expect(fieldLabel('max_instances')).toBe('Max instances');
    expect(fieldLabel('dialog_timeout_minutes')).toBe('Dialog auto-dismiss, minutes');
  });

  it('seeds every field from the stored settings', async () => {
    h.settingsOnServer = {
      ...h.settingsOnServer,
      spawn_model_default_afk: 'sonnet',
      spawn_effort_default_lander: 'max',
      spawn_remote_default: true,
      dialog_timeout_minutes: 15,
      spawn_options_afk: '{"ultracode":"true"}',
    };
    await mountPage();

    expect(selectedLabel('provider_default')).toBe('Claude Code');
    expect(selectedLabel('spawn_model_default')).toBe('Opus (1M)');
    expect(selectedLabel('spawn_effort_default')).toBe('high');
    expect(selectedLabel('spawn_model_default_afk')).toBe('Sonnet');
    expect(selectedLabel('spawn_effort_default_lander')).toBe('max');
    expect(switchOn('spawn_remote_default')).toBe(true);
    expect(input('dialog_timeout_minutes').value).toBe('15');
    expect(input('spawn_options_afk.ultracode').checked).toBe(true);
    expect(input('max_instances').value).toBe('4');
    expect(input('afk_tick_seconds').value).toBe('30');
    expect(saveBar()).toBeNull();
  });
});

describe('Agents: runs you start', () => {
  it('the agent has no inherit entry, and an unseeded store shows the first provider', async () => {
    delete h.settingsOnServer.provider_default;
    await mountPage();

    expect(selectedLabel('provider_default')).toBe('Claude Code');
    expect(fieldState('provider_default')).toBeNull();
    expect(await optionLabels('provider_default')).toEqual(['Claude Code']);
  });

  it('choosing an agent PATCHes provider_default and re-catalogs the model pick', async () => {
    h.providersOnServer = [...h.providersOnServer, CODEX];
    await mountPage();

    await chooseFromSelect('provider_default', 'Codex');

    expect(await optionLabels('spawn_model_default')).toContain('GPT-5 Codex');
    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();
    expect(h.patchBodies).toEqual([{ provider_default: 'codex' }]);
  });

  it('remote control is a switch, saved as a plain bool', async () => {
    await mountPage();
    expect(fieldHint('spawn_remote_default')).toContain("Registers the session with the agent's");

    switchButton('spawn_remote_default').click();
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ spawn_remote_default: true }]);
  });

  it('a provider without the remote knob disables both remote controls and says so', async () => {
    h.providersOnServer = [CODEX];
    h.settingsOnServer = { ...h.settingsOnServer, provider_default: 'codex' };
    await mountPage();

    expect(switchButton('spawn_remote_default').disabled).toBe(true);
    expect(fieldHint('spawn_remote_default')).toBe('Codex ignores this.');
    expect(segment('spawn_remote_default_afk', 'true').disabled).toBe(true);
    expect(fieldHint('spawn_remote_default_afk')).toBe('Codex ignores this.');
  });

  it('dialog auto-dismiss is blank while unset, and 0 ("never") saves as 0', async () => {
    await mountPage();
    expect(input('dialog_timeout_minutes').value).toBe('');
    expect(fieldHint('dialog_timeout_minutes')).toContain('0 = never.');

    await typeField('dialog_timeout_minutes', '0');
    await save();

    expect(h.patchBodies).toEqual([{ dialog_timeout_minutes: 0 }]);
  });

  it('a non-number in dialog auto-dismiss is a problem, even while it was never set', async () => {
    await mountPage();
    await typeField('dialog_timeout_minutes', 'soon');
    await save();

    expect(fieldError('dialog_timeout_minutes')).toBe('Use a whole number, 0 or more.');
    expect(h.patchBodies).toEqual([]);
  });
});

describe('Agents: AFK and lander overrides', () => {
  it('left to inherit, each says so and names the drafted base value as its first pick', async () => {
    await mountPage();

    expect(fieldState('spawn_provider_default_afk')).toBe('inherited');
    expect(selectedLabel('spawn_provider_default_afk')).toBe('Inherited · Claude Code');
    expect(selectedLabel('spawn_model_default_afk')).toBe('Inherited · Opus (1M)');
    expect(selectedLabel('spawn_effort_default_afk')).toBe('Inherited · high');
    expect(selectedLabel('spawn_model_default_lander')).toBe('Inherited · Opus (1M)');
    expect(selectedLabel('spawn_effort_default_lander')).toBe('Inherited · high');
    expect(segmentLabels('spawn_remote_default_afk')).toEqual(['Inherited · off', 'On', 'Off']);
    expect(segmentValue('spawn_remote_default_afk')).toBe('');
    expect(resetButton('spawn_model_default_afk')).toBeNull();
  });

  it('the inherited values follow the drafted base fields live, before anything is saved', async () => {
    h.providersOnServer = [...h.providersOnServer, CODEX];
    await mountPage();

    await chooseFromSelect('spawn_model_default', 'Sonnet');
    await chooseFromSelect('spawn_effort_default', 'max');
    switchButton('spawn_remote_default').click();
    await settle();

    expect(selectedLabel('spawn_model_default_afk')).toBe('Inherited · Sonnet');
    expect(selectedLabel('spawn_effort_default_afk')).toBe('Inherited · max');
    expect(selectedLabel('spawn_model_default_lander')).toBe('Inherited · Sonnet');
    expect(selectedLabel('spawn_effort_default_lander')).toBe('Inherited · max');
    expect(segmentLabels('spawn_remote_default_afk')[0]).toBe('Inherited · on');

    await chooseFromSelect('provider_default', 'Codex');
    expect(selectedLabel('spawn_provider_default_afk')).toBe('Inherited · Codex');
    // Codex's catalog carries no Sonnet: AFK runs get the agent's own default.
    expect(selectedLabel('spawn_model_default_afk')).toBe('Inherited · agent default');
    expect(h.patchBodies).toEqual([]);
  });

  it('set here: says so, shows Default: <base> following the draft, and saves the pick', async () => {
    await mountPage();

    await chooseFromSelect('spawn_model_default_afk', 'Sonnet');

    expect(fieldState('spawn_model_default_afk')).toBe('set here');
    expect(fieldDefault('spawn_model_default_afk')).toBe('Default: Opus (1M)');
    expect(resetButton('spawn_model_default_afk')).not.toBeNull();

    await chooseFromSelect('spawn_model_default', 'Sonnet');
    expect(fieldDefault('spawn_model_default_afk')).toBe('Default: Sonnet');

    await save();
    expect(h.patchBodies).toEqual([
      { spawn_model_default: 'sonnet', spawn_model_default_afk: 'sonnet' },
    ]);
  });

  it('Reset returns a stored text override to inherited — a change, saved as ""', async () => {
    h.settingsOnServer = {
      ...h.settingsOnServer,
      spawn_model_default_afk: 'sonnet',
      spawn_effort_default_lander: 'max',
    };
    await mountPage();
    expect(fieldState('spawn_model_default_afk')).toBe('set here');
    expect(fieldState('spawn_effort_default_lander')).toBe('set here');
    expect(fieldDefault('spawn_effort_default_lander')).toBe('Default: high');

    resetButton('spawn_model_default_afk')?.click();
    resetButton('spawn_effort_default_lander')?.click();
    await settle();

    expect(fieldState('spawn_model_default_afk')).toBe('inherited');
    expect(selectedLabel('spawn_model_default_afk')).toBe('Inherited · Opus (1M)');
    expect(saveBarTitle()).toBe('2 unsaved changes');
    await save();
    expect(h.patchBodies).toEqual([
      { spawn_model_default_afk: '', spawn_effort_default_lander: '' },
    ]);
  });

  it('picking the inherit entry of a stored AFK agent saves ""', async () => {
    h.providersOnServer = [...h.providersOnServer, CODEX];
    h.settingsOnServer = { ...h.settingsOnServer, spawn_provider_default_afk: 'codex' };
    await mountPage();
    expect(selectedLabel('spawn_provider_default_afk')).toBe('Codex');
    // The AFK catalogs are the AFK agent's.
    expect(await optionLabels('spawn_model_default_afk')).toContain('GPT-5 Codex');

    await chooseFromSelect('spawn_provider_default_afk', 'Inherited · Claude Code');
    await save();

    expect(h.patchBodies).toEqual([{ spawn_provider_default_afk: '' }]);
  });

  it('the AFK remote control is three-way: an explicit off, and Reset saved as null', async () => {
    await mountPage();

    await pick('spawn_remote_default_afk', 'false');
    expect(fieldState('spawn_remote_default_afk')).toBe('set here');
    expect(fieldDefault('spawn_remote_default_afk')).toBe('Default: off');
    await save();
    expect(h.patchBodies).toEqual([{ spawn_remote_default_afk: false }]);

    resetButton('spawn_remote_default_afk')?.click();
    await settle();
    expect(segmentValue('spawn_remote_default_afk')).toBe('');
    await save();
    expect(h.patchBodies.at(-1)).toEqual({ spawn_remote_default_afk: null });
  });
});

describe('Agents: option bag and seed prompt', () => {
  it('a toggle PATCHes the full declared bag; toggling back is no change', async () => {
    await mountPage();
    const box = input('spawn_options_afk.ultracode');
    expect(box.checked).toBe(false);

    toggleCheckbox(box, true);
    await settle();
    expect(saveBarTitle()).toBe('1 unsaved change');
    toggleCheckbox(input('spawn_options_afk.ultracode'), false);
    await settle();
    expect(saveBar()).toBeNull();

    toggleCheckbox(input('spawn_options_afk.ultracode'), true);
    await settle();
    await save();
    expect(h.patchBodies).toEqual([{ spawn_options_afk: { ultracode: 'true' } }]);
  });

  it('the seed prompt is blank with the built-in prompt as its placeholder, inherited', async () => {
    await mountPage();

    expect(textarea('afk_prompt').value).toBe('');
    expect(textarea('afk_prompt').placeholder).toBe('Work on issue <N> on branch <BRANCH>.');
    expect(fieldState('afk_prompt')).toBe('inherited');
    expect(fieldHint('afk_prompt')).toContain('detected as done only by an open PR');
  });

  it('Customize copies the built-in prompt in for editing, and Save sends the edit', async () => {
    await mountPage();

    container
      .querySelector<HTMLButtonElement>('[data-field="afk_prompt"] .settings-inline-action')
      ?.click();
    await settle();

    expect(textarea('afk_prompt').value).toBe('Work on issue <N> on branch <BRANCH>.');
    expect(fieldState('afk_prompt')).toBe('set here');
    expect(fieldDefault('afk_prompt')).toBe('Default: the built-in seed prompt');

    typeInto(textarea('afk_prompt'), 'Fix issue <N>.');
    await settle();
    await save();
    expect(h.patchBodies).toEqual([{ afk_prompt: 'Fix issue <N>.' }]);
  });

  it('clearing a stored prompt saves ""', async () => {
    h.settingsOnServer = { ...h.settingsOnServer, afk_prompt: 'Custom.' };
    await mountPage();
    expect(fieldState('afk_prompt')).toBe('set here');

    typeInto(textarea('afk_prompt'), '');
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ afk_prompt: '' }]);
  });
});
