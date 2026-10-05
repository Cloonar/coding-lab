// The leave guard (issue #61): leaving the repo with pending settings changes
// opens an in-page dialog — Keep editing, Discard, Save and leave — and each
// choice does what its label says. Navigation inside the repo never asks; a
// tab close or reload keeps the browser's own prompt; no window.confirm.
//
// "Leaving" is done here the way the app does it: by following a link the
// router intercepts. /elsewhere is outside every repo (the harness's
// catch-all route renders it as nothing).

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  container,
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
  saveBar,
  saveBarTitle,
  settle,
  typeInto,
  waitFor,
} from './harness';
import { isInsideRepo } from './LeaveGuard';

installRepoSettingsHooks();

const REPO = `/repos/${REPO_ID}`;
const BASE = `${REPO}/settings`;

/** An in-app link somewhere on the page (like a side rail entry). */
function linkTo(href: string, state?: unknown): HTMLAnchorElement {
  const a = document.createElement('a');
  a.setAttribute('href', href);
  if (state !== undefined) a.setAttribute('state', JSON.stringify(state));
  a.textContent = href;
  container.appendChild(a);
  return a;
}

async function mountWithChanges(): Promise<void> {
  await mountSettings(BASE);
  await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
  typeInto(input('budget_minutes'), '90');
  typeInto(input('git_author_name'), 'Dominik');
  await settle();
}

const backToRepositories = () => container.querySelector<HTMLAnchorElement>('a.back-link');

/** A button of the open dialog (the save bar has a Discard of its own). */
function choose(text: string): HTMLButtonElement {
  const el = Array.from(openDialog()?.querySelectorAll('button') ?? []).find(
    (b) => b.textContent?.trim() === text,
  );
  if (!el) throw new Error(`the dialog has no ${JSON.stringify(text)} button`);
  return el;
}

