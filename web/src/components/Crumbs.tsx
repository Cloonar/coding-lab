// Breadcrumb trail shared by every repo sub-page (Repos / <repo> / <section> /
// <leaf>). Ancestor segments carry an `href` and render as <A> links; the
// trailing current-page segment is passed WITHOUT one and renders as inert
// text, per breadcrumb convention. The " / " separators are the component's
// job, never the caller's.

import { A } from '@solidjs/router';
import { For, Show } from 'solid-js';

export interface Crumb {
  /** Visible text for this trail segment. */
  label: string;
  /** Link target; omit on the final (current-page) segment to render it inert. */
  href?: string;
}

export default function Crumbs(props: { segments: Crumb[] }) {
  return (
    <p class="crumb">
      <For each={props.segments}>
        {(segment, index) => (
          <>
            <Show when={index() > 0}>{' / '}</Show>
            <Show when={segment.href} fallback={<span>{segment.label}</span>}>
              {(href) => <A href={href()}>{segment.label}</A>}
            </Show>
          </>
        )}
      </For>
    </p>
  );
}
