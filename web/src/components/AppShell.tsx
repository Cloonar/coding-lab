// The authenticated app shell (issue #41, reworked by issue #76): a persistent
// 260px side rail beside the routed content at >=1024px (collapsible with
// Ctrl/Cmd+B, re-opened by a fixed chevron), and a bottom TabBar below that —
// Runs · New · Repos · More — on every page except the immersive Chat and
// Schedule editor (lib/tabBar). There is no mobile drawer, hamburger, scrim or
// drag gesture any more (they replaced issue #140's follow-finger drawer):
// every destination is a visible control. Below 1024px the rail is not shown
// at all. Unauthenticated sessions render their children bare — /login and
// /setup therefore never get the shell.
//
// The shell owns the SINGLE listInstances resource and hands it to the tree
// through ShellInstancesContext (lib/shellInstances): the rail, the Runs page,
// the tab bar's Runs badge, the `(N) lab` document title and the app badge all
// read this one list — a page never fetches it a second time.
//
// Refetch policy (issue #175): run.changed (spawn/stop/outcome — rows added,
// removed, or their outcome flips) still refetches the whole list via
// createLiveResource. run.messages.changed is different: it fires roughly once
// per second per streaming agent, but can only ever flip ONE run's
// conversational-state dot — refetching the entire GET /api/v1/instances list
// for that would re-render every rail row every second while any agent is
// streaming. So it's handled separately, below: mutateInstances patches just
// that row's `state` in place, and every other row keeps its exact object
// identity so the rail (which keys its rows by reference) never rebuilds them.
// An event whose runID isn't found (already stopped, out-of-order) or whose
// state already matches is a no-op — never a fetch. This stays safe under
// loss: createLiveResource's resync/run.changed refetch still replaces the
// whole list, so a missed or dropped patch self-heals on the next
// spawn/stop/reconnect.

import { useLocation } from '@solidjs/router';
import { Show, createEffect, createSignal, onCleanup, type ParentProps } from 'solid-js';
import { listInstances, type ConversationState } from '../api';
import { useAuth } from '../auth';
import { useEvents } from '../events';
import { install } from '../lib/install';
import { createLiveResource } from '../lib/liveResource';
import { createProviderLogin } from '../lib/providerLogin';
import { resourceValue } from '../lib/resource';
import { ShellInstancesContext, attentionCount } from '../lib/shellInstances';
import { tabBarHidden } from '../lib/tabBar';
import Icon from './Icon';
import InstallSheet from './InstallSheet';
import SideNav from './SideNav';
import TabBar from './TabBar';

const COLLAPSED_KEY = 'lab.rail-collapsed';
/** The shell breakpoint: >= this the side rail shows, below it the tab bar.
 *  CSS owns the switch (shell.css, tabbar.css); pages that must agree with it
 *  in script cite this constant. */
export const DESKTOP_MIN_PX = 1024;
/** index.html's <title>; the attention count is prefixed to it as `(N) lab`. */
const BASE_TITLE = 'lab';

/** The navigator's Badging API, where the platform has it (installed PWAs). */
type BadgingNavigator = Navigator & {
  setAppBadge?: (contents?: number) => Promise<void>;
  clearAppBadge?: () => Promise<void>;
};

export default function AppShell(props: ParentProps) {
  const { auth } = useAuth();
  // The shell only renders authenticated; the guard is belt-and-suspenders so a
  // stray unauthenticated render (or /login, /setup) skips the shell markup.
  return (
    <Show when={auth()?.authenticated} fallback={props.children}>
      <ShellFrame>{props.children}</ShellFrame>
    </Show>
  );
}

