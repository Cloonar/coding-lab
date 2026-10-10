// Pure route rules for the phone's bottom tab bar (issue #76). The bar shows
// below 1024px on every authenticated page except the immersive ones, and
// lights one of its four tabs from the path prefix so a sub-page keeps its
// section's tab lit. Kept free of Solid and the DOM so the rules — which tab a
// path belongs to, where the bar hides, where a re-tap goes — are pinned by a
// plain unit suite rather than through a rendered shell.

export type TabId = 'runs' | 'new' | 'repos' | 'more';

/** Each tab's section root: where a tap (or a re-tap from a sub-page) goes. */
export const TAB_ROOTS: Record<TabId, string> = {
  runs: '/',
  new: '/new',
  repos: '/repos',
  more: '/more',
};

/** `path` is `root` itself or a sub-path of it (`/repos` → `/repos/x`, not `/reposx`). */
function under(path: string, root: string): boolean {
  return path === root || path.startsWith(`${root}/`);
}

/** Drops trailing slashes (but keeps the bare `/`), so `/repos/` reads as `/repos`. */
function normalize(pathname: string): string {
  const trimmed = pathname.replace(/\/+$/, '');
  return trimmed === '' ? '/' : trimmed;
}

/**
 * The tab a path belongs to: `/`, `/history` → runs; `/new` → new; `/repos…` →
 * repos; `/credentials`, `/tokens`, `/settings…`, `/more` → more. Anything else
 * (the chat, an unknown path) lights no tab.
 */
export function activeTab(pathname: string): TabId | null {
  const path = normalize(pathname);
  if (path === '/' || path === '/history') return 'runs';
  if (path === '/new') return 'new';
  if (under(path, '/repos')) return 'repos';
  if (path === '/credentials' || path === '/tokens' || path === '/more' || under(path, '/settings'))
    return 'more';
  return null;
}

/**
 * True where the bar must not show: the Chat (`/runs/:id`) and the Schedule
 * editor (`/repos/:id/settings/schedules/new` and `…/schedules/:scheduleId`),
 * both immersive with their own back control. The schedules SECTION
 * (`/repos/:id/settings/schedules`) is an ordinary settings page and keeps it.
 */
export function tabBarHidden(pathname: string): boolean {
  const path = normalize(pathname);
  if (/^\/runs\/[^/]+/.test(path)) return true;
  return /^\/repos\/[^/]+\/settings\/schedules\/[^/]+$/.test(path);
}

/**
 * True while the bar slides out of the way of the on-screen keyboard (issue
 * #97, ADR-0083): on New run only, while `keyboardOpen` (lib/visualViewport's
 * createKeyboardOpen) reads true. Every other tab-bar page keeps the bar while
 * typing — a general rule would move the repo settings save bar under the
 * typist's thumb. Not a route rule of its own: tabBarHidden stays the one place
 * that decides where the bar exists; this only moves a bar that does.
 */
export function tabBarSlidesOut(pathname: string, keyboardOpen: boolean): boolean {
  return keyboardOpen && normalize(pathname) === '/new';
}
