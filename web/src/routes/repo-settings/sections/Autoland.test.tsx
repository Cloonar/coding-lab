// Autoland section suite (issue #198): the RepoSettings.test.tsx Autoland
// describe on /repos/:id/settings/autoland. One deliberate change from the
// monolith: the tracker-binding draft lives in the Integrations section now,
// so autoland_enabled's forge-only gate resolves against the SAVED
// props.repo().tracker_binding — the builtin-binding case below pins that.

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
  toggleCheckbox,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountAutoland = () => mountSettings(`/repos/${REPO_ID}/settings/autoland`);

// Autoland (issue #181 / ADR-0048): the four per-repo settings, default off /
// 2 / on / inherit; autoland_enabled disables on a non-forge binding (the
// poller has no PR-comment listing to read there).
describe('RepoSettings Autoland', () => {
  it('renders the defaults: off, 2 attempts, merge on, inherit agent', async () => {
    await mountAutoland();
    const autoland = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="autoland_enabled"]'),
      'autoland checkbox',
    );

    expect(autoland.checked).toBe(false);
    expect(autoland.disabled).toBe(false); // baseRepo() is forge-bound
    expect(input('auto_merge').checked).toBe(true);
    expect(input('max_fix_attempts').value).toBe('2');
    expect(selectedLabel('lander_provider')).toBe('Inherit repo agent');
    // Lander model/effort (issue #189) default to the inherit row too.
    expect(selectedLabel('lander_model')).toBe('Inherit repo default');
    expect(selectedLabel('lander_effort')).toBe('Inherit repo default');
  });

  it('disables autoland_enabled with a hint on a non-forge (builtin) SAVED binding', async () => {
    h.repoOnServer = { ...baseRepo(), tracker_binding: 'builtin', forge_kind: 'none' };
    await mountAutoland();
    const autoland = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="autoland_enabled"]'),
      'autoland checkbox',
    );

    expect(autoland.disabled).toBe(true);
    expect(container.textContent).toContain('Autoland needs a forge tracker binding.');
  });

  it('toggling autoland_enabled and auto_merge, editing attempts, and picking a lander agent PATCHes all four', async () => {
    await mountAutoland();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="autoland_enabled"]'),
      'autoland checkbox',
    );

    toggleCheckbox(input('autoland_enabled'), true);
    toggleCheckbox(input('auto_merge'), false);
    typeInto(input('max_fix_attempts'), '5');
    await chooseFromSelect('lander_provider', 'Claude Code');
    submitForm();
    await settle();

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
    submitForm();
    await settle();

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
    expect(selectedLabel('lander_model')).toBe('Inherit repo default');

    await chooseFromSelect('lander_model', 'Sonnet');
    await chooseFromSelect('lander_effort', 'high');
    submitForm();
    await settle();

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

    await chooseFromSelect('lander_model', 'Inherit repo default');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ lander_model: null }]);
  });

  it('rejects a blank max_fix_attempts client-side without PATCHing', async () => {
    await mountAutoland();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="max_fix_attempts"]'),
      'max fix attempts field',
    );

    typeInto(input('max_fix_attempts'), '');
    submitForm();
    await settle();

    expect(h.patchBodies).toHaveLength(0);
    expect(container.textContent).toContain(
      'Max fix attempts must be a whole number of 0 or more.',
    );
  });

  it('rejects a negative max_fix_attempts client-side without PATCHing', async () => {
    await mountAutoland();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="max_fix_attempts"]'),
      'max fix attempts field',
    );

    typeInto(input('max_fix_attempts'), '-1');
    submitForm();
    await settle();

    expect(h.patchBodies).toHaveLength(0);
    expect(container.textContent).toContain(
      'Max fix attempts must be a whole number of 0 or more.',
    );
  });
});
