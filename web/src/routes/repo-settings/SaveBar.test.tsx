// The save bar on the repo home's other tabs (issue #61): the frame renders
// it, so pending settings changes stay visible — and saveable — on Overview
// and Issues. From there a section link opens the Settings tab at that
// section, and a Save that finds a problem goes to the Settings tab first.
//
// These cases assert on the bar, never on what Overview or Issues render.

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  container,
  discard,
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

  it('costs the other tabs nothing: the settings catalog loads with the Settings tab only', async () => {
    const requested = (): string[] =>
      vi.mocked(globalThis.fetch).mock.calls.map(([url]) => String(url));
    await mountSettings(REPO);
    await settle();
    await followLink(repoTab('Issues'));

    expect(requested()).not.toContain('/api/v1/providers');
    expect(requested()).not.toContain('/api/v1/settings');
    expect(requested()).not.toContain('/api/v1/credentials');

    await followLink(repoTab('Settings'));
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
    expect(requested()).toContain('/api/v1/providers');
    expect(requested()).toContain('/api/v1/settings');

    // Loaded once: coming back to the tab does not ask again.
    const count = (url: string) => requested().filter((u) => u === url).length;
    await followLink(repoTab('Overview'));
    await followLink(repoTab('Settings'));
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
    expect(count('/api/v1/providers')).toBe(1);
    expect(count('/api/v1/settings')).toBe(1);
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
