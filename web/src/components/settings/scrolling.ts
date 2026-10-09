// The one-page settings' only contact with layout and the scroll position
// (issue #61, issue #85). A settings page scrolls to a section or a field,
// and marks the section in view while the operator scrolls; both need element
// positions, which jsdom does not have. Everything that measures or moves the
// page goes through `viewport` (sectionScroll.ts is its one reader), so a
// test swaps these four functions for fakes (see
// routes/repo-settings/harness.tsx) and the page's own logic — which section
// is the target, which chip is current, which field takes focus — runs
// unchanged.
//
// The page scrolls on the document (the app shell has no inner scroller), so
// positions are relative to the browser viewport.

export interface Viewport {
  /** An element's top edge, in px from the top of the viewport. */
  topOf(element: Element): number;
  /** An element's rendered height in px. */
  heightOf(element: Element): number;
  /** Scrolls the page so `element`'s top edge sits `offset` px below the viewport top. */
  scrollTo(element: Element, offset: number, smooth: boolean): void;
  /** True once the page is scrolled to its very end (and is scrolled at all). */
  atEnd(): boolean;
}

function reducedMotion(): boolean {
  return window.matchMedia?.('(prefers-reduced-motion: reduce)').matches === true;
}

export const viewport: Viewport = {
  topOf: (element) => element.getBoundingClientRect().top,
  heightOf: (element) => element.getBoundingClientRect().height,
  scrollTo: (element, offset, smooth) => {
    const top = element.getBoundingClientRect().top + window.scrollY - offset;
    window.scrollTo({
      top: Math.max(0, top),
      behavior: smooth && !reducedMotion() ? 'smooth' : 'auto',
    });
  },
  atEnd: () => {
    const page = document.documentElement;
    return window.scrollY > 0 && window.scrollY + window.innerHeight >= page.scrollHeight - 2;
  },
};
