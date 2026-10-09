// The save bar on the repo home's other tabs (issue #61): the frame renders
// it, so pending settings changes stay visible — and saveable — on Overview
// and Issues. From there a section link opens the Settings tab at that
// section, and a Save that finds a problem goes to the Settings tab first.
//
// These cases assert on the bar, never on what Overview or Issues render.

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  SHELL_PROVIDERS_GETS,
  SHELL_SETTINGS_GETS,
  container,
  discard,
  emitRepoChanged,
  fieldError,
  followLink,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  openDialog,
  repoTab,
  routerHistory,
  save,
  saveBar,
  saveBarSections,
  saveBarTitle,
  settle,
  toastText,
  typeInto,
  waitFor,
} from './harness';

installRepoSettingsHooks();

const REPO = `/repos/${REPO_ID}`;
const BASE = `${REPO}/settings`;

/** Opens the settings, edits a field in Agents and one in Branches. */
async function mountWithChanges(): Promise<void> {
  await mountSettings(BASE);
  await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
  typeInto(input('budget_minutes'), '90');
  typeInto(input('afk_branch_pattern'), 'issue-<N>');
  await settle();
}

describe('the save bar outside the Settings tab', () => {
  it('stays visible on Overview and Issues while changes are pending', async () => {
    await mountWithChanges();
    expect(saveBarTitle()).toBe('2 unsaved changes');

    await followLink(repoTab('Overview'));
    expect(routerHistory.get()).toBe(REPO);
    // The settings page is gone; the bar is not.
    expect(container.querySelector('.settings-page')).toBeNull();
    expect(saveBarTitle()).toBe('2 unsaved changes');
    expect(saveBarSections()).toEqual(['Agents', 'Branches']);

    await followLink(repoTab('Issues'));
    expect(routerHistory.get()).toBe(`${REPO}/issues`);
    expect(saveBarTitle()).toBe('2 unsaved changes');

    // Switching tabs inside the repo never asked anything.
    expect(openDialog()).toBeNull();

    // Back on Settings the edits are still in their fields.
    await followLink(repoTab('Settings'));
    await waitFor(() => container.querySelector('input[name="budget_minutes"]'), 'the page');
    expect(input('budget_minutes').value).toBe('90');
    expect(input('afk_branch_pattern').value).toBe('issue-<N>');
    expect(saveBarTitle()).toBe('2 unsaved changes');
  });

  it('is absent on Overview while nothing is pending', async () => {
    await mountSettings(REPO);
    await settle();
    expect(saveBar()).toBeNull();
  });

  it('costs the other tabs nothing: the catalog and the inherited values load with the Settings tab only', async () => {
    const requested = (): string[] =>
      vi.mocked(globalThis.fetch).mock.calls.map(([url]) => String(url));
    const count = (url: string) => requested().filter((u) => u === url).length;
    const INHERITED = `/api/v1/repos/${REPO_ID}/inherited`;
    await mountSettings(REPO);
    await settle();
    await followLink(repoTab('Issues'));

    // Only the app shell's own catalog read (its More-tab dot) so far, none
    // from the Settings tab.
    expect(count('/api/v1/providers')).toBe(SHELL_PROVIDERS_GETS);
    expect(requested()).not.toContain('/api/v1/credentials');
    expect(requested()).not.toContain(INHERITED);

    await followLink(repoTab('Settings'));
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
    expect(count('/api/v1/providers')).toBe(SHELL_PROVIDERS_GETS + 1);
    expect(count(INHERITED)).toBe(1);
    // The page resolves nothing itself, so it never reads the global settings
    // (the one read is the app shell's).
    expect(count('/api/v1/settings')).toBe(SHELL_SETTINGS_GETS);

    // Away from the tab nothing follows the repo…
    await followLink(repoTab('Overview'));
    emitRepoChanged();
    await settle();
    expect(count(INHERITED)).toBe(1);

    // …and coming back asks for the inherited values again (they may have
    // changed meanwhile); the provider catalog is loaded once.
    await followLink(repoTab('Settings'));
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
    await settle();
    expect(count('/api/v1/providers')).toBe(SHELL_PROVIDERS_GETS + 1);
    expect(count(INHERITED)).toBe(2);
  });

  it('announces the count from a status region that is in the page before the bar is', async () => {
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');

    // There while nothing is pending — empty, and not inside the bar: a live
    // region that arrives together with its text is never read out.
    const live = container.querySelector('.settings-savebar-live');
    expect(live).not.toBeNull();
    expect(live?.getAttribute('role')).toBe('status');
    expect(live?.textContent).toBe('');
    expect(saveBar()).toBeNull();

    typeInto(input('budget_minutes'), '90');
    await settle();
    // The SAME node now says it.
    expect(container.querySelector('.settings-savebar-live')).toBe(live);
    expect(live?.textContent).toBe('1 unsaved change in Agents');
    expect(saveBar()?.contains(live ?? null)).toBe(false);
    // One live region for it: the visible text is not a second one.
    expect(saveBar()?.querySelector('[role="status"]')).toBeNull();

    typeInto(input('afk_branch_pattern'), 'issue-<N>');
    await settle();
    expect(live?.textContent).toBe('2 unsaved changes in Agents, Branches');

    // A Save that finds a problem is announced the same way.
    typeInto(input('budget_minutes'), '0');
    await settle();
    await save();
    expect(live?.textContent).toBe('1 problem to fix in Agents');

    await discard();
    expect(live?.isConnected).toBe(true);
    expect(live?.textContent).toBe('');
  });

  it('reserves its own height in the page, so nothing hides behind it', async () => {
    await mountWithChanges();
    const space = container.querySelector('.settings-savebar-space');
    expect(space).not.toBeNull();
    // In the page flow, right before the bar, after the tab body.
    expect(space?.nextElementSibling).toBe(saveBar());
    expect(container.querySelector('.repo-home-body')?.nextElementSibling).toBe(space);

    await discard();
    expect(container.querySelector('.settings-savebar-space')).toBeNull();
  });

  it('Save on Overview sends the one PATCH and the bar disappears there', async () => {
    await mountWithChanges();
    await followLink(repoTab('Overview'));

    await save();

    expect(h.patchBodies).toEqual([{ budget_minutes: 90, afk_branch_pattern: 'issue-<N>' }]);
    expect(saveBar()).toBeNull();
    expect(toastText()).toBe('Saved 2 changes to coding-lab');
    expect(routerHistory.get()).toBe(REPO); // still on Overview
  });

  it('Discard on Issues restores the saved values', async () => {
    await mountWithChanges();
    await followLink(repoTab('Issues'));

    await discard();
    expect(saveBar()).toBeNull();

    await followLink(repoTab('Settings'));
    await waitFor(() => container.querySelector('input[name="budget_minutes"]'), 'the page');
    expect(input('budget_minutes').value).toBe('');
    expect(input('afk_branch_pattern').value).toBe('afk/<N>');
  });

  it('a section link on Overview opens the Settings tab at that section', async () => {
    await mountWithChanges();
    await followLink(repoTab('Overview'));
    h.scrolls = [];

    const link = Array.from(saveBar()?.querySelectorAll('a') ?? []).find(
      (a) => a.textContent === 'Branches',
    );
    await followLink(link);
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
    await settle();

    expect(routerHistory.get()).toBe(`${BASE}/branches`);
    expect(h.scrolls.length).toBeGreaterThan(0);
    expect(h.scrolls.every((scroll) => scroll.target === 'settings-branches')).toBe(true);
    // A push: Back returns to Overview.
    routerHistory.go(-1);
    await settle();
    expect(routerHistory.get()).toBe(REPO);
  });

  it('Save on another tab with a problem goes to the Settings tab, to the field', async () => {
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
    typeInto(input('afk_branch_pattern'), 'afk/');
    await settle();
    await followLink(repoTab('Overview'));
    h.scrolls = [];

    await save();
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
    await settle();

    // No request; the Settings tab is open at the field, which has the focus.
    expect(h.patchBodies).toEqual([]);
    expect(routerHistory.get()).toBe(`${BASE}/branches?field=afk_branch_pattern`);
    expect(fieldError('afk_branch_pattern')).toBe(
      'The pattern needs <N> exactly once. It stands for the issue number.',
    );
    expect(h.scrolls.at(-1)?.target).toBe('afk_branch_pattern');
    expect(document.activeElement).toBe(input('afk_branch_pattern'));
    expect(saveBarTitle()).toBe('1 problem to fix');
  });

  it('a refusal that names a field, from a Save on another tab, lands at that field too', async () => {
    h.patchRefusal = { error: "default_branch: must not start with '-'", field: 'default_branch' };
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="default_branch"]'), 'the page');
    typeInto(input('default_branch'), '-main');
    await settle();
    await followLink(repoTab('Issues'));

    await save();
    await waitFor(() => container.querySelector('input[name="default_branch"]'), 'the page');
    await settle();

    expect(h.patchBodies).toEqual([{ default_branch: '-main' }]);
    expect(fieldError('default_branch')).toBe("default_branch: must not start with '-'");
    expect(document.activeElement).toBe(input('default_branch'));
  });
});
