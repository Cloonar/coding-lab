// The page-level head row: a section heading plus an optional right-aligned
// action/controls slot, for the page sections that are NOT cards — the bare
// `.section-head` that sits above a page's body. `SectionCard` owns the
// card-level head row; between the two, no route hand-rolls a head row.
//
// Contract (issue #275, extended here): the gap BELOW the row lives with the
// component — `.section-head`'s `margin-bottom` (shell.css) — so call sites
// must not add margins of their own around it. Same reasoning as the card head:
// a flex row is sized by its 36px control, not by the heading, so anything
// calibrated to a bare heading above it collides with the row.
//
// `title` is a JSX.Element, where SectionCard's is a plain string, because the
// detail routes put an inline number span inside the heading:
// `<span class="mono muted issue-number">#12</span> Title`.
//
// The always-present `.spacer` replaces the row's old
// `justify-content: space-between`: the row's height and the action's alignment
// no longer depend on how many children the call site happens to pass.

import { type JSX } from 'solid-js';

export default function SectionHead(props: {
  /** Page-section heading; renders as the `<h2>` of the row. */
  title: JSX.Element;
  /** Head-row action/controls slot, right-aligned by the always-present spacer. */
  action?: JSX.Element;
}) {
  return (
    <div class="section-head">
      <h2>{props.title}</h2>
      {/* Always rendered: `.spacer` is flex: 1 and inert while empty, so the
          head row's markup is identical with and without an action. */}
      <span class="spacer" />
      {props.action}
    </div>
  );
}
