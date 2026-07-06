// Transient bottom toast for action outcomes ("parked", "Stopped 3"). One
// message at a time; a new one replaces the old and restarts the timer.

import { Show, createSignal, onCleanup } from 'solid-js';

const TOAST_MS = 4_000;

export interface ToastHandle {
  show(message: string): void;
  Toast: () => ReturnType<typeof ToastView>;
}

function ToastView(props: { message: string | null }) {
  return (
    <Show when={props.message}>
      <div class="toast" role="status">
        {props.message}
      </div>
    </Show>
  );
}

export function createToast(): ToastHandle {
  const [message, setMessage] = createSignal<string | null>(null);
  let timer: ReturnType<typeof setTimeout> | undefined;
  onCleanup(() => clearTimeout(timer));

  return {
    show(next: string) {
      setMessage(next);
      clearTimeout(timer);
      timer = setTimeout(() => setMessage(null), TOAST_MS);
    },
    Toast: () => <ToastView message={message()} />,
  };
}
