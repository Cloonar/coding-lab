// Tool-call detail panel (issue #145) — lifts a tool call's input/output out of
// the chat column into a dedicated surface, so a long command's I/O never
// pushes the conversation around.
//
// ONE component, TWO containers (a pure render switch on `props.desktop`, so
// internal page state survives the breakpoint flipping):
//   - Phone (<1024px, !desktop): a modal bottom SHEET. A sibling scrim + an
//     <aside role="dialog" aria-modal> that owns the dismiss idiom of the house
//     modal primitive (issue #142's InstallSheet): scrim and card are SIBLINGS
//     with the dismiss handler on the scrim alone, so a click in the card never
//     bubbles to a dismiss. Content scrolls inside the sheet.
//   - Desktop (>=1024px, desktop): an in-flow right SIDEBAR — a bare <aside>,
//     which is an implicit `complementary` landmark, so it takes NO dialog role
//     and NO aria-modal (the sidebar is a complementary region, not modal). The
//     parent page supplies the flex row that stretches it to full height and
//     sits it flush against the window's right edge; our CSS fixes its width,
//     left border, and a left-edge drag handle to resize it (issue #154 §3/§4).
// It stays ONE <aside> in the JSX (attributes vary by `props.desktop`); only
// the scrim lives in its own <Show>, so the internal `detailSeq` signal is not
// torn down when the breakpoint flips.
//
// PAGES (mobile only, driven by the internal `detailSeq` signal):
//   - List: one row per tool call (title + status mark), for a group target.
//   - Detail: the chip expansion, roomier (the 40vh chip cap is dropped) — the
//     input and output <pre> blocks, reusing the chat chip's classes.
// A group target (`entry: 'list'`) opens at the list and pushes to detail on a
// row tap (with a back affordance); a lone chip (`entry: 'detail'`) opens
// straight at detail and never shows a back button.
//
// DESKTOP is file-detail ONLY (issue #154 §2): the sidebar shows a single file's
// detail — never the list, never a back button, never a command. RunChat only
// ever targets it with one file tool, so desktop ignores `entry`/`detailSeq`
// entirely and always renders `tools[0]`. Opening another file just retargets
// (a new key); the content replaces and the sidebar stays open.
//
// RESET-ON-KEY: `detailSeq` resets to null whenever `props.target.key` changes
// (a genuinely different target), via createEffect(on(key, …)). It must NOT
// reset when only `tools` is replaced by an SSE refetch — same key, new array —
// or an open detail would snap shut on every poll.
//
// LIVE RESOLUTION: the parent recomputes `props.target.tools` on every refetch,
// replacing the message objects wholesale. So the shown tool is ALWAYS resolved
// through `props.target.tools` at render time (by seq), never captured — a
// captured ToolInfo would freeze the output while the real one keeps growing.
// Everything live falls out of props for free: new tools append rows to an open
// list, status marks flip, an open detail's output grows in place. The panel
// never opens or retargets itself.
//
// Thinking is already excluded upstream (issue #68): `tools` is kind 'tool'
// only, so this view never has to filter it.
//
// Drag-to-dismiss (issue #140 conventions) is wired below against those CSS
// hooks: lib/sheetGesture.ts decides, this component binds the listeners and
// applies the live transform / scrim opacity — the pure module never touches
// the DOM.

import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createSignal,
  createUniqueId,
  on,
  onCleanup,
  type JSX,
} from 'solid-js';
import type { ChatMessage } from '../api';
import { createSheetGesture } from '../lib/sheetGesture';
import Icon from './Icon';
import ToolViewBody from './ToolViews';

// Drag-to-resize (issue #154 §4). The desktop sidebar is flush to the window's
// right edge, so a drag's width is `window.innerWidth - clientX`; this pins it
// to a sane band. A single global localStorage key seeds the width on the next
// mount (AppShell's try/catch pattern — private-mode safe).
const WIDTH_KEY = 'lab.tool-panel-width';
const MIN_WIDTH_PX = 320;

/** Clamp a dragged/stored sidebar width to [320px, 60vw] against the given
 *  viewport width (issue #154 §4). A huge stored value can never exceed 60vw at
 *  render time; on a viewport too narrow for the 320px floor the 60vw cap wins.
 *  Pure — unit-tested in ToolPanel.test.tsx. */
