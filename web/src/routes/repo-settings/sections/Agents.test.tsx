// Agents section suite (issue #198): the RepoSettings.test.tsx describes
// whose fields now live on /repos/:id/settings/agents — stale-draft resync
// (through createSeededDrafts now), AFK defaults, the AFK seed prompt, agent
// selection and remote control. The stale-draft cases exercise Agents-native
// fields here; the original default_branch/name cases moved to the Branches
// suite with their fields.
//
// The seed/resync contract under test: drafts seed from a repo SNAPSHOT.
// When an SSE repo.changed refetch lands while the form stays mounted, a save
// of an UNRELATED field must not diff stale drafts against the refreshed repo
// and silently PATCH old values back. Untouched drafts follow the server;
// dirty drafts keep the operator's edit.

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
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
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

installRepoSettingsHooks();

const mountAgents = () => mountSettings(`/repos/${REPO_ID}/settings/agents`);

describe('RepoSettings stale-draft handling', () => {
  it('saving an unrelated edit after a server-side budget change PATCHes only that edit', async () => {
    await mountAgents();
    const budget = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="budget_minutes"]'),
      'agents form',
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
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ max_instances_override: 3 }]);
    expect(h.repoOnServer.budget_minutes).toBe(30);

    // The seed advanced with the save: a second submit has nothing to send.
    submitForm();
    await settle();
    expect(h.patchBodies).toHaveLength(1);
  });

  it('keeps a dirty draft across a refetch and PATCHes only that field', async () => {
    await mountAgents();
    const budget = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="budget_minutes"]'),
      'agents form',
    );
    typeInto(budget, '45'); // operator edits the budget first

    // Server-side changes land while the operator is mid-edit.
    h.repoOnServer = { ...h.repoOnServer, budget_minutes: 30, max_instances_override: 7 };
    emitRepoChanged();
    await settle();

    expect(budget.value).toBe('45'); // dirty draft survives the refetch
    expect(input('max_instances_override').value).toBe('7'); // untouched field follows

    submitForm();
    await settle();

    // Only the operator's edit is PATCHed — the server-side cap change is not
    // clobbered back to the stale draft value.
    expect(h.patchBodies).toEqual([{ budget_minutes: 45 }]);
    expect(h.repoOnServer.max_instances_override).toBe(7);
  });
});

describe('RepoSettings AFK defaults', () => {
  it('renders the inherit option and the schema-driven ultracode checkbox', async () => {
    await mountAgents();
    const model = await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="afk_model_default"]'),
      'AFK defaults section',
    );

    // Unset seeds to the inherit entry (value ''), which titles the trigger…
    expect(model.textContent).toContain('Inherit global AFK default');
    // …and sits first (and selected) in the open panel.
    model.click();
    await settle();
    const rows = optionRows();
    expect(rows[0]?.textContent).toBe('Inherit global AFK default');
    expect(rows[0]?.getAttribute('aria-selected')).toBe('true');
    model.click(); // toggle shut again
    await settle();

    // The ultracode bool option renders unchecked (repo afk_options is null).
    const ultracode = input('afk_options.ultracode');
    expect(ultracode.type).toBe('checkbox');
    expect(ultracode.checked).toBe(false);
  });

  it('toggling ultracode PATCHes the full declared bag', async () => {
    await mountAgents();
    const ultracode = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_options.ultracode"]'),
      'ultracode checkbox',
    );

    toggleCheckbox(ultracode, true);
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ afk_options: { ultracode: 'true' } }]);
    expect(h.repoOnServer.afk_options).toEqual({ ultracode: 'true' });

    // The seed advanced with the save: a second submit has nothing to send.
    submitForm();
    await settle();
    expect(h.patchBodies).toHaveLength(1);
  });

  it('selecting an AFK model PATCHes afk_model_default only', async () => {
    await mountAgents();
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="afk_model_default"]'),
      'AFK model select',
    );

    await chooseFromSelect('afk_model_default', 'Sonnet');
    submitForm();
    await settle();

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
    submitForm();
    await settle();

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
    submitForm();
    await settle();

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
    expect(selectedLabel('provider')).toBe('Inherit global default');

    await chooseFromSelect('provider', 'Codex');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ provider: 'codex' }]);
    expect(h.repoOnServer.provider).toBe('codex');
  });

  it('choosing inherit PATCHes {provider: null}', async () => {
    withCodex();
    h.repoOnServer = { ...h.repoOnServer, provider: 'claude-code' };
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agent select');
    expect(selectedLabel('provider')).toBe('Claude Code');

    await chooseFromSelect('provider', 'Inherit global default');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ provider: null }]);
  });

  it('choosing an AFK agent PATCHes {afk_provider_default: id}', async () => {
    withCodex();
    await mountAgents();
    await waitFor(
      () => container.querySelector('button[name="afk_provider_default"]'),
      'AFK agent select',
    );
    expect(selectedLabel('afk_provider_default')).toBe('Inherit global AFK default');

    await chooseFromSelect('afk_provider_default', 'Codex');
    submitForm();
    await settle();

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

    // After the flip (still unsaved): the catalog is the codex one, with the
    // inherit entry intact at the top.
    selectTrigger('afk_model_default').click();
    await settle();
    labels = optionRows().map((r) => r.textContent);
    expect(labels[0]).toBe('Inherit global AFK default');
    expect(labels).toContain('GPT-5 Codex');
    expect(labels).not.toContain('Sonnet');
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

    submitForm();
    await settle();

    // The flip PATCHes ONLY the provider — the stored model_default persists
    // (skip-layer makes it harmless at spawn; flipping back restores it).
    expect(h.patchBodies).toEqual([{ provider: 'codex' }]);
    expect(h.repoOnServer.model_default).toBe('weird-model');
  });
});

