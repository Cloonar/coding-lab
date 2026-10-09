// Global settings on one page (issue #85): all four sections render together,
// in the order, groups and words of GLOBAL_SETTINGS_CATEGORIES, under section
// chips below 1024px or an outline with the group labels from 1024px — no
// category index, no redirect; every section slug of issue #198 still deep-
// links, to the page scrolled to that section (the route/metadata parity
// check walks the list itself, so a new category cannot skip it); an unknown
// slug stays at the top; `?field=` scrolls a field into view and focuses it;
// moving between sections never remounts the page; and leaving with pending
// changes asks in the in-page dialog, never a browser confirm.
//
// jsdom has no layout, so "scrolled to" is asserted through the page's layout
// seam: the harness records every scroll the page asks for in `h.scrolls`.

import { describe, expect, it, vi } from 'vitest';
import { GLOBAL_SETTINGS_CATEGORIES, settingsSummary } from './categories';
import {
  container,
  dialogButton,
  fieldError,
  followLink,
  h,
  history,
  input,
  installSettingsHooks,
  leaveToOther,
  mountAt,
  mountPage,
  openDialog,
  pageSection,
  saveBar,
  saveBarTitle,
  setDesktop,
  settle,
  typeField,
  unmount,
  waitFor,
} from './harness';

installSettingsHooks();

/** The page arrived at `target` (a section id or a field key), at once. */
function expectArrivedAt(target: string): void {
  expect(h.scrolls.length).toBeGreaterThan(0);
  expect(new Set(h.scrolls.map((scroll) => scroll.target))).toEqual(new Set([target]));
  expect(h.scrolls.every((scroll) => !scroll.smooth)).toBe(true);
}

// One DOM probe per section: an element only that section renders.
const probes: Record<string, () => Element | null> = {
  agents: () => container.querySelector('button[name="spawn_provider_default_afk"]'),
  runner: () => container.querySelector('button[role="radio"][name="runner_default"]'),
  general: () => container.querySelector('input[name="git_author_name"]'),
  notifications: () =>
    Array.from(container.querySelectorAll('section.card h2')).find(
      (el) => el.textContent === 'Notifications',
    ) ?? null,
};

const chips = (): HTMLAnchorElement[] =>
  Array.from(container.querySelectorAll<HTMLAnchorElement>('nav.settings-chips a.settings-chip'));

