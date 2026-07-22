// Global settings › Agents form coverage (issue #198), ported from the old
// Settings.test.tsx: the AFK inherit entry + option bag, remote control (base
// checkbox + AFK 3-state override), the AFK seed prompt (issue #52), the agent
// defaults chain (issue #66 / ADR-0030) and the dialog auto-dismiss timeout
// (issue #124). Mounted at /settings/agents; the PATCH payload expectations
// are byte-identical to the monolith — the acceptance contract says the
// dirty-fields-only payload semantics are unchanged.

import { beforeEach, describe, expect, it } from 'vitest';
import {
  CODEX,
  baseProviders,
  button,
  cardByHeading,
  chooseFromSelect,
  container,
  h,
  input,
  installSettingsHooks,
  mountAt,
  optionRows,
  selectTrigger,
  selectedLabel,
  settle,
  submitForm,
  textarea,
  toggleCheckbox,
  typeInto,
  waitFor,
} from '../harness';

installSettingsHooks();

const mountAgents = () => mountAt('/settings/agents');

describe('Settings AFK defaults', () => {
  it('offers an inherit entry that seeds selected and the ultracode checkbox', async () => {
    await mountAgents();
    const model = await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    // Unset seeds to the inherit entry (value ''), which titles the trigger…
    expect(selectedLabel('spawn_model_default_afk')).toBe('Same as default');
    // …and sits first (and selected) in the open panel.
    model.click();
    await settle();
    const rows = optionRows();
    expect(rows[0]?.textContent).toBe('Same as default');
    expect(rows[0]?.getAttribute('aria-selected')).toBe('true');
    model.click(); // toggle shut again
    await settle();

    const ultracode = input('spawn_options_afk.ultracode');
    expect(ultracode.type).toBe('checkbox');
    expect(ultracode.checked).toBe(false);
  });

  it('seeds the AFK selects and checkbox from the stored payload', async () => {
    h.settingsOnServer = {
      spawn_model_default_afk: 'sonnet',
      spawn_options_afk: '{"ultracode":"true"}', // server returns a JSON string
    };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    expect(selectedLabel('spawn_model_default_afk')).toBe('Sonnet');
    expect(input('spawn_options_afk.ultracode').checked).toBe(true);
  });

  it('PATCHes an AFK model and the full declared option bag', async () => {
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    await chooseFromSelect('spawn_model_default_afk', 'Sonnet');
    toggleCheckbox(input('spawn_options_afk.ultracode'), true);
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([
      { spawn_model_default_afk: 'sonnet', spawn_options_afk: { ultracode: 'true' } },
    ]);
  });

  it('selecting inherit clears a stored AFK model back to an empty string', async () => {
    h.settingsOnServer = { spawn_model_default_afk: 'opus[1m]' };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );
    expect(selectedLabel('spawn_model_default_afk')).toBe('Opus (1M)');

    await chooseFromSelect('spawn_model_default_afk', 'Same as default'); // the inherit entry
    submitForm();
    await settle();

    // Empty is allowed for the AFK key — it clears back to the base default.
    expect(h.patchBodies).toEqual([{ spawn_model_default_afk: '' }]);
  });
});

