// Agents section suite (issues #198, #61): the section's fields on the
// one-page settings — stale-draft resync, AFK defaults and the option bag,
// the AFK seed prompt, agent selection and the three-way remote control —
// each saved through the page's one Save, which sends exactly the changed
// fields. What a field inherits is the (fake) server's answer; the page shows
// it as "Inherited · <value>". The stale-draft cases
// exercise Agents-native fields here; the default_branch/name cases live in
// the Branches suite with their fields.
//
// The seed/resync contract under test: a field's draft is the operator's
// edit, else the live repo. When an SSE repo.changed refetch lands while the
// page stays mounted, a save of an UNRELATED field must not send stale values
// back. Untouched fields follow the server; dirty ones keep the operator's
// edit.

import { describe, expect, it } from 'vitest';
import {
  CODEX,
  REPO_ID,
  baseProviders,
  baseRepo,
  button,
  chooseFromSelect,
  container,
  emitRepoChanged,
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
  segment,
  segmentLabels,
  segmentValue,
  selectTrigger,
  selectedLabel,
  settle,
  settleInherited,
  textarea,
  toggleCheckbox,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountAgents = () => mountSettings(`/repos/${REPO_ID}/settings/agents`);

describe('RepoSettings stale-draft handling', () => {
  it('saving an unrelated edit after a server-side budget change PATCHes only that edit', async () => {
    await mountAgents();
    const budget = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="budget_minutes"]'),
      'agents fields',
    );
    expect(budget.value).toBe('');

    // A server-side change lands (another device saves a budget) and the page
    // refetches on repo.changed while the form stays mounted.
    h.repoOnServer = { ...h.repoOnServer, budget_minutes: 30 };
    emitRepoChanged();
    await settle();

    // The untouched draft follows the server...
    expect(budget.value).toBe('30');

    // ...and saving an edit to ONLY the instance cap must not revert it.
    typeInto(input('max_instances_override'), '3');
    await save();

    expect(h.patchBodies).toEqual([{ max_instances_override: 3 }]);
    expect(h.repoOnServer.budget_minutes).toBe(30);

    // Saved: nothing is pending, so there is nothing to send a second time.
    expect(saveBar()).toBeNull();
    expect(h.patchBodies).toHaveLength(1);
  });

  it('keeps a dirty draft across a refetch and PATCHes only that field', async () => {
    await mountAgents();
    const budget = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="budget_minutes"]'),
      'agents fields',
    );
    typeInto(budget, '45'); // operator edits the budget first

    // Server-side changes land while the operator is mid-edit.
    h.repoOnServer = { ...h.repoOnServer, budget_minutes: 30, max_instances_override: 7 };
    emitRepoChanged();
    await settle();

    expect(budget.value).toBe('45'); // dirty draft survives the refetch
    expect(input('max_instances_override').value).toBe('7'); // untouched field follows

    await save();

    // Only the operator's edit is PATCHed — the server-side cap change is not
    // clobbered back to the stale draft value.
    expect(h.patchBodies).toEqual([{ budget_minutes: 45 }]);
    expect(h.repoOnServer.max_instances_override).toBe(7);
  });
});

describe('RepoSettings AFK defaults', () => {
  it('renders the inherit entry naming what it resolves to, and the schema-driven ultracode checkbox', async () => {
    await mountAgents();
    const model = await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="afk_model_default"]'),
      'AFK defaults section',
    );

    // Unset seeds to the inherit entry (value ''), which titles the trigger…
    expect(model.textContent).toContain('Inherited · Opus (1M)');
    expect(fieldState('afk_model_default')).toBe('inherited');
    // …and sits first (and selected) in the open panel.
    model.click();
    await settle();
    const rows = optionRows();
    expect(rows[0]?.textContent).toBe('Inherited · Opus (1M)');
    expect(rows[0]?.getAttribute('aria-selected')).toBe('true');
    model.click(); // toggle shut again
    await settle();

    // The ultracode bool option renders unchecked (repo afk_options is null,
    // and the bag it inherits does not switch it on).
    const ultracode = input('afk_options.ultracode');
    expect(ultracode.type).toBe('checkbox');
    expect(ultracode.checked).toBe(false);
  });
});

