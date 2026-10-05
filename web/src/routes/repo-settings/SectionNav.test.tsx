// The settings page's section navigation (issue #61): chips below 1024px, the
// outline with its group labels from 1024px — never both. Both mark the
// section in view (aria-current, driven by the page's scroll position through
// the layout seam) and a section with pending changes (a dot plus the words
// "unsaved changes"), and an entry jumps to its section, replacing the URL
// without a history entry and without ever asking to leave.

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  container,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  openDialog,
  routerHistory,
  save,
  scrollPage,
  setDesktop,
  settle,
  typeInto,
  waitFor,
} from './harness';

installRepoSettingsHooks();

const BASE = `/repos/${REPO_ID}/settings`;

const waitForPage = () => waitFor(() => container.querySelector('#settings-danger'), 'the page');
const chips = () => container.querySelector<HTMLElement>('nav.settings-chips');
const outline = () => container.querySelector<HTMLElement>('nav.settings-outline');
const nav = (): HTMLElement => {
  const el = chips() ?? outline();
  if (!el) throw new Error('missing section navigation');
  return el;
};
const entries = () => Array.from(nav().querySelectorAll<HTMLAnchorElement>('a[data-section]'));
const entry = (slug: string): HTMLAnchorElement => {
  const el = nav().querySelector<HTMLAnchorElement>(`a[data-section="${slug}"]`);
  if (!el) throw new Error(`missing nav entry "${slug}"`);
  return el;
};
/** The slugs marked as the section in view (there must be exactly one). */
const current = () =>
  entries()
    .filter((a) => a.getAttribute('aria-current') === 'location')
    .map((a) => a.dataset.section);
/** The slugs marked as holding unsaved changes. */
const marked = () =>
  entries()
    .filter((a) => a.querySelector('.settings-nav-dot') !== null)
    .map((a) => a.dataset.section);

const ORDER = [
  'agents',
  'runner',
  'autoland',
  'schedules',
  'secrets',
  'imports',
  'general',
  'integrations',
  'branches',
  'danger',
];

describe('settings section navigation by breakpoint', () => {
  it('below 1024px: a row of chips, one per section in page order, and no outline', async () => {
    await mountSettings(BASE);
    await waitForPage();

    expect(chips()).not.toBeNull();
    expect(outline()).toBeNull();
    expect(chips()?.getAttribute('aria-label')).toBe('Settings sections');
    expect(entries().map((a) => a.dataset.section)).toEqual(ORDER);
    expect(entries().map((a) => a.getAttribute('href'))).toEqual(
      ORDER.map((slug) => `${BASE}/${slug}`),
    );
    // Chips carry no group labels.
    expect(chips()?.textContent).not.toContain('Automation');
    expect(entry('danger').classList.contains('danger')).toBe(true);
  });

  it('from 1024px: the outline with its group labels replaces the chips', async () => {
    setDesktop(true);
    await mountSettings(BASE);
    await waitForPage();

    expect(outline()).not.toBeNull();
    expect(chips()).toBeNull();
    expect(entries().map((a) => a.dataset.section)).toEqual(ORDER);

    const groups = Array.from(outline()?.querySelectorAll('.settings-outline-group') ?? []);
    expect(
      groups.map((group) => [
        group.querySelector('.settings-outline-label')?.textContent ?? null,
        Array.from(group.querySelectorAll('a')).map((a) => a.dataset.section),
      ]),
    ).toEqual([
      ['Runs', ['agents', 'runner']],
      ['Automation', ['autoland', 'schedules']],
      ['Access', ['secrets', 'imports']],
      ['Setup', ['general', 'integrations', 'branches']],
      [null, ['danger']],
    ]);
    // A labelled group is named by its label.
    const runs = groups[0];
    expect(runs?.getAttribute('role')).toBe('group');
    expect(runs?.getAttribute('aria-labelledby')).toBe(
      runs?.querySelector('.settings-outline-label')?.id,
    );
  });

  it('crossing the breakpoint swaps one for the other, live', async () => {
    setDesktop(false); // installs the matchMedia fake the page then listens to
    await mountSettings(BASE);
    await waitForPage();
    expect(chips()).not.toBeNull();

    setDesktop(true);
    await settle();
    expect(chips()).toBeNull();
    expect(outline()).not.toBeNull();

    setDesktop(false);
    await settle();
    expect(outline()).toBeNull();
    expect(chips()).not.toBeNull();
  });
});

