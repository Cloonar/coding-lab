// The in-page modal (issue #61): a bottom sheet below 1024px and a centered
// dialog from 1024px — CSS alone switches the two (controls.css), so there is
// no JS breakpoint. One primitive for the repo delete confirmation and the
// leave-with-unsaved-changes prompt; no browser confirm.
//
// Accessibility: role="dialog" (or "alertdialog" via `role`), aria-modal, and
// aria-labelledby pointing at the title (the body is aria-describedby for an
// alertdialog). Opening moves focus into the dialog — to `initialFocus()` when
// given, else to the title — and Tab/Shift+Tab are trapped inside while it is
// open; closing returns focus to the element that had it before opening. A
// scrim click and Escape call onClose unless `dismissable` is false. While any
// dialog is open the page behind it does not scroll.
//
// Scrim and panel are siblings (the InstallSheet precedent): the close handler
// lives on the scrim alone, so a click inside the panel never reaches it.

import { Show, createUniqueId, onCleanup, onMount, type JSX } from 'solid-js';

export interface DialogProps {
  open: boolean;
  /** Called on scrim click and Escape (unless dismissable is false). */
  onClose: () => void;
  /** The heading; also the dialog's accessible name. */
  title: JSX.Element;
  /** Body content. */
  children?: JSX.Element;
  /** The action row (buttons), last in the panel. */
  actions?: JSX.Element;
  /** "alertdialog" for a confirmation that interrupts; default "dialog". */
  role?: 'dialog' | 'alertdialog';
  /** false = neither the scrim nor Escape closes it. Default true. */
  dismissable?: boolean;
  /** The element to focus on open; default: the title. */
  initialFocus?: () => HTMLElement | undefined | null;
  /** Extra classes on the panel. */
  class?: string;
}

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

// Nested dialogs share one scroll lock; the first to open saves the page's own
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

export default function Dialog(props: DialogProps) {
  const uid = createUniqueId();
  const titleId = `dialog-${uid}-title`;
  const bodyId = `dialog-${uid}-body`;
  const dismissable = () => props.dismissable !== false;

  return (
    <Show when={props.open}>
      <DialogPanel {...props} titleId={titleId} bodyId={bodyId} dismissable={dismissable()} />
    </Show>
  );
}

function DialogPanel(
  props: DialogProps & { titleId: string; bodyId: string; dismissable: boolean },
) {
  let panel: HTMLDivElement | undefined;
  let heading: HTMLHeadingElement | undefined;
  // Captured when the panel mounts — before focus moves in — so closing can
  // hand focus back to the control that opened it.
  const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;

  const focusables = (): HTMLElement[] =>
    panel === undefined ? [] : Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE));

  const onKeyDown = (event: KeyboardEvent): void => {
    if (event.key === 'Escape') {
      if (!props.dismissable) return;
      event.preventDefault();
      event.stopPropagation();
      props.onClose();
      return;
    }
    if (event.key !== 'Tab' || panel === undefined) return;
    const items = focusables();
    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement;
    if (first === undefined || last === undefined) {
      event.preventDefault();
      heading?.focus();
      return;
    }
    if (event.shiftKey && (active === first || active === heading || !panel.contains(active))) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && (active === last || !panel.contains(active))) {
      event.preventDefault();
      first.focus();
    }
  };

  // Focus that lands outside the panel (a click on the page behind is blocked
  // by the scrim, but programmatic focus or assistive tech can still move it)
  // is pulled back in.
  const onFocusIn = (event: FocusEvent): void => {
    if (panel === undefined || !(event.target instanceof Node)) return;
    if (!panel.contains(event.target)) (focusables()[0] ?? heading)?.focus();
  };

  // Bubble phase: a control inside the panel that handles Escape itself (an
  // open InlineConfirm) stops it before it reaches here.
  document.addEventListener('keydown', onKeyDown);
  document.addEventListener('focusin', onFocusIn);
  const unlock = lockScroll();
  onCleanup(() => {
    document.removeEventListener('keydown', onKeyDown);
    document.removeEventListener('focusin', onFocusIn);
    unlock();
    if (opener?.isConnected === true) opener.focus();
  });

  // Focus moves in once the panel is in the document.
  onMount(() => {
    const target = props.initialFocus?.() ?? heading;
    target?.focus();
  });

  return (
    <>
      <div
        class="dialog-scrim"
        aria-hidden="true"
        onClick={() => {
          if (props.dismissable) props.onClose();
        }}
      />
      <div
        ref={panel}
        class={props.class === undefined ? 'dialog' : `dialog ${props.class}`}
        role={props.role ?? 'dialog'}
        aria-modal="true"
        aria-labelledby={props.titleId}
        aria-describedby={props.role === 'alertdialog' ? props.bodyId : undefined}
      >
        <h2 class="dialog-title" id={props.titleId} ref={heading} tabIndex={-1}>
          {props.title}
        </h2>
        <div class="dialog-body" id={props.bodyId}>
          {props.children}
        </div>
        <Show when={props.actions}>
          <div class="dialog-actions">{props.actions}</div>
        </Show>
      </div>
    </>
  );
}
