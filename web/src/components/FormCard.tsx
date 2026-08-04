// The add/create form card behind every "new X" route and settings section: a
// `.card.form-card` holding an optional heading, an optional standing intro
// line, the dismissible error banner, the caller's fields, and a submit button
// that swaps its own label and goes disabled while the request is in flight.
// Only the fields — the labels, inputs, selects and hints — stay with the
// caller, as `children`.
//
// Contract (issue #281): the card wrapper, the heading, the error banner and
// the submit button all live HERE. A call site passes `title`, `error` /
// `onDismissError`, `submitLabel` / `busyLabel` and its `busy` signal, and must
// NOT re-add a `<div class="card form-card">` around this, its own `<h2>`, its
// own `<Banner>`, or its own `<button type="submit">`. Seven sites hand-
// rolled that same shell and drifted: one of them (Schedules, which also
// cancels) wraps the button in `.card-actions` while the other six leave it
// bare, five ask for `primary wide` and two for `primary`, and one disables the
// button on a second condition (an unset select) as well as on busy. All of
// those variations are props — `actions`, `wide`, `disabled` — so the drift
// becomes a parameter instead of a copy.
//
// Why `intro` is its own slot and not just the first of `children`: the two
// kinds of prose in this card sit on opposite sides of the banner, and which
// side is not a style choice. `intro` is STANDING explanatory text — AddRepo's
// "lab clones a bare mirror it owns…" — which is equally true before, during
// and after an error, so it outranks the transient banner and renders above it.
// Schedules' success note is the other kind: a result of the last submit, which
// belongs below the banner and therefore stays in `children`. Routing AddRepo's
// paragraph through `children` would push it under the banner and silently
// reorder the card the moment an error showed, which #281's "no visual change"
// forbids. That this slot belongs to the shell rather than to one site's quirk
// is already written into the CSS: `.form-card > p { margin-top: 0 }`
// (cards.css) exists to style exactly that paragraph.
//
// Why the `<form>` IS the card, rather than a `<div class="card form-card">`
// wrapping one: six sites render `div.card.form-card > (h2, Banner, form)`
// while NewIssue already renders `form.card.form-card` with the banner inside
// the form, and one shape had to win. Form-as-card is the CSS-safe one and it
// keeps NewIssue's existing `form.form-card` assertion meaningful. Checked
// against `web/src/styles/`: `.form-card` (cards.css) only sets `margin-bottom`
// plus `.form-card > p { margin-top: 0 }` — and an `intro` paragraph stays a
// direct child of the element carrying `.form-card` under this shape, so that
// rule still bites. No rule anywhere selects a bare `form` element; every form
// selector is classed or scoped (`details.start-instance form`, `.comment-form`,
// `.label-form`, `.chat-title-form`), and nothing selects `.card > h2` or any
// other direct-child structure of a card. A `<form>` carries no UA-default
// margin or padding, so hoisting the heading and the banner inside it moves
// nothing on screen.

import { Show, type JSX } from 'solid-js';
import Banner from './Banner';

export default function FormCard(props: {
  /** Card heading; the `<h2>` is omitted entirely when this is not passed. */
  title?: string;
  /**
   * Standing explanation under the heading — usually a `<p class="muted">`.
   * Renders ABOVE the error banner, because it is true regardless of any
   * error; per-submit prose (a success note) goes in `children` instead.
   */
  intro?: JSX.Element;
  /** Current error text, or null; the banner hides itself when null. */
  error: string | null;
  /** Called when the banner's dismiss control is clicked. */
  onDismissError: () => void;
  /** Form submit handler; the event is forwarded untouched, un-defaulted. */
  onSubmit: (event: SubmitEvent) => void;
  /** Request in flight: swaps the button's label and disables it. */
  busy: boolean;
  /** Button label at rest. */
  submitLabel: string;
  /** Button label while `busy`. */
  busyLabel: string;
  /** Full-width button (`primary wide` instead of `primary`). */
  wide?: boolean;
  /** Extra disabling condition, OR'd with `busy` (e.g. a select with no value). */
  disabled?: boolean;
  /** Sibling controls for the button; their presence adds the `.card-actions` row. */
  actions?: JSX.Element;
  /** The form's fields, plus anything the caller wants under the banner. */
  children?: JSX.Element;
}) {
  const buttonClass = () => (props.wide === true ? 'primary wide' : 'primary');
  const isDisabled = () => props.busy || props.disabled === true;
  const label = () => (props.busy ? props.busyLabel : props.submitLabel);

  return (
    // No `event.preventDefault()` here: every call site's handler defaults the
    // event itself, and some validate before doing so.
    <form class="card form-card" onSubmit={(event) => props.onSubmit(event)}>
      <Show when={props.title !== undefined}>
        <h2>{props.title}</h2>
      </Show>
      {/* Interpolated bare rather than wrapped in a `<Show>`: an absent slot
          already renders nothing, and no wrapper means the omitted case emits
          no element at all. Position is the whole point — above the banner. */}
      {props.intro}
      <Banner message={props.error} onDismiss={() => props.onDismissError()} />
      {props.children}
      {/* The button is written out in both branches on purpose: a helper that
          returns JSX and is called from two prop positions is a Solid footgun,
          and three duplicated lines are the honest cost of avoiding it. */}
      <Show
        when={props.actions !== undefined}
        fallback={
          <button type="submit" class={buttonClass()} disabled={isDisabled()}>
            {label()}
          </button>
        }
      >
        <div class="card-actions">
          <button type="submit" class={buttonClass()} disabled={isDisabled()}>
            {label()}
          </button>
          {props.actions}
        </div>
      </Show>
    </form>
  );
}
