// ToolPanel contract (issue #145): closed renders nothing; a group target opens
// at a list of tool-call rows and pushes to a per-tool detail on tap (with a
// back button); a lone chip opens straight at detail with no back button. The
// phone container is a modal sheet with a dismiss-only scrim; the desktop
// container is a non-modal in-flow sidebar with no scrim and Escape-to-close.
// Everything live falls out of props: the panel re-resolves the shown tool
// through props.target.tools on every refetch and never resets on a same-key
// refetch.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ChatMessage, ToolView } from '../api';
import ToolPanel, {
  clampPanelWidth,
  toolStatusMark,
  type PanelTarget,
  type ToolPanelProps,
} from './ToolPanel';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function tool(
  seq: number,
  title: string,
  status: 'running' | 'ok' | 'error',
  io?: {
    input?: string;
    output?: string;
    view?: ToolView;
  },
): ChatMessage {
  return {
    seq,
    kind: 'tool',
    tool: { name: title, title, status, input: io?.input, output: io?.output, view: io?.view },
  };
}

/** A lone-chip target (entry 'detail') around one tool — opens straight at the
 *  detail page, the shortest path to exercising the rich-view renderers. */
function detailTarget(t: ChatMessage): PanelTarget {
  return { key: `tool:${t.seq}`, entry: 'detail', tools: [t] };
}

function groupTarget(): PanelTarget {
  return {
    key: 'group:1',
    entry: 'list',
    tools: [
      tool(1, 'read foo.ts', 'ok', { input: 'path: foo.ts', output: 'file contents' }),
      tool(2, 'write bar.ts', 'error', { input: 'path: bar.ts', output: 'permission denied' }),
      tool(3, 'grep baz', 'running', { input: 'pattern: baz' }),
    ],
  };
}

/** A lone file tool (a diff), for the desktop file-sidebar / resize tests. */
function fileTool(seq = 5): ChatMessage {
  return tool(seq, 'edit foo.ts', 'ok', {
    view: { kind: 'diff', path: 'foo.ts', text: '@@ -1 +1 @@\n+x' },
  });
}

/** Mount with a live `target` and `desktop` signal so a test can drive prop
 *  changes (retarget, refetch, breakpoint flip) after mount. */
function mountPanel(init: Partial<ToolPanelProps> = {}): {
  onClose: ReturnType<typeof vi.fn>;
  setTarget: (t: PanelTarget | null) => void;
  setDesktop: (d: boolean) => void;
} {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onClose = vi.fn();
  const [target, setTarget] = createSignal<PanelTarget | null>(init.target ?? null);
  const [desktop, setDesktop] = createSignal<boolean>(init.desktop ?? false);
  dispose = render(
    () => <ToolPanel target={target()} desktop={desktop()} onClose={onClose} />,
    container,
  );
  return { onClose, setTarget, setDesktop };
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container?.remove();
  localStorage.clear(); // the resize width persists under lab.tool-panel-width
});

function aside(): HTMLElement | null {
  return container.querySelector<HTMLElement>('aside.tool-panel');
}
function scrim(): HTMLElement | null {
  return container.querySelector<HTMLElement>('.tool-scrim');
}
function heading(): HTMLElement | null {
  return container.querySelector<HTMLElement>('.tool-panel-title');
}
function rows(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('.tool-panel-row'));
}
function pres(): HTMLPreElement[] {
  return Array.from(container.querySelectorAll<HTMLPreElement>('pre.tool-body'));
}
function backBtn(): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>('button[aria-label="Back to list"]');
}
function closeBtn(): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>('button[aria-label="Close"]');
}
// --- Rich-view (issue #146) queries ----------------------------------------
function pathHeader(): HTMLElement | null {
  return container.querySelector<HTMLElement>('.tool-view-path');
}
function diffSpans(): HTMLSpanElement[] {
  return Array.from(container.querySelectorAll<HTMLSpanElement>('pre.tool-diff span'));
}
function spanFor(text: string): HTMLSpanElement | undefined {
  return diffSpans().find((s) => s.textContent === text);
}
function cmdPre(): HTMLElement | null {
  return container.querySelector<HTMLElement>('pre.tool-cmd');
}
function outputPre(): HTMLElement | null {
  return container.querySelector<HTMLElement>('pre.tool-output');
}
function pressEscape(defaultPrevented = false): void {
  const e = new KeyboardEvent('keydown', { key: 'Escape', cancelable: true });
  if (defaultPrevented) e.preventDefault();
  window.dispatchEvent(e);
}

