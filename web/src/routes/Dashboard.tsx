// Dashboard: Claude auth card, then repo cards (name, remote host, chips,
// clone status with live SSE progress) each carrying its start-instance form,
// the AFK section (start/auto/paused), live instance list, stop-all and
// parked strip — or the add-repo empty state. repo.changed → refetch repos
// (paused banner + auto toggle follow the repo row); run.changed → refetch
// instances; clone.progress → per-card signal only; parked.changed lives in
// the strip.

import { A } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import {
  errorMessage,
  getSpawnDefaults,
  listCRs,
  listInstances,
  listProviders,
  listRepos,
  retryClone,
  stopAll,
  type Instance,
  type Repo,
  type Run,
} from '../api';
import AFKSection from '../components/AFKSection';
import ClaudeAuthCard from '../components/ClaudeAuthCard';
import ErrorBanner from '../components/ErrorBanner';
import InstanceList from '../components/InstanceList';
import ParkedSection from '../components/ParkedSection';
import RequireAuth from '../components/RequireAuth';
import StartInstanceForm from '../components/StartInstanceForm';
import { createToast } from '../components/Toast';
import TopBar from '../components/TopBar';
import { useEvents } from '../events';
import { remoteHost } from '../lib/repoName';
import { resourceValue } from '../lib/resource';
import { providerFor } from '../lib/spawn';
import { createCloneProgressStore, type CloneProgress } from '../stores/cloneProgress';

export default function Dashboard() {
  return (
    <RequireAuth>
      <DashboardView />
    </RequireAuth>
  );
}