// The AFK option bag (issue #19): while the repo has no bag of its own the
// boxes show the bag it inherits. The first toggle gives it one — the full
// declared bag — and Reset returns it to inherited (null).
describe('RepoSettings AFK option bag', () => {
  const PLAN = { key: 'plan', label: 'Plan first', type: 'bool', default: 'false' };
  const waitForBag = () =>
    waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_options.ultracode"]'),
      'ultracode checkbox',
    );

  it('toggling ultracode PATCHes the full declared bag', async () => {
    await mountAgents();
    const ultracode = await waitForBag();
    expect(fieldState('afk_options')).toBe('inherited');

    toggleCheckbox(ultracode, true);
    await settle();
    expect(fieldState('afk_options')).toBe('set here');
    await save();

    expect(h.patchBodies).toEqual([{ afk_options: { ultracode: 'true' } }]);
    expect(h.repoOnServer.afk_options).toEqual({ ultracode: 'true' });

    // Saved: nothing is pending, so there is nothing to send a second time.
    expect(saveBar()).toBeNull();
    expect(h.patchBodies).toHaveLength(1);
    expect(fieldState('afk_options')).toBe('set here');
  });

  it('shows the inherited bag while the repo has none of its own', async () => {
    h.providersOnServer = [
      { ...baseProviders()[0]!, options: [baseProviders()[0]!.options[0]!, PLAN] },
    ];
    h.settingsOnServer = { ...h.settingsOnServer, spawn_options_afk: { ultracode: 'true' } };
    await mountAgents();
    await waitFor(
      () => (input('afk_options.ultracode').checked ? true : null),
      'the inherited bag',
    );

    // The global bag: ultracode on, plan not in it.
    expect(input('afk_options.ultracode').checked).toBe(true);
    expect(input('afk_options.plan').checked).toBe(false);
    expect(fieldState('afk_options')).toBe('inherited');
    expect(resetButton('afk_options')).toBeNull();
    // Shown, not drafted: nothing is pending and nothing would be sent.
    expect(saveBar()).toBeNull();
  });

  it('the first toggle sends the full declared bag, starting from the inherited one', async () => {
    h.providersOnServer = [
      { ...baseProviders()[0]!, options: [baseProviders()[0]!.options[0]!, PLAN] },
    ];
    h.settingsOnServer = { ...h.settingsOnServer, spawn_options_afk: { ultracode: 'true' } };
    await mountAgents();
    await waitFor(
      () => (input('afk_options.ultracode').checked ? true : null),
      'the inherited bag',
    );

    toggleCheckbox(input('afk_options.plan'), true);
    await settle();

    expect(fieldState('afk_options')).toBe('set here');
    expect(fieldDefault('afk_options')).toBe('Default: Ultracode (multi-agent workflows) on');
    // The untouched box kept what the repo inherited.
    expect(input('afk_options.ultracode').checked).toBe(true);
    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();

    expect(h.patchBodies).toEqual([{ afk_options: { ultracode: 'true', plan: 'true' } }]);
  });

  it('toggling back to the inherited bag is no change, and no bag of its own', async () => {
    await mountAgents();
    const ultracode = await waitForBag();

    toggleCheckbox(ultracode, true);
    await settle();
    expect(saveBarTitle()).toBe('1 unsaved change');

    toggleCheckbox(ultracode, false);
    await settle();
    expect(fieldState('afk_options')).toBe('inherited');
    expect(saveBar()).toBeNull();
  });

  it('Reset returns a stored bag to inherited and PATCHes null', async () => {
    h.repoOnServer = { ...baseRepo(), afk_options: { ultracode: 'true' } };
    await mountAgents();
    const ultracode = await waitForBag();
    expect(ultracode.checked).toBe(true);
    expect(fieldState('afk_options')).toBe('set here');
    expect(fieldDefault('afk_options')).toBe('Default: all off');
    expect(resetButton('afk_options')?.getAttribute('aria-label')).toBe(
      'Reset Options (AFK runs) to inherited',
    );

    resetButton('afk_options')?.click();
    await settle();

    // Back to what the repo inherits: the global bag switches nothing on.
    expect(fieldState('afk_options')).toBe('inherited');
    expect(input('afk_options.ultracode').checked).toBe(false);
    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();

    expect(h.patchBodies).toEqual([{ afk_options: null }]);
    expect(h.repoOnServer.afk_options).toBeNull();
  });

  it('never sends a bag the operator did not touch', async () => {
    h.repoOnServer = { ...baseRepo(), afk_options: { ultracode: 'true' } };
    await mountAgents();
    await waitForBag();

    typeInto(input('budget_minutes'), '45');
    await save();

    expect(h.patchBodies).toEqual([{ budget_minutes: 45 }]);
  });
});