// --- Touch gesture fabrication (jsdom has no TouchEvent) --------------------
// jsdom ships no TouchEvent constructor and lays everything out at zero, so we
// fake a bubbling/cancelable Event and pin the fields the binding reads
// (touches / changedTouches / target / timeStamp), plus stub offsetHeight and
// scroll metrics per test. We are exercising the BINDING; the pure state
// machine is already unit-tested in lib/sheetGesture.test.ts.

type TouchInit = { identifier: number; clientX: number; clientY: number };

function touchList(touches: TouchInit[]): TouchList {
  const list: Record<string | number, unknown> = {
    length: touches.length,
    item: (i: number) => touches[i] ?? null,
  };
  touches.forEach((t, i) => {
    list[i] = t;
  });
  return list as unknown as TouchList;
}

/** A fake touch event: `touch` is the single finger (id 1); `t` its timestamp;
 *  `target` the element the touch started over (defaults to the aside). */
function touchEvent(
  type: 'touchstart' | 'touchmove' | 'touchend' | 'touchcancel',
  touch: { x: number; y: number } | null,
  o: { t?: number; target?: EventTarget } = {},
): Event {
  const e = new Event(type, { bubbles: true, cancelable: true });
  const touches = touch ? [{ identifier: 1, clientX: touch.x, clientY: touch.y }] : [];
  const list = touchList(touches);
  Object.defineProperty(e, 'touches', { value: list, configurable: true });
  Object.defineProperty(e, 'changedTouches', { value: list, configurable: true });
  if (o.target) Object.defineProperty(e, 'target', { value: o.target, configurable: true });
  if (o.t !== undefined) Object.defineProperty(e, 'timeStamp', { value: o.t, configurable: true });
  return e;
}

/** Give the aside a non-zero layout height (jsdom reports 0). */
function stubHeight(el: HTMLElement, px: number): void {
  Object.defineProperty(el, 'offsetHeight', { value: px, configurable: true });
}

function hasDragging(el: HTMLElement | null): boolean {
  return el?.classList.contains('tool-panel-dragging') ?? false;
}

// --- Pointer event fabrication (issue #154 resize) --------------------------
// jsdom ships a PointerEvent via its MouseEvent fallback in recent versions;
// where it doesn't, a bubbling MouseEvent carries the one field the drag reads
// (clientX). setPointerCapture is optional-chained in the component (jsdom lacks
// it), and pointerId is read loosely, so either constructor suffices.
function pointerEvt(type: string, clientX: number): Event {
  if (typeof PointerEvent === 'function') {
    return new PointerEvent(type, { bubbles: true, cancelable: true, clientX, pointerId: 1 });
  }
  return new MouseEvent(type, { bubbles: true, cancelable: true, clientX });
}

function resizeHandle(): HTMLElement | null {
  return container.querySelector<HTMLElement>('.tool-panel-resize');
}

describe('ToolPanel closed', () => {
  it('renders nothing when target is null', () => {
    mountPanel({ target: null });
    expect(aside()).toBeNull();
    expect(scrim()).toBeNull();
    expect(container.innerHTML).toBe('');
  });
});

