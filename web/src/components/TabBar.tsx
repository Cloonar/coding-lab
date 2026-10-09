// The phone's bottom tab bar (issue #76): Runs · New · Repos · More, one tap
// from every authenticated page below 1024px to each section root. It replaces
// the old mobile top strip, hamburger drawer and follow-finger drag — every
// destination is a visible control now. AppShell decides WHERE it renders
// (lib/tabBar's tabBarHidden: not on the Chat or the Schedule editor) and CSS
// hides it from 1024px, where the side rail takes over (styles/tabbar.css).
//
// The lit tab comes from the path prefix (lib/tabBar's activeTab), so a
// sub-page keeps its section lit. Runs carries the attention count (live runs
// in needs-input / question — the shell's one instances list, never a second
// fetch), hidden at zero; More carries a dot only when the agent provider is
// logged out. Both are props: the bar itself fetches nothing.
//
// Re-tap: tapping the lit tab returns to the section root and scrolls to the
// top; on the root it only scrolls. Any other tap is an ordinary link the
// router handles (and scrolls) itself. Modified clicks (Ctrl/Cmd/Shift/middle)
// are left to the browser so "open in new tab" still works.

import { useNavigate } from '@solidjs/router';
import { For, Show } from 'solid-js';
import { TAB_ROOTS, activeTab, type TabId } from '../lib/tabBar';
import Icon, { type IconName } from './Icon';

interface TabSpec {
  id: TabId;
  label: string;
  icon: IconName;
}

const TABS: TabSpec[] = [
  { id: 'runs', label: 'Runs', icon: 'inbox' },
  { id: 'new', label: 'New', icon: 'plus' },
  { id: 'repos', label: 'Repos', icon: 'folder' },
  { id: 'more', label: 'More', icon: 'more-horizontal' },
];

export default function TabBar(props: {
  /** The current location's pathname; decides the lit tab. */
  pathname: string;
  /** Runs badge: live runs that need you. Hidden at zero. */
  attention: number;
  /** More dot: the agent provider is logged out. */
  moreAlert: boolean;
}) {
  const navigate = useNavigate();
  const lit = () => activeTab(props.pathname);

  // The accessible name carries what the badge / dot say visually; both are
  // aria-hidden so a screen reader hears "Runs, 2 need you" once.
  const nameOf = (tab: TabSpec): string => {
    if (tab.id === 'runs' && props.attention > 0)
      return `Runs, ${props.attention} ${props.attention === 1 ? 'needs' : 'need'} you`;
    if (tab.id === 'more' && props.moreAlert) return 'More, provider logged out';
    return tab.label;
  };

  const onClick = (tab: TabSpec, e: MouseEvent): void => {
    if (lit() !== tab.id) return; // ordinary link — the router navigates
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault(); // the router skips a default-prevented click
    const root = TAB_ROOTS[tab.id];
    if (props.pathname !== root) navigate(root);
    window.scrollTo({ top: 0 });
  };

  return (
    <nav class="tabbar" aria-label="Tabs">
      <For each={TABS}>
        {(tab) => (
          <a
            href={TAB_ROOTS[tab.id]}
            classList={{ tab: true, [`tab-${tab.id}`]: true, on: lit() === tab.id }}
            aria-current={lit() === tab.id ? 'page' : undefined}
            aria-label={nameOf(tab)}
            onClick={(e) => onClick(tab, e)}
          >
            <Show
              when={tab.id === 'new'}
              fallback={<Icon name={tab.icon} size={24} class="tab-icon" />}
            >
              <span class="tab-plus">
                <Icon name={tab.icon} size={20} class="tab-icon" />
              </span>
            </Show>
            <span class="tab-label">{tab.label}</span>
            <Show when={tab.id === 'runs' && props.attention > 0}>
              <span class="tab-badge" aria-hidden="true">
                {props.attention}
              </span>
            </Show>
            <Show when={tab.id === 'more' && props.moreAlert}>
              <span class="tab-dot" aria-hidden="true" />
            </Show>
          </a>
        )}
      </For>
    </nav>
  );
}
