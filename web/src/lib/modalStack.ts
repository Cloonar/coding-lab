// One stack for everything modal (issue #61): the in-page Dialog, the
// schedule editor, the editor's stand-in while a Schedule loads. Each of them
// traps focus with document-level listeners — and two traps that both insist
// on their own panel bounce focus between them forever. So only the modal on
// TOP of the stack acts: it alone pulls escaped focus back, cycles Tab, and
// hears Escape. One below it yields until it is on top again.
//
// createModal() is the whole contract of "being modal", for a component body:
//   - opening moves focus in (to `initialFocus()`, else `fallback()`);
//   - Tab and Shift+Tab cycle inside the panel; focus that lands outside is
//     pulled back in;
//   - Escape reaches `onEscape` unless a control inside already handled it
//     (an open pick, an open inline confirmation: they prevent the default);
//   - the page behind does not scroll (one lock, shared by every modal);
//   - closing returns focus to the element that had it before opening.

import { onCleanup, onMount } from 'solid-js';

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

// Modals share one scroll lock; the first to open saves the page's own
// overflow value and the last to close restores it.
let scrollLocks = 0;
let savedOverflow = '';

function lockScroll(): () => void {
  if (scrollLocks === 0) {
    savedOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
  }
  scrollLocks += 1;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    scrollLocks -= 1;
    if (scrollLocks === 0) document.body.style.overflow = savedOverflow;
  };
}

/** The open modals, the topmost last. */
const stack: symbol[] = [];

export interface ModalOptions {
  /** The modal's own element; focus is kept inside it. */
  panel: () => HTMLElement | undefined;
  /** Takes focus when nothing else in the panel can (its heading, tabindex="-1"). */
  fallback: () => HTMLElement | undefined;
  /** The element to focus on open; default: `fallback()`. */
  initialFocus?: () => HTMLElement | undefined | null;
  /** Escape, while this modal is on top and nothing inside handled the key. */
  onEscape?: (event: KeyboardEvent) => void;
}

export interface ModalHandle {
  /** True while no other modal is open above this one. */
  isTop: () => boolean;
}

/** Makes the calling component a modal for as long as it is mounted. */
export function createModal(options: ModalOptions): ModalHandle {
  const id = Symbol('modal');
  // Captured before focus moves in, so closing can hand it back.
  const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  stack.push(id);
  const isTop = (): boolean => stack[stack.length - 1] === id;

  const focusables = (): HTMLElement[] => {
    const panel = options.panel();
    return panel === undefined ? [] : Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE));
  };

  const onKeyDown = (event: KeyboardEvent): void => {
    if (!isTop()) return;
    if (event.key === 'Escape') {
      if (!event.defaultPrevented) options.onEscape?.(event);
      return;
    }
    const panel = options.panel();
    if (event.key !== 'Tab' || panel === undefined) return;
    const items = focusables();
    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement;
    if (first === undefined || last === undefined) {
      event.preventDefault();
      options.fallback()?.focus();
      return;
    }
    const outside = !panel.contains(active);
    if (event.shiftKey && (active === first || active === options.fallback() || outside)) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && (active === last || outside)) {
      event.preventDefault();
      first.focus();
    }
  };

  // Focus that lands outside the panel (a scrim blocks clicks on the page
  // behind, but programmatic focus or assistive tech can still move it) is
  // pulled back in — by the top modal only.
  const onFocusIn = (event: FocusEvent): void => {
    if (!isTop()) return;
    const panel = options.panel();
    if (panel === undefined || !(event.target instanceof Node)) return;
    if (!panel.contains(event.target)) (focusables()[0] ?? options.fallback())?.focus();
  };

  document.addEventListener('keydown', onKeyDown);
  document.addEventListener('focusin', onFocusIn);
  const unlock = lockScroll();
  onCleanup(() => {
    document.removeEventListener('keydown', onKeyDown);
    document.removeEventListener('focusin', onFocusIn);
    const at = stack.indexOf(id);
    if (at !== -1) stack.splice(at, 1);
    unlock();
    if (opener?.isConnected === true) opener.focus();
  });

  // Focus moves in once the panel is in the document.
  onMount(() => (options.initialFocus?.() ?? options.fallback())?.focus());

  return { isTop };
}