describe('ToolPanel group (list page) on mobile', () => {
  it('renders a row per tool with title and status mark, and a scrim', () => {
    mountPanel({ target: groupTarget(), desktop: false });

    expect(scrim()).not.toBeNull();
    expect(heading()?.textContent).toBe('3 tool calls');

    const r = rows();
    expect(r).toHaveLength(3);
    expect(r[0]?.textContent).toContain('read foo.ts');
    expect(r[0]?.textContent).toContain(toolStatusMark('ok')); // ✓
    expect(r[1]?.textContent).toContain(toolStatusMark('error')); // ✕
    expect(r[1]?.classList.contains('tool-error')).toBe(true);
    expect(r[2]?.textContent).toContain(toolStatusMark('running')); // …
  });

  it('a row click opens that tool at detail with input/output and a back button', () => {
    mountPanel({ target: groupTarget(), desktop: false });

    rows()[0]?.click();

    expect(rows()).toHaveLength(0); // list gone, detail shown
    expect(backBtn()).not.toBeNull();
    expect(heading()?.textContent).toContain('read foo.ts');

    const p = pres();
    expect(p).toHaveLength(2);
    expect(p[0]?.textContent).toBe('path: foo.ts');
    expect(p[1]?.textContent).toBe('file contents');

    backBtn()?.click();
    expect(rows()).toHaveLength(3); // back to the list
    expect(backBtn()).toBeNull();
  });
});

describe('ToolPanel lone chip (detail entry)', () => {
  it('opens directly at detail with no back button', () => {
    const target: PanelTarget = {
      key: 'tool:9',
      entry: 'detail',
      tools: [tool(9, 'bash ls', 'ok', { input: 'ls -la', output: 'total 0' })],
    };
    mountPanel({ target, desktop: false });

    expect(rows()).toHaveLength(0);
    expect(backBtn()).toBeNull();
    expect(heading()?.textContent).toContain('bash ls');
    expect(pres().map((p) => p.textContent)).toEqual(['ls -la', 'total 0']);
  });
});

// --- Rich detail views (issue #146) ----------------------------------------
// The detail body switches on view.kind; the panel stays provider-blind (it
// never inspects the tool name). No view → the raw input/output fallback, which
// the lone-chip / group tests above already cover; one absence test here pins
// that the switch didn't swallow it.

describe('ToolPanel diff view', () => {
  const diffText = [
    '--- a/foo.ts',
    '+++ b/foo.ts',
    '@@ -1,3 +1,3 @@',
    ' unchanged',
    '-was here',
    '+is here',
  ].join('\n');

  it('renders a path header and styles lines by prefix (headers beat +/-)', () => {
    const t = tool(5, 'edit foo.ts', 'ok', {
      view: { kind: 'diff', path: 'foo.ts', text: diffText },
      output: 'file updated',
    });
    mountPanel({ target: detailTarget(t), desktop: false });

    expect(pathHeader()?.textContent).toBe('foo.ts');

    // Single-char markers.
    expect(spanFor('+is here')?.classList.contains('tool-diff-add')).toBe(true);
    expect(spanFor('-was here')?.classList.contains('tool-diff-del')).toBe(true);
    // Hunk marker.
    expect(spanFor('@@ -1,3 +1,3 @@')?.classList.contains('tool-diff-hunk')).toBe(true);
    // The precedence trap: +++/--- file headers are HUNK, never add/removed.
    const plus = spanFor('+++ b/foo.ts');
    expect(plus?.classList.contains('tool-diff-hunk')).toBe(true);
    expect(plus?.classList.contains('tool-diff-add')).toBe(false);
    const minus = spanFor('--- a/foo.ts');
    expect(minus?.classList.contains('tool-diff-hunk')).toBe(true);
    expect(minus?.classList.contains('tool-diff-del')).toBe(false);
    // A context line carries neither.
    expect(spanFor(' unchanged')?.classList.contains('tool-diff-ctx')).toBe(true);
  });

  it('hides the output block on ok but shows it on error', () => {
    const ok = tool(5, 'edit foo.ts', 'ok', {
      view: { kind: 'diff', path: 'foo.ts', text: diffText },
      output: 'file updated',
    });
    const { setTarget } = mountPanel({ target: detailTarget(ok), desktop: false });
    expect(outputPre()).toBeNull(); // success output is noise

    const failed = tool(6, 'edit foo.ts', 'error', {
      view: { kind: 'diff', path: 'foo.ts', text: diffText },
      output: 'patch does not apply',
    });
    setTarget(detailTarget(failed));
    expect(outputPre()?.textContent).toBe('patch does not apply');
  });

  it('re-renders grown diff text on a same-key refetch (liveness)', () => {
    const running = tool(8, 'edit big.ts', 'running', {
      view: { kind: 'diff', path: 'big.ts', text: '@@ -1 +1 @@\n+first' },
    });
    const { setTarget } = mountPanel({ target: detailTarget(running), desktop: false });
    expect(spanFor('+first')).not.toBeUndefined();
    expect(spanFor('+second')).toBeUndefined();

    // Same seq → same key → no page reset; message replaced wholesale.
    const done = tool(8, 'edit big.ts', 'ok', {
      view: { kind: 'diff', path: 'big.ts', text: '@@ -1 +2 @@\n+first\n+second' },
    });
    setTarget(detailTarget(done));
    expect(spanFor('+second')).not.toBeUndefined();
  });
});