function DashboardView() {
  const events = useEvents();
  const [repos, { refetch }] = createResource(() => listRepos());
  const [instances, { refetch: refetchInstances }] = createResource(() => listInstances());
  const [providers] = createResource(() => listProviders());
  const [spawnDefaults] = createResource(() => getSpawnDefaults());
  const progress = createCloneProgressStore(events);
  const toast = createToast();
  onCleanup(progress.dispose);
  onCleanup(events.subscribe('repo.changed', () => void refetch()));
  onCleanup(events.subscribe('run.changed', () => void refetchInstances()));

  const [error, setError] = createSignal<string | null>(null);

  const instancesOf = (repoID: string): Instance[] =>
    (instances() ?? []).filter((instance) => instance.repo_id === repoID);

  const retry = async (repo: Repo) => {
    setError(null);
    progress.clear(repo.id); // stale percent from the failed attempt
    try {
      await retryClone(repo.id);
      await refetch(); // immediate feedback; SSE keeps it fresh after
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const stopAllIn = async (repo: Repo) => {
    setError(null);
    try {
      const res = await stopAll(repo.id);
      toast.show(`Stopped ${res.stopped} instance${res.stopped === 1 ? '' : 's'}`);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      void refetchInstances();
    }
  };

  const afkStarted = (run: Run) => {
    toast.show(
      run.issue_number === null ? 'AFK run started' : `AFK run started on #${run.issue_number}`,
    );
    void refetchInstances();
  };

  return (
    <main class="page">
      <TopBar />
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <div class="stack">
        <ClaudeAuthCard />
      </div>
      <Switch>
        <Match when={repos.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(repos.error)}</span>
          </div>
        </Match>
        <Match when={repos()?.length === 0}>
          <p class="empty">
            No repositories yet — <A href="/repos/new">add one</A> to get started.
          </p>
        </Match>
        <Match when={repos()}>
          <div class="card-list">
            <For each={repos()}>
              {(repo) => (
                <RepoCard
                  repo={repo}
                  instances={instancesOf(repo.id)}
                  provider={providerFor(providers() ?? [], repo.provider)}
                  defaults={spawnDefaults() ?? {}}
                  progress={progress.progress(repo.id)}
                  onRetry={() => void retry(repo)}
                  onStopAll={() => void stopAllIn(repo)}
                  onInstancesChanged={() => void refetchInstances()}
                  onRepoChanged={() => void refetch()}
                  onAFKStarted={afkStarted}
                  onStopped={(outcome) => toast.show(outcome)}
                  onError={setError}
                />
              )}
            </For>
          </div>
        </Match>
      </Switch>
      <Show when={(repos()?.length ?? 0) > 0}>
        <A href="/repos/new" class="add-row">
          + Add repository
        </A>
      </Show>
      {toast.Toast()}
    </main>
  );
}

function RepoCard(props: {
  repo: Repo;
  instances: Instance[];
  provider: ReturnType<typeof providerFor>;
  defaults: { model?: string; effort?: string };
  progress: CloneProgress | null;
  onRetry: () => void;
  onStopAll: () => void;
  onInstancesChanged: () => void;
  onRepoChanged: () => void;
  onAFKStarted: (run: Run) => void;
  onStopped: (outcome: 'removed' | 'parked') => void;
  onError: (message: string) => void;
}) {
  return (
    <article class="card repo-card">
      <div class="card-head">
        <span class="card-title">{props.repo.name}</span>
        <span class="spacer" />
        <Show when={props.instances.length > 1}>
          <button type="button" class="danger stop-all" onClick={() => props.onStopAll()}>
            Stop all
          </button>
        </Show>
        <A href={`/repos/${props.repo.id}/issues`} class="card-link">
          Issues
        </A>
        <Show when={props.repo.tracker_binding === 'builtin'}>
          <A href={`/repos/${props.repo.id}/crs`} class="card-link">
            CRs
          </A>
        </Show>
        <A href={`/repos/${props.repo.id}/settings`} class="card-link">
          Settings
        </A>
      </div>
      <p class="muted card-sub mono">{remoteHost(props.repo.remote_url)}</p>
      <div class="chip-row">
        <span class="chip">
          {props.repo.tracker_binding === 'forge'
            ? `forge · ${props.repo.forge_kind}`
            : 'builtin tracker'}
        </span>
        <Show when={props.repo.incogni}>
          <span class="chip incogni">incogni</span>
        </Show>
        <Show when={props.repo.tracker_binding === 'builtin'}>
          <OpenCRChip repoID={props.repo.id} />
        </Show>
        <Show when={props.repo.clone_status === 'cloning'}>
          <span class="chip status-cloning">cloning</span>
        </Show>
        <Show when={props.repo.clone_status === 'error'}>
          <span class="chip status-error">clone failed</span>
        </Show>
      </div>
      <Show when={props.repo.clone_status === 'cloning'}>
        <CloneProgressBar progress={props.progress} />
      </Show>
      <Show when={props.repo.clone_status === 'error'}>
        <div class="banner error clone-error" role="alert">
          <span class="banner-text">{props.repo.clone_error ?? 'clone failed'}</span>
          <button type="button" onClick={() => props.onRetry()}>
            Retry
          </button>
        </div>
      </Show>
      <Show when={props.instances.length > 0}>
        <InstanceList
          instances={props.instances}
          onStopped={props.onStopped}
          onChanged={props.onInstancesChanged}
          onError={props.onError}
        />
      </Show>
      <Show when={props.repo.clone_status === 'ready'}>
        <AFKSection
          repo={props.repo}
          onRepoChanged={props.onRepoChanged}
          onStarted={props.onAFKStarted}
          onError={props.onError}
        />
        <StartInstanceForm
          repo={props.repo}
          provider={props.provider}
          defaults={props.defaults}
          onStarted={props.onInstancesChanged}
        />
        <ParkedSection repoID={props.repo.id} />
      </Show>
    </article>
  );
}

/**
 * Open-CR count chip (builtin-bound repos only — the caller gates): the CR
 * entry point on the repo card. Self-fetching with a scoped cr.changed
 * refetch; a failing CR endpoint hides the chip instead of breaking the
 * dashboard (non-throwing resource read).
 */
function OpenCRChip(props: { repoID: string }) {
  const events = useEvents();
  const [crs, { refetch }] = createResource(
    () => props.repoID,
    (repoID) => listCRs(repoID, 'open'),
  );
  onCleanup(
    // eslint-disable-next-line solid/reactivity -- the handler re-reads props.repoID fresh on every SSE event
    events.subscribe('cr.changed', (event) => {
      if (event.repoID === props.repoID) void refetch();
    }),
  );
  const count = () => resourceValue(crs)?.length ?? 0;
  return (
    <Show when={count() > 0}>
      <A href={`/repos/${props.repoID}/crs`} class="chip cr-count">
        {count()} open CR{count() === 1 ? '' : 's'}
      </A>
    </Show>
  );
}

function CloneProgressBar(props: { progress: CloneProgress | null }) {
  const percent = () => props.progress?.percent ?? null;
  return (
    <div class="clone-progress">
      <div class="progress-meta">
        <span class="muted">{props.progress?.phase ?? 'starting…'}</span>
        <span class="spacer" />
        <Show when={percent() !== null}>
          <span class="muted">{percent()}%</span>
        </Show>
      </div>
      <div
        class="progress-track"
        role="progressbar"
        aria-valuemin="0"
        aria-valuemax="100"
        aria-valuenow={percent() ?? undefined}
      >
        <div
          classList={{ 'progress-fill': true, indeterminate: percent() === null }}
          style={percent() !== null ? { width: `${percent()}%` } : undefined}
        />
      </div>
      <Show when={props.progress?.line}>
        <p class="progress-line mono muted">{props.progress!.line}</p>
      </Show>
    </div>
  );
}
