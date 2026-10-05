// The settings page's layout seam (issue #61): the real `viewport` functions,
// against a stubbed window — where a scroll lands, when it is animated, and
// when the page counts as scrolled to its end.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { viewport } from './scrolling';

function elementAt(top: number, height = 40): Element {
  const el = document.createElement('div');
  el.getBoundingClientRect = () =>
    ({ top, bottom: top + height, height, left: 0, right: 0, width: 0, x: 0, y: top }) as DOMRect;
  return el;
}

function stubWindow(scrollY: number, innerHeight: number, scrollHeight: number) {
  const scrollTo = vi.fn();
  vi.stubGlobal('scrollTo', scrollTo);
  vi.stubGlobal('scrollY', scrollY);
  vi.stubGlobal('innerHeight', innerHeight);
  vi.spyOn(document.documentElement, 'scrollHeight', 'get').mockReturnValue(scrollHeight);
  return scrollTo;
}

function stubReducedMotion(reduce: boolean): void {
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({ matches: reduce && query.includes('prefers-reduced-motion') })),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('viewport', () => {
  it('measures an element against the viewport', () => {
    const el = elementAt(120, 48);
    expect(viewport.topOf(el)).toBe(120);
    expect(viewport.heightOf(el)).toBe(48);
  });

  it('scrolls so the element sits `offset` px below the top of the viewport', () => {
    const scrollTo = stubWindow(1000, 800, 6000);
    stubReducedMotion(false);

    viewport.scrollTo(elementAt(500), 110, true);
    // 500px below the viewport top at scrollY 1000 is at 1500 on the page.
    expect(scrollTo).toHaveBeenCalledWith({ top: 1390, behavior: 'smooth' });

    viewport.scrollTo(elementAt(-300), 110, false);
    expect(scrollTo).toHaveBeenLastCalledWith({ top: 590, behavior: 'auto' });
  });

  it('never asks for a position above the page', () => {
    const scrollTo = stubWindow(0, 800, 6000);
    stubReducedMotion(false);

    viewport.scrollTo(elementAt(60), 110, false);
    expect(scrollTo).toHaveBeenCalledWith({ top: 0, behavior: 'auto' });
  });

  it('does not animate for an operator who asked for reduced motion', () => {
    const scrollTo = stubWindow(0, 800, 6000);
    stubReducedMotion(true);

    viewport.scrollTo(elementAt(900), 24, true);
    expect(scrollTo).toHaveBeenCalledWith({ top: 876, behavior: 'auto' });
  });

  it('is at the end once the last pixel is in view — but never at the very top', () => {
    stubWindow(5200, 800, 6000);
    expect(viewport.atEnd()).toBe(true);
    stubWindow(5199, 800, 6000); // within the 2px tolerance
    expect(viewport.atEnd()).toBe(true);
    stubWindow(5000, 800, 6000);
    expect(viewport.atEnd()).toBe(false);
    // A page shorter than the viewport is not "scrolled to its end".
    stubWindow(0, 800, 600);
    expect(viewport.atEnd()).toBe(false);
  });
});