describe('global settings on one page', () => {
  it('probes cover exactly the declared sections', () => {
    expect(Object.keys(probes).sort()).toEqual(
      GLOBAL_SETTINGS_CATEGORIES.map((c) => c.slug).sort(),
    );
  });

  it('renders Agents, Runner, General and Notifications in order, grouped Runs, Setup, This device', async () => {
    await mountPage();

    const sections = Array.from(container.querySelectorAll('section.settings-section'));
    expect(sections.map((section) => section.id)).toEqual([
      'settings-agents',
      'settings-runner',
      'settings-general',
      'settings-notifications',
    ]);
    expect(
      sections.map((section) => section.querySelector('.settings-section-head h2')?.textContent),
    ).toEqual(['Agents', 'Runner', 'General', 'Notifications']);
    expect(GLOBAL_SETTINGS_CATEGORIES.map((c) => c.group)).toEqual([
      'Runs',
      'Runs',
      'Setup',
      'This device',
    ]);
    for (const category of GLOBAL_SETTINGS_CATEGORIES) {
      const section = pageSection(category.slug);
      await waitFor(() => probes[category.slug]?.() ?? null, `${category.slug} content`);
      expect(section.contains(probes[category.slug]?.() ?? null)).toBe(true);
      expect(section.getAttribute('aria-labelledby')).toBe(`settings-${category.slug}-title`);
      expect(section.querySelector('.settings-section-head p')?.textContent).toBe(
        category.description,
      );
    }
    // The page heading, and no trace of the old category index.
    expect(container.querySelector('main.global-settings > .section-head h2')?.textContent).toBe(
      'Settings',
    );
    expect(container.querySelector('a.settings-index-row')).toBeNull();
    expect(history.get()).toBe('/settings');
  });

  it('tags Notifications, and only Notifications, as applying immediately', async () => {
    await mountPage();

    const tagged = GLOBAL_SETTINGS_CATEGORIES.filter(
      (c) => pageSection(c.slug).querySelector('.settings-tag') !== null,
    ).map((c) => c.slug);
    expect(tagged).toEqual(['notifications']);
    expect(pageSection('notifications').querySelector('.settings-tag')?.textContent).toBe(
      'applies immediately',
    );
  });

  it('keeps the More tab hint in its own order', () => {
    expect(settingsSummary()).toBe('General · Agents · Notifications · Runner');
  });

  it('below 1024px: one chip per section, in page order, and no outline', async () => {
    await mountPage();

    expect(chips().map((chip) => chip.textContent)).toEqual([
      'Agents',
      'Runner',
      'General',
      'Notifications',
    ]);
    expect(chips().map((chip) => chip.getAttribute('href'))).toEqual(
      GLOBAL_SETTINGS_CATEGORIES.map((c) => `/settings/${c.slug}`),
    );
    expect(container.querySelector('nav.settings-outline')).toBeNull();
    // The section in view is marked: at the top, the first one.
    expect(chips()[0]?.getAttribute('aria-current')).toBe('location');
  });

  it('from 1024px: the outline with the group labels replaces the chips — no redirect', async () => {
    setDesktop(true);
    await mountPage();

    expect(history.get()).toBe('/settings');
    expect(container.querySelector('nav.settings-chips')).toBeNull();
    const outline = container.querySelector('nav.settings-outline');
    expect(
      Array.from(outline?.querySelectorAll('.settings-outline-label') ?? []).map(
        (label) => label.textContent,
      ),
    ).toEqual(['Runs', 'Setup', 'This device']);
    expect(
      Array.from(outline?.querySelectorAll('.settings-outline-group') ?? []).map((group) =>
        Array.from(group.querySelectorAll('a')).map((a) => a.textContent),
      ),
    ).toEqual([['Agents', 'Runner'], ['General'], ['Notifications']]);
  });

  it('deep-links every section slug to the page scrolled to it (metadata ↔ route parity)', async () => {
    for (const category of GLOBAL_SETTINGS_CATEGORIES) {
      h.scrolls = [];
      await mountPage(`/settings/${category.slug}`);
      expectArrivedAt(`settings-${category.slug}`);
      expect(history.get()).toBe(`/settings/${category.slug}`);
      // The chip of that section is the one marked.
      expect(chips().find((chip) => chip.getAttribute('aria-current'))?.textContent).toBe(
        category.title,
      );
      unmount();
    }
  });

  it('an unknown slug opens the page at the top, without a redirect', async () => {
    await mountPage('/settings/never-heard-of-it');

    expect(h.scrolls).toEqual([]);
    expect(history.get()).toBe('/settings/never-heard-of-it');
    expect(container.querySelectorAll('section.settings-section')).toHaveLength(4);
  });

  it('?field= scrolls that field into view and focuses its control', async () => {
    await mountPage('/settings/runner?field=dev_image_default');

    expectArrivedAt('dev_image_default');
    expect(document.activeElement).toBe(input('dev_image_default'));
  });

  it('?field= wins over a mismatched section, and an unknown key is ignored', async () => {
    await mountPage('/settings/general?field=max_instances');
    expectArrivedAt('max_instances');
    expect(document.activeElement).toBe(input('max_instances'));
    unmount();

    h.scrolls = [];
    await mountPage('/settings/general?field=afk_prompt_default');
    expectArrivedAt('settings-general');
  });

  it('a chip scrolls to its section and replaces the URL; the page is not remounted', async () => {
    await mountPage();
    await typeField('git_author_name', 'Dominik');
    const field = input('git_author_name');

    await followLink(chips()[2]);

    expect(history.get()).toBe('/settings/general');
    expect(h.scrolls.at(-1)).toMatchObject({ target: 'settings-general', smooth: true });
    expect(chips()[2]?.getAttribute('aria-current')).toBe('location');
    // Same page, same control, same draft — and moving inside /settings
    // never asks about leaving.
    expect(input('git_author_name')).toBe(field);
    expect(field.value).toBe('Dominik');
    expect(openDialog()).toBeNull();

    await followLink(chips()[0]);
    expect(history.get()).toBe('/settings/agents');
    expect(input('git_author_name')).toBe(field);
  });

  it('says when the settings could not be loaded, and loads them on Try again', async () => {
    h.settingsError = 'database is locked';
    await mountAt('/settings');
    const banner = await waitFor(
      () => container.querySelector('main.global-settings > .banner'),
      'the load error',
    );
    expect(banner.textContent).toContain('The settings could not be loaded. database is locked');
    expect(container.querySelector('section.settings-section')).toBeNull();

    h.settingsError = null;
    Array.from(banner.querySelectorAll('button'))
      .find((b) => b.textContent === 'Try again')
      ?.click();
    await waitFor(() => container.querySelector('#settings-agents'), 'the page');
    expect(container.querySelector('main.global-settings > .banner')).toBeNull();
  });

  it('says when the agent catalog could not be loaded, with a retry', async () => {
    h.providersError = 'registry down';
    await mountPage();
    const banner = container.querySelector('.settings-sections > .banner');
    expect(banner?.textContent).toContain(
      'The agent catalog could not be loaded, so the agent, model and effort picks are incomplete. registry down',
    );

    h.providersError = null;
    Array.from(banner?.querySelectorAll('button') ?? [])
      .find((b) => b.textContent === 'Try again')
      ?.click();
    await settle();
    expect(container.querySelector('.settings-sections > .banner')).toBeNull();
    expect(container.querySelector('input[name="spawn_options_afk.ultracode"]')).not.toBeNull();
  });
});

