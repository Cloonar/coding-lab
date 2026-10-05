// Autoland section suite (issues #198, #61): the section's fields on the
// one-page settings, saved through the page's one Save. With every section on
// one page and one form store, the forge-only gate of the Autoland switch
// follows the tracker binding DRAFT: flipping the binding in Integrations
// disables the switch at once — the last case below pins that.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  baseRepo,
  chooseFromSelect,
  container,
  fieldError,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  pageSection,
  save,
  saveBarTitle,
  segment,
  selectedLabel,
  setSwitch,
  settle,
  switchButton,
  switchOn,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountAutoland = () => mountSettings(`/repos/${REPO_ID}/settings/autoland`);
const waitForAutoland = () =>
  waitFor(() => container.querySelector('button[name="autoland_enabled"]'), 'autoland switch');

// Autoland (issue #181 / ADR-0048): the per-repo settings, default off / 2 /
// on / inherit; autoland_enabled disables on a non-forge binding (the poller
// has no PR-comment listing to read there).
describe('RepoSettings Autoland', () => {
  it('renders the defaults: off, 2 attempts, merge on, inherit agent', async () => {
    await mountAutoland();
    await waitForAutoland();

    expect(switchOn('autoland_enabled')).toBe(false);
    expect(switchButton('autoland_enabled').disabled).toBe(false); // baseRepo() is forge-bound
    expect(switchOn('auto_merge')).toBe(true);
    expect(input('max_fix_attempts').value).toBe('2');
    expect(selectedLabel('lander_provider')).toBe('Inherit repo agent');
    // Lander model/effort (issue #189) default to the inherit row too.
    expect(selectedLabel('lander_model')).toBe('Inherit global lander default');
    expect(selectedLabel('lander_effort')).toBe('Inherit global lander default');
  });

  it('disables autoland_enabled with a note on a non-forge (builtin) binding', async () => {
    h.repoOnServer = { ...baseRepo(), tracker_binding: 'builtin', forge_kind: 'none' };
    await mountAutoland();
    await waitForAutoland();

    expect(switchButton('autoland_enabled').disabled).toBe(true);
    expect(pageSection('autoland').textContent).toContain(
      'Autoland needs a forge tracker binding.',
    );
  });

  it('toggling autoland_enabled and auto_merge, editing attempts, and picking a lander agent PATCHes all four', async () => {
    await mountAutoland();
    await waitForAutoland();

    setSwitch('autoland_enabled', true);
    setSwitch('auto_merge', false);
    typeInto(input('max_fix_attempts'), '5');
    await chooseFromSelect('lander_provider', 'Claude Code');
    expect(saveBarTitle()).toBe('4 unsaved changes');
    await save();

    expect(h.patchBodies).toEqual([
      {
        autoland_enabled: true,
        auto_merge: false,
        max_fix_attempts: 5,
        lander_provider: 'claude-code',
      },
    ]);
    expect(h.repoOnServer.autoland_enabled).toBe(true);
    expect(h.repoOnServer.auto_merge).toBe(false);
    expect(h.repoOnServer.max_fix_attempts).toBe(5);
    expect(h.repoOnServer.lander_provider).toBe('claude-code');
  });

  it('picking inherit for the lander agent PATCHes null', async () => {
    h.repoOnServer = { ...baseRepo(), lander_provider: 'claude-code' };
    await mountAutoland();
    await waitFor(() => container.querySelector('button[name="lander_provider"]'), 'lander select');
    expect(selectedLabel('lander_provider')).toBe('Claude Code');

    await chooseFromSelect('lander_provider', 'Inherit repo agent');
    await save();

    expect(h.patchBodies).toEqual([{ lander_provider: null }]);
  });

  it('picking a lander model and effort PATCHes lander_model / lander_effort', async () => {
    await mountAutoland();
    await waitFor(
      () => container.querySelector('button[name="lander_model"]'),
      'lander model select',
    );
    // The catalog resolves against the lander's effective provider — here the
    // repo's chain falls back to provider_default (claude-code).
    expect(selectedLabel('lander_model')).toBe('Inherit global lander default');

    await chooseFromSelect('lander_model', 'Sonnet');
    await chooseFromSelect('lander_effort', 'high');
    await save();

    expect(h.patchBodies).toEqual([{ lander_model: 'sonnet', lander_effort: 'high' }]);
    expect(h.repoOnServer.lander_model).toBe('sonnet');
    expect(h.repoOnServer.lander_effort).toBe('high');
  });

  it('clearing a stored lander model back to inherit PATCHes null', async () => {
    h.repoOnServer = { ...baseRepo(), lander_model: 'sonnet' };
    await mountAutoland();
    await waitFor(
      () => container.querySelector('button[name="lander_model"]'),
      'lander model select',
    );
    expect(selectedLabel('lander_model')).toBe('Sonnet');

    await chooseFromSelect('lander_model', 'Inherit global lander default');
    await save();

    expect(h.patchBodies).toEqual([{ lander_model: null }]);
  });

  it('rejects a blank max_fix_attempts in the browser without PATCHing', async () => {
    await mountAutoland();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="max_fix_attempts"]'),
      'max fix attempts field',
    );

    typeInto(input('max_fix_attempts'), '');
    await save();

    expect(h.patchBodies).toHaveLength(0);
    expect(fieldError('max_fix_attempts')).toBe('Use a whole number, 0 or more.');
    expect(document.activeElement).toBe(input('max_fix_attempts'));
  });

  it('rejects a negative max_fix_attempts in the browser without PATCHing', async () => {
    await mountAutoland();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="max_fix_attempts"]'),
      'max fix attempts field',
    );

    typeInto(input('max_fix_attempts'), '-1');
    await save();

    expect(h.patchBodies).toHaveLength(0);
    expect(fieldError('max_fix_attempts')).toBe('Use a whole number, 0 or more.');

    // 0 is a real value: no fix runs at all.
    typeInto(input('max_fix_attempts'), '0');
    await save();
    expect(h.patchBodies).toEqual([{ max_fix_attempts: 0 }]);
  });

  it('the lander catalogs follow the DRAFTED repo agent', async () => {
    h.providersOnServer = [
      ...h.providersOnServer,
      {
        id: 'codex',
        display_name: 'Codex',
        supports_remote: false,
        auth: { kind: 'api-key' },
        models: [{ value: 'gpt-5-codex', label: 'GPT-5 Codex', efforts: [] }],
        efforts: [{ value: 'medium', label: 'medium' }],
        options: [],
      },
    ];
    await mountAutoland();
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agent select');

    // The lander inherits the repo's agent; flipping that agent — in the
    // Agents section, unsaved — re-catalogs the lander's model pick.
    await chooseFromSelect('provider', 'Codex');
    await chooseFromSelect('lander_model', 'GPT-5 Codex');
    await save();

    expect(h.patchBodies).toEqual([{ provider: 'codex', lander_model: 'gpt-5-codex' }]);
  });

  it('follows the tracker binding DRAFT: flipping it to builtin disables the switch at once', async () => {
    await mountAutoland();
    await waitForAutoland();
    expect(switchButton('autoland_enabled').disabled).toBe(false);

    segment('tracker_binding', 'builtin').click();
    await settle();

    expect(switchButton('autoland_enabled').disabled).toBe(true);
    expect(pageSection('autoland').textContent).toContain(
      'Autoland needs a forge tracker binding.',
    );

    // Back to forge, still unsaved: enabled again.
    segment('tracker_binding', 'forge').click();
    await settle();
    expect(switchButton('autoland_enabled').disabled).toBe(false);
    expect(h.patchBodies).toHaveLength(0);
  });
});
