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
//     parent page supplies the flex row that stretches it to full height; our
//     CSS only fixes its width and the left border.
// It stays ONE <aside> in the JSX (attributes vary by `props.desktop`); only
// the scrim lives in its own <Show>, so the internal `detailSeq` signal is not
// torn down when the breakpoint flips.
//
// TWO pages, driven by the internal `detailSeq` signal:
//   - List: one row per tool call (title + status mark), for a group target.
//   - Detail: the chip expansion, roomier (the 40vh chip cap is dropped) — the
//     input and output <pre> blocks, reusing the chat chip's classes.
// A group target (`entry: 'list'`) opens at the list and pushes to detail on a
// row tap (with a back affordance); a lone chip (`entry: 'detail'`) opens
// straight at detail and never shows a back button.
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
import type { ChatMessage, ToolView } from '../api';
import { createSheetGesture } from '../lib/sheetGesture';
import Icon from './Icon';

/** The status mark for a tool call. Moved here from RunChat for issue #145 —
 *  the chat chips and the panel both render it. */
export function toolStatusMark(status?: 'running' | 'ok' | 'error'): string {
  if (status === 'ok') return '✓';
  if (status === 'error') return '✕';
  return '…';
}

// --- Rich detail views (issue #146) ----------------------------------------
// The server tags a tool call with a `view` union; the detail page renders by
// kind and stays provider-blind (no tool-name logic, no brand token). These
// readers each re-derive the typed variant from a message resolved LIVE — the
// caller passes tool() at render time, never a captured object, so an SSE
// refetch that grows view.text flows straight through (same rule the whole
// panel lives by, see the header note). A missing/unknown view falls back to
// the raw input/output <pre> blocks.

type DiffKind = Extract<ToolView, { kind: 'diff' | 'write' }>;

/** A path-carrying view (diff or write) or undefined. */
function pathView(m: ChatMessage): DiffKind | undefined {
  const v = m.tool?.view;
  return v?.kind === 'diff' || v?.kind === 'write' ? v : undefined;
}

/** The command view or undefined. */
function commandView(m: ChatMessage): Extract<ToolView, { kind: 'command' }> | undefined {
  const v = m.tool?.view;
  return v?.kind === 'command' ? v : undefined;
}

type DiffLine = { text: string; cls: string };

/** Split diff/write text into per-line class assignments. Prefix precedence is
 *  load-bearing: the multi-char file/hunk headers (`+++`, `---`, `@@`) MUST be
 *  tested before the single-char `+`/`-` markers, or a `+++ b/foo` header would
 *  read as an added line. `forceAdd` paints every line added for the write view
 *  — a new file is, in effect, an all-insert diff — and short-circuits the
 *  prefix test so raw content that happens to start with `-`/`@@` isn't mistyped.
 *  Empty lines are kept (blank context lines carry diff alignment); the CSS
 *  gives each line span a min-height so a blank one still occupies a row. */
function diffLines(text: string, forceAdd = false): DiffLine[] {
  return text.split('\n').map((line) => {
    let cls = 'tool-diff-ctx';
    if (forceAdd) cls = 'tool-diff-add';
    else if (line.startsWith('+++') || line.startsWith('---') || line.startsWith('@@'))
      cls = 'tool-diff-hunk';
    else if (line.startsWith('+')) cls = 'tool-diff-add';
    else if (line.startsWith('-')) cls = 'tool-diff-del';
    return { text: line, cls };
  });
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
  // (never captured). null means show the list. A lone chip (entry 'detail')
  // always shows tools[0]; a group resolves the pushed seq and falls back to the
  // list if it no longer resolves after a refetch.
  const detailTool = (): ChatMessage | null => {
    const t = props.target;
    if (!t) return null;
    if (t.entry === 'detail') return t.tools[0] ?? null;
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
        }}
        role={props.desktop ? undefined : 'dialog'}
        aria-modal={props.desktop ? undefined : 'true'}
        aria-labelledby={headingId}
      >
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
                  {/* Back only for a group (entry 'list'); a lone chip has no
                      list to return to. */}
                  <Show when={props.target?.entry === 'list'}>
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
                {/* Detail body renders by view.kind (issue #146). Every read
                    goes through tool() so an SSE refetch's grown text flows in;
                    a missing view falls back to the raw input/output panes,
                    byte-for-byte the pre-#146 behaviour. */}
                <div class="tool-panel-content">
                  <Switch
                    fallback={
                      <>
                        <Show when={tool().tool?.input}>
                          <pre class="tool-body mono">{tool().tool?.input}</pre>
                        </Show>
                        <Show when={tool().tool?.output}>
                          <pre class="tool-body tool-output mono">{tool().tool?.output}</pre>
                        </Show>
                      </>
                    }
                  >
                    {/* diff / write: a path header over a line-styled body. Write
                        forces every line to the added style (a new file is an
                        all-insert diff). The raw output block rides below ONLY on
                        error — a successful edit's "file updated" line is noise,
                        but a failed one must still surface its error text. */}
                    <Match when={pathView(tool())}>
                      {(view) => (
                        <>
                          <div class="tool-view-path mono">{view().path}</div>
                          <pre class="tool-body mono tool-diff">
                            <For each={diffLines(view().text, view().kind === 'write')}>
                              {(ln) => <span class={ln.cls}>{ln.text}</span>}
                            </For>
                          </pre>
                          <Show when={tool().tool?.status === 'error' && tool().tool?.output}>
                            <pre class="tool-body tool-output mono">{tool().tool?.output}</pre>
                          </Show>
                        </>
                      )}
                    </Match>
                    {/* command: a $-prefixed command line, then its output as
                        terminal text below (shown whenever non-empty, any
                        status — a command speaks through its stdout). */}
                    <Match when={commandView(tool())}>
                      {(view) => (
                        <>
                          <pre class="tool-body mono tool-cmd">
                            <span class="tool-cmd-prompt">$ </span>
                            {view().command}
                          </pre>
                          <Show when={tool().tool?.output}>
                            <pre class="tool-body tool-output mono">{tool().tool?.output}</pre>
                          </Show>
                        </>
                      )}
                    </Match>
                  </Switch>
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