export function clampPanelWidth(width: number, viewportWidth: number): number {
  const max = Math.round(viewportWidth * 0.6);
  return Math.min(Math.max(width, MIN_WIDTH_PX), max);
}

/** The stored sidebar width, or null for a first-time user (then the CSS default
 *  clamp(320px, 30vw, 480px) applies). Guards a garbage/zero value to null. */
function readStoredWidth(): number | null {
  try {
    const raw = localStorage.getItem(WIDTH_KEY);
    if (raw === null) return null;
    const n = Number(raw);
    return Number.isFinite(n) && n > 0 ? n : null;
  } catch {
    return null;
  }
}

function writeStoredWidth(width: number): void {
  try {
    localStorage.setItem(WIDTH_KEY, String(width));
  } catch {
    // Private mode / storage disabled — the in-memory width still applies.
  }
}

/** The status mark for a tool call. Moved here from RunChat for issue #145 —
 *  the chat chips and the panel both render it. */
export function toolStatusMark(status?: 'running' | 'ok' | 'error'): string {
  if (status === 'ok') return '✓';
  if (status === 'error') return '✕';
  return '…';
}

/** What the panel is showing. Owned by RunChat; the panel is a pure view. */
export type PanelTarget = {
  /** Identity — "group:<firstToolSeq>" or "tool:<seq>". Internal page state
   *  resets when this changes; it stays stable across SSE refetches. */
  key: string;
  /** Entry page: a group opens at its list; a lone chip opens directly at
   *  detail (and never shows a back affordance). */
  entry: 'list' | 'detail';
  /** The target's tool messages (kind 'tool' only, thinking already excluded),
   *  transcript order. Live — recomputed by the parent on every refetch. */
  tools: ChatMessage[];
};

export interface ToolPanelProps {
  target: PanelTarget | null; // null = closed (render nothing)
  /** >=1024px container: in-layout sidebar instead of modal sheet. A pure
   *  render switch — internal page state must survive it flipping. */
  desktop: boolean;
  onClose: () => void;
}