describe('RepoSettings AFK model', () => {
  it('selecting an AFK model PATCHes afk_model_default only', async () => {
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="afk_model_default"]'),
      'AFK model select',
    );

    await chooseFromSelect('afk_model_default', 'Sonnet');
    await save();

    expect(h.patchBodies).toEqual([{ afk_model_default: 'sonnet' }]);
    expect(h.repoOnServer.afk_model_default).toBe('sonnet');
  });
});

describe('RepoSettings AFK seed prompt (issue #52)', () => {
  it("renders empty with the repo's afk_prompt_effective as the placeholder", async () => {
    await mountAgents();
    const field = await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    expect(field.value).toBe('');
    expect(field.placeholder).toBe(h.repoOnServer.afk_prompt_effective);
  });

  it('says that the inherited template follows Incogni once saved, instead of guessing it', async () => {
    const NOTE =
      'The inherited template follows Incogni once saved. The one shown is for the saved setting.';
    await mountAgents();
    const field = await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );
    const text = () => container.querySelector('[data-field="afk_prompt"]')?.textContent ?? '';
    expect(text()).not.toContain(NOTE);

    // Incogni decides which built-in template the repo inherits, and only
    // the server composes it — for the SAVED repo.
    container.querySelector<HTMLButtonElement>('button[name="incogni"]')?.click();
    await settle();

    expect(text()).toContain(NOTE);
    // The placeholder is still the saved repo's template: nothing is made up.
    expect(field.placeholder).toBe(h.repoOnServer.afk_prompt_effective);
    // The note describes the field.
    const note = Array.from(container.querySelectorAll('[data-field="afk_prompt"] p')).find(
      (p) => p.textContent === NOTE,
    );
    expect(field.getAttribute('aria-describedby')?.split(' ')).toContain(note?.id);

    // Saved: the note has nothing left to say.
    await save();
    expect(h.patchBodies).toEqual([{ incogni: true }]);
    expect(text()).not.toContain(NOTE);
  });

  it('Customize copies afk_prompt_effective into the textarea for editing', async () => {
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    button('Customize').click();

    expect(textarea('afk_prompt').value).toBe(h.repoOnServer.afk_prompt_effective);
  });

  it('editing the prompt and saving PATCHes afk_prompt as a string', async () => {
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    typeInto(textarea('afk_prompt'), 'Always branch from main and open a PR when finished.');
    await save();

    expect(h.patchBodies).toEqual([
      { afk_prompt: 'Always branch from main and open a PR when finished.' },
    ]);
    expect(h.repoOnServer.afk_prompt).toBe('Always branch from main and open a PR when finished.');
  });

  it('clearing a stored override PATCHes afk_prompt as null', async () => {
    h.repoOnServer = { ...h.repoOnServer, afk_prompt: 'A previously customized prompt.' };
    await mountAgents();
    const field = await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );
    expect(field.value).toBe('A previously customized prompt.');

    typeInto(field, '');
    await save();

    expect(h.patchBodies).toEqual([{ afk_prompt: null }]);
  });
});

