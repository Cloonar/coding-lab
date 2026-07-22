// Settings category descriptor (issue #198): both settings areas (global
// /settings and per-repo /repos/:id/settings) declare their subpages as a
// SettingsCategory[] and hand it to SettingsLayout, which renders the mobile
// index rows and the desktop master-detail nav from it. lucide-solid is the
// app icon system; icon VALUES are per-icon deep imports
// (`import Bell from 'lucide-solid/icons/bell'`) — the package root is
// type-only territory, hence the `import type` below.

import type { Component } from 'solid-js';
import type { LucideProps } from 'lucide-solid';

/** A lucide-solid icon component: size/class pass through, color inherits. */
export type SettingsIcon = Component<LucideProps>;

export interface SettingsCategory {
  slug: string;
  title: string;
  /** One line, shown in the mobile index rows. */
  description: string;
  /** A lucide-solid icon component (deep import). */
  icon: SettingsIcon;
  /** Danger category: pinned last by convention, rendered in --danger red. */
  danger?: boolean;
}
