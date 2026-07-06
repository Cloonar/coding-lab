// The `Closes #N` chips a change request carries: one per built-in issue the
// merge will close, each linking to that issue. Renders nothing when the CR
// closes nothing — callers add their own placeholder if they want one.

import { A } from '@solidjs/router';
import { For } from 'solid-js';

export default function ClosesChips(props: { repoID: string; closes: number[] }) {
  return (
    <For each={props.closes}>
      {(number) => (
        <A href={`/repos/${props.repoID}/issues/${number}`} class="chip closes-chip">
          closes #{number}
        </A>
      )}
    </For>
  );
}
