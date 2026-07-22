// The global settings area's subpages (issue #198). This array is the SINGLE
// source of truth: SettingsLayout renders the mobile index rows and the
// desktop master-detail nav from it, index.tsx switches its Section on the
// same slugs, and the route/metadata parity test walks it directly — add a
// category here and the route, the index row and the nav all follow. lucide
// icons are per-icon deep imports (the package root is type-only territory).

import type { SettingsCategory } from '../../components/settings/categories';
import Settings2 from 'lucide-solid/icons/settings-2';
import Bot from 'lucide-solid/icons/bot';
import Bell from 'lucide-solid/icons/bell';

export const GLOBAL_SETTINGS_CATEGORIES: SettingsCategory[] = [
  {
    slug: 'general',
    title: 'General',
    description: 'Git author identity used for commits.',
    icon: Settings2,
  },
  {
    slug: 'agents',
    title: 'Agents',
    description: 'Spawn defaults, AFK behavior and capacity.',
    icon: Bot,
  },
  {
    slug: 'notifications',
    title: 'Notifications',
    description: 'Push notifications and app install — for this device.',
    icon: Bell,
  },
];
