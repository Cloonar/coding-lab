// SettingsLayout contract (issue #198): mobile bare index = tappable category
// rows (danger styled + last), each carrying its vendored Icon at size 20 and
// aria-hidden (ADR-0019, issue #199 — the row's text is the accessible name);
// mobile section = back header + children; desktop bare index = redirect to
// the first slug — including LIVE when the viewport grows to desktop while the
// index sits open; unknown slug = bounce to base (desktop re-redirects to the
// first slug); desktop section = master-detail with a plain-text nav (no
// icons), active link marked.

import { MemoryRouter, Route, createMemoryHistory, useParams } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { MemoryHistory } from '@solidjs/router';
import type { SettingsCategory } from './categories';
import SettingsLayout from './SettingsLayout';

// --- controllable matchMedia fake (harness stubMatchMedia idiom, single
// query): `matches` is a live getter so the change listener re-reads it. ---

let desktopMatches = false;
const changeListeners = new Set<() => void>();

function stubMatchMedia(desktop: boolean): void {
  desktopMatches = desktop;
  changeListeners.clear();
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      get matches() {
        return desktopMatches;
      },
      media: query,
      addEventListener: (type: string, listener: () => void) => {
        if (type === 'change') changeListeners.add(listener);
      },
      removeEventListener: (_type: string, listener: () => void) => {
        changeListeners.delete(listener);
      },
      onchange: null,
      dispatchEvent: () => false,
    })),
  );
}

/** Flip the fake and fire its change listeners — a live viewport resize. */
function setDesktop(matches: boolean): void {
  desktopMatches = matches;
  for (const listener of [...changeListeners]) listener();
}

// --- fixtures ---

// Real vendored icon names (the ones the shipped registries use for these
// categories): the layout renders the actual glyphs, so there is nothing to
// stub.
const CATEGORIES: SettingsCategory[] = [
  {
    slug: 'general',
    title: 'General',
    description: 'Spawn defaults and identity.',
    icon: 'settings-2',
  },
  {
    slug: 'notifications',
    title: 'Notifications',
    description: 'Push devices for this account.',
    icon: 'bell',
  },
  {
    slug: 'danger',
    title: 'Danger zone',
    description: 'Delete things for good.',
    icon: 'triangle-alert',
    danger: true,
  },
];

function Index() {
  return (
    <SettingsLayout
      base="/settings"
      categories={CATEGORIES}
      indexTitle={<h2 class="test-index-title">Settings</h2>}
    />
  );
}

function Section() {
  const params = useParams<{ section: string }>();
  return (
    <SettingsLayout base="/settings" categories={CATEGORIES} section={params.section}>
      <p class="test-section-content">content: {params.section}</p>
    </SettingsLayout>
  );
}

let dispose: (() => void) | undefined;
let container: HTMLDivElement;
let history: MemoryHistory;

function mount(path: string): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  history = createMemoryHistory();
  history.set({ value: path });
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route path="/settings" component={Index} />
        <Route path="/settings/:section" component={Section} />
      </MemoryRouter>
    ),
    container,
  );
}

/** Two macrotasks: enough for a chained redirect (unknown slug → base → first). */
async function settle(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  changeListeners.clear();
  vi.unstubAllGlobals();
});

describe('SettingsLayout', () => {
  it('mobile bare index: rows in order, linked to base/slug, danger styled and last', async () => {
    stubMatchMedia(false);
    mount('/settings');
    await settle();

    expect(history.get()).toBe('/settings'); // no redirect on mobile
    expect(container.querySelector('.test-index-title')?.textContent).toBe('Settings');

    const rows = Array.from(container.querySelectorAll<HTMLAnchorElement>('a.settings-index-row'));
    expect(rows.map((row) => row.getAttribute('href'))).toEqual([
      '/settings/general',
      '/settings/notifications',
      '/settings/danger',
    ]);
    expect(rows.map((row) => row.querySelector('.settings-index-title')?.textContent)).toEqual([
      'General',
      'Notifications',
      'Danger zone',
    ]);
    // The row icon: size 20, no color prop (currentColor tints it through the
    // class), and aria-hidden — the row's own text is the accessible name.
    const rowIcon = rows[0]?.querySelector('svg.settings-index-icon');
    expect(rowIcon).not.toBeNull();
    expect(rowIcon?.getAttribute('width')).toBe('20');
    expect(rowIcon?.getAttribute('height')).toBe('20');
    expect(rowIcon?.getAttribute('aria-hidden')).toBe('true');
    // Danger: last row, danger class (icon + title tint in CSS).
    expect(rows[2]?.classList.contains('danger')).toBe(true);
    expect(rows[0]?.classList.contains('danger')).toBe(false);
  });

  it('mobile section: back header links to base, children rendered', async () => {
    stubMatchMedia(false);
    mount('/settings/notifications');
    await settle();

    const back = container.querySelector<HTMLAnchorElement>('a.settings-back-link');
    expect(back?.getAttribute('href')).toBe('/settings');
    expect(container.querySelector('.settings-back-head h2')?.textContent).toBe('Notifications');
    expect(container.querySelector('.test-section-content')?.textContent).toBe(
      'content: notifications',
    );
    expect(container.querySelector('.settings-split')).toBeNull();
  });

  it('desktop bare index redirects to the first slug', async () => {
    stubMatchMedia(true);
    mount('/settings');
    await settle();

    expect(history.get()).toBe('/settings/general');
    expect(container.querySelector('.test-section-content')?.textContent).toBe('content: general');
  });

  it('desktop unknown slug lands on base/first-slug (via the index redirect)', async () => {
    stubMatchMedia(true);
    mount('/settings/never-heard-of-it');
    await settle();

    expect(history.get()).toBe('/settings/general');
  });

  it('growing to desktop while on the bare index redirects live', async () => {
    stubMatchMedia(false);
    mount('/settings');
    await settle();
    expect(history.get()).toBe('/settings');
    expect(container.querySelectorAll('a.settings-index-row')).toHaveLength(3);

    setDesktop(true);
    await settle();
    expect(history.get()).toBe('/settings/general');
  });

  it('desktop section: plain-text nav with the active link marked, children rendered', async () => {
    stubMatchMedia(true);
    mount('/settings/notifications');
    await settle();

    expect(container.querySelector('.settings-split')).not.toBeNull();
    const nav = container.querySelector('nav.settings-split-nav');
    const links = Array.from(
      nav?.querySelectorAll<HTMLAnchorElement>('a.settings-split-link') ?? [],
    );
    expect(links.map((link) => [link.textContent, link.getAttribute('href')])).toEqual([
      ['General', '/settings/general'],
      ['Notifications', '/settings/notifications'],
      ['Danger zone', '/settings/danger'],
    ]);
    expect(links.map((link) => link.classList.contains('active'))).toEqual([false, true, false]);
    expect(links[2]?.classList.contains('danger')).toBe(true);
    expect(nav?.querySelector('svg')).toBeNull(); // plain text — no icons
    expect(container.querySelector('.test-section-content')?.textContent).toBe(
      'content: notifications',
    );
    expect(container.querySelector('.settings-back-head')).toBeNull();
  });
});
