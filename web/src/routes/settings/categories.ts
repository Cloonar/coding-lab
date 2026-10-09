// The global settings sections (issues #198, #85): every section renders on
// ONE page, in this order and under these groups —
//
//   Runs          Agents, Runner
//   Setup         General
//   This device   Notifications (its rows act at once: "applies immediately")
//
// — and /settings/:section opens the page scrolled to its section. The slugs
// are the ones issue #198 introduced: they are deep-link targets (bookmarks,
// the readiness Fix links) and must not change.
//
// This list is the page's one source for a section's title, its one-line
// description, its group label (the desktop outline prints it) and whether its
// rows act at once. The page (index.tsx) hands it to the shared settings core
// — the section chips and outline, the save bar's section links, the page's
// scroll logic — and the route/metadata parity test walks it directly. Icon
// values are vendored Icon names (ADR-0019, issue #199).
//
// The More tab's Settings row names the sections in its own, fixed order
// (settingsSummary below): that hint predates the page order and is pinned
// by ADR-0077's row, so reordering the page does not reorder it.

import { findCategory, type SettingsCategory } from '../../components/settings/categories';

/** The outline's group labels, in page order. */
export type GlobalSettingsGroup = 'Runs' | 'Setup' | 'This device';

/** A global settings section: every one names its group. */
export interface GlobalSettingsCategory extends SettingsCategory<GlobalSettingsGroup> {
  group: GlobalSettingsGroup;
}

export const GLOBAL_SETTINGS_CATEGORIES: GlobalSettingsCategory[] = [
  {
    slug: 'agents',
    title: 'Agents',
    description: 'Defaults for runs you start, AFK runs and the lander, and capacity.',
    icon: 'bot',
    group: 'Runs',
  },
  {
    slug: 'runner',
    title: 'Runner',
    description: 'Where runs execute by default, with which dev image and limits.',
    icon: 'container',
    group: 'Runs',
  },
  {
    slug: 'general',
    title: 'General',
    description: 'Git author identity, transcript retention, and integration status.',
    icon: 'settings-2',
    group: 'Setup',
  },
  {
    slug: 'notifications',
    title: 'Notifications',
    description: 'Push notifications and app install, for this device.',
    icon: 'bell',
    group: 'This device',
    immediate: true,
  },
];

/** The order the More tab's Settings row names the sections in. */
const SUMMARY_ORDER = ['general', 'agents', 'notifications', 'runner'] as const;

/** "General · Agents · Notifications · Runner" — the More tab's Settings hint. */
export function settingsSummary(): string {
  return SUMMARY_ORDER.map(
    (slug) => findCategory(GLOBAL_SETTINGS_CATEGORIES, slug)?.title ?? slug,
  ).join(' · ');
}