export default function ToolPanel(props: ToolPanelProps): JSX.Element {
  const headingId = `tool-panel-${createUniqueId()}-heading`;

  // The pushed detail's tool seq, or null for the list page. Reset on a genuine
  // retarget (key change), NOT on a same-key SSE refetch — see the header note.
  // The key goes through a memo first: Solid's `on` re-fires on any read it
  // tracks (here, props.target being replaced wholesale every refetch), not on
  // the derived value changing — the memo's default === equality collapses a
  // same-key refetch to a no-op, so an open detail survives the poll.
  const [detailSeq, setDetailSeq] = createSignal<number | null>(null);
  const targetKey = createMemo(() => props.target?.key);
  createEffect(on(targetKey, () => setDetailSeq(null)));

  // The tool message to show in detail, resolved LIVE through props.target.tools
  // (never captured). null means show the list. DESKTOP is file-detail only
  // (issue #154 §2): it ignores entry/detailSeq and always shows tools[0] — the
  // single file RunChat's guard guarantees. A lone chip (entry 'detail') also
  // shows tools[0]; a group resolves the pushed seq and falls back to the list
  // if it no longer resolves after a refetch.
  const detailTool = (): ChatMessage | null => {
    const t = props.target;
    if (!t) return null;
    if (props.desktop || t.entry === 'detail') return t.tools[0] ?? null;
    const seq = detailSeq();
    if (seq === null) return null;
    return t.tools.find((m) => m.seq === seq) ?? null;
  };

  // Escape closes on desktop only (the sheet has scrim/✕/drag). The
  // defaultPrevented guard lets an inner editor (rename input, composer
  // autocomplete) consume Esc first. ChatMenu's window-keydown precedent.
  const onKeyDown = (e: KeyboardEvent) => {
    if (props.target && props.desktop && e.key === 'Escape' && !e.defaultPrevented) {
      props.onClose();
    }
  };
  window.addEventListener('keydown', onKeyDown);
  onCleanup(() => window.removeEventListener('keydown', onKeyDown));

  // --- follow-finger sheet drag-to-dismiss (mobile, issues #145/#140) --------
  // lib/sheetGesture.ts owns the arbitration (scrolled scrollers, editables,
  // axis dominance, downward-only claim, flick-vs-position commit) and never
  // touches the DOM. This is the binding seam: it wires the touch listeners to
  // the sheet <aside>, answers the machine's environment queries at touch start,
  // and turns its decisions into a live transform on the sheet plus opacity on
  // the scrim while dragging, or a settle on release. Mirrors AppShell's drawer
  // wiring rotated onto the Y axis. Listeners live on the <aside> (never window)
  // and follow its mount/unmount via the ref callback + onCleanup; touchmove is
  // bound passive:false so a claimed drag can preventDefault the page scroll.
  let sheetEl: HTMLElement | undefined;
  let scrimEl: HTMLDivElement | undefined;
  const gesture = createSheetGesture();
  let activeTouchId: number | null = null;

  // Vertical drags inside an editable are cursor/selection, never a dismiss —
  // mirrors AppShell's editable helper.
  const inEditable = (target: EventTarget | null): boolean => {
    if (!(target instanceof Element)) return false;
    if (target.closest('input, textarea, select')) return true;
    return target instanceof HTMLElement && target.isContentEditable;
  };

  // A vertically-scrolled ancestor (the .tool-panel-content list) wins the
  // touch unless it is fully at the top: at scrollTop === 0 a downward pull has
  // nothing left to scroll, so the sheet may claim it. Cheap scrollTop /
  // scrollHeight checks gate the getComputedStyle call; the walk stops at the
  // sheet <aside>.
  const inScrolledScroller = (target: EventTarget | null): boolean => {
    if (!(target instanceof Element)) return false;
    let n: Element | null = target;
    while (n) {
      if (n.scrollTop > 0 && n.scrollHeight > n.clientHeight) {
        const overflowY = getComputedStyle(n).overflowY;
        if (overflowY === 'auto' || overflowY === 'scroll') return true;
      }
      if (n === sheetEl) break;
      n = n.parentElement;
    }
    return false;
  };

  // The tracked TouchList only ever holds the one claimed finger; pick it back
  // out by identifier (extra fingers were ignored at touchstart).
  const findActive = (list: TouchList): Touch | null => {
    for (let i = 0; i < list.length; i++) {
      const t = list.item(i);
      if (t && t.identifier === activeTouchId) return t;
    }
    return null;
  };

  const clearDragStyles = (): void => {
    sheetEl?.classList.remove('tool-panel-dragging');
    sheetEl?.style.removeProperty('transform');
    scrimEl?.style.removeProperty('opacity');
  };

  const onTouchStart = (e: TouchEvent): void => {
    // Stop first, before anything else: AppShell's drawer gesture listens
    // window-wide, and its arbitration would otherwise claim a horizontal drag
    // that starts on the sheet — a rightward pull at scrollLeft 0 would haul the
    // nav drawer out from under this modal scrim. Stopping here keeps the touch
    // off that window listener. (The scrim blocks it the same way via its own
    // JSX onTouchStart; click-dismiss is a separate event and still fires.)
    e.stopPropagation();
    if (props.desktop) return; // desktop is a non-modal sidebar, not a sheet
    // Self-heal a leaked sequence (mirrors AppShell): touch events fire at the
    // element the finger went DOWN on, so if a re-render detaches that node
    // mid-touch (streaming tool output), its touchend/touchcancel never bubble
    // to the sheet and the tracked id — plus a claimed drag's inline styles —
    // would pin until the sheet unmounts. Gone from e.touches, or "back" in
    // changedTouches (identifier reuse): the old sequence ended unheard.
    if (activeTouchId !== null && (findActive(e.changedTouches) || !findActive(e.touches))) {
      activeTouchId = null;
      if (gesture.cancel().kind === 'cancel-reset') clearDragStyles();
    }
    if (activeTouchId !== null) return; // one finger tracked at a time
    if (e.touches.length !== 1) return; // multi-touch: ignore
    const t = e.changedTouches[0]!;
    const decision = gesture.start(
      { x: t.clientX, y: t.clientY, t: e.timeStamp },
      {
        sheetHeight: sheetEl?.offsetHeight ?? 0,
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
      e.preventDefault(); // claimed — override page scroll (needs passive:false)
      sheetEl?.classList.add('tool-panel-dragging');
      sheetEl?.style.setProperty('transform', `translateY(${d.y}px)`);
      scrimEl?.style.setProperty('opacity', String(d.progress));
    } else if (d.kind === 'ignore') {
      activeTouchId = null; // handed to the page (horizontal / upward intent)
    }
    // 'track': intent still undecided — nothing to apply yet.
  };
  const onTouchEnd = (e: TouchEvent): void => {
    if (activeTouchId === null) return;
    const t = findActive(e.changedTouches);
    if (!t) return; // a different finger lifted
    activeTouchId = null;
    const d = gesture.end({ x: t.clientX, y: t.clientY, t: e.timeStamp });
    if (d.kind === 'commit-dismiss') {
      // The sheet unmounts on close and the house has no exit animation
      // (InstallSheet precedent), so drop the drag styles and close immediately.
      clearDragStyles();
      props.onClose();
    } else if (d.kind === 'settle-open') {
      // Release under the threshold: clear the inline transform/opacity and the
      // dragging class so the CSS transition eases the sheet back to rest.
      clearDragStyles();
    }
    // 'ignore' (a tap): nothing was applied, nothing to clear.
  };
  const onTouchCancel = (): void => {
    if (activeTouchId === null) return;
    activeTouchId = null;
    // OS stole the touch mid-drag (edge swipe, nav gesture): animate back to
    // rest — the open state never changed, so clearing the styles is enough.
    if (gesture.cancel().kind === 'cancel-reset') clearDragStyles();
  };

  // Bind on the element itself so the listener lifecycle follows the sheet's
  // mount/unmount (<Show when={props.target}>); never on window.
  const bindSheet = (el: HTMLElement): void => {
    sheetEl = el;
    el.addEventListener('touchstart', onTouchStart, { passive: true });
    el.addEventListener('touchmove', onTouchMove, { passive: false });
    el.addEventListener('touchend', onTouchEnd, { passive: true });
    el.addEventListener('touchcancel', onTouchCancel, { passive: true });
    onCleanup(() => {
      el.removeEventListener('touchstart', onTouchStart);
      el.removeEventListener('touchmove', onTouchMove);
      el.removeEventListener('touchend', onTouchEnd);
      el.removeEventListener('touchcancel', onTouchCancel);
      activeTouchId = null; // drop any in-flight tracking with the element
    });
  };

  // --- desktop drag-to-resize (issue #154 §4) -------------------------------
  // The sidebar is flush to the window's right edge, so a drag's target width is
  // the distance from the pointer to that edge; clamp to [320px, 60vw]. Width is
  // seeded from localStorage and applied inline ONLY in the desktop variant when
  // a value exists — a first-time user keeps the CSS default clamp. Persisted on
  // release (pointerup), never per move. Pointer events, no lib; setPointerCapture
  // is optional-chained (jsdom lacks it). Listeners ride the drag, added on
  // pointerdown and dropped on up/cancel (and on unmount, belt-and-suspenders).
  const [width, setWidth] = createSignal<number | null>(readStoredWidth());
  const [resizing, setResizing] = createSignal(false);

  // The inline width for the desktop aside: null (first-time) leaves the CSS
  // default; a stored/dragged value is re-clamped against the CURRENT viewport
  // at render, so a huge stored value can never exceed 60vw on this screen.
  const panelWidth = (): string | undefined => {
    if (!props.desktop) return undefined;
    const w = width();
    return w === null ? undefined : `${clampPanelWidth(w, window.innerWidth)}px`;
  };

  const onResizeMove = (e: PointerEvent): void => {
    setWidth(clampPanelWidth(window.innerWidth - e.clientX, window.innerWidth));
  };
  const endResize = (): void => {
    if (!resizing()) return;
    setResizing(false);
    window.removeEventListener('pointermove', onResizeMove);
    window.removeEventListener('pointerup', endResize);
    window.removeEventListener('pointercancel', endResize);
    const w = width();
    if (w !== null) writeStoredWidth(w); // persist on release, not per move
  };
  const onResizeStart = (e: PointerEvent): void => {
    e.preventDefault();
    setResizing(true);
    (e.currentTarget as Element).setPointerCapture?.(e.pointerId);
    window.addEventListener('pointermove', onResizeMove);
    window.addEventListener('pointerup', endResize);
    window.addEventListener('pointercancel', endResize);
  };
  onCleanup(() => {
    window.removeEventListener('pointermove', onResizeMove);
    window.removeEventListener('pointerup', endResize);
    window.removeEventListener('pointercancel', endResize);
  });

  const closeButton = () => (
    <button
      type="button"
      class="icon-btn tool-panel-btn"
      aria-label="Close"
      onClick={() => props.onClose()}
    >
      <Icon name="x" />
    </button>
  );

  return (
    <Show when={props.target}>
      {/* Scrim and card are siblings, not nested: the dismiss handler lives only
          on the scrim, so a click in the card never bubbles to a dismiss (issue
          #142 idiom). Phone only — the desktop sidebar is non-modal. */}
      <Show when={!props.desktop}>
        {/* onTouchStart stops the drawer gesture the same way the sheet does —
            see onTouchStart below. Click-dismiss is a separate event, unaffected. */}
        <div
          ref={scrimEl}
          class="tool-scrim"
          aria-hidden="true"
          onClick={() => props.onClose()}
          onTouchStart={(e) => e.stopPropagation()}
        />
      </Show>
      <aside
        ref={bindSheet}
        classList={{
          'tool-panel': true,
          'tool-panel-sheet': !props.desktop,
          'tool-panel-side': props.desktop,
          'tool-panel-resizing': resizing(),
        }}
        style={{ width: panelWidth() }}
        role={props.desktop ? undefined : 'dialog'}
        aria-modal={props.desktop ? undefined : 'true'}
        aria-labelledby={headingId}
      >
        {/* Desktop drag-to-resize handle (issue #154 §4): a thin separator over
            the sidebar's left border. Pointer events only; touch-action:none so
            a touch-drag here never scrolls the page. Desktop-only. */}
        <Show when={props.desktop}>
          <div
            class="tool-panel-resize"
            role="separator"
            aria-orientation="vertical"
            aria-label="Resize panel"
            onPointerDown={onResizeStart}
          />
        </Show>
        {/* Pure-CSS drag pill — a bottom-sheet affordance, phone only. */}
        <Show when={!props.desktop}>
          <div class="tool-panel-grabber" aria-hidden="true" />
        </Show>
        <Switch>
          {/* Detail page. */}
          <Match when={detailTool()}>
            {(tool) => (
              <>
                <div class="tool-panel-head">
                  {/* Back only for a group (entry 'list') on MOBILE; a lone chip
                      has no list to return to, and the desktop sidebar is
                      file-detail only — never a list, so never a back button
                      (issue #154 §2). */}
                  <Show when={!props.desktop && props.target?.entry === 'list'}>
                    <button
                      type="button"
                      class="icon-btn tool-panel-btn"
                      aria-label="Back to list"
                      onClick={() => setDetailSeq(null)}
                    >
                      <Icon name="arrow-left" />
                    </button>
                  </Show>
                  <h2 id={headingId} class="tool-panel-title">
                    <span class="tool-title">{tool().tool?.title}</span>
                    <span class="tool-status">{toolStatusMark(tool().tool?.status)}</span>
                  </h2>
                  {closeButton()}
                </div>
                {/* Detail body renders by view.kind — extracted to ToolViews.tsx
                    (issue #154) so a second surface can reuse it. `tool()` is
                    passed live (never captured) so an SSE refetch's grown text
                    flows straight through; a missing view falls back to the raw
                    input/output panes, byte-for-byte the pre-#146 behaviour. */}
                <div class="tool-panel-content">
                  <ToolViewBody message={tool()} />
                </div>
              </>
            )}
          </Match>
          {/* List page (fallback: props.target is truthy inside the outer Show). */}
          <Match when={props.target}>
            {(target) => (
              <>
                <div class="tool-panel-head">
                  <h2 id={headingId} class="tool-panel-title">
                    {target().tools.length} tool calls
                  </h2>
                  {closeButton()}
                </div>
                <div class="tool-panel-content">
                  <For each={target().tools}>
                    {(m) => (
                      <button
                        type="button"
                        classList={{
                          'tool-panel-row': true,
                          'tool-error': m.tool?.status === 'error',
                        }}
                        onClick={() => setDetailSeq(m.seq)}
                      >
                        <span class="tool-title">{m.tool?.title}</span>
                        <span class="tool-status">{toolStatusMark(m.tool?.status)}</span>
                      </button>
                    )}
                  </For>
                </div>
              </>
            )}
          </Match>
        </Switch>
      </aside>
    </Show>
  );
}