describe('leaving the repo with pending changes', () => {
  it('opens the in-page dialog and holds the navigation', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    await mountWithChanges();

    await followLink(linkTo('/elsewhere'));

    const dialog = openDialog();
    expect(dialog).not.toBeNull();
    expect(dialog?.getAttribute('role')).toBe('alertdialog');
    expect(dialog?.getAttribute('aria-modal')).toBe('true');
    expect(dialog?.querySelector('.dialog-title')?.textContent).toBe(
      'Leave with 2 unsaved changes?',
    );
    expect(dialog?.textContent).toContain('Your changes to coding-lab are not saved yet.');
    expect(Array.from(dialog?.querySelectorAll('button') ?? []).map((b) => b.textContent)).toEqual([
      'Keep editing',
      'Discard',
      'Save and leave',
    ]);
    // The safe choice has the focus.
    expect(document.activeElement).toBe(choose('Keep editing'));
    // Still here, nothing sent, and no browser confirm.
    expect(routerHistory.get()).toBe(BASE);
    expect(h.patchBodies).toEqual([]);
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('the back link to the list asks too, in the singular for one change', async () => {
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="budget_minutes"]'), 'the page');
    typeInto(input('budget_minutes'), '90');
    await settle();

    await followLink(backToRepositories());

    expect(openDialog()?.querySelector('.dialog-title')?.textContent).toBe(
      'Leave with 1 unsaved change?',
    );
    expect(routerHistory.get()).toBe(BASE);
  });

  it('Keep editing stays, with every edit intact', async () => {
    await mountWithChanges();
    await followLink(linkTo('/elsewhere'));

    choose('Keep editing').click();
    await settle();

    expect(openDialog()).toBeNull();
    expect(routerHistory.get()).toBe(BASE);
    expect(input('budget_minutes').value).toBe('90');
    expect(saveBarTitle()).toBe('2 unsaved changes');
    expect(h.patchBodies).toEqual([]);
  });

  it('Escape is Keep editing', async () => {
    await mountWithChanges();
    await followLink(linkTo('/elsewhere'));

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await settle();

    expect(openDialog()).toBeNull();
    expect(routerHistory.get()).toBe(BASE);
    expect(saveBarTitle()).toBe('2 unsaved changes');
  });

  it('Discard drops the edits and leaves, without saving', async () => {
    await mountWithChanges();
    await followLink(linkTo('/elsewhere'));

    choose('Discard').click();
    await settle();

    expect(routerHistory.get()).toBe('/elsewhere');
    expect(h.patchBodies).toEqual([]);

    // Coming back, nothing is pending: the edits are gone for good.
    routerHistory.go(-1);
    await settle();
    await waitFor(() => container.querySelector('input[name="budget_minutes"]'), 'the page');
    expect(input('budget_minutes').value).toBe('');
    expect(input('git_author_name').value).toBe('');
    expect(saveBar()).toBeNull();
  });

  it('holds Discard while a save is in flight: the edits are being saved, not dropped', async () => {
    let release = (): void => {};
    h.patchHold = new Promise<void>((resolve) => (release = resolve));
    await mountWithChanges();
    // Save from the bar, and try to leave while the request is on its way.
    Array.from(saveBar()?.querySelectorAll('button') ?? [])
      .find((b) => b.classList.contains('primary'))
      ?.click();
    await settle();
    await followLink(linkTo('/elsewhere'));
    expect(openDialog()).not.toBeNull();

    expect(choose('Discard').disabled).toBe(true);
    expect(choose('Save and leave').disabled).toBe(true);
    expect(choose('Keep editing').disabled).toBe(false);
    choose('Discard').click();
    await settle();
    expect(routerHistory.get()).toBe(BASE);

    // The save lands: nothing is pending, so the navigation simply goes ahead.
    release();
    await settle();
    expect(h.patchBodies).toHaveLength(1);
    expect(routerHistory.get()).toBe('/elsewhere');
  });

  it('Save and leave saves in one PATCH, then leaves', async () => {
    await mountWithChanges();
    await followLink(linkTo('/elsewhere?x=1'));

    choose('Save and leave').click();
    await settle();

    expect(h.patchBodies).toEqual([{ budget_minutes: 90, git_author_name: 'Dominik' }]);
    expect(h.repoOnServer.budget_minutes).toBe(90);
    // It went where the operator was going.
    expect(routerHistory.get()).toBe('/elsewhere?x=1');
  });

  it('Save and leave stays when the browser finds a problem, and shows it at the field', async () => {
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="afk_branch_pattern"]'), 'the page');
    typeInto(input('afk_branch_pattern'), 'afk/');
    await settle();
    await followLink(linkTo('/elsewhere'));

    choose('Save and leave').click();
    await settle();

    expect(h.patchBodies).toEqual([]);
    expect(openDialog()).toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/branches?field=afk_branch_pattern`);
    expect(fieldError('afk_branch_pattern')).toBe(
      'The pattern needs <N> exactly once. It stands for the issue number.',
    );
    // The dialog is gone, so the field can take — and has — the focus.
    expect(document.activeElement).toBe(input('afk_branch_pattern'));
    expect(saveBarTitle()).toBe('1 problem to fix');
  });

  it('Save and leave stays when the server refuses, with the refusal on screen', async () => {
    h.patchRefusal = { error: 'repo is being deleted', status: 409 };
    await mountWithChanges();
    await followLink(linkTo('/elsewhere'));

    choose('Save and leave').click();
    await settle();

    expect(h.patchBodies).toHaveLength(1);
    expect(openDialog()).toBeNull();
    expect(routerHistory.get()).toBe(BASE);
    expect(saveBar()?.querySelector('.settings-savebar-error')?.textContent).toBe(
      'Not saved. repo is being deleted',
    );
    expect(input('budget_minutes').value).toBe('90');

    // Leaving is asked about again.
    await followLink(linkTo('/elsewhere'));
    expect(openDialog()).not.toBeNull();
  });

  it('asks from Overview and Issues as well: the changes travel with the repo', async () => {
    await mountWithChanges();
    await followLink(repoTab('Overview'));

    await followLink(linkTo('/elsewhere'));
    expect(openDialog()).not.toBeNull();
    expect(routerHistory.get()).toBe(REPO);

    choose('Keep editing').click();
    await settle();
    await followLink(repoTab('Issues'));
    await followLink(backToRepositories());
    expect(openDialog()?.querySelector('.dialog-title')?.textContent).toBe(
      'Leave with 2 unsaved changes?',
    );
  });

  it('goes ahead by itself once nothing is pending any more', async () => {
    await mountWithChanges();
    await followLink(linkTo('/elsewhere'));
    expect(openDialog()).not.toBeNull();

    // A refresh shows the server already holds both edits (saved elsewhere).
    h.repoOnServer = { ...h.repoOnServer, budget_minutes: 90, git_author_name: 'Dominik' };
    emitRepoChanged();
    await settle();

    expect(routerHistory.get()).toBe('/elsewhere');
    expect(h.patchBodies).toEqual([]);
  });

  it('another repo is outside this one', async () => {
    await mountWithChanges();

    await followLink(linkTo('/repos/repo_10/settings/imports'));

    expect(openDialog()).not.toBeNull();
    expect(routerHistory.get()).toBe(BASE);
  });
});

describe('navigation that never asks', () => {
  it('inside the repo: tabs, section URLs, issue pages and the schedule editor URLs', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    await mountWithChanges();

    const inside = [
      `${BASE}/branches`,
      `${BASE}/integrations?field=forge_credential_id`,
      `${BASE}/schedules/new`,
      `${BASE}/schedules/sched_1`,
      `${REPO}/issues`,
      `${REPO}/labels`,
      REPO,
      BASE,
    ];
    for (const path of inside) {
      await followLink(linkTo(path));
      expect(openDialog()).toBeNull();
      expect(routerHistory.get()).toBe(path);
    }
    // The changes rode along the whole way.
    expect(saveBarTitle()).toBe('2 unsaved changes');
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('leaving with nothing pending', async () => {
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('input[name="budget_minutes"]'), 'the page');

    await followLink(linkTo('/elsewhere'));

    expect(openDialog()).toBeNull();
    expect(routerHistory.get()).toBe('/elsewhere');
  });

  it('leaving after the changes were saved', async () => {
    await mountWithChanges();
    const save = Array.from(saveBar()?.querySelectorAll('button') ?? []).find(
      (b) => b.textContent === 'Save',
    );
    save?.click();
    await settle();

    await followLink(linkTo('/elsewhere'));

    expect(openDialog()).toBeNull();
    expect(routerHistory.get()).toBe('/elsewhere');
  });

  it('a navigation that reports a finished action (the repo was deleted) is not held back', async () => {
    await mountWithChanges();

    // What the delete dialog does after the DELETE went through.
    await followLink(linkTo('/elsewhere', { notice: 'Deleted coding-lab from lab' }));

    expect(openDialog()).toBeNull();
    expect(routerHistory.get()).toBe('/elsewhere');
    expect(h.patchBodies).toEqual([]);
  });
});

describe('a tab close or reload', () => {
  it('keeps the browser prompt armed only while changes are pending', async () => {
    await mountSettings(BASE);
    const author = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="git_author_name"]'),
      'the page',
    );
    const unload = (): Event => {
      const event = new Event('beforeunload', { cancelable: true });
      window.dispatchEvent(event);
      return event;
    };

    // Clean: the tab may close freely.
    expect(unload().defaultPrevented).toBe(false);

    typeInto(author, 'Dominik'); // dirty
    await settle();
    expect(unload().defaultPrevented).toBe(true);

    // Still armed on another tab of the repo.
    await followLink(repoTab('Overview'));
    expect(unload().defaultPrevented).toBe(true);

    // Discarded: clean again.
    Array.from(saveBar()?.querySelectorAll('button') ?? [])
      .find((b) => b.textContent === 'Discard')
      ?.click();
    await settle();
    expect(unload().defaultPrevented).toBe(false);
  });

  it('is disarmed once the repo home is gone', async () => {
    await mountWithChanges();
    await followLink(linkTo('/elsewhere'));
    choose('Discard').click();
    await settle();
    expect(routerHistory.get()).toBe('/elsewhere');

    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});

describe('isInsideRepo', () => {
  it('is the repo home and everything under it — nothing else', () => {
    expect(isInsideRepo('/repos/repo_1', 'repo_1')).toBe(true);
    expect(isInsideRepo('/repos/repo_1/', 'repo_1')).toBe(true);
    expect(isInsideRepo('/repos/repo_1/settings/branches', 'repo_1')).toBe(true);
    expect(isInsideRepo('/repos/repo_1/settings?field=name', 'repo_1')).toBe(true);
    expect(isInsideRepo('/repos/repo_1?x=1#top', 'repo_1')).toBe(true);
    expect(isInsideRepo('/repos/repo_1/issues/12#comment', 'repo_1')).toBe(true);

    expect(isInsideRepo('/repos', 'repo_1')).toBe(false);
    expect(isInsideRepo('/repos/new', 'repo_1')).toBe(false);
    // A longer id that merely starts the same is another repo.
    expect(isInsideRepo('/repos/repo_10', 'repo_1')).toBe(false);
    expect(isInsideRepo('/repos/repo_10/settings', 'repo_1')).toBe(false);
    expect(isInsideRepo('/settings', 'repo_1')).toBe(false);
    expect(isInsideRepo('/?next=/repos/repo_1', 'repo_1')).toBe(false);
  });
});
