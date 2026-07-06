// Dismissible error banner: shows the server's real message text (rendered as
// text content, never HTML) — the v0 sticky-banner property.

import { Show } from 'solid-js';

export default function ErrorBanner(props: { message: string | null; onDismiss: () => void }) {
  return (
    <Show when={props.message}>
      <div class="banner error" role="alert">
        <span class="banner-text">{props.message}</span>
        <button
          type="button"
          class="banner-dismiss"
          aria-label="Dismiss"
          onClick={() => props.onDismiss()}
        >
          ×
        </button>
      </div>
    </Show>
  );
}
