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
//
// One repo per mount: the blocks are keyed on the repo id, so moving from
// /repos/a to /repos/b inside the still-mounted frame rebuilds every block
// for b (readiness, live runs, parked work, the AFK card, clone progress)
// and shows nothing of a. A request still in flight for a when that happens
// lands harmlessly: its callbacks go through a repo scope that only reaches
// the frame (toast, refetch, mutate) while a's blocks are on the page.

import { Show, createMemo, onCleanup } from 'solid-js';
import { type Repo } from '../../api';
import { AFKCard } from '../../components/AFKStrip';
import ParkedSection from '../../components/ParkedSection';
import { useEvents } from '../../events';
import { afkStartedMessage } from '../../lib/afk';
import { createLiveInstances } from '../../lib/liveInstances';
import { resourceValue } from '../../lib/resource';
import { createCloneProgressStore, type CloneProgressStore } from '../../stores/cloneProgress';
import { useRepoHome, type RepoHomeState } from './context';
import LiveRuns from './overview/LiveRuns';
import ReadinessBlock from './overview/Readiness';

export default function Overview() {
  const home = useRepoHome();
  const progress = createCloneProgressStore(useEvents());
  onCleanup(progress.dispose);

  return (
    <section class="repo-overview" aria-label="Overview">
      <Show when={home.repo()?.id} keyed>
        {(id) => <RepoOverview id={id} progress={progress} />}
      </Show>
    </section>
  );
}

/**
 * The frame, as one repo's blocks may use it: every call is dropped once the
 * blocks have left the page or the frame shows another repo, so a late answer
 * for repo a never toasts, refetches or writes into repo b's view.
 */
interface RepoScope {
  notify: (message: string) => void;
  refetch: () => Promise<unknown>;
  mutate: (next: Repo) => void;
}

function createRepoScope(home: RepoHomeState, repoID: string): RepoScope {
  let alive = true;
  onCleanup(() => (alive = false));
  const current = () => alive && home.id() === repoID;
  return {
    notify: (message) => {
      if (current()) home.notify(message);
    },
    refetch: async () => (current() ? home.refetch() : undefined),
    mutate: (next) => {
      if (current() && next.id === repoID) home.mutate(next);
    },
  };
}

function RepoOverview(props: { id: string; progress: CloneProgressStore }) {
  const home = useRepoHome();
  // eslint-disable-next-line solid/reactivity -- keyed: this mount is for one repo id
  const scope = createRepoScope(home, props.id);
  // This mount's repo, latched: it never turns undefined (or into another
  // repo) while the keyed Show tears the blocks down.
  const repo = createMemo<Repo | undefined>((last) => {
    const current = home.repo();
    return current !== undefined && current.id === props.id ? current : last;
  });

  return (
    <Show when={repo()}>
      {(r) => (
        <>
          <ReadinessBlock
            repo={r()}
            progress={props.progress.progress(props.id)}
            onCloneRetried={() => props.progress.clear(props.id)}
            onRepoChanged={() => scope.refetch()}
            notify={(message) => scope.notify(message)}
          />
          <Show when={r().clone_status === 'ready'}>
            <ReadyBlocks repo={r()} scope={scope} />
          </Show>
        </>
      )}
    </Show>
  );
}

/** Live runs, AFK and Parked work: only once the repo's clone is ready. */
function ReadyBlocks(props: { repo: Repo; scope: RepoScope }) {
  const { instances, refetch } = createLiveInstances();
  // Names for the toasts, read when the request started (handlers capture
  // what they need before awaiting).
  const name = () => props.repo.name;

  return (
    <div class="overview-grid">
      <div class="overview-live">
        <LiveRuns
          repoID={props.repo.id}
          repoName={props.repo.name}
          instances={resourceValue(instances)}
          onStopped={refetch}
          notify={(message) => props.scope.notify(message)}
        />
      </div>
      <div class="overview-afk">
        <AFKCard
          repo={props.repo}
          onRepoChanged={() => props.scope.refetch()}
          onRepo={(next) => props.scope.mutate(next)}
          onStarted={(run) => props.scope.notify(afkStartedMessage(run, name()))}
          onError={(message) => props.scope.notify(message)}
          onAutoChanged={(enabled) =>
            props.scope.notify(`Auto-spawn ${enabled ? 'on' : 'off'} for ${name()}`)
          }
          onReset={() => props.scope.notify(`AFK runs resumed for ${name()}`)}
        />
      </div>
      <div class="overview-parked">
        <ParkedSection
          repoID={props.repo.id}
          onDiscarded={(branch) => props.scope.notify(`Discarded ${branch}`)}
        />
      </div>
    </div>
  );
}
