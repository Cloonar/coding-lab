// The authenticated app shell (issue #41, Phase 1): a persistent 260px side
// rail beside the routed content at >=1024px; the same SideNav as a left overlay
// drawer below that, opened by the mobile top strip's hamburger or a
// follow-finger rightward drag from anywhere (issue #140) and closed by
// scrim / Escape / route change / leftward drag. Owns the SINGLE
// listInstances resource shared by the rail rows and the hamburger's attention
// badge, plus the desktop collapse state. Unauthenticated sessions render their
// children bare — /login and /setup therefore never get the shell.
//
// Refetch policy (issue #175): run.changed (spawn/stop/outcome — rows added,
// removed, or their outcome flips) still refetches the whole list via
// createLiveResource. run.messages.changed is different: it fires roughly once
// per second per streaming agent, but can only ever flip ONE run's
// conversational-state dot — refetching the entire GET /api/v1/instances list
// for that would re-render every rail row every second while any agent is
// streaming. So it's handled separately, below: mutateInstances patches just
// that row's `state` in place, and every other row keeps its exact object
// identity so SideNav (which keys its rows by reference) never rebuilds them.
// An event whose runID isn't found (already stopped, out-of-order) or whose
// state already matches is a no-op — never a fetch. This stays safe under
// loss: createLiveResource's resync/run.changed refetch still replaces the
// whole list, so a missed or dropped patch self-heals on the next
// spawn/stop/reconnect.

import { A, useLocation } from '@solidjs/router';
import { Show, batch, createEffect, createSignal, on, onCleanup, type ParentProps } from 'solid-js';
import { listInstances, type ConversationState } from '../api';
import { useAuth } from '../auth';
import { useEvents } from '../events';
import { createDrawerGesture, type GestureDecision } from '../lib/drawerGesture';
import { install } from '../lib/install';
import { createLiveResource } from '../lib/liveResource';
import { resourceValue } from '../lib/resource';
import Icon from './Icon';
import InstallSheet from './InstallSheet';
import SideNav from './SideNav';

