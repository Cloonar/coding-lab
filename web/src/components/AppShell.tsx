// The authenticated app shell (issue #41, Phase 1): a persistent 260px side
// rail beside the routed content at >=1024px; the same SideNav as a left overlay
// drawer below that, opened by the mobile top strip's hamburger or a left-edge
// swipe and closed by scrim / Escape / route change / leftward swipe. Owns the
// SINGLE listInstances resource (refetched on run.changed, debounced on
// run.messages.changed) shared by the rail rows and the hamburger's attention
// badge, plus the desktop collapse state. Unauthenticated sessions render their
// children bare — /login and /setup therefore never get the shell.

import { useLocation } from '@solidjs/router';
import {
  Show,
  createEffect,
  createResource,
  createSignal,
  on,
  onCleanup,
  type ParentProps,
} from 'solid-js';
import { listInstances } from '../api';
import { useAuth } from '../auth';
import { useEvents } from '../events';
import { resourceValue } from '../lib/resource';
import Icon from './Icon';
import SideNav from './SideNav';

const COLLAPSED_KEY = 'lab.rail-collapsed';
// run.messages.changed only flips conversational state (a dot color), so its
// refetch is debounced; run.changed (spawn/stop) refetches immediately.
const MESSAGES_DEBOUNCE_MS = 250;
const EDGE_ZONE_PX = 24; // left hot-zone width for the open swipe
const SWIPE_THRESHOLD_PX = 40; // horizontal travel that commits open/close
const INTENT_PX = 8; // travel before we decide horizontal vs vertical
const DESKTOP_MIN_PX = 1024; // >= this: persistent rail, no drawer/gestures

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

  // The single shell-wide instances resource (spec pin): rail rows and the
  // mobile attention badge both read this one list.
  const [instances, { refetch }] = createResource(() => listInstances());
  onCleanup(events.subscribe('run.changed', () => void refetch()));
  let debounce: ReturnType<typeof setTimeout> | undefined;
  onCleanup(
    events.subscribe('run.messages.changed', () => {
      clearTimeout(debounce);
      debounce = setTimeout(() => void refetch(), MESSAGES_DEBOUNCE_MS);
    }),
  );
  onCleanup(() => clearTimeout(debounce));

  const all = () => resourceValue(instances) ?? [];
  const hasAttention = () =>
    all().some(
      (instance) =>
        instance.live && (instance.state === 'needs_input' || instance.state === 'question'),
    );

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

  // --- drawer (mobile) ------------------------------------------------------
  const [drawerOpen, setDrawerOpen] = createSignal(false);

  // The chat owns its own immersive header (back → home), so the mobile strip is
  // hidden on /runs/:id; the edge-swipe still opens the drawer there.
  const isChat = () => /^\/runs\/.+/.test(location.pathname);

  // Close the drawer on route change (TopBar's old pattern); the initial run is
  // a harmless no-op.
  createEffect(
    on(
      () => location.pathname,
      () => setDrawerOpen(false),
    ),
  );

  // Escape closes the drawer; Ctrl/Cmd+B toggles collapse — skipped while typing
  // so it never eats an editor shortcut.
  const onKeyDown = (e: KeyboardEvent): void => {
    if (e.key === 'Escape' && drawerOpen()) {
      setDrawerOpen(false);
      return;
    }
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

  // --- left-edge swipe to open / leftward swipe to close (mobile) -----------
  // A ~24px left hot-zone opens the drawer; a leftward drag on the open drawer
  // closes it. Handlers are non-passive so preventDefault can claim the gesture
  // once horizontal intent is established — never before, so vertical scroll
  // stays free. Implemented as a clientX threshold, not an overlay element, so
  // it never intercepts a tap on the chat's top-left back button.
  //
  // iOS caveat: a standalone (installed) PWA may hand the system left-edge
  // back-gesture to the OS on some versions, pre-empting this handler. It is
  // best-effort; the hamburger is the reliable open path.
  let startX = 0;
  let startY = 0;
  let tracking = false;
  let decided: 'none' | 'horizontal' | 'vertical' = 'none';
  let opening = false;

  const onTouchStart = (e: TouchEvent): void => {
    if (window.innerWidth >= DESKTOP_MIN_PX || e.touches.length !== 1) return;
    const t = e.touches[0]!;
    startX = t.clientX;
    startY = t.clientY;
    decided = 'none';
    opening = !drawerOpen() && startX <= EDGE_ZONE_PX;
    // Track only when opening from the edge, or when the drawer is open (to
    // close it) — any other touch is left entirely to the page.
    tracking = opening || drawerOpen();
  };
  const onTouchMove = (e: TouchEvent): void => {
    if (!tracking || e.touches.length !== 1) return;
    const t = e.touches[0]!;
    const dx = t.clientX - startX;
    const dy = t.clientY - startY;
    if (decided === 'none') {
      if (Math.abs(dx) < INTENT_PX && Math.abs(dy) < INTENT_PX) return;
      decided = Math.abs(dx) > Math.abs(dy) ? 'horizontal' : 'vertical';
      if (decided === 'vertical') {
        tracking = false; // let the page scroll
        return;
      }
    }
    e.preventDefault(); // horizontal intent claimed — own the gesture
    if (opening && dx > SWIPE_THRESHOLD_PX) {
      setDrawerOpen(true);
      tracking = false;
    } else if (!opening && dx < -SWIPE_THRESHOLD_PX) {
      setDrawerOpen(false);
      tracking = false;
    }
  };
  const onTouchEnd = (): void => {
    tracking = false;
    decided = 'none';
  };
  window.addEventListener('touchstart', onTouchStart, { passive: true });
  window.addEventListener('touchmove', onTouchMove, { passive: false });
  window.addEventListener('touchend', onTouchEnd, { passive: true });
  onCleanup(() => {
    window.removeEventListener('touchstart', onTouchStart);
    window.removeEventListener('touchmove', onTouchMove);
    window.removeEventListener('touchend', onTouchEnd);
  });

  return (
    <div classList={{ shell: true, 'rail-collapsed': collapsed(), 'drawer-open': drawerOpen() }}>
      {/* Mobile top strip — hidden on the chat route; hidden >=1024 by CSS. */}
      <Show when={!isChat()}>
        <div class="shell-topstrip">
          <span class="brand">
            lab<span class="brand-dot">.</span>
          </span>
          <span class="spacer" />
          <span
            classList={{ 'live-dot': true, on: events.connected() }}
            role="status"
            aria-label={events.connected() ? 'Live' : 'Reconnecting'}
            title={events.connected() ? 'Live' : 'Reconnecting…'}
          />
          <button
            type="button"
            class="icon-btn strip-hamburger"
            aria-label="Open menu"
            title="Menu"
            aria-expanded={drawerOpen()}
            onClick={() => setDrawerOpen(true)}
          >
            <Icon name="menu" />
            <Show when={hasAttention()}>
              <span class="attn-badge" aria-hidden="true" />
            </Show>
          </button>
        </div>
      </Show>

      {/* Scrim behind the drawer (mobile). */}
      <Show when={drawerOpen()}>
        <div class="shell-scrim" aria-hidden="true" onClick={() => setDrawerOpen(false)} />
      </Show>

      <div class="shell-rail">
        <SideNav
          instances={all()}
          onNavigate={() => setDrawerOpen(false)}
          onCollapse={() => setCollapsedPersisted(true)}
        />
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
    </div>
  );
}