describe('ToolPanel write view', () => {
  it('renders a path header with every line styled as added', () => {
    const t = tool(7, 'create baz.ts', 'ok', {
      view: { kind: 'write', path: 'baz.ts', text: 'line one\n-not a deletion\n@@ not a hunk' },
      output: 'file created',
    });
    mountPanel({ target: detailTarget(t), desktop: false });

    expect(pathHeader()?.textContent).toBe('baz.ts');
    const spans = diffSpans();
    expect(spans).toHaveLength(3);
    // A new file is an all-insert diff: even a line that looks like a deletion
    // or a hunk header is added, never mistyped by prefix.
    expect(spans.every((s) => s.classList.contains('tool-diff-add'))).toBe(true);
    expect(outputPre()).toBeNull(); // ok write: output suppressed
  });
});

describe('ToolPanel command view', () => {
  it('renders a $-prefixed command and its output below', () => {
    const t = tool(9, 'run build', 'ok', {
      view: { kind: 'command', command: 'npm run build' },
      output: 'built in 2s',
    });
    mountPanel({ target: detailTarget(t), desktop: false });

    const cmd = cmdPre();
    expect(cmd?.querySelector('.tool-cmd-prompt')?.textContent).toBe('$ ');
    expect(cmd?.textContent).toContain('npm run build');
    expect(outputPre()?.textContent).toBe('built in 2s'); // shown regardless of status
  });
});

describe('ToolPanel read view', () => {
  it('renders a path header and the excerpt text, with no diff coloring', () => {
    const t = tool(11, 'read config.ts', 'ok', {
      view: { kind: 'read', path: 'config.ts', text: 'export const x = 1;' },
    });
    mountPanel({ target: detailTarget(t), desktop: false });

    expect(pathHeader()?.textContent).toBe('config.ts');
    expect(diffSpans()).toHaveLength(0);
    expect(pres().map((p) => p.textContent)).toEqual(['export const x = 1;']);
  });

  it('a still-running read with an empty excerpt shows the header but no body pre', () => {
    const t = tool(12, 'read config.ts', 'running', {
      view: { kind: 'read', path: 'config.ts', text: '' },
    });
    mountPanel({ target: detailTarget(t), desktop: false });

    expect(pathHeader()?.textContent).toBe('config.ts');
    expect(pres()).toHaveLength(0);
  });
});

