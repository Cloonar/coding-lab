// Overview tab of the repo home (issue #61) — the index route of /repos/:id.
// A placeholder until the Overview part lands: readiness, live runs with Stop
// all, the AFK strip and parked work replace this note. It reads the repo from
// the frame (useRepoHome), never fetching it itself.

import { Show } from 'solid-js';
import { useRepoHome } from './context';

export default function Overview() {
  const home = useRepoHome();
  return (
    <section class="repo-overview" aria-label="Overview">
      <Show when={home.repo()}>
        {(repo) => (
          <p class="muted">
            Readiness, live runs, AFK and parked work for {repo().name} appear here.
          </p>
        )}
      </Show>
    </section>
  );
}
