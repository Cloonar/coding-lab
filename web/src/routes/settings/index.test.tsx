// Global settings route wiring (issue #198): metadata↔route parity (every
// category slug deep-links to its section), the mobile index rows, the desktop
// bare-index redirect, the unknown-slug bounce, and the unsaved-changes guard
// on an in-app leave. GLOBAL_SETTINGS_CATEGORIES is the single source, so this
// walks it directly — a new category can't skip the routing contract.

import { describe, expect, it, vi } from 'vitest';
import { GLOBAL_SETTINGS_CATEGORIES } from './categories';
import {
  cardByHeadingOrNull,
  container,
  history,
  input,
  installSettingsHooks,
  mountAt,
  settle,
  stubDesktop,
  typeInto,
  unmount,
  waitFor,
} from './harness';

installSettingsHooks();

// A distinctive section-card heading per slug — proof the deep link rendered
// that section's own content (not just the layout chrome).
const SECTION_CARD: Record<string, string> = {
  general: 'Git author',
  agents: 'Spawn defaults',
  notifications: 'Notifications',
};

describe('Settings route wiring (issue #198)', () => {
  it('deep-links every category slug to its section (metadata ↔ route parity)', async () => {
    for (const cat of GLOBAL_SETTINGS_CATEGORIES) {
      const heading = SECTION_CARD[cat.slug];
      if (heading === undefined) throw new Error(`no section-card marker for slug ${cat.slug}`);
      await mountAt(`/settings/${cat.slug}`);
      await waitFor(() => cardByHeadingOrNull(heading), `${cat.slug} section card`);
      expect(cardByHeadingOrNull(heading)).not.toBeNull();
      unmount();
    }
  });

  it('mobile bare /settings renders one index row per category, in order', async () => {
    await mountAt('/settings');
    // jsdom ships no matchMedia → createMediaQuery reads mobile: no redirect.
    expect(history.get()).toBe('/settings');
    const rows = Array.from(container.querySelectorAll<HTMLAnchorElement>('a.settings-index-row'));
    expect(rows.map((r) => r.getAttribute('href'))).toEqual(
      GLOBAL_SETTINGS_CATEGORIES.map((c) => `/settings/${c.slug}`),
    );
    expect(rows.map((r) => r.querySelector('.settings-index-title')?.textContent)).toEqual(
      GLOBAL_SETTINGS_CATEGORIES.map((c) => c.title),
    );
  });

  it('desktop bare /settings redirects to the first slug (general)', async () => {
    stubDesktop(true);
    await mountAt('/settings');
    await settle();
    expect(history.get()).toBe('/settings/general');
    expect(cardByHeadingOrNull('Git author')).not.toBeNull();
  });

  it('an unknown slug redirects back to the index', async () => {
    await mountAt('/settings/never-heard-of-it');
    await settle();
    expect(history.get()).toBe('/settings');
    // The mobile index rows are back — one per category.
    expect(container.querySelectorAll('a.settings-index-row')).toHaveLength(
      GLOBAL_SETTINGS_CATEGORIES.length,
    );
  });

  it('guards an in-app leave while a General field is dirty', async () => {
    await mountAt('/settings/general');
    await waitFor(
      () => container.querySelector('input[name="git_author_name"]'),
      'git author card',
    );
    typeInto(input('git_author_name'), 'Changed Name');

    const back = () => container.querySelector<HTMLAnchorElement>('a.settings-back-link');
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);

    // confirm=false: the guard holds, the route stays put.
    back()!.click();
    await settle();
    expect(confirm).toHaveBeenCalledWith('Discard unsaved changes?');
    expect(history.get()).toBe('/settings/general');

    // beforeunload while dirty is armed (defaultPrevented) by the same guard.
    const evt = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(evt);
    expect(evt.defaultPrevented).toBe(true);

    // confirm=true: the guard retries past itself, the route changes.
    confirm.mockReturnValue(true);
    back()!.click();
    await settle();
    expect(history.get()).toBe('/settings');
  });
});
