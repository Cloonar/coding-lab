// Settings category descriptor (issue #198): both settings areas (global
// /settings and per-repo /repos/:id/settings) declare their subpages as a
// SettingsCategory[] and hand it to SettingsLayout, which renders the mobile
// index rows and the desktop master-detail nav from it. The vendored Icon
// component is the app icon system (ADR-0019, issue #199), so an icon VALUE is
// just its registry name — a string literal from IconName, resolved to a glyph
// by <Icon> at render time, not a component imported per category.

import type { IconName } from '../Icon';

export interface SettingsCategory {
  slug: string;
  title: string;
  /** One line, shown in the mobile index rows. */
  description: string;
  /** A vendored icon name, rendered as <Icon name={...} />. */
  icon: IconName;
  /** Danger category: pinned last by convention, rendered in --danger red. */
  danger?: boolean;
}
