// The "nothing here yet" line every list route and settings section falls back
// to: a single `<p class="empty">` around the caller's message, and nothing
// else. `.empty` in `web/src/styles/banners.css` (dashed border, muted centered
// text) is the one styling hook, and after issue #279 this component is the
// only production code that applies it — so a change to how an empty state
// looks is one edit here, not ~16 hand-rolled call sites.
//
// The message stays a JSX slot rather than a `text: string` prop because real
// call sites are not all static strings: History interpolates its outcome
// filter, RepoCRs passes a computed `emptyText()`, Repos and RepoIssues embed
// an `<A>` link mid-sentence. Passing children through untouched keeps those
// reactive and element-bearing messages working exactly as they did inline.
// The markup is byte-for-byte what the call sites already rendered, so the
// migration is not a visual change.

import { type JSX } from 'solid-js';

export default function EmptyState(props: {
  /** The empty-state message; JSX so it can be computed, reactive, or embed links. */
  children?: JSX.Element;
}) {
  return <p class="empty">{props.children}</p>;
}
