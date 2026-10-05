// Repositories (/repos, issue #61): the status list for picking a repo. Top to
// bottom: the heading with "+ Add" (→ /repos/new), the filter field, the
// Needs you block (absent when empty, hidden while a filter is active), then
// every repo — latest run first, never-run repos after, newest first. Below
// 1024px a row stacks name, remote and one status line; from 1024px the same
// data is a table (routes/repos/RepoRows.tsx). Every row opens the repo home,
// where Stop all, parked work and the Issues/CRs/Settings tabs now live.
//
// Data: listRepos (live on repo.changed) carries each repo's summary —
// claimable count and readiness — so the page makes no request per repo and
// none to a forge; listInstances (live on run.changed, patched in place on
// run.messages.changed) gives the live and waiting counts; clone.progress
// feeds the cloning rows. A route notice (e.g. "Deleted x from lab" after a
// repo delete) shows once in the toast.

import { A } from '@solidjs/router';
import { Match, Show, Switch, createMemo, createSignal, onCleanup } from 'solid-js';
import { errorMessage, listRepos, resetAFK, retryClone } from '../api';
import Banner from '../components/Banner';
import EmptyState from '../components/EmptyState';
import Icon from '../components/Icon';
import RequireAuth from '../components/RequireAuth';
import { createToast } from '../components/Toast';
import { useEvents } from '../events';
import { createLiveInstances } from '../lib/liveInstances';
import { createLiveResource } from '../lib/liveResource';
import { createMediaQuery } from '../lib/media';
import {
  filterRepos,
  isFilterActive,
  needsYou,
  orderRepos,
  runCounts,
  type NeedsYouEntry,
} from '../lib/repoList';
import { resourceValue } from '../lib/resource';
import { useRouteNotice } from '../lib/routeNotice';
import { createCloneProgressStore } from '../stores/cloneProgress';
import NeedsYou from './repos/NeedsYou';
import { RepoList, RepoTable, type RepoRowData } from './repos/RepoRows';

export default function Repos() {
  return (
    <RequireAuth>
      <ReposView />
    </RequireAuth>
  );
}

function ReposView() {
  const [repos, { refetch }] = createLiveResource(() => listRepos(), [{ type: 'repo.changed' }]);
  const { instances } = createLiveInstances();
  const progress = createCloneProgressStore(useEvents());
  onCleanup(progress.dispose);
  const toast = createToast();
  useRouteNotice((message) => toast.show(message));
  const desktop = createMediaQuery('(min-width: 1024px)');

  // "Last run" times move on without a refetch.
  const [now, setNow] = createSignal(Date.now());
  const ticker = setInterval(() => setNow(Date.now()), 60_000);
  onCleanup(() => clearInterval(ticker));

  const [query, setQuery] = createSignal('');
  const [error, setError] = createSignal<string | null>(null);
  let filterInput: HTMLInputElement | undefined;

  const ordered = createMemo(() => orderRepos(resourceValue(repos) ?? []));
  const filtering = () => isFilterActive(query());
  // One row object per repo, rebuilt only when the list or the filter changes.
  // Counts and clone progress are getters read by the row's own JSX, so a run
  // state change or a progress tick updates that text in place instead of
  // rebuilding every row (and dropping keyboard focus with it).
  const rows = createMemo((): RepoRowData[] =>
    filterRepos(ordered(), query()).map((repo) => ({
      repo,
      get counts() {
        return runCounts(resourceValue(instances) ?? [], repo.id);
      },
      get progress() {
        return progress.progress(repo.id);
      },
    })),
  );
  const problems = createMemo(() => needsYou(ordered()));

  const retry = async (entry: NeedsYouEntry) => {
    setError(null);
    progress.clear(entry.repo.id); // stale percent from the failed attempt
    try {
      await retryClone(entry.repo.id);
      toast.show(`Retrying the clone of ${entry.repo.name}`);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      void refetch();
    }
  };

  const reset = async (entry: NeedsYouEntry) => {
    setError(null);
    try {
      await resetAFK(entry.repo.id);
      toast.show(`AFK runs resumed for ${entry.repo.name}`);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      void refetch();
    }
  };

  const listTitle = () => (filtering() ? 'Matches' : 'All repositories');
  const noMatch = () => <EmptyState>No repository matches that filter.</EmptyState>;

  return (
    <main class="page page-wide repos-page">
      <div class="repos-head">
        <h1>Repositories</h1>
        <span class="spacer" />
        <A href="/repos/new" class="repos-add">
          <Icon name="plus" size={18} />
          Add
          <span class="visually-hidden"> repository</span>
        </A>
      </div>
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Switch>
        <Match when={repos.error !== undefined}>
          <Banner message={errorMessage(repos.error)} />
        </Match>
        <Match when={resourceValue(repos)?.length === 0}>
          <EmptyState>
            No repositories yet — <A href="/repos/new">add one</A> to get started.
          </EmptyState>
        </Match>
        <Match when={resourceValue(repos)}>
          <div class="repos-filter" role="search">
            <Icon name="search" size={18} class="repos-filter-icon" />
            <input
              ref={filterInput}
              type="search"
              name="filter"
              placeholder="Filter by name or host"
              aria-label="Filter repositories by name or host"
              autocomplete="off"
              spellcheck={false}
              value={query()}
              onInput={(event) => setQuery(event.currentTarget.value)}
            />
            <Show when={query() !== ''}>
              <button
                type="button"
                class="icon-btn repos-filter-clear"
                aria-label="Clear the filter"
                onClick={() => {
                  setQuery('');
                  filterInput?.focus();
                }}
              >
                <Icon name="x" size={18} />
              </button>
            </Show>
          </div>

          <Show when={!filtering() && problems().length > 0}>
            <NeedsYou entries={problems()} onRetry={retry} onReset={reset} />
          </Show>

          <section class="repos-all" aria-labelledby="repos-list-heading">
            <div class="repos-eyebrow-row">
              <h2 class="repos-eyebrow" id="repos-list-heading">
                {listTitle()}
                <span class="visually-hidden"> ({rows().length})</span>
              </h2>
              <span class="count" aria-hidden="true">
                {rows().length}
              </span>
              <span class="spacer" />
              <span class="repos-sort-note">Latest run first</span>
            </div>
            <Show
              when={desktop()}
              fallback={
                <RepoList rows={rows()} labelledBy="repos-list-heading" empty={noMatch()} />
              }
            >
              <RepoTable
                rows={rows()}
                caption={`${listTitle()}, latest run first`}
                now={now()}
                empty={noMatch()}
              />
            </Show>
          </section>
        </Match>
      </Switch>
      {toast.Toast()}
    </main>
  );
}