describe('ToolPanel absent view (fallback)', () => {
  it('renders the raw input/output panes when no view is present', () => {
    const t = tool(10, 'read notes', 'ok', { input: 'path: notes.md', output: 'hello' });
    mountPanel({ target: detailTarget(t), desktop: false });

    expect(pathHeader()).toBeNull();
    expect(diffSpans()).toHaveLength(0);
    expect(cmdPre()).toBeNull();
    expect(pres().map((p) => p.textContent)).toEqual(['path: notes.md', 'hello']);
  });
});

describe('ToolPanel close affordance', () => {
  it('the ✕ button fires onClose on the list page', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: false });
    closeBtn()?.click();
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('the ✕ button fires onClose on the desktop detail sidebar', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: true });
    // Desktop is file-detail only (issue #154 §2): no list, no rows to push —
    // the ✕ is right there on the detail head.
    expect(rows()).toHaveLength(0);
    closeBtn()?.click();
    expect(onClose).toHaveBeenCalledOnce();
  });
});

describe('ToolPanel scrim vs card', () => {
  it('scrim click fires onClose; a click inside the aside does not', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: false });

    aside()?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(onClose).not.toHaveBeenCalled();

    scrim()?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('desktop renders no scrim', () => {
    mountPanel({ target: groupTarget(), desktop: true });
    expect(scrim()).toBeNull();
    expect(aside()).not.toBeNull();
  });
});

describe('ToolPanel accessibility', () => {
  it('mobile aside is a labelled modal dialog', () => {
    mountPanel({ target: groupTarget(), desktop: false });
    const el = aside();
    expect(el?.getAttribute('role')).toBe('dialog');
    expect(el?.getAttribute('aria-modal')).toBe('true');

    const labelledBy = el?.getAttribute('aria-labelledby');
    expect(labelledBy).toBeTruthy();
    const label = container.querySelector(`#${labelledBy}`);
    expect(label?.textContent).toBe('3 tool calls');
  });

  it('desktop aside is a bare complementary region (no role, no aria-modal)', () => {
    mountPanel({ target: groupTarget(), desktop: true });
    const el = aside();
    expect(el?.hasAttribute('role')).toBe(false);
    expect(el?.hasAttribute('aria-modal')).toBe(false);
    expect(el?.getAttribute('aria-labelledby')).toBeTruthy();
  });
});

describe('ToolPanel Escape', () => {
  it('fires onClose on desktop', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: true });
    pressEscape();
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('does not fire on mobile', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: false });
    pressEscape();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('does not fire when the event was already defaultPrevented (desktop)', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: true });
    pressEscape(true);
    expect(onClose).not.toHaveBeenCalled();
  });
});

describe('ToolPanel retarget resets the page', () => {
  it('swapping to a different key returns to the new target entry page', () => {
    const { setTarget } = mountPanel({ target: groupTarget(), desktop: false });
    rows()[1]?.click();
    expect(backBtn()).not.toBeNull(); // detail open

    setTarget({
      key: 'group:100',
      entry: 'list',
      tools: [tool(100, 'other-a', 'ok'), tool(101, 'other-b', 'ok')],
    });

    expect(backBtn()).toBeNull(); // reset to the new list
    expect(rows()).toHaveLength(2);
    expect(heading()?.textContent).toBe('2 tool calls');
  });
});