// Remote control (issue #163): a plain on/off checkbox at the BASE scope (there
// is nothing above it to inherit from) and a 3-state override in the AFK card,
// both PATCHed as JSON bools — with null for "same as default".
describe('Settings remote control', () => {
  const remoteBox = () => input('spawn_remote_default');

  it('seeds the base checkbox from the stored bool and the AFK override to inherit', async () => {
    h.settingsOnServer = { spawn_remote_default: true };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="spawn_remote_default"]'),
      'spawn defaults section',
    );

    expect(remoteBox().type).toBe('checkbox');
    expect(remoteBox().checked).toBe(true);
    expect(selectedLabel('spawn_remote_default_afk')).toBe('Same as default');
    // The base control lives in the base spawn-defaults card, the override in AFK.
    expect(
      cardByHeading('Spawn defaults').querySelector('input[name="spawn_remote_default"]'),
    ).not.toBeNull();
    expect(
      cardByHeading('AFK defaults').querySelector('button[name="spawn_remote_default_afk"]'),
    ).not.toBeNull();
  });

  it('PATCHes the base bool and an explicit AFK off', async () => {
    h.settingsOnServer = { spawn_remote_default: false };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="spawn_remote_default"]'),
      'spawn defaults section',
    );
    expect(remoteBox().checked).toBe(false);

    toggleCheckbox(remoteBox(), true);
    await chooseFromSelect('spawn_remote_default_afk', 'Off');
    submitForm();
    await settle();

    // Both go as JSON bools — `false` is a value here, never an omission.
    expect(h.patchBodies).toEqual([
      { spawn_remote_default: true, spawn_remote_default_afk: false },
    ]);
  });

  it('clears an AFK override back to inherit as null, and never sends an untouched field', async () => {
    h.settingsOnServer = { spawn_remote_default: true, spawn_remote_default_afk: false };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="spawn_remote_default"]'),
      'spawn defaults section',
    );
    // The stored explicit off shows as Off — NOT as the inherit row.
    expect(selectedLabel('spawn_remote_default_afk')).toBe('Off');
    expect(remoteBox().checked).toBe(true);

    await chooseFromSelect('spawn_remote_default_afk', 'Same as default');
    submitForm();
    await settle();

    // Only the AFK key: the untouched base checkbox stays out of the patch.
    expect(h.patchBodies).toEqual([{ spawn_remote_default_afk: null }]);
  });

  it('disables both controls with a note when the resolved provider has no remote knob', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    h.settingsOnServer = { provider_default: 'codex' };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="spawn_remote_default"]'),
      'spawn defaults section',
    );

    expect(remoteBox().disabled).toBe(true);
    expect(selectTrigger('spawn_remote_default_afk').disabled).toBe(true);
    // Named by the provider's display_name, never a hardcoded brand.
    expect(container.textContent).toContain('Codex ignores this.');
  });
});

describe('Settings AFK seed prompt (issue #52)', () => {
  const DEFAULT_PROMPT = 'Resolve issue #<N> on branch <BRANCH>, then open a PR.';

  it('renders empty with the built-in default as the placeholder', async () => {
    h.settingsOnServer = { afk_prompt_default: DEFAULT_PROMPT };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    const field = textarea('afk_prompt');
    expect(field.value).toBe('');
    expect(field.placeholder).toBe(DEFAULT_PROMPT);
  });

  it('Customize copies the effective default into the textarea for editing', async () => {
    h.settingsOnServer = { afk_prompt_default: DEFAULT_PROMPT };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    button('Customize').click();

    expect(textarea('afk_prompt').value).toBe(DEFAULT_PROMPT);
  });

  it('editing the prompt and saving PATCHes afk_prompt', async () => {
    h.settingsOnServer = { afk_prompt_default: DEFAULT_PROMPT };
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    typeInto(textarea('afk_prompt'), 'Always branch from main and open a PR when finished.');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([
      { afk_prompt: 'Always branch from main and open a PR when finished.' },
    ]);
  });

  it('clearing a stored prompt back to empty PATCHes afk_prompt as ""', async () => {
    h.settingsOnServer = { afk_prompt: 'A previously customized prompt.' };
    await mountAgents();
    const field = await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );
    expect(field.value).toBe('A previously customized prompt.');

    typeInto(field, '');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ afk_prompt: '' }]);
  });
});

