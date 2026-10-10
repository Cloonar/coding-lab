// lib/visualViewport.ts: the Chat page sizes from the visual viewport (iOS
// ignores interactive-widget), exposed as --vv-height / --vv-top on an element;
// and the keyboard-open reading the shell slides the tab bar out by (issue #97):
// a per-width max-height baseline, a shrink threshold, rotation resets.

import { createRoot } from 'solid-js';
import { afterEach, describe, expect, it } from 'vitest';
import {
  KEYBOARD_MIN_SHRINK_PX,
  bindVisualViewport,
  createKeyboardOpen,
  createKeyboardTracker,
  createVisualViewport,
} from './visualViewport';

class FakeVisualViewport extends EventTarget {
  height = 800;
  offsetTop = 0;
  width = 390;
  scale: number | undefined = 1;
}

const original = Object.getOwnPropertyDescriptor(window, 'visualViewport');

function install(value: unknown): void {
  Object.defineProperty(window, 'visualViewport', { configurable: true, value });
}

function setup(): { vv: FakeVisualViewport; el: HTMLElement; dispose: () => void } {
  const vv = new FakeVisualViewport();
  install(vv);
  const el = document.createElement('div');
  const dispose = createRoot((d) => {
    bindVisualViewport(el);
    return d;
  });
  return { vv, el, dispose };
}

afterEach(() => {
  if (original === undefined) {
    Reflect.deleteProperty(window, 'visualViewport');
  } else {
    Object.defineProperty(window, 'visualViewport', original);
  }
});

describe('visualViewport', () => {
  it('reads null and sets no property when visualViewport is absent', () => {
    install(undefined);
    const el = document.createElement('div');
    createRoot((dispose) => {
      expect(createVisualViewport()()).toBeNull();
      bindVisualViewport(el);
      dispose();
    });
    expect(el.style.getPropertyValue('--vv-height')).toBe('');
    expect(el.style.getPropertyValue('--vv-top')).toBe('');
  });

  it('writes the initial reading and follows visualViewport resize and scroll', () => {
    const { vv, el, dispose } = setup();
    expect(el.style.getPropertyValue('--vv-height')).toBe('800px');
    expect(el.style.getPropertyValue('--vv-top')).toBe('0px');

    vv.height = 500;
    vv.dispatchEvent(new Event('resize'));
    expect(el.style.getPropertyValue('--vv-height')).toBe('500px');

    vv.offsetTop = 40;
    vv.dispatchEvent(new Event('scroll'));
    expect(el.style.getPropertyValue('--vv-top')).toBe('40px');
    dispose();
  });

  it('also re-reads on window resize', () => {
    const { vv, el, dispose } = setup();
    vv.height = 620;
    window.dispatchEvent(new Event('resize'));
    expect(el.style.getPropertyValue('--vv-height')).toBe('620px');
    dispose();
  });

  it('removes the properties while pinch-zoomed and restores them after', () => {
    const { vv, el, dispose } = setup();
    vv.scale = 2;
    vv.height = 400;
    vv.dispatchEvent(new Event('resize'));
    expect(el.style.getPropertyValue('--vv-height')).toBe('');
    expect(el.style.getPropertyValue('--vv-top')).toBe('');

    vv.scale = 1;
    vv.height = 700;
    vv.dispatchEvent(new Event('resize'));
    expect(el.style.getPropertyValue('--vv-height')).toBe('700px');
    expect(el.style.getPropertyValue('--vv-top')).toBe('0px');
    dispose();
  });

  it('treats a missing scale as 1', () => {
    const vv = new FakeVisualViewport();
    vv.scale = undefined;
    install(vv);
    createRoot((dispose) => {
      expect(createVisualViewport()()).toEqual({ height: 800, offsetTop: 0, width: 390 });
      dispose();
    });
  });

  it('removes the properties and listeners on dispose', () => {
    const { vv, el, dispose } = setup();
    dispose();
    expect(el.style.getPropertyValue('--vv-height')).toBe('');
    expect(el.style.getPropertyValue('--vv-top')).toBe('');

    vv.height = 300;
    vv.dispatchEvent(new Event('resize'));
    vv.dispatchEvent(new Event('scroll'));
    window.dispatchEvent(new Event('resize'));
    expect(el.style.getPropertyValue('--vv-height')).toBe('');
    expect(el.style.getPropertyValue('--vv-top')).toBe('');
  });
});

