// The per-repo settings categories (issue #198): the subpages of
// /repos/:id/settings/:section, in display order. SettingsLayout renders the
// mobile index rows and the desktop master-detail nav from this list. Danger
// is pinned last (danger: true tints it via CSS). Icon values are lucide-solid
// per-icon deep imports — the app icon system.

import Bot from 'lucide-solid/icons/bot';
import CalendarClock from 'lucide-solid/icons/calendar-clock';
import ContainerIcon from 'lucide-solid/icons/container';
import FolderInput from 'lucide-solid/icons/folder-input';
import GitBranch from 'lucide-solid/icons/git-branch';
import LockKeyhole from 'lucide-solid/icons/lock-keyhole';
import PlaneLanding from 'lucide-solid/icons/plane-landing';
import Plug from 'lucide-solid/icons/plug';
import Settings2 from 'lucide-solid/icons/settings-2';
import TriangleAlert from 'lucide-solid/icons/triangle-alert';
import type { SettingsCategory } from '../../components/settings/categories';

export const REPO_SETTINGS_CATEGORIES: SettingsCategory[] = [
  {
    slug: 'general',
    title: 'General',
    description: 'Name, git author identity, Incogni.',
    icon: Settings2,
  },
  {
    slug: 'integrations',
    title: 'Integrations',
    description: 'Git credential, tracker binding, forge token.',
    icon: Plug,
  },
  {
    slug: 'branches',
    title: 'Branches',
    description: 'Default branch and branch naming.',
    icon: GitBranch,
  },
  {
    slug: 'agents',
    title: 'Agents',
    description: 'Spawn and AFK defaults, auto-spawn, budget.',
    icon: Bot,
  },
  {
    slug: 'runner',
    title: 'Runner',
    description: 'Container or host execution, resource limits.',
    icon: ContainerIcon,
  },
  {
    slug: 'autoland',
    title: 'Autoland',
    description: 'Lander validation and merge policy for claim PRs.',
    icon: PlaneLanding,
  },
  {
    slug: 'secrets',
    title: 'Secrets',
    description: 'Write-only secrets for agents.',
    icon: LockKeyhole,
  },
  {
    slug: 'schedules',
    title: 'Schedules',
    description: 'Cron-fired scheduled runs: prompt, flows, cadence.',
    icon: CalendarClock,
  },
  {
    slug: 'imports',
    title: 'Imports',
    description: "Other lab repos this repo's instances may read as read-only snapshots.",
    icon: FolderInput,
  },
  {
    slug: 'danger',
    title: 'Danger zone',
    description: 'Delete this repository.',
    icon: TriangleAlert,
    danger: true,
  },
];
