// PWA install bottom sheet (issue #142) — the app's first modal primitive.
// A bottom sheet, not a centered dialog: it's the native idiom for phone
// install prompts (Chrome's own "Add to Home screen" banner, most app
// onboarding) — thumb-reachable, and on iOS it sits visually adjacent to
// Safari's bottom toolbar Share button that the instructions point at, so
// the sheet and the thing it references are in the same glance.
//
// Two variants, no shared install affordance: android has a real
// beforeinstallprompt-backed Install button; iOS has no install API at all
// (Apple never shipped one), so it only ever walks the operator through the
// manual Share -> Add to Home Screen steps.
//
// Purely props-driven and self-contained — wiring to the actual install
// signal (lib/install.ts) happens elsewhere.

import { Match, Show, Switch, createUniqueId } from 'solid-js';
import Icon from './Icon';

export interface InstallSheetProps {
  open: boolean;
  variant: 'ios' | 'android';
  onInstall: () => void; // android Install button
  onDismiss: () => void; // Not now, scrim tap
}

export default function InstallSheet(props: InstallSheetProps) {
  const headingId = `install-sheet-${createUniqueId()}-heading`;

  return (
    <Show when={props.open}>
      {/* Scrim and card are siblings, not nested: the dismiss handler lives
          only on the scrim element, so a click anywhere in the card never
          bubbles through it — no stopPropagation to remember or forget. */}
      <div class="install-scrim" aria-hidden="true" onClick={() => props.onDismiss()} />
      <div class="install-sheet" role="dialog" aria-modal="true" aria-labelledby={headingId}>
        <h2 id={headingId} class="install-sheet-heading">
          Install lab
        </h2>
        <Switch>
          <Match when={props.variant === 'android'}>
            <p class="install-sheet-blurb">Add lab to your Home Screen for a full-screen app.</p>
            <div class="install-sheet-actions">
              <button type="button" class="primary wide" onClick={() => props.onInstall()}>
                Install
              </button>
              <button type="button" class="wide" onClick={() => props.onDismiss()}>
                Not now
              </button>
            </div>
          </Match>
          <Match when={props.variant === 'ios'}>
            <ol class="install-steps">
              <li class="install-step">
                <Icon name="share" class="install-step-icon" />
                <span>1. Tap Share in the toolbar.</span>
              </li>
              <li class="install-step">
                <Icon name="square-plus" class="install-step-icon" />
                <span>2. Choose Add to Home Screen.</span>
              </li>
            </ol>
            <div class="install-sheet-actions">
              <button type="button" class="wide" onClick={() => props.onDismiss()}>
                Not now
              </button>
            </div>
          </Match>
        </Switch>
      </div>
    </Show>
  );
}