describe('createKeyboardTracker (issue #97)', () => {
  const box = (height: number, width = 390) => ({ height, width, offsetTop: 0 });

  it('uses a threshold above a URL-bar collapse', () => {
    expect(KEYBOARD_MIN_SHRINK_PX).toBeGreaterThanOrEqual(120);
  });

  it('keeps the largest height seen as the baseline', () => {
    const track = createKeyboardTracker();
    expect(track(box(760))).toBe(false);
    expect(track(box(844))).toBe(false); // URL bar collapsed: new, larger baseline
    // 844 - 600 = 244 below the max, though only 160 below the first reading.
    expect(track(box(600))).toBe(true);
  });

  it('reads open once the shrink passes the threshold, closed at or under it', () => {
    const track = createKeyboardTracker();
    track(box(844));
    expect(track(box(844 - KEYBOARD_MIN_SHRINK_PX))).toBe(false);
    expect(track(box(844 - KEYBOARD_MIN_SHRINK_PX - 1))).toBe(true);
    expect(track(box(500))).toBe(true);
  });

  it('never reads an 80px URL-bar change as a keyboard', () => {
    const track = createKeyboardTracker();
    expect(track(box(844))).toBe(false);
    expect(track(box(764))).toBe(false);
    expect(track(box(844))).toBe(false);
  });

  it('reads closed again when the viewport grows back', () => {
    const track = createKeyboardTracker();
    track(box(844));
    expect(track(box(500))).toBe(true);
    expect(track(box(844))).toBe(false);
  });

  it('resets the baseline on a width change (rotation)', () => {
    const track = createKeyboardTracker();
    expect(track(box(844, 390))).toBe(false);
    // Landscape: 454px shorter than the portrait baseline, but a new width.
    expect(track(box(390, 844))).toBe(false);
    expect(track(box(200, 844))).toBe(true);
    // Back to portrait: the landscape baseline does not carry over either.
    expect(track(box(844, 390))).toBe(false);
  });

  it('reads a null box (absent or pinch-zoomed) as closed and keeps the baseline', () => {
    const track = createKeyboardTracker();
    expect(track(null)).toBe(false);
    track(box(844));
    expect(track(box(500))).toBe(true);
    expect(track(null)).toBe(false);
    expect(track(box(500))).toBe(true);
  });
});

describe('createKeyboardOpen (issue #97)', () => {
  it('is a constant false without visualViewport', () => {
    install(undefined);
    createRoot((dispose) => {
      const open = createKeyboardOpen();
      expect(open()).toBe(false);
      window.dispatchEvent(new Event('resize'));
      expect(open()).toBe(false);
      dispose();
    });
  });

  it('follows the visual viewport shrinking past the threshold and back', () => {
    const vv = new FakeVisualViewport();
    vv.height = 844;
    install(vv);
    createRoot((dispose) => {
      const open = createKeyboardOpen();
      expect(open()).toBe(false);

      vv.height = 764; // URL bar
      vv.dispatchEvent(new Event('resize'));
      expect(open()).toBe(false);

      vv.height = 500; // keyboard
      vv.dispatchEvent(new Event('resize'));
      expect(open()).toBe(true);

      vv.height = 844;
      vv.dispatchEvent(new Event('resize'));
      expect(open()).toBe(false);
      dispose();
    });
  });

  it('does not read a rotation to landscape as a keyboard', () => {
    const vv = new FakeVisualViewport();
    vv.height = 844;
    install(vv);
    createRoot((dispose) => {
      const open = createKeyboardOpen();
      vv.height = 390;
      vv.width = 844;
      window.dispatchEvent(new Event('resize'));
      expect(open()).toBe(false);
      dispose();
    });
  });

  it('reads closed while pinch-zoomed', () => {
    const vv = new FakeVisualViewport();
    vv.height = 844;
    install(vv);
    createRoot((dispose) => {
      const open = createKeyboardOpen();
      vv.scale = 2;
      vv.height = 422;
      vv.width = 195;
      vv.dispatchEvent(new Event('resize'));
      expect(open()).toBe(false);
      dispose();
    });
  });
});
