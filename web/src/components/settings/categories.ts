// Settings category descriptor (issue #198): both settings areas (global
// /settings and per-repo /repos/:id/settings) declare their sections as a
// SettingsCategory[] — the one list a settings page reads for a section's
// title, its one-line description, its icon, its group label (the desktop
// outline prints it, SectionNav.tsx) and whether its rows act at once (the
// "applies immediately" tag, SettingsSection.tsx). The vendored Icon
// component is the app icon system (ADR-0019, issue #199), so an icon VALUE is
// just its registry name — a string literal from IconName, resolved to a glyph
// by <Icon> at render time, not a component imported per category.

import type { IconName } from '../Icon';

export interface SettingsCategory<Group extends string = string> {
  slug: string;
  title: string;
  /** One line, shown under the section heading (and in the mobile index rows). */
  description: string;
  /** A vendored icon name, rendered as <Icon name={...} />. */
  icon: IconName;
  /** Danger category: pinned last by convention, rendered in --danger red. */
  danger?: boolean;
  /**
   * The group the outline lists the section under; null or absent =
   * ungrouped (the repo page's Danger zone).
   */
  group?: Group | null;
  /**
   * True for a section whose rows are independent objects with their own
   * server calls (the repo page's Schedules, Secrets, Imports): they apply at
   * once and never wait for the page's save bar, so the heading carries a tag
   * saying so — and no field of the form lives there.
   */
  immediate?: boolean;
}

/** The section a slug names in `categories`, or undefined for an unknown one. */
export function findCategory<C extends SettingsCategory>(
  categories: readonly C[],
  slug: string | undefined,
): C | undefined {
  return categories.find((category) => category.slug === slug);
}

/** A section's DOM id — the page's scroll targets and the chips' anchors. */
export function sectionElementId(slug: string): string {
  return `settings-${slug}`;
}
