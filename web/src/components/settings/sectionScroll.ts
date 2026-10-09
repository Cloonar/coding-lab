// The one-page settings' scroll logic (issue #61, issue #85): everything a
// settings page does with its scroll position, shared by every such page.
// The page renders its sections (SettingsSection.tsx) under SectionNav and
// hands this hook its page element and its chips row; the hook then
//
//   - marks the section in view (`current`) while the operator scrolls —
//     the section the page was last SENT to stays marked until the operator
//     scrolls on their own;
//   - acts on the URL: /<base>/:section opens the page scrolled to that
//     section (an unknown slug stays at the top), `?field=<key>` scrolls that
//     field into view, focuses its control and outlines it for a moment. The
//     first arrival is a deep link — no animation, and HELD in place while
//     late content lands above it — every later URL change and every
//     form.reveal() (a problem found by Save, a save bar link) scrolls
//     smoothly;
//   - `go(slug)`, for a chip or outline entry: scrolls there and REPLACES the
//     URL with the section's — no history entry per section.
//
// Every position is read and every scroll made through `viewport`
// (scrolling.ts), so a test fakes the layout and this logic runs unchanged.

import { useIsRouting, useLocation, useNavigate } from '@solidjs/router';
import {
  createComputed,
  createEffect,
  createSignal,
  on,
  onCleanup,
  onMount,
  untrack,
  type Accessor,
} from 'solid-js';
import { sectionInView } from '../../lib/sectionSpy';
import { findCategory, sectionElementId, type SettingsCategory } from './categories';
import { focusFieldControl } from './Field';
import { useSettingsFormContext, type RevealTarget } from './form';
import { viewport } from './scrolling';

/** The app shell's layout breakpoint (AppShell DESKTOP_MIN_PX). */
export const DESKTOP_QUERY = '(min-width: 1024px)';

/** Where scroll targets land, in px. */
export interface ScrollOffsets {
  /** Air between whatever is stuck to the top of the viewport and a scroll target. */
  targetGap: number;
  /** Extra room above a field, so its label is not flush under the chips. */
  fieldHeadroom: number;
  /** The outline does not cover the page, so desktop targets only clear the page padding. */
  desktopOffset: number;
}

export const SCROLL_OFFSETS: ScrollOffsets = {
  targetGap: 12,
  fieldHeadroom: 36,
  desktopOffset: 24,
};

/** How long a deep link keeps its target in place while late content lands above it. */
const HOLD_MS = 3000;
/** How long the outline around a revealed field stays. */
const FLASH_MS = 1800;

/** What the operator does to scroll or to take over the page. */
const SCROLL_INPUTS = ['wheel', 'touchstart', 'pointerdown', 'keydown'] as const;

export interface SectionScrollOptions<K extends string> {
  /** The page's sections, in page order. */
  categories: readonly SettingsCategory[];
  /** The settings index path; a section's URL is `<base>/<slug>`. */
  base: Accessor<string>;
  /** The route's :section segment. */
  section: Accessor<string | undefined>;
  /** Narrows a `?field=` value to a field of the page's table. */
  isField: (value: string) => value is K;
  /** True from 1024px: the outline replaces the chips (and covers nothing). */
  desktop: Accessor<boolean>;
  /**
   * Content whose arrival adds, unfolds or folds fields (a catalog, inherited
   * values): a held deep link goes to its target again once it is on the page.
   */
  lateContent?: Accessor<unknown>;
  /** Overrides of SCROLL_OFFSETS, for a page whose sticky frame differs. */
  offsets?: Partial<ScrollOffsets>;
}

export interface SectionScroll {
  /** Ref for the page element (`.settings-page`): fields are looked up inside it. */
  page: (element: HTMLElement) => void;
  /** Ref for SectionNav's chips row, whose height the scroll offsets include. */
  chips: (element: HTMLElement | undefined) => void;
  /** The slug of the section in view. */
  current: Accessor<string>;
  /** A chip or outline entry: scroll to the section, replace the URL with its own. */
  go: (slug: string) => void;
}

/**
 * The scroll logic of one settings page. Call it in the page component, under
 * the page's SettingsFormProvider; the sections must render once
 * `form.saved()` is defined.
 */