for (const desktop of [false, true]) {
  const kind = desktop ? 'outline' : 'chips';

  describe(`settings ${kind}: the section in view`, () => {
    it('marks the first section at the top of the page', async () => {
      setDesktop(desktop);
      await mountSettings(BASE);
      await waitForPage();

      expect(current()).toEqual(['agents']);
    });

    it('follows the scroll position: the last section whose top reached the line', async () => {
      setDesktop(desktop);
      await mountSettings(BASE);
      await waitForPage();

      // Agents and Runner have scrolled past; Autoland has not reached the top.
      await scrollPage({
        'settings-agents': -900,
        'settings-runner': -40,
        'settings-autoland': 600,
      });
      expect(current()).toEqual(['runner']);

      await scrollPage({
        'settings-agents': -2400,
        'settings-runner': -1500,
        'settings-autoland': -900,
        'settings-schedules': -300,
        'settings-secrets': 5,
        'settings-imports': 700,
      });
      expect(current()).toEqual(['secrets']);

      // The end of the page: the short last section can never reach the line.
      await scrollPage({ 'settings-agents': -5000, 'settings-branches': -300 }, true);
      expect(current()).toEqual(['danger']);
    });
  });

  describe(`settings ${kind}: pending changes`, () => {
    it('marks each section with unsaved changes, in words too, and clears on save', async () => {
      setDesktop(desktop);
      await mountSettings(BASE);
      await waitForPage();
      expect(marked()).toEqual([]);

      typeInto(input('budget_minutes'), '90');
      typeInto(input('afk_branch_pattern'), 'issue-<N>');
      await settle();

      expect(marked()).toEqual(['agents', 'branches']);
      expect(entry('agents').textContent).toBe('Agents (unsaved changes)');
      expect(entry('branches').textContent).toBe('Branches (unsaved changes)');
      expect(entry('runner').textContent).toBe('Runner');

      await save();
      expect(marked()).toEqual([]);
      expect(entry('agents').textContent).toBe('Agents');
    });
  });

  describe(`settings ${kind}: jumping to a section`, () => {
    it('scrolls there and replaces the URL — no history entry, no leave prompt', async () => {
      const confirmSpy = vi.spyOn(window, 'confirm');
      setDesktop(desktop);
      await mountSettings(BASE);
      await waitForPage();
      typeInto(input('budget_minutes'), '90'); // pending changes must not matter
      await settle();

      entry('secrets').click();
      await settle();

      expect(h.scrolls).toEqual([
        expect.objectContaining({ target: 'settings-secrets', smooth: true }),
      ]);
      expect(routerHistory.get()).toBe(`${BASE}/secrets`);

      entry('branches').click();
      await settle();
      expect(h.scrolls.map((scroll) => scroll.target)).toEqual([
        'settings-secrets',
        'settings-branches',
      ]);
      expect(routerHistory.get()).toBe(`${BASE}/branches`);

      // The same entry again still scrolls (the URL has nothing to change).
      entry('branches').click();
      await settle();
      expect(h.scrolls).toHaveLength(3);

      // Replaced, not pushed: one step back leaves the settings altogether —
      // to the entry the page was opened from (the memory history's "/").
      routerHistory.go(-1);
      await settle();
      expect(routerHistory.get()).toBe('/');

      expect(openDialog()).toBeNull();
      expect(confirmSpy).not.toHaveBeenCalled();
    });

    it('keeps the entry it was sent to marked until the operator scrolls', async () => {
      setDesktop(desktop);
      await mountSettings(BASE);
      await waitForPage();

      // Branches sits too close to the end of the page to climb to the line:
      // the layout alone would mark Danger zone.
      h.tops = { 'settings-agents': -5000, 'settings-branches': 300 };
      h.pageAtEnd = true;
      entry('branches').click();
      await settle();
      expect(current()).toEqual(['branches']);

      // The page's own (animated) scroll does not hand the mark over...
      window.dispatchEvent(new Event('scroll'));
      await settle();
      expect(current()).toEqual(['branches']);

      // ...the operator's scroll does.
      window.dispatchEvent(new Event('wheel'));
      await scrollPage({ 'settings-agents': -5000, 'settings-branches': 300 }, true);
      expect(current()).toEqual(['danger']);
    });

    it('leaves a modified click to the browser', async () => {
      setDesktop(desktop);
      await mountSettings(BASE);
      await waitForPage();

      const event = new MouseEvent('click', { bubbles: true, cancelable: true, ctrlKey: true });
      entry('runner').dispatchEvent(event);
      await settle();

      // Neither the page nor the router acted: the browser opens the link.
      expect(event.defaultPrevented).toBe(false);
      expect(h.scrolls).toEqual([]);
      expect(routerHistory.get()).toBe(BASE);
    });
  });
}