// Agent selection (issue #66 / ADR-0030): base + AFK provider selects with an
// explicit inherit entry, PATCHing via the '' → null convention; the
// model/effort catalogs re-resolve live against the DRAFTED providers; stored
// foreign values persist ("(not in catalog)") — nothing auto-clears.
describe('RepoSettings agent selection (issue #66)', () => {
  const withCodex = () => {
    h.providersOnServer = [...baseProviders(), CODEX];
  };

  it('choosing an agent PATCHes {provider: id}', async () => {
    withCodex();
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agent select');
    expect(selectedLabel('provider')).toBe('Inherited · Claude Code');
    expect(fieldState('provider')).toBe('inherited');

    await chooseFromSelect('provider', 'Codex');
    expect(fieldState('provider')).toBe('set here');
    expect(fieldDefault('provider')).toBe('Default: Claude Code');
    await save();

    expect(h.patchBodies).toEqual([{ provider: 'codex' }]);
    expect(h.repoOnServer.provider).toBe('codex');
  });

  it('choosing inherit PATCHes {provider: null}', async () => {
    withCodex();
    h.repoOnServer = { ...h.repoOnServer, provider: 'claude-code' };
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agent select');
    expect(selectedLabel('provider')).toBe('Claude Code');

    await chooseFromSelect('provider', 'Inherited · Claude Code');
    expect(fieldState('provider')).toBe('inherited');
    await save();

    expect(h.patchBodies).toEqual([{ provider: null }]);
  });

  it('choosing an AFK agent PATCHes {afk_provider_default: id}', async () => {
    withCodex();
    await mountAgents();
    await waitFor(
      () => container.querySelector('button[name="afk_provider_default"]'),
      'AFK agent select',
    );
    expect(selectedLabel('afk_provider_default')).toBe('Inherited · Claude Code');

    await chooseFromSelect('afk_provider_default', 'Codex');
    await save();

    expect(h.patchBodies).toEqual([{ afk_provider_default: 'codex' }]);
    expect(h.repoOnServer.afk_provider_default).toBe('codex');
  });

  it('flipping the AFK agent draft re-catalogs the AFK model select before save', async () => {
    withCodex();
    await mountAgents();
    await waitFor(
      () => container.querySelector('button[name="afk_provider_default"]'),
      'AFK agent select',
    );

    // Before the flip: the AFK model catalog is the claude-code one.
    selectTrigger('afk_model_default').click();
    await settle();
    let labels = optionRows().map((r) => r.textContent);
    expect(labels).toContain('Sonnet');
    expect(labels).not.toContain('GPT-5 Codex');
    selectTrigger('afk_model_default').click(); // toggle shut
    await settle();

    await chooseFromSelect('afk_provider_default', 'Codex');

    // After the flip (still unsaved): the catalog is the codex one at once —
    // the drafted agent IS the effective one…
    selectTrigger('afk_model_default').click();
    await settle();
    labels = optionRows().map((r) => r.textContent);
    expect(labels).toContain('GPT-5 Codex');
    expect(labels).not.toContain('Sonnet');
    selectTrigger('afk_model_default').click(); // toggle shut
    // …and the inherit entry follows once the server has answered for the
    // drafted agent.
    await settleInherited();
    expect(h.inheritedBodies.at(-1)).toEqual({ afk_provider_default: 'codex' });
    expect(selectedLabel('afk_model_default')).toBe('Inherited · GPT-5 Codex');
  });

  it('keeps a stored foreign model_default marked "(not in catalog)" across a provider flip', async () => {
    withCodex();
    h.repoOnServer = { ...h.repoOnServer, model_default: 'weird-model' };
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agent select');

    // Foreign to the effective catalog: offered as-is, marked, never dropped.
    expect(selectedLabel('model_default')).toBe('weird-model (not in catalog)');

    await chooseFromSelect('provider', 'Codex');
    expect(selectedLabel('model_default')).toBe('weird-model (not in catalog)');

    await save();

    // The flip PATCHes ONLY the provider — the stored model_default persists
    // (skip-layer makes it harmless at spawn; flipping back restores it).
    expect(h.patchBodies).toEqual([{ provider: 'codex' }]);
    expect(h.repoOnServer.model_default).toBe('weird-model');
  });
});