describe('leaving /settings with pending changes', () => {
  async function mountWithChanges(): Promise<void> {
    await mountPage();
    await typeField('max_instances', '8');
    await typeField('git_author_name', 'Dominik');
  }

  it('opens the in-page dialog, holds the navigation and never uses a browser confirm', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    await mountWithChanges();

    await leaveToOther();

    const dialog = openDialog();
    expect(dialog?.getAttribute('role')).toBe('alertdialog');
    expect(dialog?.querySelector('.dialog-title')?.textContent).toBe(
      'Leave with 2 unsaved changes?',
    );
    expect(dialog?.textContent).toContain('Your changes to settings are not saved yet.');
    expect(Array.from(dialog?.querySelectorAll('button') ?? []).map((b) => b.textContent)).toEqual([
      'Keep editing',
      'Discard',
      'Save and leave',
    ]);
    expect(document.activeElement).toBe(dialogButton('Keep editing'));
    expect(history.get()).toBe('/settings');
    expect(h.patchBodies).toEqual([]);
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('Keep editing stays with every edit', async () => {
    await mountWithChanges();
    await leaveToOther();

    dialogButton('Keep editing').click();
    await settle();

    expect(openDialog()).toBeNull();
    expect(history.get()).toBe('/settings');
    expect(input('max_instances').value).toBe('8');
    expect(saveBarTitle()).toBe('2 unsaved changes');
  });

  it('Discard drops the edits and leaves, sending nothing', async () => {
    await mountWithChanges();
    await leaveToOther();

    dialogButton('Discard').click();
    await settle();

    expect(history.get()).toBe('/other');
    expect(container.querySelector('.other-page')).not.toBeNull();
    expect(h.patchBodies).toEqual([]);
  });

  it('Save and leave saves the pending changes in one PATCH, then leaves', async () => {
    await mountWithChanges();
    await leaveToOther();

    dialogButton('Save and leave').click();
    await settle();

    expect(h.patchBodies).toEqual([{ max_instances: 8, git_author_name: 'Dominik' }]);
    expect(history.get()).toBe('/other');
  });

  it('Save and leave stays on the page when the save fails, the problem at its field', async () => {
    await mountWithChanges();
    h.patchRefusal = { error: 'max_instances must be at least 1', field: 'max_instances' };
    await leaveToOther();

    dialogButton('Save and leave').click();
    await settle();

    expect(history.get()).not.toBe('/other');
    expect(openDialog()).toBeNull();
    expect(fieldError('max_instances')).toBe('max_instances must be at least 1');
    expect(input('git_author_name').value).toBe('Dominik');
    expect(saveBar()).not.toBeNull();
  });

  it('Save and leave stays when a browser check finds a problem, sending nothing', async () => {
    await mountPage();
    await typeField('max_instances', 'lots');
    await leaveToOther();

    dialogButton('Save and leave').click();
    await settle();

    expect(h.patchBodies).toEqual([]);
    expect(history.get()).not.toBe('/other');
    expect(fieldError('max_instances')).toBe('Use a whole number, 1 or more.');
  });

  it('a tab close or reload keeps the browser prompt while changes are pending', async () => {
    await mountPage();
    const clean = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(clean);
    expect(clean.defaultPrevented).toBe(false);

    await typeField('git_author_name', 'Dominik');
    const dirty = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(dirty);
    expect(dirty.defaultPrevented).toBe(true);
  });

  it('leaving with nothing pending never asks', async () => {
    await mountPage();
    await leaveToOther();
    expect(openDialog()).toBeNull();
    expect(history.get()).toBe('/other');
  });
});