describe('ToolPanel liveness (same-key refetch)', () => {
  it('an open detail shows grown output and does not reset', () => {
    const { setTarget } = mountPanel({ target: groupTarget(), desktop: false });
    rows()[2]?.click(); // the running tool, seq 3, no output yet
    expect(pres().some((p) => p.textContent === 'pattern: baz')).toBe(true);
    expect(pres().some((p) => p.textContent?.includes('MATCH'))).toBe(false);

    // Same key, message objects replaced wholesale, seq-3 output now present.
    setTarget({
      key: 'group:1',
      entry: 'list',
      tools: [
        tool(1, 'read foo.ts', 'ok', { input: 'path: foo.ts', output: 'file contents' }),
        tool(2, 'write bar.ts', 'error', { input: 'path: bar.ts', output: 'permission denied' }),
        tool(3, 'grep baz', 'ok', { input: 'pattern: baz', output: 'MATCH at line 4' }),
      ],
    });

    expect(backBtn()).not.toBeNull(); // still on the pushed detail
    expect(pres().some((p) => p.textContent === 'MATCH at line 4')).toBe(true);
  });

  it('an open list grows by a row when a tool is appended', () => {
    const { setTarget } = mountPanel({ target: groupTarget(), desktop: false });
    expect(rows()).toHaveLength(3);

    const grown = groupTarget();
    grown.tools = [...grown.tools, tool(4, 'edit qux', 'running')];
    setTarget(grown);

    expect(rows()).toHaveLength(4);
    expect(heading()?.textContent).toBe('4 tool calls');
  });
});

describe('ToolPanel breakpoint flip', () => {
  it('swaps mobile detail chrome for the desktop file sidebar (no back button)', () => {
    const { setDesktop } = mountPanel({ target: groupTarget(), desktop: false });
    rows()[0]?.click(); // mobile: push tools[0] "read foo.ts" to detail, with back
    expect(aside()?.getAttribute('role')).toBe('dialog');
    expect(scrim()).not.toBeNull();
    expect(backBtn()).not.toBeNull();
    expect(heading()?.textContent).toContain('read foo.ts');

    setDesktop(true);

    // Desktop chrome: dialog role and scrim gone. It is file-detail only
    // (issue #154 §2) — tools[0], and no back button (never a list to return to).
    expect(aside()?.hasAttribute('role')).toBe(false);
    expect(scrim()).toBeNull();
    expect(backBtn()).toBeNull();
    expect(heading()?.textContent).toContain('read foo.ts');
  });
});

