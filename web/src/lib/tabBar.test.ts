// Tab bar route rules (issue #76): which tab a path lights (sub-pages keep
// their section's tab), where the bar hides (the Chat and the Schedule
// editor, but not the schedules section itself), and each tab's root.

import { describe, expect, it } from 'vitest';
import { TAB_ROOTS, activeTab, tabBarHidden } from './tabBar';

describe('activeTab', () => {
  it.each([
    ['/', 'runs'],
    ['/history', 'runs'],
    ['/new', 'new'],
    ['/repos', 'repos'],
    ['/repos/', 'repos'],
    ['/repos/new', 'repos'],
    ['/repos/x', 'repos'],
    ['/repos/x/issues', 'repos'],
    ['/repos/x/issues/12', 'repos'],
    ['/repos/x/settings/schedules', 'repos'],
    ['/credentials', 'more'],
    ['/tokens', 'more'],
    ['/settings', 'more'],
    ['/settings/runner', 'more'],
    ['/more', 'more'],
  ] as const)('%s lights %s', (path, tab) => {
    expect(activeTab(path)).toBe(tab);
  });

  it.each(['/runs/abc', '/runs', '/login', '/reposx', '/newx', '/settingsx', '/history/x'])(
    '%s lights no tab',
    (path) => {
      expect(activeTab(path)).toBeNull();
    },
  );
});

describe('tabBarHidden', () => {
  it.each([
    '/runs/abc',
    '/runs/abc/',
    '/repos/x/settings/schedules/new',
    '/repos/x/settings/schedules/sch_1',
  ])('hides on %s', (path) => {
    expect(tabBarHidden(path)).toBe(true);
  });

  it.each([
    '/',
    '/history',
    '/new',
    '/repos',
    '/repos/x',
    '/repos/x/settings',
    '/repos/x/settings/schedules',
    '/repos/x/settings/schedules/',
    '/repos/x/settings/general',
    '/settings/runner',
    '/more',
  ])('shows on %s', (path) => {
    expect(tabBarHidden(path)).toBe(false);
  });
});

describe('TAB_ROOTS', () => {
  it('maps each tab to its section root, which lights that same tab', () => {
    expect(TAB_ROOTS).toEqual({ runs: '/', new: '/new', repos: '/repos', more: '/more' });
    for (const [tab, root] of Object.entries(TAB_ROOTS)) expect(activeTab(root)).toBe(tab);
  });
});