// Remote control (issue #163): a 3-way Inherit / On / Off select at BOTH scopes
// over a tri-state column — the reference implementation for a nullable bool
// override (it sidesteps issue #21's 2-state-checkbox-over-a-3-state-model bug
// by construction). The inherit row names what it currently resolves to.
describe('RepoSettings remote control', () => {
  it('offers Inherit / On / Off at both scopes, naming the effective inherited value', async () => {
    h.settingsOnServer = { provider_default: 'claude-code', spawn_remote_default: true };
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="remote_default"]'), 'remote select');

    expect(selectedLabel('remote_default')).toBe('Inherit global default — currently on');
    expect(selectedLabel('afk_remote_default')).toBe('Inherit global AFK default — currently on');

    selectTrigger('remote_default').click();
    await settle();
    expect(optionRows().map((r) => r.querySelector('.select-option-label')?.textContent)).toEqual([
      'Inherit global default — currently on',
      'On',
      'Off',
    ]);
  });

  it('an explicit repo off PATCHes false, and the AFK inherit row follows the draft live', async () => {
    h.settingsOnServer = { provider_default: 'claude-code', spawn_remote_default: true };
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="remote_default"]'), 'remote select');

    await chooseFromSelect('remote_default', 'Off');
    // The AFK chain walks through the repo's manual default, so the (unsaved)
    // draft already changes what AFK inherit means.
    expect(selectedLabel('afk_remote_default')).toBe('Inherit global AFK default — currently off');

    submitForm();
    await settle();

    // `false` is an explicit off, not an omission — it must reach the server.
    expect(h.patchBodies).toEqual([{ remote_default: false }]);
    expect(h.repoOnServer.remote_default).toBe(false);
  });

  it('seeds a stored AFK override and clears it back to inherit as null', async () => {
    h.repoOnServer = { ...baseRepo(), afk_remote_default: true };
    await mountAgents();
    await waitFor(
      () => container.querySelector('button[name="afk_remote_default"]'),
      'AFK remote select',
    );
    expect(selectedLabel('afk_remote_default')).toBe('On');

    await chooseFromSelect('afk_remote_default', 'Inherit global AFK default — currently off');
    submitForm();
    await settle();

    // Only the AFK key — the untouched base select stays out of the patch.
    expect(h.patchBodies).toEqual([{ afk_remote_default: null }]);
  });

  it('disables both selects with a note when the resolved provider has no remote knob', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    h.repoOnServer = { ...baseRepo(), provider: 'codex' };
    await mountAgents();
    await waitFor(() => container.querySelector('button[name="remote_default"]'), 'remote select');

    expect(selectTrigger('remote_default').disabled).toBe(true);
    expect(selectTrigger('afk_remote_default').disabled).toBe(true);
    expect(container.textContent).toContain('Codex ignores this.');
  });
});