// Global agent defaults (issue #66 / ADR-0030): provider_default is the ROOT
// of the chain (no inherit entry); spawn_provider_default_afk inherits it
// ("" = same as default); the model/effort catalogs re-resolve live against
// the DRAFTED providers before anything is saved.
describe('Settings agent defaults (issue #66)', () => {
  beforeEach(() => {
    h.providersOnServer = [...baseProviders(), CODEX];
  });

  it('choosing a base agent PATCHes provider_default', async () => {
    h.settingsOnServer = { provider_default: 'claude-code' };
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="provider_default"]'), 'agent select');
    expect(selectedLabel('provider_default')).toBe('Claude Code');

    await chooseFromSelect('provider_default', 'Codex');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ provider_default: 'codex' }]);
  });

  it('shows the effective first provider when the store is unseeded — no inherit entry', async () => {
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="provider_default"]'), 'agent select');

    // The root of the chain has nothing to inherit from: the trigger resolves
    // to the first registered provider and the panel offers no inherit row.
    expect(selectedLabel('provider_default')).toBe('Claude Code');
    selectTrigger('provider_default').click();
    await settle();
    expect(optionRows().map((r) => r.textContent)).toEqual(['Claude Code', 'Codex']);
  });

  it('choosing an AFK agent PATCHes spawn_provider_default_afk, "" on inherit', async () => {
    h.settingsOnServer = { spawn_provider_default_afk: 'codex' };
    await mountAgents();
    await waitFor(
      () => container.querySelector('button[name="spawn_provider_default_afk"]'),
      'AFK agent select',
    );
    expect(selectedLabel('spawn_provider_default_afk')).toBe('Codex');

    await chooseFromSelect('spawn_provider_default_afk', 'Same as default');
    submitForm();
    await settle();

    // Empty is the inherit value for the AFK key — it clears back to the base.
    expect(h.patchBodies).toEqual([{ spawn_provider_default_afk: '' }]);
  });

  it('re-catalogs the base model select against the drafted provider_default', async () => {
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="provider_default"]'), 'agent select');

    // Before the flip: the claude-code catalog.
    selectTrigger('spawn_model_default').click();
    await settle();
    let labels = optionRows().map((r) => r.textContent);
    expect(labels).toContain('Sonnet');
    expect(labels).not.toContain('GPT-5 Codex');
    selectTrigger('spawn_model_default').click(); // toggle shut
    await settle();

    await chooseFromSelect('provider_default', 'Codex');

    // After the flip (still unsaved): the codex catalog.
    selectTrigger('spawn_model_default').click();
    await settle();
    labels = optionRows().map((r) => r.textContent);
    expect(labels).toContain('GPT-5 Codex');
    expect(labels).not.toContain('Sonnet');
  });
});

// Dialog auto-dismiss timeout (issue #124): a typed-int setting that is NOT
// seeded server-side (absent from GET = never set = blank input, not 0) and
// renders in the Spawn defaults card rather than Capacity & AFK, where every
// other int field lives. 0 ("never") must be an accepted, saveable value.
describe('Settings dialog timeout (issue #124)', () => {
  it('renders in the Spawn defaults card, blank when unset', async () => {
    await mountAgents();
    await waitFor(
      () => container.querySelector('input[name="dialog_timeout_minutes"]'),
      'dialog timeout field',
    );

    const field = input('dialog_timeout_minutes');
    expect(field.value).toBe('');
    expect(cardByHeading('Spawn defaults').contains(field)).toBe(true);
    expect(cardByHeading('Capacity & AFK').contains(field)).toBe(false);
    expect(container.textContent).toContain('Dialog auto-dismiss (minutes)');
  });

  it('typing 0 and saving PATCHes dialog_timeout_minutes as 0 — 0 is not rejected', async () => {
    await mountAgents();
    await waitFor(
      () => container.querySelector('input[name="dialog_timeout_minutes"]'),
      'dialog timeout field',
    );

    typeInto(input('dialog_timeout_minutes'), '0');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ dialog_timeout_minutes: 0 }]);
  });

  it('a non-numeric value shows the validation error and blocks the save', async () => {
    await mountAgents();
    await waitFor(
      () => container.querySelector('input[name="dialog_timeout_minutes"]'),
      'dialog timeout field',
    );

    typeInto(input('dialog_timeout_minutes'), 'abc');
    submitForm();
    await settle();

    expect(container.textContent).toContain('Enter a whole number.');
    expect(h.patchBodies).toEqual([]);
  });
});
