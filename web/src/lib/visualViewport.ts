// Reactive visualViewport primitive (issue #82): iOS Safari ignores
// `interactive-widget=resizes-content`, so the on-screen keyboard shrinks only
// the VISUAL viewport and `100dvh` never follows it. The Chat page therefore
// sizes itself from window.visualViewport via CSS custom properties. Absence
// of visualViewport (jsdom, old browsers) reads as null, never a crash.

import { createEffect, createSignal, onCleanup } from 'solid-js';
import type { Accessor } from 'solid-js';

export interface ViewportBox {
  height: number;
  offsetTop: number;
}

// Pinch-zoom shrinks the visual viewport too; past this scale the reading is
// the zoom, not the keyboard, and the page must not shrink with it.
const ZOOM_THRESHOLD = 1.01;

const sameBox = (a: ViewportBox | null, b: ViewportBox | null): boolean =>
  a === b || (a !== null && b !== null && a.height === b.height && a.offsetTop === b.offsetTop);

export function createVisualViewport(): Accessor<ViewportBox | null> {
  const viewport = window.visualViewport;
  if (viewport === null || viewport === undefined) return () => null;
  const read = (): ViewportBox | null =>
    (viewport.scale ?? 1) > ZOOM_THRESHOLD
      ? null
      : { height: viewport.height, offsetTop: viewport.offsetTop };
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
