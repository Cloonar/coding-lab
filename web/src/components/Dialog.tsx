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
// All of that is lib/modalStack.ts's createModal(): the dialog shares one
// stack with every other modal on the page (the schedule editor), so a
// dialog that opens OVER another modal is the one that traps focus and hears
// Escape, and the one below waits — two traps never fight over the focus.
//
// Scrim and panel are siblings (the InstallSheet precedent): the close handler
// lives on the scrim alone, so a click inside the panel never reaches it.

import { Show, createUniqueId, type JSX } from 'solid-js';
import { createModal } from '../lib/modalStack';

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

  // A control inside the panel that handles Escape itself (an open
  // InlineConfirm, an open pick) keeps it from reaching here.
  createModal({
    panel: () => panel,
    fallback: () => heading,
    initialFocus: () => props.initialFocus?.(),
    onEscape: (event) => {
      if (!props.dismissable) return;
      event.preventDefault();
      event.stopPropagation();
      props.onClose();
    },
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
