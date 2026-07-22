// Repo-settings area wiring (issue #198): every category slug deep-links to
// its section; the mobile bare index lists the rows in order (danger last,
// tinted) under the repo section-head; desktop redirects the bare index to
// the first category; the crumbs follow the index/section split (Settings
// inert on the index, a link back to it on a section); the per-section
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
    autoland: () => container.querySelector('input[name="autoland_enabled"]'),
    secrets: () => buttonByText('+ Add secret'),
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

    // The monolith's section-head survives as the index title: repo name h2,
    // clone-status chip (baseRepo is mid-clone) and the remote host line.
    expect(
      Array.from(container.querySelectorAll('h2')).some((el) => el.textContent === 'coding-lab'),
    ).toBe(true);
    expect(container.querySelector('.section-head .chip')?.textContent).toBe('cloning');
    expect(container.textContent).toContain('git.cloonar.com');

    const rows = Array.from(container.querySelectorAll<HTMLAnchorElement>('a.settings-index-row'));
    expect(rows.map((row) => row.getAttribute('href'))).toEqual(
      REPO_SETTINGS_CATEGORIES.map((c) => `${BASE}/${c.slug}`),
    );
    expect(rows.map((row) => row.querySelector('.settings-index-title')?.textContent)).toEqual([
      'General',
      'Integrations',
      'Branches',
      'Agents',
      'Autoland',
      'Secrets',
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

describe('repo-settings crumbs', () => {
  it('the index shows Repos / <name> / Settings with an inert Settings leaf', async () => {
    await mountSettings(BASE);
    await waitFor(() => container.querySelector('a.settings-index-row'), 'index rows');

    const crumb = container.querySelector('p.crumb');
    expect(crumb?.textContent).toBe('Repos / coding-lab / Settings');
    const links = Array.from(crumb?.querySelectorAll('a') ?? []);
    expect(links.map((a) => [a.textContent, a.getAttribute('href')])).toEqual([
      ['Repos', '/repos'],
      ['coding-lab', `/repos/${REPO_ID}/issues`],
    ]);
  });

  it('a section appends its title and turns Settings into a link to the index', async () => {
    await mountSettings(`${BASE}/agents`);
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agents section');

    const crumb = container.querySelector('p.crumb');
    expect(crumb?.textContent).toBe('Repos / coding-lab / Settings / Agents');
    const links = Array.from(crumb?.querySelectorAll('a') ?? []);
    expect(links.map((a) => [a.textContent, a.getAttribute('href')])).toEqual([
      ['Repos', '/repos'],
      ['coding-lab', `/repos/${REPO_ID}/issues`],
      ['Settings', BASE],
    ]);
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
