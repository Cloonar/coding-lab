// The entity list-row card: an `<article class="card">` whose head row is the
// row's title, whatever chips belong beside that title, and the row's action
// buttons pushed to the far end — plus one optional metadata line
// (`<p class="muted card-sub">`) directly underneath. Everything below that
// line — chip rows, error banners, inline editors, expandable bodies — stays
// with the caller as `children`.
//
// Contract (issue #282): the markup here is what the five call sites (Tokens,
// repo settings → Imports, Credentials, settings → Notifications, repo
// settings → Schedules) already emitted by hand, so adopting it is a refactor
// and not a visual change. The single addition anywhere is a `.spacer` in
// Credentials' head, which is inert there — see below.
//
// Two head slots rather than one, because the halves of the head row live on
// opposite sides of the spacer. `badges` renders *before* it, so chips keep
// hugging the title — that is Credentials, whose head is the name plus a kind
// chip plus a conditional "in use" chip, and which has no spacer at all today.
// `actions` renders *after* it, so buttons are pushed to the trailing edge —
// that is Tokens, Imports, Notifications and Schedules. One combined slot
// would have to pick a side, and picking "after the spacer" would slide
// Credentials' chips out to the right edge: a visual change.
//
// The spacer is always rendered, even with no actions, for the same reason as
// SectionCard: `.spacer` is `flex: 1` — i.e. `flex: 1 1 0%` — so a trailing
// empty spacer claims only space that was already empty in a left-packed flex
// row, and at `flex-basis: 0` it cannot itself force a wrap in the wrapping
// `.card-head`. That is what makes it safe to introduce into Credentials'
// head, and it keeps the head markup identical with and without actions.
//
// `title` is a JSX slot and not a `string` because Notifications decorates its
// device label with a `Show`-gated `<span class="muted"> · this device</span>`
// *inside* the `.card-title` span. As a string prop that suffix would have to
// become a sibling of the title and pick up the head row's 0.5rem flex gap.
//
// `sub` vanishes entirely when omitted rather than rendering empty — Imports
// has no metadata line at all, and an empty `<p class="muted card-sub">` would
// still contribute its `margin: 0.15rem 0 0` and a line box. The gate is
// `!== undefined` and not truthiness, because every caller that passes a `sub`
// rendered that line unconditionally before the migration: a falsy-but-passed
// value must still print it. Schedules is the live case — `cadenceSummary`
// falls back to `expr.trim()`, which is `''` for a blank cadence — and under a
// truthiness gate that row would silently lose its metadata line.
//
// It is one line and not a list because none of the five rows stacks two of
// them; repo settings → Secrets does (a `Show`-gated description above a fixed
// "Updated …" line), and that is precisely the shape this component is not.
// The class on that line is fixed at `muted card-sub` for the same reason: the
// rows that instead want `card-sub mono` (Repos, History, RepoCRs) are out of
// scope here rather than a knob.
//
// Deliberately NOT owned here: the confirm-then-delete flow and the busy
// labels on the action buttons. The callers' busy models genuinely differ —
// Tokens and Imports hold a boolean signal, Schedules a
// `'delete' | 'reenable' | null` so it can tell which of its two buttons is
// working, Credentials a shared local `run()` helper that also drives an
// inline Banner and a view/rename/replace mode. Folding them together
// would either change behaviour or need one knob per call site. The shared
// thing here is the markup, not the wiring.

import { Show, type JSX } from 'solid-js';

export default function ListRowCard(props: {
  /** The row's identity, rendered inside the `.card-title` span; JSX so it can be decorated. */
  title: JSX.Element;
  /** Chips that belong next to the title; rendered before the spacer, so they stay left. */
  badges?: JSX.Element;
  /** Head-row buttons; rendered after the spacer, so they sit at the trailing edge. */
  actions?: JSX.Element;
  /** The metadata line under the head row; omission — not falsiness — drops the element. */
  sub?: JSX.Element;
  /** Extra classes on the `<article>` (e.g. `token-card`). */
  class?: string;
  children?: JSX.Element;
}) {
  return (
    <article class={props.class === undefined ? 'card' : `card ${props.class}`}>
      <div class="card-head">
        <span class="card-title">{props.title}</span>
        {props.badges}
        <span class="spacer" />
        {props.actions}
      </div>
      <Show when={props.sub !== undefined}>
        <p class="muted card-sub">{props.sub}</p>
      </Show>
      {props.children}
    </article>
  );
}
