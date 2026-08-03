// The per-repo settings categories (issue #198): the subpages of
// /repos/:id/settings/:section, in display order. SettingsLayout renders the
// mobile index rows and the desktop master-detail nav from this list. Danger
// is pinned last (danger: true tints it via CSS). Icon values are vendored
// Icon names — the app icon system (ADR-0019, issue #199).

import type { SettingsCategory } from '../../components/settings/categories';

export const REPO_SETTINGS_CATEGORIES: SettingsCategory[] = [
  {
    slug: 'general',
    title: 'General',
    description: 'Name, git author identity, Incogni.',
    icon: 'settings-2',
  },
  {
    slug: 'integrations',
    title: 'Integrations',
    description: 'Git credential, tracker binding, forge token.',
    icon: 'plug',
  },
  {
    slug: 'branches',
    title: 'Branches',
    description: 'Default branch and branch naming.',
    icon: 'git-branch',
  },
  {
    slug: 'agents',
    title: 'Agents',
    description: 'Spawn and AFK defaults, auto-spawn, budget.',
    icon: 'bot',
  },
  {
    slug: 'runner',
    title: 'Runner',
    description: 'Container or host execution, resource limits.',
    icon: 'container',
  },
  {
    slug: 'autoland',
    title: 'Autoland',
    description: 'Lander validation and merge policy for claim PRs.',
    icon: 'plane-landing',
  },
  {
    slug: 'secrets',
    title: 'Secrets',
    description: 'Write-only secrets for agents.',
    icon: 'lock-keyhole',
  },
  {
    slug: 'schedules',
    title: 'Schedules',
    description: 'Cron-fired scheduled runs: prompt, flows, cadence.',
    icon: 'calendar-clock',
  },
  {
    slug: 'imports',
    title: 'Imports',
    description: "Other lab repos this repo's instances may read as read-only snapshots.",
    icon: 'folder-input',
  },
  {
    slug: 'danger',
    title: 'Danger zone',
    description: 'Delete this repository.',
    icon: 'triangle-alert',
    danger: true,
  },
];