function ShellFrame(props: ParentProps) {
  const events = useEvents();
  const location = useLocation();

  // The single shell-wide instances resource (spec pin): everything that shows
  // runs reads this one list through ShellInstancesContext. run.changed is the
  // only spec here — it's the only event that can add/remove/reorder rows;
  // run.messages.changed is handled below by patching in place instead.
  const [instances, { mutate: mutateInstances }] = createLiveResource(
    () => listInstances(),
    [{ type: 'run.changed' }],
  );

  // run.messages.changed patch-in-place (issue #175): see the file header for
  // why this bypasses createLiveResource's refetch-the-whole-list path. Never
  // calls refetch — only mutateInstances, and only when the event actually
  // changes something.
  onCleanup(
    events.subscribe('run.messages.changed', (event) => {
      if (typeof event.runID !== 'string' || typeof event.state !== 'string') return;
      const runID = event.runID;
      const state = event.state as ConversationState;
      // state_detail (issue #79) is omitted when empty — absent clears it.
      const detail = typeof event.state_detail === 'string' ? event.state_detail : '';
      mutateInstances((prev) => {
        if (prev === undefined) return prev;
        const idx = prev.findIndex((row) => row.id === runID);
        if (idx === -1) return prev;
        const row = prev[idx]!;
        if (row.state === state && (row.state_detail ?? '') === detail) return prev;
        const next = prev.slice();
        next[idx] = { ...row, state, state_detail: detail };
        return next;
      });
    }),
  );

  const all = () => resourceValue(instances) ?? [];
  // Sticky: once the first fetch has settled (ready or errored) a later
  // refetch's pending state doesn't flip consumers back to "loading".
  const [loaded, setLoaded] = createSignal(false);
  createEffect(() => {
    if (instances.state === 'ready' || instances.state === 'errored') setLoaded(true);
  });
  const error = () => instances.error as unknown;
  const attention = () => attentionCount(all());

  // --- attention outside the page (issue #76) --------------------------------
  // `(N) lab` in the browser tab while N runs need you, so a backgrounded tab
  // still reports attention; the base title comes back on cleanup (logout).
  createEffect(() => {
    const n = attention();
    document.title = n > 0 ? `(${n}) ${BASE_TITLE}` : BASE_TITLE;
  });
  onCleanup(() => {
    document.title = BASE_TITLE;
  });

  // The installed PWA's icon badge mirrors the same count where the Badging
  // API exists. Feature-detected; a rejection (permission, unsupported
  // context) is swallowed — the badge is a nicety, never an error.
  const badging = navigator as BadgingNavigator;
  const setBadge = (n: number): void => {
    try {
      const pending = n > 0 ? badging.setAppBadge?.(n) : badging.clearAppBadge?.();
      pending?.catch(() => {});
    } catch {
      // A synchronous throw from an odd implementation — same as a rejection.
    }
  };
  createEffect(() => setBadge(attention()));
  onCleanup(() => setBadge(0));

  // The More tab's dot: the agent provider is logged out.
  const providerLogin = createProviderLogin();

  const showTabBar = () => !tabBarHidden(location.pathname);

  // --- collapse (desktop only; CSS gates the visual effect to >=1024px) -----
  const readCollapsed = (): boolean => {
    try {
      return localStorage.getItem(COLLAPSED_KEY) === '1';
    } catch {
      return false;
    }
  };
  const [collapsed, setCollapsed] = createSignal(readCollapsed());
  const setCollapsedPersisted = (next: boolean): void => {
    setCollapsed(next);
    try {
      if (next) localStorage.setItem(COLLAPSED_KEY, '1');
      else localStorage.removeItem(COLLAPSED_KEY);
    } catch {
      // Private mode / storage disabled — the in-memory signal still works.
    }
  };

  // Ctrl/Cmd+B toggles collapse — skipped while typing so it never eats an
  // editor shortcut.
  const onKeyDown = (e: KeyboardEvent): void => {
    if ((e.ctrlKey || e.metaKey) && (e.key === 'b' || e.key === 'B')) {
      const el = e.target as HTMLElement | null;
      const tag = el?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el?.isContentEditable)
        return;
      e.preventDefault();
      setCollapsedPersisted(!collapsed());
    }
  };
  window.addEventListener('keydown', onKeyDown);
  onCleanup(() => window.removeEventListener('keydown', onKeyDown));

  return (
    <ShellInstancesContext.Provider value={{ all, loaded, error }}>
      <div
        classList={{
          shell: true,
          'rail-collapsed': collapsed(),
          // Reserves --tabbar-space below the content (styles/tabbar.css).
          'has-tabbar': showTabBar(),
        }}
      >
        {/* The side rail: >=1024px only (CSS); the tab bar replaces it below. */}
        <div class="shell-rail">
          <SideNav instances={all()} onCollapse={() => setCollapsedPersisted(true)} />
        </div>

        {/* Collapsed (desktop): a small fixed re-open chevron top-left. */}
        <Show when={collapsed()}>
          <button
            type="button"
            class="icon-btn rail-reopen"
            aria-label="Open sidebar"
            title="Open sidebar (Ctrl/Cmd+B)"
            onClick={() => setCollapsedPersisted(false)}
          >
            <Icon name="chevrons-right" />
          </button>
        </Show>

        <div class="shell-content">{props.children}</div>

        {/* Bottom tab bar: <1024px only (CSS); not on the Chat or the Schedule
            editor, which are immersive and own their back control. */}
        <Show when={showTabBar()}>
          <TabBar
            pathname={location.pathname}
            attention={attention()}
            moreAlert={providerLogin.loggedOut()}
          />
        </Show>

        {/* PWA install sheet (issue #142): mounted post-auth so it can never show
            on /login or /setup. Opening is reactive — on Android it appears when
            the captured beforeinstallprompt lands (even seconds after mount), on
            iOS when the platform gates pass — or manually from the Settings row. */}
        <InstallSheet
          open={install.open()}
          variant={install.variant()}
          onInstall={() => install.promptInstall()}
          onDismiss={() => install.dismiss()}
        />
      </div>
    </ShellInstancesContext.Provider>
  );
}
