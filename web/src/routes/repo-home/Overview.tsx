// Overview tab of the repo home (issue #61) — the index route of /repos/:id.
// Blocks, in order: Readiness (first, full width), then Live runs, AFK and
// Parked work — on a phone one column; from 1024px two columns, Live runs and
// Parked work on the left, AFK on the right. Until the clone is ready only
// Readiness shows (with the clone's progress); the other blocks need a clone.
//
// It reads the repo from the frame (useRepoHome), never fetching it itself,
// and uses the frame's one toast. On load it requests only the readiness
// report, the instance list and the parked list — no request reaches a forge:
// the AFK card's count is the repo summary's claimable count. Every other call
// (Run one, Auto, Reset, Stop all, Retry clone, Discard) happens on a click.

import { Show, onCleanup } from 'solid-js';
import { type Repo } from '../../api';
import { AFKCard } from '../../components/AFKStrip';
import ParkedSection from '../../components/ParkedSection';
import { useEvents } from '../../events';
import { afkStartedMessage } from '../../lib/afk';
import { createLiveInstances } from '../../lib/liveInstances';
import { resourceValue } from '../../lib/resource';
import { createCloneProgressStore } from '../../stores/cloneProgress';
import { useRepoHome } from './context';
import LiveRuns from './overview/LiveRuns';
import ReadinessBlock from './overview/Readiness';

export default function Overview() {
  const home = useRepoHome();
  const progress = createCloneProgressStore(useEvents());
  onCleanup(progress.dispose);

  return (
    <section class="repo-overview" aria-label="Overview">
      <Show when={home.repo()}>
        {(repo) => (
          <>
            <ReadinessBlock
              repo={repo()}
              progress={progress.progress(repo().id)}
              onCloneRetried={() => progress.clear(repo().id)}
              onRepoChanged={() => home.refetch()}
              notify={(message) => home.notify(message)}
            />
            <Show when={repo().clone_status === 'ready'}>
              <ReadyBlocks repo={repo()} />
            </Show>
          </>
        )}
      </Show>
    </section>
  );
}

/** Live runs, AFK and Parked work: only once the repo's clone is ready. */
function ReadyBlocks(props: { repo: Repo }) {
  const home = useRepoHome();
  const { instances, refetch } = createLiveInstances();

  return (
    <div class="overview-grid">
      <div class="overview-live">
        <LiveRuns
          repoID={props.repo.id}
          repoName={props.repo.name}
          instances={resourceValue(instances)}
          onStopped={refetch}
          notify={(message) => home.notify(message)}
        />
      </div>
      <div class="overview-afk">
        <AFKCard
          repo={props.repo}
          onRepoChanged={() => home.refetch()}
          onStarted={(run) => home.notify(afkStartedMessage(run, props.repo.name))}
          onError={(message) => home.notify(message)}
          onAutoChanged={(enabled) =>
            home.notify(`Auto-spawn ${enabled ? 'on' : 'off'} for ${props.repo.name}`)
          }
          onReset={() => home.notify(`AFK runs resumed for ${props.repo.name}`)}
        />
      </div>
      <div class="overview-parked">
        <ParkedSection
          repoID={props.repo.id}
          onDiscarded={(branch) => home.notify(`Discarded ${branch}`)}
        />
      </div>
    </div>
  );
}
