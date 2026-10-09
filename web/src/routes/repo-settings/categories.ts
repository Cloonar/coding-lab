// The per-repo settings sections (issue #61): every section renders on ONE
// page, in this order, and /repos/:id/settings/:section opens the page
// scrolled to its section. The slugs are the ones issue #198 introduced —
// they are deep-link targets and must not change. Sections are ordered by how
// often they are touched, not by the data model: the set-once ones come last,
// and Danger zone is pinned at the very end, outside any group.
//
// This list is the page's one source for a section's title, its one-line
// description, its group label (the desktop outline prints it) and whether
// its rows act at once. The page (index.tsx) hands it to the shared settings
// core — the section chips and outline (components/settings/SectionNav.tsx),
// the page's scroll logic — and the repo home frame to the save bar, whose
// section links it names. Icon values are vendored Icon names (ADR-0019,
// issue #199).

import type { SettingsCategory } from '../../components/settings/categories';

/** The outline's group labels, in page order. */
export type RepoSettingsGroup = 'Runs' | 'Automation' | 'Access' | 'Setup';

/** A repo settings section: every one names its group (null = Danger zone). */
export interface RepoSettingsCategory extends SettingsCategory<RepoSettingsGroup> {
  group: RepoSettingsGroup | null;
}

export const REPO_SETTINGS_CATEGORIES: RepoSettingsCategory[] = [
  {
    slug: 'agents',
    title: 'Agents',
    description: 'Defaults for runs you start and for AFK runs.',
    icon: 'bot',
    group: 'Runs',
  },
  {
    slug: 'runner',
    title: 'Runner',
    description: 'Where instances run, and with which dev image and limits.',
    icon: 'container',
    group: 'Runs',
  },
  {
    slug: 'autoland',
    title: 'Autoland',
    description: 'Validation and merge policy for the PRs AFK runs open.',
    icon: 'plane-landing',
    group: 'Automation',
  },
  {
    slug: 'schedules',
    title: 'Schedules',
    description: 'Scheduled runs that start on a cadence, with your prompt.',
    icon: 'calendar-clock',
    group: 'Automation',
    immediate: true,
  },
  {
    slug: 'secrets',
    title: 'Secrets',
    description: 'What agents may use. Values are write-only.',
    icon: 'lock-keyhole',
    group: 'Access',
    immediate: true,
  },
  {
    slug: 'imports',
    title: 'Imports',
    description: 'Other lab repositories this one may read.',
    icon: 'folder-input',
    group: 'Access',
    immediate: true,
  },
  {
    slug: 'general',
    title: 'General',
    description: 'Name, git author identity, Incogni.',
    icon: 'settings-2',
    group: 'Setup',
  },
  {
    slug: 'integrations',
    title: 'Integrations',
    description: 'Git credential and tracker binding.',
    icon: 'plug',
    group: 'Setup',
  },
  {
    slug: 'branches',
    title: 'Branches',
    description: 'Default branch and branch naming.',
    icon: 'git-branch',
    group: 'Setup',
  },
  {
    slug: 'danger',
    title: 'Danger zone',
    description: 'Delete this repository.',
    icon: 'triangle-alert',
    group: null,
    danger: true,
  },
];