export function createSectionScroll<K extends string>(
  options: SectionScrollOptions<K>,
): SectionScroll {
  const location = useLocation();
  const navigate = useNavigate();
  const isRouting = useIsRouting();
  const form = useSettingsFormContext();
  const offsets: ScrollOffsets = { ...SCROLL_OFFSETS, ...options.offsets };
  const categories = options.categories;
  const firstSection = categories[0]?.slug ?? '';
  const section = options.section;

  const fieldParam = (): K | undefined => {
    const field = location.query.field;
    return typeof field === 'string' && options.isField(field) ? field : undefined;
  };
  // Where the URL points: a field (its own section wins over a mismatched
  // :section), else a known section, else nowhere — the top of the page.
  const urlTarget = (): RevealTarget | undefined => {
    const field = fieldParam();
    if (field !== undefined) return { section: form.field(field).spec.section, field };
    const slug = findCategory(categories, section())?.slug;
    return slug !== undefined ? { section: slug } : undefined;
  };
  // What a URL means to the page's scroll position: its section and field.
  // Anything else in the URL (the repo page's schedule editor segment) is
  // left out on purpose — opening or closing it is not an arrival, and must
  // not move the page under it.
  const arrivalKey = (slug: string | undefined, field: string | undefined): string =>
    `${findCategory(categories, slug)?.slug ?? ''}|${field ?? ''}`;

  // A section that folds fields away unfolds the one the URL points at.
  // A computed, so it has happened before the page looks for the field.
  createComputed(() => form.pointAt(urlTarget()?.field));

  let page: HTMLElement | undefined;
  let chips: HTMLElement | undefined;
  const sectionElement = (slug: string): HTMLElement | null =>
    document.getElementById(sectionElementId(slug));

  // --- what sticks to the top ---------------------------------------------------
  // Below 1024px the chips stick to the viewport top; a scroll target has to
  // clear them.
  const stuckOffset = (): number =>
    options.desktop()
      ? offsets.desktopOffset
      : (chips !== undefined ? viewport.heightOf(chips) : 0) + offsets.targetGap;

  // --- the section in view ------------------------------------------------------
  const [current, setCurrent] = createSignal(firstSection);
  // The section the page was last SENT to (a chip, a deep link, a problem
  // found by Save). It stays marked until the operator scrolls on their own:
  // a section near the end of the page cannot always climb to the line, and
  // the entry just tapped must not hand its mark to a neighbour.
  let sentTo: string | undefined;
  const updateCurrent = (): void => {
    if (sentTo !== undefined) {
      setCurrent(sentTo);
      return;
    }
    const tops = categories.flatMap((category) => {
      const element = sectionElement(category.slug);
      return element !== null ? [{ id: category.slug, top: viewport.topOf(element) }] : [];
    });
    setCurrent(sectionInView(tops, stuckOffset() + 8, viewport.atEnd()) ?? firstSection);
  };

  // --- going to a section or a field --------------------------------------------
  // While a deep link is being held, this re-applies it.
  let held: ((smooth: boolean) => void) | undefined;
  let stopHold: (() => void) | undefined;
  let flashTimer: ReturnType<typeof setTimeout> | undefined;
  const flash = (wrapper: HTMLElement): void => {
    clearTimeout(flashTimer);
    page?.querySelectorAll('.sfield.flash').forEach((el) => el.classList.remove('flash'));
    wrapper.classList.add('flash');
    flashTimer = setTimeout(() => wrapper.classList.remove('flash'), FLASH_MS);
  };

  /**
   * Scrolls to the target and, for a field, focuses its control. `hold` keeps
   * the target in place for a moment while the page is still filling in: rows
   * that load after the page push everything below them down, and a field
   * that waits for late content is not even there yet. A deep link must end
   * up AT its target, not where the target was before the rest landed. Any
   * input from the operator ends the hold at once.
   */
  const goTo = (target: RevealTarget, how: { smooth: boolean; hold: boolean }): void => {
    stopHold?.();
    sentTo = target.section;
    let focused = false;
    const apply = (smooth: boolean): void => {
      const sectionNode = sectionElement(target.section);
      if (sectionNode === null) return;
      const fieldNode =
        target.field !== undefined
          ? (page?.querySelector<HTMLElement>(`[data-field="${target.field}"]`) ?? null)
          : null;
      if (fieldNode !== null && target.field !== undefined) {
        viewport.scrollTo(fieldNode, stuckOffset() + offsets.fieldHeadroom, smooth);
        if (!focused) {
          focused = focusFieldControl(fieldNode, target.field);
          flash(fieldNode);
        }
      } else {
        // A touch above the offset, so the section reads as "in view".
        viewport.scrollTo(sectionNode, stuckOffset() - 4, smooth);
      }
      updateCurrent();
    };
    apply(how.smooth);
    if (!how.hold) return;

    held = apply;
    const observer =
      page !== undefined && typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(() => held?.(false))
        : undefined;
    if (page !== undefined) observer?.observe(page);
    const release = (): void => stopHold?.();
    for (const type of SCROLL_INPUTS) window.addEventListener(type, release, { passive: true });
    const timer = setTimeout(release, HOLD_MS);
    stopHold = () => {
      held = undefined;
      stopHold = undefined;
      observer?.disconnect();
      clearTimeout(timer);
      for (const type of SCROLL_INPUTS) window.removeEventListener(type, release);
    };
  };
  // Late content adds fields and unfolds or folds others: a held deep link
  // goes to its target again once it is on the page.
  const lateContent = options.lateContent;
  if (lateContent !== undefined) {
    createEffect(on(lateContent, () => held?.(false), { defer: true }));
  }

  // A chip or outline entry: scroll there, and put the section's URL in the
  // address bar without a history entry. `byNav` tells the arrival effect
  // below that this URL change has already been acted on.
  let byNav: string | undefined;
  const go = (slug: string): void => {
    const key = arrivalKey(slug, undefined);
    if (arrivalKey(section(), fieldParam()) !== key) byNav = key;
    goTo({ section: slug }, { smooth: true, hold: false });
    navigate(`${options.base()}/${slug}`, { replace: true, scroll: false });
  };

  // Arrival: act on the URL once the sections exist and the router is done
  // with the navigation (it scrolls to the top as its last step — after that,
  // the page's own scroll stands). Runs again for every later URL change and
  // every form.reveal() — a Save that found a problem, a save bar link.
  let seenKey: string | undefined;
  let seenTick = 0;
  createEffect(() => {
    const ready = form.saved() !== undefined;
    const routing = isRouting();
    const key = arrivalKey(section(), fieldParam());
    const tick = form.revealTick();
    if (!ready || routing) return;
    untrack(() => {
      const first = seenKey === undefined;
      if (!first && key === seenKey && tick === seenTick) return;
      const handled = key === byNav;
      seenKey = key;
      seenTick = tick;
      byNav = undefined;
      if (handled) return;
      const target = urlTarget();
      if (target === undefined) return;
      // The first arrival is a deep link: no animation, and held in place.
      goTo(target, first ? { smooth: false, hold: true } : { smooth: true, hold: false });
    });
  });

  onMount(() => {
    updateCurrent();
    const onResize = (): void => {
      updateCurrent();
    };
    // The operator takes over the scroll position: from here on the section
    // in view is whatever the page shows. (A scroll event alone cannot tell —
    // the page's own animated scroll fires them too.)
    const onOwnScroll = (): void => {
      sentTo = undefined;
    };
    for (const type of SCROLL_INPUTS) {
      window.addEventListener(type, onOwnScroll, { passive: true });
    }
    // A scroll fires many events per frame, and one pass reads the position
    // of every section: one pass per frame is all the page can show anyway.
    let frame: number | undefined;
    const onScroll = (): void => {
      if (frame !== undefined) return;
      frame = requestAnimationFrame(() => {
        frame = undefined;
        updateCurrent();
      });
    };
    window.addEventListener('scroll', onScroll, { passive: true });
    window.addEventListener('resize', onResize);
    // Late content moves every section below it.
    const observer =
      page !== undefined && typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(updateCurrent)
        : undefined;
    if (page !== undefined) observer?.observe(page);
    onCleanup(() => {
      for (const type of SCROLL_INPUTS) window.removeEventListener(type, onOwnScroll);
      window.removeEventListener('scroll', onScroll);
      if (frame !== undefined) cancelAnimationFrame(frame);
      window.removeEventListener('resize', onResize);
      observer?.disconnect();
    });
  });
  onCleanup(() => {
    stopHold?.();
    clearTimeout(flashTimer);
  });

  return {
    page: (element) => {
      page = element;
    },
    chips: (element) => {
      chips = element;
    },
    current,
    go,
  };
}
