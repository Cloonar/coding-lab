// Reactive visualViewport primitive (issue #82): iOS Safari ignores
// `interactive-widget=resizes-content`, so the on-screen keyboard shrinks only
// the VISUAL viewport and `100dvh` never follows it. The Chat page therefore
// sizes itself from window.visualViewport via CSS custom properties. Absence
// of visualViewport (jsdom, old browsers) reads as null, never a crash.
//
// Keyboard-open reading (issue #97, ADR-0083): the shell slides the tab bar out
// on New run while the on-screen keyboard is open. "Open" is decided from the
// same visual-viewport readings, never from focus: after Android's back-button
// dismissal the field keeps focus with no keyboard on screen, and a hardware
// keyboard types into a focused field with no on-screen keyboard at all — focus
// would hide the bar wrongly in both cases, the viewport shrink reads both
// right. The decision itself (createKeyboardTracker) is DOM-free so its
// baseline / threshold / rotation rules are pinned by plain unit tests.

import { createEffect, createMemo, createSignal, onCleanup } from 'solid-js';
import type { Accessor } from 'solid-js';

export interface ViewportBox {
  height: number;
  offsetTop: number;
  /** visualViewport.width — the keyboard baseline is kept per width (see
   *  createKeyboardTracker). Read from the same object, in the same read, as
   *  the height, so a rotation never pairs a new height with an old width. It
   *  also moves with pinch-zoom, but a zoomed reading is null anyway. */
  width: number;
}

// Pinch-zoom shrinks the visual viewport too; past this scale the reading is
// the zoom, not the keyboard, and the page must not shrink with it.
const ZOOM_THRESHOLD = 1.01;

/** How far (px) the visual viewport must sit below its baseline before the
 *  on-screen keyboard counts as open. Above iOS Safari's URL-bar collapse
 *  (roughly 60–100px, which must never read as a keyboard) and iPad's
 *  hardware-keyboard shortcut bar (~55px), well below the smallest phone
 *  keyboard (landscape, ~160–200px with the suggestion strip). */
export const KEYBOARD_MIN_SHRINK_PX = 120;

const sameBox = (a: ViewportBox | null, b: ViewportBox | null): boolean =>
  a === b ||
  (a !== null &&
    b !== null &&
    a.height === b.height &&
    a.offsetTop === b.offsetTop &&
    a.width === b.width);

export function createVisualViewport(): Accessor<ViewportBox | null> {
  const viewport = window.visualViewport;
  if (viewport === null || viewport === undefined) return () => null;
  const read = (): ViewportBox | null =>
    (viewport.scale ?? 1) > ZOOM_THRESHOLD
      ? null
      : { height: viewport.height, offsetTop: viewport.offsetTop, width: viewport.width };
  const [box, setBox] = createSignal(read(), { equals: sameBox });
  // Re-read the live properties instead of trusting event payloads. The window
  // resize listener is belt and braces against iOS leaving the visual viewport
  // "stuck short" after the keyboard closes.
  const onChange = () => setBox(read());
  viewport.addEventListener('resize', onChange);
  viewport.addEventListener('scroll', onChange);
  window.addEventListener('resize', onChange);
  onCleanup(() => {
    viewport.removeEventListener('resize', onChange);
    viewport.removeEventListener('scroll', onChange);
    window.removeEventListener('resize', onChange);
  });
  return box;
}

// Binds the tracked box onto `el` as --vv-height / --vv-top. A null box removes
// both so the CSS falls back to 100dvh / 0px. Call inside a reactive owner.
export function bindVisualViewport(el: HTMLElement): void {
  const box = createVisualViewport();
  const clear = () => {
    el.style.removeProperty('--vv-height');
    el.style.removeProperty('--vv-top');
  };
  createEffect(() => {
    const current = box();
    if (current === null) {
      clear();
      return;
    }
    el.style.setProperty('--vv-height', `${current.height}px`);
    el.style.setProperty('--vv-top', `${current.offsetTop}px`);
  });
  onCleanup(clear);
}

/**
 * The pure keyboard-open decision (issue #97): feed it every reading in order;
 * it answers whether the on-screen keyboard is open for that reading. The
 * baseline is the LARGEST height seen at the current width — the viewport with
 * no keyboard and the URL bar collapsed — and the keyboard is open while the
 * height sits more than KEYBOARD_MIN_SHRINK_PX below it. A width change
 * (rotation) restarts the baseline at the new reading, so landscape's short
 * height is never compared with portrait's tall one. (Rotating WITH the
 * keyboard up starts the baseline short and reads closed until the keyboard
 * goes away once — the bar simply stays, the safe side.) A null reading (no
 * visualViewport, or pinch-zoomed) is closed and leaves the baseline alone.
 */
export function createKeyboardTracker(): (box: ViewportBox | null) => boolean {
  let width: number | null = null;
  let baseline = 0;
  return (box) => {
    if (box === null) return false;
    if (box.width !== width) {
      width = box.width;
      baseline = box.height;
    } else if (box.height > baseline) {
      baseline = box.height;
    }
    return baseline - box.height > KEYBOARD_MIN_SHRINK_PX;
  };
}

/** Reactive keyboard-open signal on top of createVisualViewport. Without
 *  visualViewport the box is always null, so this is a constant false. Call
 *  inside a reactive owner. */
export function createKeyboardOpen(): Accessor<boolean> {
  const box = createVisualViewport();
  const track = createKeyboardTracker();
  const open = createMemo(() => track(box()));
  return open;
}
