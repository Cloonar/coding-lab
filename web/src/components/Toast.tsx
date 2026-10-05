// Transient bottom toast for action outcomes ("parked", "Stopped 3"). One
// message at a time; a new one replaces the old and restarts the timer.
//
// Optional action (issue #61): show(message, { action: { label, run } })
// renders one button beside the text — the Undo pattern for actions that
// apply at once and can be reversed. An action toast stays up longer (6.5s
// against 4s) so there is time to reach for it, and its timer pauses while
// the pointer or keyboard focus is inside the toast. Running the action hides
// the toast first. A plain show(message) renders exactly as before: the text
// alone in the pill.
//
// Announcing: a live region must already be in the page when its content
// changes, or screen readers never hear the change. So the status region
// (`.toast-region`, role="status") is ALWAYS rendered — empty, out of flow and
// zero-sized while nothing shows — and only the `.toast` pill inside it comes
// and goes.

import { Show, createSignal, onCleanup } from 'solid-js';

const TOAST_MS = 4_000;
const ACTION_TOAST_MS = 6_500;

export interface ToastAction {
  /** Button text, e.g. "Undo". */
  label: string;
  /** Runs after the toast hides. */
  run: () => void;
}

export interface ToastOptions {
  action?: ToastAction;
  /** Overrides the default lifetime (4s plain, 6.5s with an action). */
  durationMs?: number;
}

export interface ToastHandle {
  show(message: string, options?: ToastOptions): void;
  /** Hides the current toast at once. */
  dismiss(): void;
  Toast: () => ReturnType<typeof ToastView>;
}

interface ToastEntry {
  message: string;
  action?: ToastAction;
}

function ToastView(props: {
  entry: ToastEntry | null;
  onAction: (action: ToastAction) => void;
  onHold: () => void;
  onRelease: () => void;
}) {
  return (
    <div class="toast-region" role="status">
      <Show when={props.entry}>
        {(entry) => (
          <Show when={entry().action} fallback={<div class="toast">{entry().message}</div>}>
            {(action) => (
              <div
                class="toast toast-with-action"
                onMouseEnter={() => props.onHold()}
                onMouseLeave={() => props.onRelease()}
                onFocusIn={() => props.onHold()}
                onFocusOut={() => props.onRelease()}
              >
                <span class="toast-text">{entry().message}</span>
                <button type="button" class="toast-action" onClick={() => props.onAction(action())}>
                  {action().label}
                </button>
              </div>
            )}
          </Show>
        )}
      </Show>
    </div>
  );
}

export function createToast(): ToastHandle {
  const [entry, setEntry] = createSignal<ToastEntry | null>(null);
  let timer: ReturnType<typeof setTimeout> | undefined;
  let lifetime = TOAST_MS;
  onCleanup(() => clearTimeout(timer));

  const arm = (ms: number): void => {
    clearTimeout(timer);
    timer = setTimeout(() => setEntry(null), ms);
  };
  const dismiss = (): void => {
    clearTimeout(timer);
    setEntry(null);
  };

  return {
    show(message: string, options?: ToastOptions) {
      const action = options?.action;
      lifetime = options?.durationMs ?? (action !== undefined ? ACTION_TOAST_MS : TOAST_MS);
      setEntry(action !== undefined ? { message, action } : { message });
      arm(lifetime);
    },
    dismiss,
    Toast: () => (
      <ToastView
        entry={entry()}
        onAction={(action) => {
          dismiss();
          action.run();
        }}
        onHold={() => clearTimeout(timer)}
        onRelease={() => arm(lifetime)}
      />
    ),
  };
}