describe('ToolPanel drag-to-dismiss (mobile sheet)', () => {
  it('a downward drag past half-height applies live styles then dismisses', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: false });
    const el = aside()!;
    stubHeight(el, 400); // half = 200px

    // Start, then a claimed downward drag to y=400 (dy=300, past half).
    el.dispatchEvent(touchEvent('touchstart', { x: 50, y: 100 }, { t: 0, target: el }));
    el.dispatchEvent(touchEvent('touchmove', { x: 50, y: 400 }, { t: 50, target: el }));

    // Mid-drag: dragging class + live transform + scrim opacity are applied.
    expect(hasDragging(el)).toBe(true);
    expect(el.style.transform).toContain('translateY(300px)');
    expect(scrim()?.style.opacity).toBe('0.25'); // (400 - 300) / 400

    // Release well after the velocity window so position (not a flick) decides.
    el.dispatchEvent(touchEvent('touchend', { x: 50, y: 400 }, { t: 500, target: el }));

    expect(onClose).toHaveBeenCalledOnce();
    expect(hasDragging(el)).toBe(false);
    expect(el.style.transform).toBe('');
  });

  it('a short downward drag released under half settles back open (no close)', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: false });
    const el = aside()!;
    stubHeight(el, 400);

    el.dispatchEvent(touchEvent('touchstart', { x: 50, y: 100 }, { t: 0, target: el }));
    el.dispatchEvent(touchEvent('touchmove', { x: 50, y: 180 }, { t: 50, target: el }));

    // Claimed (dy=80 > INTENT), but only 80px < 200px half.
    expect(hasDragging(el)).toBe(true);
    expect(el.style.transform).toContain('translateY(80px)');

    el.dispatchEvent(touchEvent('touchend', { x: 50, y: 180 }, { t: 800, target: el }));

    expect(onClose).not.toHaveBeenCalled();
    expect(hasDragging(el)).toBe(false);
    expect(el.style.transform).toBe('');
    expect(scrim()?.style.opacity).toBe('');
  });

  it('a touch starting in a scrolled content list never claims (scroller wins)', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: false });
    const el = aside()!;
    stubHeight(el, 400);

    // The .tool-panel-content list is scrolled down: overflow-y set inline so
    // jsdom's getComputedStyle sees it; scroll metrics stubbed (jsdom is 0).
    const content = container.querySelector<HTMLElement>('.tool-panel-content')!;
    content.style.overflowY = 'auto';
    Object.defineProperty(content, 'scrollTop', { value: 40, configurable: true });
    Object.defineProperty(content, 'scrollHeight', { value: 800, configurable: true });
    Object.defineProperty(content, 'clientHeight', { value: 200, configurable: true });

    el.dispatchEvent(touchEvent('touchstart', { x: 50, y: 100 }, { t: 0, target: content }));
    el.dispatchEvent(touchEvent('touchmove', { x: 50, y: 400 }, { t: 50, target: content }));

    expect(hasDragging(el)).toBe(false); // scroller keeps the touch
    expect(el.style.transform).toBe('');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('touchcancel mid-drag clears the styles without closing', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: false });
    const el = aside()!;
    stubHeight(el, 400);

    el.dispatchEvent(touchEvent('touchstart', { x: 50, y: 100 }, { t: 0, target: el }));
    el.dispatchEvent(touchEvent('touchmove', { x: 50, y: 250 }, { t: 50, target: el }));
    expect(hasDragging(el)).toBe(true);

    el.dispatchEvent(touchEvent('touchcancel', null, { t: 60 }));

    expect(onClose).not.toHaveBeenCalled();
    expect(hasDragging(el)).toBe(false);
    expect(el.style.transform).toBe('');
    expect(scrim()?.style.opacity).toBe('');
  });

  it('touchstart on the sheet does not reach a window-level listener', () => {
    mountPanel({ target: groupTarget(), desktop: false });
    const el = aside()!;
    stubHeight(el, 400);

    const windowSpy = vi.fn();
    window.addEventListener('touchstart', windowSpy);
    try {
      el.dispatchEvent(touchEvent('touchstart', { x: 50, y: 100 }, { t: 0, target: el }));
      expect(windowSpy).not.toHaveBeenCalled(); // stopPropagation blocked it
    } finally {
      window.removeEventListener('touchstart', windowSpy);
    }
  });

  it('does nothing on desktop (sidebar, not a sheet)', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: true });
    const el = aside()!;
    stubHeight(el, 400);

    el.dispatchEvent(touchEvent('touchstart', { x: 50, y: 100 }, { t: 0, target: el }));
    el.dispatchEvent(touchEvent('touchmove', { x: 50, y: 400 }, { t: 50, target: el }));
    el.dispatchEvent(touchEvent('touchend', { x: 50, y: 400 }, { t: 500, target: el }));

    expect(hasDragging(el)).toBe(false);
    expect(onClose).not.toHaveBeenCalled();
  });
});

// --- Desktop file-detail-only contract (issue #154 §2) ---------------------
// The desktop sidebar shows ONE file's detail: never a list, never a back
// button. RunChat only ever targets it with a single file tool, but the panel
// itself renders tools[0] defensively regardless of entry.

describe('ToolPanel desktop file detail', () => {
  it('shows the detail of tools[0] with no list and no back button', () => {
    // Even handed a group-shaped target (entry list, many tools), desktop shows
    // the first tool's detail — never the list page.
    mountPanel({ target: groupTarget(), desktop: true });
    expect(rows()).toHaveLength(0);
    expect(backBtn()).toBeNull();
    expect(heading()?.textContent).toContain('read foo.ts'); // tools[0]
    expect(pres().map((p) => p.textContent)).toEqual(['path: foo.ts', 'file contents']);
  });

  it('replaces the content in place when retargeted to another file', () => {
    const { setTarget } = mountPanel({ target: detailTarget(fileTool()), desktop: true });
    expect(pathHeader()?.textContent).toBe('foo.ts');

    setTarget(
      detailTarget(
        tool(6, 'edit bar.ts', 'ok', {
          view: { kind: 'diff', path: 'bar.ts', text: '@@ -1 +1 @@\n+y' },
        }),
      ),
    );
    // The sidebar stays open; only the shown file changes.
    expect(aside()).not.toBeNull();
    expect(pathHeader()?.textContent).toBe('bar.ts');
  });
});

