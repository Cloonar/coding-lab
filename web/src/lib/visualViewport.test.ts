// lib/visualViewport.ts: the Chat page sizes from the visual viewport (iOS
// ignores interactive-widget), exposed as --vv-height / --vv-top on an element.

import { createRoot } from 'solid-js';
import { afterEach, describe, expect, it } from 'vitest';
import { bindVisualViewport, createVisualViewport } from './visualViewport';

class FakeVisualViewport extends EventTarget {
  height = 800;
  offsetTop = 0;
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
      expect(createVisualViewport()()).toEqual({ height: 800, offsetTop: 0 });
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
