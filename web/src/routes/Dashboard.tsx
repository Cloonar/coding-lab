// Dashboard: repo cards (name, remote host, binding/forge/incogni chips,
// clone status with live SSE progress) or the add-repo empty state.
// repo.changed → refetch the list; clone.progress → per-card signal only.

import { A } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import { errorMessage, listRepos, retryClone, type Repo } from '../api';
import ErrorBanner from '../components/ErrorBanner';
import RequireAuth from '../components/RequireAuth';
import TopBar from '../components/TopBar';
import { useEvents } from '../events';
import { remoteHost } from '../lib/repoName';
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
  const progress = createCloneProgressStore(events);
  onCleanup(progress.dispose);
  onCleanup(events.subscribe('repo.changed', () => void refetch()));

  const [error, setError] = createSignal<string | null>(null);

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

  return (
    <main class="page">
      <TopBar />
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
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
                  progress={progress.progress(repo.id)}
                  onRetry={() => void retry(repo)}
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
    </main>
  );
}

function RepoCard(props: { repo: Repo; progress: CloneProgress | null; onRetry: () => void }) {
  return (
    <article class="card repo-card">
      <div class="card-head">
        <span class="card-title">{props.repo.name}</span>
        <span class="spacer" />
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
    </article>
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
