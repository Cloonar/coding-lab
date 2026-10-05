// In-place confirmation for an action that cannot be undone (issue #61): the
// trigger button is replaced, where it stands, by "Cancel" plus a solid danger
// button that names the action ("Stop 2 runs", "Delete for good"). Nothing
// opens over the page and no browser confirm is involved.
//
// Focus: opening moves focus to Cancel — the safe default, so a held or
// repeated Enter cannot run the action — and Cancel or Escape closes it and
// returns focus to the trigger. The confirm button ignores the second click of
// a double-click (event.detail > 1), because it appears where the trigger was.
// While the promise onConfirm returns is pending, both buttons are disabled
// and the confirm button shows `busyLabel`; once it settles (either way) the
// trigger comes back. Report a failure from the caller, as usual.

import { Show, createSignal, createUniqueId, type JSX } from 'solid-js';

export interface InlineConfirmProps {
  /** Trigger content, e.g. "Stop all (2)". */
  label: JSX.Element;
  /** The named danger action, e.g. "Stop 2 runs". */
  confirmLabel: string;
  onConfirm: () => unknown;
  /** Defaults to "Cancel". */
  cancelLabel?: string;
  /** Confirm-button text while the action runs; defaults to confirmLabel + "…". */
  busyLabel?: string;
  /** Optional question shown before the two buttons, e.g. "Discard your changes?". */
  prompt?: JSX.Element;
  /** Classes on the trigger button; defaults to "danger". */
  class?: string;
  /** Renders all three buttons in the compact `small` size. */
  small?: boolean;
  /** Accessible name for the trigger when its content is not text alone. */
  'aria-label'?: string;
  disabled?: boolean;
  /** Fires when the confirm row opens or closes. */
  onOpenChange?: (open: boolean) => void;
}

export default function InlineConfirm(props: InlineConfirmProps) {
  const promptId = `inline-confirm-${createUniqueId()}-prompt`;
  const [open, setOpen] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  let trigger: HTMLButtonElement | undefined;
  let cancel: HTMLButtonElement | undefined;

  const size = () => (props.small === true ? ' small' : '');

  const setOpenState = (next: boolean): void => {
    setOpen(next);
    props.onOpenChange?.(next);
  };

  const openConfirm = (): void => {
    setOpenState(true);
    cancel?.focus();
  };

  const close = (): void => {
    setOpenState(false);
    // The trigger re-renders synchronously inside the Show; focus it if this
    // component is still on the page.
    if (trigger?.isConnected === true) trigger.focus();
  };

  const confirm = async (event: MouseEvent): Promise<void> => {
    if (event.detail > 1 || busy()) return;
    setBusy(true);
    try {
      await props.onConfirm();
    } finally {
      setBusy(false);
      close();
    }
  };

  return (
    <Show
      when={open()}
      fallback={
        <button
          type="button"
          ref={trigger}
          class={`${props.class ?? 'danger'}${size()}`}
          aria-label={props['aria-label']}
          disabled={props.disabled}
          onClick={openConfirm}
        >
          {props.label}
        </button>
      }
    >
      <span
        class="inline-confirm"
        role="group"
        aria-labelledby={props.prompt !== undefined ? promptId : undefined}
        aria-label={props.prompt === undefined ? props.confirmLabel : undefined}
        aria-busy={busy() ? 'true' : undefined}
        onKeyDown={(event) => {
          if (event.key !== 'Escape' || busy()) return;
          event.preventDefault();
          event.stopPropagation();
          close();
        }}
      >
        <Show when={props.prompt}>
          <span class="inline-confirm-prompt" id={promptId}>
            {props.prompt}
          </span>
        </Show>
        <button type="button" ref={cancel} class={size().trim()} disabled={busy()} onClick={close}>
          {props.cancelLabel ?? 'Cancel'}
        </button>
        <button
          type="button"
          class={`solid-danger${size()}`}
          disabled={busy()}
          onClick={(event) => void confirm(event)}
        >
          {busy() ? (props.busyLabel ?? `${props.confirmLabel}…`) : props.confirmLabel}
        </button>
      </span>
    </Show>
  );
}