const COLLAPSED_KEY = 'lab.rail-collapsed';
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
  // mobile attention badge both read this one list. run.changed is the only
  // spec here — it's the only event that can add/remove/reorder rows;
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
      mutateInstances((prev) => {
        if (prev === undefined) return prev;
        const idx = prev.findIndex((row) => row.id === runID);
        if (idx === -1) return prev;
        const row = prev[idx]!;
        if (row.state === state) return prev;
        const next = prev.slice();
        next[idx] = { ...row, state };
        return next;
      });
    }),
  );

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
  // True only while a touch is actively dragging the rail; distinct from
  // drawerOpen so the settle transition (base.css) can tell "just dragged"
  // apart from "opened via hamburger/click".
  const [dragging, setDragging] = createSignal(false);

  // The chat owns its own immersive header (back → home), so the mobile strip is
  // hidden on /runs/:id; the drag still works there.
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

  // --- follow-finger drawer drag (mobile, issue #140) ------------------------
  // The state machine in lib/drawerGesture.ts owns the arbitration (scrollers,
  // editables, axis dominance, rightward-only-when-closed, flick-vs-position
  // commit); it never touches the DOM. AppShell's job is everything the
  // machine can't do itself: bind the touch listeners, answer its environment
  // queries at touch start, and turn its decisions into a live transform on
  // the rail / opacity on the scrim while dragging, or a settled signal flip
  // on release. See that file's header for the full model.
  //
  // No edge special case, unlike the EDGE_ZONE_PX gesture this replaces: a
  // touch starting anywhere is tracked the same way. Where the OS owns the
  // edge (iOS back-swipe, Android gesture nav) it steals the touch mid-drag —
  // we get a touchcancel instead of further touchmoves, which resets the drag
  // (see onTouchCancel below) rather than fighting the OS for it.
  let railEl: HTMLDivElement | undefined;
  let scrimEl: HTMLDivElement | undefined;
  const gesture = createDrawerGesture();
  let activeTouchId: number | null = null;

  // Horizontal drags inside the composer are cursor/selection, not the
  // drawer — mirrors the editable check in the Ctrl/Cmd+B guard above.
  const inEditable = (target: EventTarget | null): boolean => {
    if (!(target instanceof Element)) return false;
    if (target.closest('input, textarea, select')) return true;
    return target instanceof HTMLElement && target.isContentEditable;
  };

  // Horizontal scrollers (.diff-scroll, .tool-body, .md-pre, table wraps) win
  // the touch unless they're fully scrolled left: at scrollLeft === 0 a
  // rightward pull has nothing left to scroll, so the drawer may claim it —
  // the same trade iOS makes between its edge back-swipe and a carousel. The
  // cheap scrollLeft/scrollWidth checks run before the getComputedStyle call.
  const inScrolledScroller = (target: EventTarget | null): boolean => {
    if (!(target instanceof Element)) return false;
    let n: Element | null = target;
    while (n && n !== document.body) {
      if (n.scrollLeft > 0 && n.scrollWidth > n.clientWidth) {
        const overflowX = getComputedStyle(n).overflowX;
        if (overflowX === 'auto' || overflowX === 'scroll') return true;
      }
      n = n.parentElement;
    }
    return false;
  };

  // The tracked TouchList only ever holds the one finger we claimed (extra
  // fingers are ignored at touchstart); this picks it back out by identifier.
  const findActive = (list: TouchList): Touch | null => {
    for (let i = 0; i < list.length; i++) {
      const t = list.item(i);
      if (t && t.identifier === activeTouchId) return t;
    }
    return null;
  };

  const clearDragStyles = (): void => {
    railEl?.style.removeProperty('transform');
    scrimEl?.style.removeProperty('opacity');
  };

  // Settle a released/cancelled drag. The inline styles are cleared and the
  // signals flip in the same tick (batch — these are manual window listeners,
  // so Solid does not auto-batch them for us), so the browser computes one
  // style change and the .shell-rail transition (base.css) animates the
  // settle from wherever the finger left off. Under prefers-reduced-motion
  // that transition is none, so the settle snaps instead of sliding.
  const settle = (d: GestureDecision): void => {
    switch (d.kind) {
      case 'commit-open':
        clearDragStyles();
        batch(() => {
          setDragging(false);
          setDrawerOpen(true);
        });
        break;
      case 'commit-closed':
        clearDragStyles();
        batch(() => {
          setDragging(false);
          setDrawerOpen(false);
        });
        break;
      case 'cancel-reset':
        // The open/closed signal never changed during the drag — clearing the
        // inline styles is enough for CSS to animate back to the pre-drag state.
        clearDragStyles();
        setDragging(false);
        break;
      case 'ignore':
        break;
    }
  };

  const onTouchStart = (e: TouchEvent): void => {
    if (window.innerWidth >= DESKTOP_MIN_PX) return;
    // Self-heal a leaked sequence: touch events fire at the element the finger
    // went DOWN on, so when a re-render (SSE-driven chat/rail updates) detaches
    // that node mid-touch, its touchend/touchcancel dispatch on the detached
    // node and never bubble to window — the tracked id would pin forever, and a
    // claimed drag would pin the scrim over the whole viewport. The tracked
    // finger is gone if e.touches (every finger currently down) no longer holds
    // it, or if it re-appears in changedTouches (a finger can't go down twice —
    // Android reuses identifiers, so "our" id starting again means the old
    // sequence ended unheard). Either way: settle as cancelled, start fresh.
    if (activeTouchId !== null && (findActive(e.changedTouches) || !findActive(e.touches))) {
      activeTouchId = null;
      settle(gesture.cancel());
    }
    if (activeTouchId !== null) return; // extra fingers ignored; first touch keeps tracking
    if (e.touches.length !== 1) return; // multi-touch: ignore, as before
    const t = e.changedTouches[0]!;
    const decision = gesture.start(
      { x: t.clientX, y: t.clientY, t: e.timeStamp },
      {
        drawerOpen: drawerOpen(),
        drawerWidth: railEl?.offsetWidth ?? 0,
        inEditable: inEditable(e.target),
        inScrolledScroller: inScrolledScroller(e.target),
      },
    );
    if (decision.kind === 'track') activeTouchId = t.identifier;
  };
  const onTouchMove = (e: TouchEvent): void => {
    if (activeTouchId === null) return;
    const t = findActive(e.changedTouches);
    if (!t) return;
    const d = gesture.move({ x: t.clientX, y: t.clientY, t: e.timeStamp });
    if (d.kind === 'drag') {
      e.preventDefault(); // claimed — required so the drag can override page scroll
      if (!dragging()) setDragging(true); // mounts the scrim (Show) before it's styled
      railEl?.style.setProperty('transform', `translateX(${d.x}px)`);
      scrimEl?.style.setProperty('opacity', String(d.progress));
    } else if (d.kind === 'ignore') {
      activeTouchId = null; // handed to the page for good (e.g. vertical intent)
    }
    // 'track': intent still undecided — nothing to apply yet.
  };
  const onTouchEnd = (e: TouchEvent): void => {
    if (activeTouchId === null) return;
    const t = findActive(e.changedTouches);
    if (!t) return; // a different finger lifted
    activeTouchId = null;
    settle(gesture.end({ x: t.clientX, y: t.clientY, t: e.timeStamp }));
  };
  // The OS stealing the gesture (iOS back-swipe, Android nav) cancels ALL
  // touches at once, so any cancel while tracking resets the drag.
  const onTouchCancel = (): void => {
    if (activeTouchId === null) return;
    activeTouchId = null;
    settle(gesture.cancel());
  };
  // iOS can background the PWA mid-touch (app switcher, notification pull)
  // without delivering a touchcancel; reset on hide so a drag can't outlive
  // the touch — otherwise the first tap after returning only self-heals.
  const onVisibilityHidden = (): void => {
    if (document.visibilityState !== 'hidden' || activeTouchId === null) return;
    activeTouchId = null;
    settle(gesture.cancel());
  };
  window.addEventListener('touchstart', onTouchStart, { passive: true });
  window.addEventListener('touchmove', onTouchMove, { passive: false });
  window.addEventListener('touchend', onTouchEnd, { passive: true });
  window.addEventListener('touchcancel', onTouchCancel, { passive: true });
  document.addEventListener('visibilitychange', onVisibilityHidden);
  onCleanup(() => {
    window.removeEventListener('touchstart', onTouchStart);
    window.removeEventListener('touchmove', onTouchMove);
    window.removeEventListener('touchend', onTouchEnd);
    window.removeEventListener('touchcancel', onTouchCancel);
    document.removeEventListener('visibilitychange', onVisibilityHidden);
  });

  return (
    <div
      classList={{
        shell: true,
        'rail-collapsed': collapsed(),
        'drawer-open': drawerOpen(),
        'drawer-dragging': dragging(),
      }}
    >
      {/* Mobile top strip — hidden on the chat route; hidden >=1024 by CSS. */}
      <Show when={!isChat()}>
        <div class="shell-topstrip">
          {/* Same brand-links-home affordance as the desktop rail (SideNav). */}
          <A href="/" class="brand plain">
            lab<span class="brand-dot">.</span>
          </A>
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

      {/* Scrim behind the drawer (mobile). Mounted while dragging too — not
          just drawerOpen — so its opacity can track drag progress before the
          drawer signal itself flips "open". */}
      <Show when={drawerOpen() || dragging()}>
        <div
          ref={scrimEl}
          class="shell-scrim"
          aria-hidden="true"
          onClick={() => setDrawerOpen(false)}
        />
      </Show>

      <div class="shell-rail" ref={railEl}>
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
  );
}
