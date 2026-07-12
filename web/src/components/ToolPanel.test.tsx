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
import type { ChatMessage } from '../api';
import ToolPanel, { toolStatusMark, type PanelTarget, type ToolPanelProps } from './ToolPanel';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function tool(
  seq: number,
  title: string,
  status: 'running' | 'ok' | 'error',
  io?: {
    input?: string;
    output?: string;
  },
): ChatMessage {
  return {
    seq,
    kind: 'tool',
    tool: { name: title, title, status, input: io?.input, output: io?.output },
  };
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

describe('ToolPanel close affordance', () => {
  it('the ✕ button fires onClose on the list page', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: false });
    closeBtn()?.click();
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('the ✕ button fires onClose on the detail page (desktop)', () => {
    const { onClose } = mountPanel({ target: groupTarget(), desktop: true });
    rows()[0]?.click();
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
  it('keeps the pushed detail page while swapping the chrome', () => {
    const { setDesktop } = mountPanel({ target: groupTarget(), desktop: false });
    rows()[0]?.click();
    expect(aside()?.getAttribute('role')).toBe('dialog');
    expect(scrim()).not.toBeNull();
    expect(heading()?.textContent).toContain('read foo.ts');

    setDesktop(true);

    // Same detail page, other chrome: dialog role and scrim gone.
    expect(aside()?.hasAttribute('role')).toBe(false);
    expect(scrim()).toBeNull();
    expect(backBtn()).not.toBeNull();
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
