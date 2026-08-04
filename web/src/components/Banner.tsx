// The one banner in the app: a `.banner` row showing the server's real message
// text (rendered as text content, never HTML — the v0 sticky-banner property),
// with an optional caller-supplied control and an optional dismiss button.
// After issue #283 this component is the only production code that builds
// `class="banner …"` markup, so a change to how a banner looks or announces
// itself is one edit here rather than ~33 hand-rolled copies.
//
// Why it is `Banner` and no longer `ErrorBanner`: it renders the success
// variant too, and a component named for one of its three variants invites the
// next author to hand-roll the other two — which is how the 33 copies
// accumulated in the first place. The rename is the extension, not a fork:
// same file, same markup, same dismiss behaviour; every former `ErrorBanner`
// call site passes the identical props under the new name.
//
// `variant` picks the palette and the role together, because in this app they
// are one decision, not two: 'error' (default) is the destructive alert and is
// the only variant that interrupts a screen reader (`role="alert"`); 'notice'
// (issue #149) is an informational non-error message such as a reply's notice
// body; 'success' confirms a save that already happened. Neither of the latter
// two is an emergency, so both announce politely as `role="status"` — which is
// what all 12 inline `banner success` copies already hard-coded, and pairing
// role to variant here is what stops the thirteenth from getting it wrong.
//
// `onDismiss` is optional, and its absence IS the non-dismissible case: no
// dismiss button is rendered at all. That is the majority shape in this
// codebase — a banner driven by a resource's error or by a `paused` flag has
// nowhere to dismiss to, since the next render would bring it straight back —
// and requiring a no-op handler would have made those sites lie about being
// interactive. The sites that own a `setError(null)` signal keep passing it,
// and keep their button.
//
// `action` is a slot rather than more props because the three sites that carry
// a control inside the banner (the AFK strip's Reset, a paused schedule's
// Re-enable, a failed clone's Retry) each need their own label, handler and
// disabled condition; a `<button>` from the caller is smaller than the four
// props it would take to describe one. It renders between the text and the
// dismiss button, which is where both of those already sit today, so no
// migrated site's markup moves. The message stays a `string` prop and never
// becomes a slot: text-only is the property this component exists to guarantee.
//
// `class` appends site-specific modifiers after the variant — `afk-strip-paused`
// (select.css) and `clone-error` (chips.css) are real styling hooks on two of
// these banners, and passing them through keeps `banner error afk-strip-paused`
// rendering byte-for-byte as it did inline.

import { Show, type JSX } from 'solid-js';
import Icon from './Icon';

export default function Banner(props: {
  /** Message text, or null to render nothing; always rendered as text, never HTML. */
  message: string | null;
  /** Palette and ARIA role: 'error' (default) alerts, 'notice' and 'success' announce politely. */
  variant?: 'error' | 'notice' | 'success';
  /** Dismiss handler; omit it entirely for a banner with no dismiss button. */
  onDismiss?: () => void;
  /** A control to sit inside the banner (Reset, Retry, Re-enable), after the text. */
  action?: JSX.Element;
  /** Extra classes after the variant (e.g. `afk-strip-paused`, `clone-error`). */
  class?: string;
}) {
  const variant = () => props.variant ?? 'error';
  const classes = () =>
    props.class === undefined ? `banner ${variant()}` : `banner ${variant()} ${props.class}`;
  return (
    <Show when={props.message}>
      <div class={classes()} role={variant() === 'error' ? 'alert' : 'status'}>
        <span class="banner-text">{props.message}</span>
        {/* Interpolated bare rather than wrapped in a `<Show>`: an absent slot
            already renders nothing, and no wrapper means the omitted case emits
            no element between the text and the dismiss button. */}
        {props.action}
        <Show when={props.onDismiss !== undefined}>
          <button
            type="button"
            class="banner-dismiss"
            aria-label="Dismiss"
            title="Dismiss"
            onClick={() => props.onDismiss?.()}
          >
            <Icon name="x" size={18} />
          </button>
        </Show>
      </div>
    </Show>
  );
}
