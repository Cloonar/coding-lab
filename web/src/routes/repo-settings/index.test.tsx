// Repo-settings area wiring (issue #198): every category slug deep-links to
// its section; the mobile bare index lists the rows in order (danger last,
// tinted) under the repo home header; desktop redirects the bare index to
// the first category; the area renders as the Settings tab of the repo home
// frame (issue #61) — the frame names the repo, so the area carries no crumb
// trail of its own; the schedule editor URLs reach the area; the per-section
// unsaved-changes guard intercepts in-app navigation and tab close.

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  container,
  installRepoSettingsHooks,
  mountSettings,
  routerHistory,
  setDesktop,
  settle,
  typeInto,
  unmount,
  waitFor,
} from './harness';
import { REPO_SETTINGS_CATEGORIES } from './categories';

installRepoSettingsHooks();

const BASE = `/repos/${REPO_ID}/settings`;

const buttonByText = (text: string): HTMLButtonElement | null =>
  Array.from(container.querySelectorAll('button')).find((b) => b.textContent?.trim() === text) ??
  null;

describe('repo-settings deep links', () => {
  // One DOM probe per category: an element only that section renders. The
  // completeness check below keeps this map honest when categories change.
  const probes: Record<string, () => Element | null> = {
    general: () => container.querySelector('input[name="name"]'),
    integrations: () => container.querySelector('select[name="tracker_binding"]'),
    branches: () => container.querySelector('input[name="default_branch"]'),
    agents: () => container.querySelector('button[name="afk_provider_default"]'),
    runner: () => container.querySelector('button[name="runner"]'),
    autoland: () => container.querySelector('input[name="autoland_enabled"]'),
    secrets: () => buttonByText('+ Add secret'),
    schedules: () => buttonByText('+ Add schedule'),
    imports: () => buttonByText('+ Add import'),
    danger: () => buttonByText('Delete repository'),
  };

  it('probes cover exactly the declared categories', () => {
    expect(Object.keys(probes).sort()).toEqual(REPO_SETTINGS_CATEGORIES.map((c) => c.slug).sort());
  });

  for (const category of REPO_SETTINGS_CATEGORIES) {
    it(`renders the ${category.slug} section at ${BASE}/${category.slug}`, async () => {
      await mountSettings(`${BASE}/${category.slug}`);
      await waitFor(() => probes[category.slug]?.() ?? null, `${category.slug} content`);
      expect(routerHistory.get()).toBe(`${BASE}/${category.slug}`);
    });
  }
});

describe('repo-settings mobile index', () => {
  it('lists the category rows in order under the repo head, danger last and tinted', async () => {
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('a.settings-index-row'), 'index rows');

    // The repo home frame's header heads the index (issue #61): repo name h1,
    // clone-status chip (baseRepo is mid-clone) and the remote line.
    expect(container.querySelector('.repo-head h1')?.textContent).toBe('coding-lab');
    expect(container.querySelector('.repo-head .chip.status-cloning')?.textContent).toBe('cloning');
    expect(container.querySelector('.repo-head-remote')?.textContent).toBe(
      'git.cloonar.com/Cloonar/coding-lab',
    );

    const rows = Array.from(container.querySelectorAll<HTMLAnchorElement>('a.settings-index-row'));
    expect(rows.map((row) => row.getAttribute('href'))).toEqual(
      REPO_SETTINGS_CATEGORIES.map((c) => `${BASE}/${c.slug}`),
    );
    expect(rows.map((row) => row.querySelector('.settings-index-title')?.textContent)).toEqual([
      'General',
      'Integrations',
      'Branches',
      'Agents',
      'Runner',
      'Autoland',
      'Secrets',
      'Schedules',
      'Imports',
      'Danger zone',
    ]);
    // Danger: pinned last, danger class (icon + title tint in CSS).
    expect(rows[rows.length - 1]?.classList.contains('danger')).toBe(true);
    expect(rows.slice(0, -1).every((row) => !row.classList.contains('danger'))).toBe(true);

    // No redirect on mobile — the index is a real page here.
    expect(routerHistory.get()).toBe(BASE);
  });
});

describe('repo-settings desktop redirect', () => {
  it('bounces the bare index to the first category', async () => {
    setDesktop(true);
    await mountSettings(BASE);
    await settle();

    expect(routerHistory.get()).toBe(`${BASE}/general`);
    expect(container.querySelector('.settings-split')).not.toBeNull();
  });
});

describe('repo-settings in the repo home frame', () => {
  const settingsTab = () =>
    container.querySelector<HTMLAnchorElement>(
      `nav.repo-tabs a[href="/repos/${REPO_ID}/settings"]`,
    );

  it('the index carries no crumb trail: the frame names the repo, Settings is the current tab', async () => {
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('a.settings-index-row'), 'index rows');

    // The old "Repos / <name> / Settings" trail moved into the frame: the back
    // link returns to the list, the header names the repo.
    expect(container.querySelector('p.crumb')).toBeNull();
    expect(container.querySelector('a.back-link')?.getAttribute('href')).toBe('/repos');
    expect(container.querySelector('.repo-head h1')?.textContent).toBe('coding-lab');
    expect(settingsTab()?.getAttribute('aria-current')).toBe('page');
  });

  it('a section keeps the Settings tab current and links back to the index', async () => {
    await mountSettings(`${BASE}/agents`);
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agents section');

    expect(container.querySelector('p.crumb')).toBeNull();
    expect(settingsTab()?.getAttribute('aria-current')).toBe('page');
    // The mobile back head still returns to the category index.
    expect(container.querySelector('a.settings-back-link')?.getAttribute('href')).toBe(BASE);
    expect(container.querySelector('.settings-back-head h2')?.textContent).toBe('Agents');
  });

  it('the schedule editor URLs reach the area and open the Schedules section', async () => {
    for (const path of [`${BASE}/schedules/new`, `${BASE}/schedules/sched_1`]) {
      await mountSettings(path);
      await waitFor(() => buttonByText('+ Add schedule'), `schedules section at ${path}`);
      expect(routerHistory.get()).toBe(path); // no redirect away from the editor URL
      expect(settingsTab()?.getAttribute('aria-current')).toBe('page');
      unmount();
    }
  });
});

describe('repo-settings unsaved-changes guard', () => {
  it('in-app navigation while dirty asks; declining stays, confirming leaves', async () => {
    await mountSettings(`${BASE}/general`);
    const author = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="git_author_name"]'),
      'general form',
    );
    typeInto(author, 'Dominik'); // dirty

    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false);
    const back = container.querySelector<HTMLAnchorElement>('a.settings-back-link');
    expect(back).not.toBeNull();
    back?.click();
    await settle();

    expect(confirmSpy).toHaveBeenCalledWith('Discard unsaved changes?');
    expect(routerHistory.get()).toBe(`${BASE}/general`); // stayed put

    confirmSpy.mockReturnValue(true);
    back?.click();
    await settle();

    expect(routerHistory.get()).toBe(BASE); // left for the index
  });

  it('beforeunload is prevented only while dirty', async () => {
    await mountSettings(`${BASE}/general`);
    const author = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="git_author_name"]'),
      'general form',
    );

    // Clean: the tab may close freely.
    const cleanEvent = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(cleanEvent);
    expect(cleanEvent.defaultPrevented).toBe(false);

    typeInto(author, 'Dominik'); // dirty

    const dirtyEvent = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(dirtyEvent);
    expect(dirtyEvent.defaultPrevented).toBe(true);
  });
});