// Remote control (issues #163, #61): a three-way pick — inherited, On, Off —
// at BOTH scopes over a tri-state column (it sidesteps issue #21's
// 2-state-checkbox-over-a-3-state-model bug by construction). The inherit
// segment names what it resolves to, and Off is saved as false, never null.
describe('RepoSettings remote control', () => {
  const waitForRemote = () =>
    waitFor(
      () => container.querySelector('button[role="radio"][name="remote_default"]'),
      'remote control',
    );

  it('offers inherited / On / Off at both scopes, naming the inherited value', async () => {
    h.settingsOnServer = { provider_default: 'claude-code', spawn_remote_default: true };
    await mountAgents();
    await waitForRemote();

    expect(segmentLabels('remote_default')).toEqual(['Inherited · on', 'On', 'Off']);
    expect(segmentLabels('afk_remote_default')).toEqual(['Inherited · on', 'On', 'Off']);
    expect(segmentValue('remote_default')).toBe('');
    expect(segmentValue('afk_remote_default')).toBe('');
    expect(fieldState('remote_default')).toBe('inherited');
    expect(fieldState('afk_remote_default')).toBe('inherited');
    // A radio group named by the field's label.
    const group = segment('remote_default', '').closest('[role="radiogroup"]');
    expect(group?.getAttribute('aria-labelledby')).toBe('rs-remote_default-label');
  });

  it('an explicit off PATCHes false, and the AFK inherit segment follows the draft', async () => {
    h.settingsOnServer = { provider_default: 'claude-code', spawn_remote_default: true };
    await mountAgents();
    await waitForRemote();

    segment('remote_default', 'false').click();
    await settle();
    expect(fieldState('remote_default')).toBe('set here');
    expect(fieldDefault('remote_default')).toBe('Default: on');
    // The AFK chain walks through the repo's manual default, so the (unsaved)
    // draft changes what AFK inherit means — as the server answers for it.
    await settleInherited();
    expect(h.inheritedBodies.at(-1)).toEqual({ remote_default: false });
    expect(segmentLabels('afk_remote_default')[0]).toBe('Inherited · off');

    await save();

    // `false` is an explicit off, not an omission — it must reach the server.
    expect(h.patchBodies).toEqual([{ remote_default: false }]);
    expect(h.repoOnServer.remote_default).toBe(false);
    expect(segmentValue('remote_default')).toBe('false');
  });

  it('an explicit AFK off PATCHes false as well', async () => {
    h.settingsOnServer = { provider_default: 'claude-code', spawn_remote_default_afk: true };
    await mountAgents();
    await waitForRemote();
    expect(segmentLabels('afk_remote_default')[0]).toBe('Inherited · on');

    segment('afk_remote_default', 'false').click();
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ afk_remote_default: false }]);
    expect(h.repoOnServer.afk_remote_default).toBe(false);
  });

  it('seeds a stored AFK override; Reset clears it back to inherit as null', async () => {
    h.repoOnServer = { ...baseRepo(), afk_remote_default: true };
    await mountAgents();
    await waitForRemote();
    expect(segmentValue('afk_remote_default')).toBe('true');
    expect(fieldState('afk_remote_default')).toBe('set here');
    expect(fieldDefault('afk_remote_default')).toBe('Default: off');

    resetButton('afk_remote_default')?.click();
    await settle();
    expect(segmentValue('afk_remote_default')).toBe('');
    expect(fieldState('afk_remote_default')).toBe('inherited');
    await save();

    // Only the AFK key — the untouched base pick stays out of the patch.
    expect(h.patchBodies).toEqual([{ afk_remote_default: null }]);
  });

  it('picking the inherit segment clears an override the same way', async () => {
    h.repoOnServer = { ...baseRepo(), remote_default: false };
    await mountAgents();
    await waitForRemote();
    expect(segmentValue('remote_default')).toBe('false');

    segment('remote_default', '').click();
    await settle();
    await save();

    expect(h.patchBodies).toEqual([{ remote_default: null }]);
  });

  it('disables both picks with a note when the effective provider has no remote knob', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    h.repoOnServer = { ...baseRepo(), provider: 'codex' };
    await mountAgents();
    await waitForRemote();
    await waitFor(
      () => (segment('afk_remote_default', 'true').disabled ? true : null),
      'the AFK provider to resolve',
    );

    for (const name of ['remote_default', 'afk_remote_default']) {
      expect(segment(name, '').disabled).toBe(true);
      expect(segment(name, 'true').disabled).toBe(true);
      expect(segment(name, 'false').disabled).toBe(true);
    }
    // The server clamps the inherited value for such a provider.
    expect(segmentLabels('remote_default')[0]).toBe('Inherited · off');
    expect(container.textContent).toContain('Codex ignores this.');
  });
});
