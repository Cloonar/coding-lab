// The settings-page section card: a `.card` whose head row is a title, an
// optional right-aligned action button, and an optional one-line hint directly
// underneath. Everything else — error banners, add-forms, empty-vs-list bodies —
// stays with the caller as `children`.
//
// Contract (issue #275): ALL spacing between the head row, the hint and the body
// lives here and in `.section-card-head` (cards.css). Call sites must not re-add
// margins around any of the three. That collision — "+ Add import" printing on
// top of its hint on repo settings → Imports — happened because `.hint-block`
// carried a negative top margin calibrated to a bare heading above it, while a
// flex head row is sized by its 36px button, not by the heading. The head row
// now owns the gap explicitly, so the hint needs no negative margin.

import { Show, type JSX } from 'solid-js';

export default function SectionCard(props: {
  /** Section heading; renders as the `<h2>` in the head row. */
  title: string;
  /** Head-row action slot (a button), right-aligned by the always-present spacer. */
  action?: JSX.Element;
  /** One-line explanation under the head row; JSX so hints can embed `<code>`. */
  hint?: JSX.Element;
  /** Extra classes on the `<section>` (e.g. `danger-zone`). */
  class?: string;
  children?: JSX.Element;
}) {
  return (
    <section class={props.class === undefined ? 'card' : `card ${props.class}`}>
      <div class="card-head section-card-head">
        <h2>{props.title}</h2>
        {/* Always rendered: `.spacer` is flex: 1 and inert while empty, so the
            head row's markup is identical with and without an action. */}
        <span class="spacer" />
        {props.action}
      </div>
      <Show when={props.hint}>
        <small class="hint hint-block">{props.hint}</small>
      </Show>
      {props.children}
    </section>
  );
}
