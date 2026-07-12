// Dismissible banner: shows the server's real message text (rendered as text
// content, never HTML) — the v0 sticky-banner property. `variant` (issue
// #149) picks the palette and role: 'error' (default) is the destructive
// alert; 'notice' is an informational, non-error message (e.g. a reply's
// notice body) — status role, neutral palette, dismissible the same way.

import { Show } from 'solid-js';
import Icon from './Icon';

export default function ErrorBanner(props: {
  message: string | null;
  onDismiss: () => void;
  variant?: 'error' | 'notice';
}) {
  const variant = () => props.variant ?? 'error';
  return (
    <Show when={props.message}>
      <div class={`banner ${variant()}`} role={variant() === 'error' ? 'alert' : 'status'}>
        <span class="banner-text">{props.message}</span>
        <button
          type="button"
          class="banner-dismiss"
          aria-label="Dismiss"
          title="Dismiss"
          onClick={() => props.onDismiss()}
        >
          <Icon name="x" size={18} />
        </button>
      </div>
    </Show>
  );
}
