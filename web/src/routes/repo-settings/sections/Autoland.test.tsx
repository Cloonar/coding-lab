// Autoland section suite (issues #198, #61): the section's fields on the
// one-page settings, saved through the page's one Save.
//
// Only what applies (issue #61 §7), read from the drafts: with the builtin
// tracker binding the section says why Autoland is unavailable, links to the
// tracker binding field and disables the switch; with Autoland off, the merge
// policy, the fix-attempt bound and the lander's picks are replaced by a
// note. Folding only hides — a folded field keeps its value, stays in the
// PATCH when it is pending, and comes back when it is needed.

import { describe, expect, it } from 'vitest';
import {
  CODEX,
  REPO_ID,
  baseProviders,
  baseRepo,
  chooseFromSelect,
  container,
  fieldChanged,
  fieldDefault,
  fieldError,
  fieldState,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  pageSection,
  resetButton,
  routerHistory,
  save,
  saveBarSections,
  saveBarTitle,
  segment,
  segmentValue,
  selectedLabel,
  setSwitch,
  settle,
  settleInherited,
  switchButton,
  switchOn,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const BASE = `/repos/${REPO_ID}/settings`;
const mountAutoland = async (path = `${BASE}/autoland`): Promise<void> => {
  await mountSettings(path);
  await waitFor(() => container.querySelector('button[name="autoland_enabled"]'), 'autoland');
};
/** A repo with Autoland on, so its options show. */
const onRepo = () => ({ ...baseRepo(), autoland_enabled: true });

const OPTIONS = [
  'auto_merge',
  'max_fix_attempts',
  'lander_provider',
  'lander_model',
  'lander_effort',
];
const OFF_NOTE = 'Merge policy and lander options appear when Autoland is on.';
const BUILTIN_NOTE =
  'Autoland needs a forge tracker binding, and this repository uses the built-in one.';
const shows = (key: string) => container.querySelector(`[data-field="${key}"]`) !== null;
const autolandText = () => pageSection('autoland').textContent ?? '';
const textOf = (ids: string | null): string =>
  (ids ?? '')
    .split(' ')
    .filter(Boolean)
    .map((id) => document.getElementById(id)?.textContent ?? '')
    .join(' ');

// Autoland (issue #181 / ADR-0048): the per-repo settings, default off / 2 /
// on / inherit.
describe('RepoSettings Autoland', () => {
  it('renders the defaults of an Autoland that is on: 2 attempts, merge on, inherited lander', async () => {
    h.repoOnServer = onRepo();
    await mountAutoland();

    expect(switchOn('autoland_enabled')).toBe(true);
    expect(switchButton('autoland_enabled').disabled).toBe(false); // forge-bound
    expect(switchOn('auto_merge')).toBe(true);
    expect(input('max_fix_attempts').value).toBe('2');
    // The lander's picks inherit, and name what they resolve to: the repo's
    // agent, and that agent's model and effort (issue #189).
    expect(selectedLabel('lander_provider')).toBe('Inherited · Claude Code');
    expect(selectedLabel('lander_model')).toBe('Inherited · Opus (1M)');
    expect(selectedLabel('lander_effort')).toBe('Inherited · high');
    for (const key of ['lander_provider', 'lander_model', 'lander_effort']) {
      expect(fieldState(key)).toBe('inherited');
    }
    // Not overridable: no state at the label.
    expect(fieldState('max_fix_attempts')).toBeNull();
    expect(autolandText()).not.toContain(OFF_NOTE);
  });

  it('toggling Autoland on, merge off, editing attempts, and picking a lander agent PATCHes all four', async () => {
    await mountAutoland();

    setSwitch('autoland_enabled', true);
    await settle();
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

  it('Reset on the lander agent PATCHes null', async () => {
    h.repoOnServer = { ...onRepo(), lander_provider: 'claude-code' };
    await mountAutoland();
    expect(selectedLabel('lander_provider')).toBe('Claude Code');
    expect(fieldState('lander_provider')).toBe('set here');
    expect(fieldDefault('lander_provider')).toBe('Default: Claude Code');
    expect(resetButton('lander_provider')?.getAttribute('aria-label')).toBe(
      'Reset Agent (Lander) to inherited',
    );

    resetButton('lander_provider')?.click();
    await settle();
    expect(selectedLabel('lander_provider')).toBe('Inherited · Claude Code');
    await save();

    expect(h.patchBodies).toEqual([{ lander_provider: null }]);
  });

  it('picking a lander model and effort PATCHes lander_model / lander_effort', async () => {
    h.repoOnServer = onRepo();
    await mountAutoland();

    await chooseFromSelect('lander_model', 'Sonnet');
    await chooseFromSelect('lander_effort', 'high');
    await save();

    expect(h.patchBodies).toEqual([{ lander_model: 'sonnet', lander_effort: 'high' }]);
    expect(h.repoOnServer.lander_model).toBe('sonnet');
    expect(h.repoOnServer.lander_effort).toBe('high');
  });

  it('picking the inherit entry for a stored lander model PATCHes null', async () => {
    h.repoOnServer = { ...onRepo(), lander_model: 'sonnet' };
    await mountAutoland();
    expect(selectedLabel('lander_model')).toBe('Sonnet');

    await chooseFromSelect('lander_model', 'Inherited · Opus (1M)');
    await save();

    expect(h.patchBodies).toEqual([{ lander_model: null }]);
  });

  it('rejects a blank max_fix_attempts in the browser without PATCHing', async () => {
    h.repoOnServer = onRepo();
    await mountAutoland();

    typeInto(input('max_fix_attempts'), '');
    await save();

    expect(h.patchBodies).toHaveLength(0);
    expect(fieldError('max_fix_attempts')).toBe('Use a whole number, 0 or more.');
    expect(document.activeElement).toBe(input('max_fix_attempts'));
  });

  it('rejects a negative max_fix_attempts in the browser without PATCHing', async () => {
    h.repoOnServer = onRepo();
    await mountAutoland();

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
    h.providersOnServer = [...baseProviders(), CODEX];
    h.repoOnServer = onRepo();
    await mountAutoland();

    // The lander inherits the repo's agent; flipping that agent — in the
    // Agents section, unsaved — changes what the lander inherits, as the
    // server answers for the draft, and with it the lander's model catalog.
    await chooseFromSelect('provider', 'Codex');
    await settleInherited();
    expect(h.inheritedBodies.at(-1)).toEqual({ provider: 'codex' });
    expect(selectedLabel('lander_provider')).toBe('Inherited · Codex');
    expect(selectedLabel('lander_model')).toBe('Inherited · GPT-5 Codex');

    await chooseFromSelect('lander_model', 'GPT-5 Codex');
    await save();

    expect(h.patchBodies).toEqual([{ provider: 'codex', lander_model: 'gpt-5-codex' }]);
  });
});

describe('RepoSettings Autoland: off replaces its options with a note', () => {
  it('shows the note instead of the merge policy, the attempts and the lander', async () => {
    await mountAutoland(); // baseRepo(): Autoland off

    expect(switchOn('autoland_enabled')).toBe(false);
    for (const key of OPTIONS) expect(shows(key)).toBe(false);
    expect(autolandText()).toContain(OFF_NOTE);
    expect(container.querySelector('.settings-savebar')).toBeNull();
  });

  it('switching Autoland on brings the options back; off folds them again', async () => {
    await mountAutoland();

    setSwitch('autoland_enabled', true);
    await settle();
    for (const key of OPTIONS) expect(shows(key)).toBe(true);
    expect(autolandText()).not.toContain(OFF_NOTE);

    setSwitch('autoland_enabled', false);
    await settle();
    for (const key of OPTIONS) expect(shows(key)).toBe(false);
    expect(autolandText()).toContain(OFF_NOTE);
  });

  it('switching Autoland off keeps every option value and PATCHes only the switch', async () => {
    h.repoOnServer = {
      ...onRepo(),
      auto_merge: false,
      max_fix_attempts: 5,
      lander_model: 'sonnet',
    };
    await mountAutoland();
    expect(input('max_fix_attempts').value).toBe('5');

    setSwitch('autoland_enabled', false);
    await settle();
    expect(shows('max_fix_attempts')).toBe(false);
    await save();

    expect(h.patchBodies).toEqual([{ autoland_enabled: false }]);
    expect(h.repoOnServer.auto_merge).toBe(false);
    expect(h.repoOnServer.max_fix_attempts).toBe(5);
    expect(h.repoOnServer.lander_model).toBe('sonnet');

    // On again: the values are where they were.
    setSwitch('autoland_enabled', true);
    await settle();
    expect(input('max_fix_attempts').value).toBe('5');
    expect(switchOn('auto_merge')).toBe(false);
    expect(selectedLabel('lander_model')).toBe('Sonnet');
  });

  it('an option with a pending change is never folded away', async () => {
    h.repoOnServer = onRepo();
    await mountAutoland();

    typeInto(input('max_fix_attempts'), '4');
    setSwitch('autoland_enabled', false);
    await settle();

    expect(input('max_fix_attempts').value).toBe('4');
    expect(fieldChanged('max_fix_attempts')).toBe(true);
    expect(saveBarTitle()).toBe('2 unsaved changes');
    expect(saveBarSections()).toEqual(['Autoland']);
    await save();
    expect(h.patchBodies).toEqual([{ autoland_enabled: false, max_fix_attempts: 4 }]);
  });

  it('?field=max_fix_attempts unfolds the options of an Autoland that is off', async () => {
    await mountAutoland(`${BASE}/autoland?field=max_fix_attempts`);
    await waitFor(() => container.querySelector('input[name="max_fix_attempts"]'), 'attempts');
    await settle();

    expect(document.activeElement).toBe(input('max_fix_attempts'));
    expect(switchOn('autoland_enabled')).toBe(false);
    expect(container.querySelector('.settings-savebar')).toBeNull();
  });
});

describe('RepoSettings Autoland: the builtin tracker binding', () => {
  const builtinRepo = () => ({
    ...baseRepo(),
    tracker_binding: 'builtin' as const,
    forge_kind: 'none' as const,
  });

  it('says why it is unavailable, links to the tracker binding and disables the switch', async () => {
    h.repoOnServer = { ...builtinRepo(), autoland_enabled: true };
    await mountAutoland();

    expect(autolandText()).toContain(BUILTIN_NOTE);
    const toggle = switchButton('autoland_enabled');
    expect(toggle.disabled).toBe(true);
    // The reason is the switch's description — not only a note beside it.
    expect(textOf(toggle.getAttribute('aria-describedby'))).toBe(
      'Not available: Autoland needs a forge tracker binding.',
    );
    // Its options are not offered either, and no "appear when on" note.
    for (const key of OPTIONS) expect(shows(key)).toBe(false);
    expect(autolandText()).not.toContain(OFF_NOTE);
  });

  it('the link reveals the tracker binding field', async () => {
    h.repoOnServer = builtinRepo();
    await mountAutoland();
    h.scrolls = [];

    const link = Array.from(pageSection('autoland').querySelectorAll('a')).find(
      (a) => a.textContent === 'Change in Integrations',
    );
    expect(link?.getAttribute('href')).toBe(`${BASE}/integrations?field=tracker_binding`);
    link?.click();
    await settle();

    expect(h.scrolls.at(-1)).toMatchObject({ target: 'tracker_binding', smooth: true });
    expect(document.activeElement).toBe(segment('tracker_binding', 'builtin'));
    expect(routerHistory.get()).toBe(`${BASE}/integrations?field=tracker_binding`);
  });

  it('follows the tracker binding DRAFT: flipping it disables and re-enables the switch at once', async () => {
    await mountAutoland(); // forge-bound
    expect(switchButton('autoland_enabled').disabled).toBe(false);
    expect(autolandText()).not.toContain(BUILTIN_NOTE);

    segment('tracker_binding', 'builtin').click();
    await settle();
    expect(switchButton('autoland_enabled').disabled).toBe(true);
    expect(autolandText()).toContain(BUILTIN_NOTE);

    // Back to forge, still unsaved: enabled again.
    segment('tracker_binding', 'forge').click();
    await settle();
    expect(switchButton('autoland_enabled').disabled).toBe(false);
    expect(autolandText()).not.toContain(BUILTIN_NOTE);
    expect(h.patchBodies).toHaveLength(0);
  });

  it('flipping a repo to builtin never changes its Autoland fields in the PATCH', async () => {
    h.repoOnServer = { ...onRepo(), max_fix_attempts: 5 };
    await mountAutoland();

    segment('tracker_binding', 'builtin').click();
    await settle();
    expect(segmentValue('tracker_binding')).toBe('builtin');
    expect(shows('max_fix_attempts')).toBe(false);
    await save();

    expect(h.patchBodies).toEqual([{ tracker_binding: 'builtin' }]);
    expect(h.repoOnServer.autoland_enabled).toBe(true);
    expect(h.repoOnServer.max_fix_attempts).toBe(5);
  });
});
