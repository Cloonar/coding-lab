// Repositories (/repos, issue #61): the status list for picking a repo. Top to
// bottom: the heading with "+ Add" (→ /repos/new), the filter field, the
// Needs you block (absent when empty, hidden while a filter is active), then
// every repo — latest run first, never-run repos after, newest first. Below
// 1024px a row stacks name, remote and one status line; from 1024px the same
// data is a table (routes/repos/RepoRows.tsx). Every row opens the repo home,
// where Stop all, parked work and the Issues/CRs/Settings tabs now live.
//
// Data: listRepos (live on repo.changed, and — debounced — on the issue, run,
// parked and agent login events that move a summary without a repo.changed:
// lib/repoList.ts summaryRefreshSpecs) carries each repo's summary —
// claimable count and readiness — so the page makes no request per repo and
// none to a forge; listInstances (live on run.changed, patched in place on
// run.messages.changed) gives the live and waiting counts; clone.progress
// feeds the cloning rows. A route notice (e.g. "Deleted x from lab" after a
// repo delete) shows once in the toast.
//
// Identity: every refetch returns fresh objects for EVERY repo, so the rows
// and the Needs you entries render from stores reconciled by key (repo id,
// problem key), never from the raw response — one repo.changed for any repo
// patches what changed in place instead of rebuilding every row and dropping
// a keyboard user's focus. A failed refetch keeps the list it had and shows
// the error above it. The Needs you busy state lives here, keyed by problem,
// so a Retry or Reset cannot be sent twice, and once a fixed entry leaves the
// block, focus moves to the block's heading (or the list's) instead of
// falling to the page.

import { A } from '@solidjs/router';
import { Match, Show, Switch, createComputed, createMemo, createSignal, onCleanup } from 'solid-js';
import { createStore, reconcile } from 'solid-js/store';
import { errorMessage, listRepos, resetAFK, retryClone, type Repo } from '../api';
import Banner from '../components/Banner';
import EmptyState from '../components/EmptyState';
import Icon from '../components/Icon';
import RequireAuth from '../components/RequireAuth';
import { createToast } from '../components/Toast';
import { useEvents } from '../events';
import { createLiveInstances } from '../lib/liveInstances';
import { rescueFocus } from '../lib/focus';
import { createLiveResource } from '../lib/liveResource';
import { createMediaQuery } from '../lib/media';
import {
  filterRepos,
  isFilterActive,
  needsYou,
  orderRepos,
  runCounts,
  summaryRefreshSpecs,
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
  const [repoList, { refetch }] = createLiveResource(
    () => listRepos(),
    [{ type: 'repo.changed' }, ...summaryRefreshSpecs()],
  );
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
  let needsYouHeading: HTMLHeadingElement | undefined;
  let listHeading: HTMLHeadingElement | undefined;

  // One store object per repo id, patched in place by each refetch (see the
  // header). `loaded` stays true after a failed refetch: the list it had
  // stays up under the error banner.
  const [repos, setRepos] = createStore<Repo[]>([]);
  const [loaded, setLoaded] = createSignal(false);
  createComputed(() => {
    const next = resourceValue(repoList);
    if (next === undefined) return;
    setRepos(reconcile(next, { key: 'id' }));
    setLoaded(true);
  });

  const ordered = createMemo(() => orderRepos(repos));
  const filtering = () => isFilterActive(query());
  // One row object per repo, kept for as long as the repo's store object is:
  // a refetch or a reorder reuses it, so its row (and the focus in it)
  // survives. Counts and clone progress are getters read by the row's own
  // JSX, so a run state change or a progress tick updates that text in place.
  const rowData = (repo: Repo): RepoRowData => ({
    repo,
    get counts() {
      return runCounts(resourceValue(instances) ?? [], repo.id);
    },
    get progress() {
      return progress.progress(repo.id);
    },
  });
  const allRows = createMemo<RepoRowData[]>((previous) => {
    const kept = new Map(previous.map((row) => [row.repo.id, row]));
    return ordered().map((repo) => {
      const row = kept.get(repo.id);
      return row !== undefined && row.repo === repo ? row : rowData(repo);
    });
  }, []);
  const rows = createMemo(() => {
    const shown = new Set(filterRepos(ordered(), query()).map((repo) => repo.id));
    return allRows().filter((row) => shown.has(row.repo.id));
  });

  // Needs you entries, one store object per problem key.
  const [problems, setProblems] = createStore<NeedsYouEntry[]>([]);
  createComputed(() => setProblems(reconcile(needsYou(ordered()), { key: 'key' })));

  // The problems whose Retry/Reset is in flight, by entry key: held here, not
  // in the entry's row, so it outlives any re-render, and checked before
  // sending, so a second click while one is pending sends nothing.
  const [busy, setBusy] = createSignal<ReadonlySet<string>>(new Set());
  const setKeyBusy = (key: string, on: boolean) =>
    setBusy((prev) => {
      const next = new Set(prev);
      if (on) next.add(key);
      else next.delete(key);
      return next;
    });

  const act = async (
    entry: NeedsYouEntry,
    send: (repoID: string) => Promise<unknown>,
    done: (name: string) => string,
  ) => {
    const key = entry.key;
    if (busy().has(key)) return;
    // Captured before any await: the entry may be gone (or reconciled) after.
    const repoID = entry.repo.id;
    const name = entry.repo.name;
    setKeyBusy(key, true);
    setError(null);
    try {
      await send(repoID);
      toast.show(done(name));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      try {
        await refetch();
      } catch {
        // The list shows its own load error.
      }
      setKeyBusy(key, false);
      // The fixed entry left the block with the button that had focus.
      rescueFocus(needsYouHeading, listHeading);
    }
  };

  const retry = (entry: NeedsYouEntry) =>
    act(
      entry,
      (repoID) => {
        progress.clear(repoID); // stale percent from the failed attempt
        return retryClone(repoID);
      },
      (name) => `Retrying the clone of ${name}`,
    );

  const reset = (entry: NeedsYouEntry) =>
    act(entry, resetAFK, (name) => `AFK runs resumed for ${name}`);

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
      <Show when={repoList.error !== undefined}>
        <Banner message={errorMessage(repoList.error)} />
      </Show>
      <Switch>
        <Match when={loaded() && repos.length === 0}>
          <EmptyState>
            No repositories yet — <A href="/repos/new">add one</A> to get started.
          </EmptyState>
        </Match>
        <Match when={loaded()}>
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

          <Show when={!filtering() && problems.length > 0}>
            <NeedsYou
              entries={problems}
              busy={(entry) => busy().has(entry.key)}
              onRetry={retry}
              onReset={reset}
              headingRef={(el) => (needsYouHeading = el)}
            />
          </Show>

          <section class="repos-all" aria-labelledby="repos-list-heading">
            <div class="repos-eyebrow-row">
              <h2 class="repos-eyebrow" id="repos-list-heading" tabIndex={-1} ref={listHeading}>
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