// --- Desktop drag-to-resize (issue #154 §4) --------------------------------

describe('ToolPanel resize', () => {
  it('renders the resize handle only on desktop', () => {
    const { setDesktop } = mountPanel({ target: detailTarget(fileTool()), desktop: false });
    expect(resizeHandle()).toBeNull(); // mobile sheet: no handle
    setDesktop(true);
    const handle = resizeHandle();
    expect(handle).not.toBeNull();
    expect(handle?.getAttribute('role')).toBe('separator');
    expect(handle?.getAttribute('aria-orientation')).toBe('vertical');
  });

  it('resizes the aside within [320px, 60vw] on a pointer drag', () => {
    // jsdom's window.innerWidth is 1024, so width = 1024 - clientX, clamped to
    // [320, round(1024*0.6)=614].
    mountPanel({ target: detailTarget(fileTool()), desktop: true });
    const el = aside()!;
    const handle = resizeHandle()!;

    handle.dispatchEvent(pointerEvt('pointerdown', 600));
    window.dispatchEvent(pointerEvt('pointermove', 500)); // 1024-500 = 524
    expect(el.style.width).toBe('524px');
    expect(el.classList.contains('tool-panel-resizing')).toBe(true);

    window.dispatchEvent(pointerEvt('pointermove', 900)); // 1024-900 = 124 → floor 320
    expect(el.style.width).toBe('320px');

    window.dispatchEvent(pointerEvt('pointermove', 40)); // 1024-40 = 984 → 60vw 614
    expect(el.style.width).toBe('614px');

    window.dispatchEvent(pointerEvt('pointerup', 40));
    expect(el.classList.contains('tool-panel-resizing')).toBe(false);
  });

  it('persists the width on pointerup and seeds the next mount', () => {
    mountPanel({ target: detailTarget(fileTool()), desktop: true });
    resizeHandle()!.dispatchEvent(pointerEvt('pointerdown', 600));
    window.dispatchEvent(pointerEvt('pointermove', 500)); // 524
    window.dispatchEvent(pointerEvt('pointerup', 500));
    expect(localStorage.getItem('lab.tool-panel-width')).toBe('524');

    // Remount: the stored width seeds the aside's inline width.
    dispose?.();
    dispose = undefined;
    container.remove();
    mountPanel({ target: detailTarget(fileTool()), desktop: true });
    expect(aside()?.style.width).toBe('524px');
  });

  it('re-clamps a stored width beyond 60vw at apply time', () => {
    localStorage.setItem('lab.tool-panel-width', '5000');
    mountPanel({ target: detailTarget(fileTool()), desktop: true });
    // 60vw of the 1024 jsdom viewport = round(1024*0.6) = 614, never 5000.
    expect(aside()?.style.width).toBe('614px');
  });

  it('applies no inline width for a first-time user (CSS default stands)', () => {
    mountPanel({ target: detailTarget(fileTool()), desktop: true });
    expect(aside()?.style.width).toBe('');
  });
});

describe('clampPanelWidth', () => {
  it('clamps a width to [320px, 60vw] of the viewport', () => {
    expect(clampPanelWidth(524, 1024)).toBe(524); // in band
    expect(clampPanelWidth(100, 1024)).toBe(320); // below the floor
    expect(clampPanelWidth(5000, 1024)).toBe(614); // above 60vw (rounded)
    expect(clampPanelWidth(400, 2000)).toBe(400); // wider viewport, same floor
  });
});
