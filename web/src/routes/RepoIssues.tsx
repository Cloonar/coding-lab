// Issues view for one repo (/repos/:id/issues): the ready-for-agent queue on
// top, a server-side state filter (open/closed/all), client-side label chips
// narrowing the fetched page, and phone-first issue cards (number, title,
// state badge, label chips, comment count). Builtin repos get the New issue
// and Labels entry points; forge repos read fine but are managed on the
// forge. issue.changed (scoped to this repo) refetches everything.

import { A, useParams, useSearchParams } from '@solidjs/router';
import { For, Match, Show, Switch, createResource } from 'solid-js';
import {
  errorMessage,
  getRepo,
  listIssues,
  listLabels,
  listReadyIssues,
  type IssueStateFilter,
  type IssueSummary,
} from '../api';
import Crumbs, { type Crumb } from '../components/Crumbs';
import EmptyState from '../components/EmptyState';
import LabelChip from '../components/LabelChip';
import RequireAuth from '../components/RequireAuth';
import {
  availableLabelNames,
  canMutateTracker,
  filterByLabel,
  formatDateTime,
  hasReadyLabel,
} from '../lib/issues';
import { labelChipStyle } from '../lib/labels';
import { createLiveResource } from '../lib/liveResource';
import { forgeWebUrl } from '../lib/repoName';
import { resourceValue } from '../lib/resource';

const STATE_FILTERS: IssueStateFilter[] = ['open', 'closed', 'all'];

export default function RepoIssues() {
  return (
    <RequireAuth>
      <RepoIssuesView />
    </RequireAuth>
  );
}

function RepoIssuesView() {
  const params = useParams<{ id: string }>();
  const [query, setQuery] = useSearchParams<{ state?: string; label?: string }>();

  const state = (): IssueStateFilter =>
    query.state === 'closed' || query.state === 'all' ? query.state : 'open';
  const labelFilter = (): string | null =>
    typeof query.label === 'string' && query.label !== '' ? query.label : null;

  const [repo] = createResource(
    () => params.id,
    (id) => getRepo(id),
  );
  // String key: a search-param change that leaves the state untouched (label
  // narrowing) yields the same key, so the page is NOT refetched — the label
  // chips filter the already-fetched page client-side.
  const [page] = createLiveResource(
    () => `${params.id}\n${state()}`,
    (key) => {
      const sep = key.indexOf('\n');
      return listIssues(key.slice(0, sep), key.slice(sep + 1) as IssueStateFilter);
    },
    [{ type: 'issue.changed', match: (event) => event.repoID === params.id }],
  );
  const [ready] = createLiveResource(
    () => params.id,
    (id) => listReadyIssues(id),
    [{ type: 'issue.changed', match: (event) => event.repoID === params.id }],
  );
  // Every read outside the guarded <Match> branches goes through these
  // non-throwing accessors: a failed listIssues must render the error banner
  // (not a silently blank page), and a failed /ready must not take the issue
  // cards down with it.
  const repoData = () => resourceValue(repo);
  const pageData = () => resourceValue(page);
  const readyData = () => resourceValue(ready);
  // Label colors exist only on the builtin tracker; the resource stays idle
  // (and GET /labels is never called) for a forge-bound repo.
  const [labels] = createLiveResource(
    () => (pageData()?.binding === 'builtin' ? params.id : null),
    (id) => listLabels(id),
    [
      {
        type: 'issue.changed',
        match: (event) => event.repoID === params.id && pageData()?.binding === 'builtin',
      },
    ],
  );
  const labelList = () => resourceValue(labels);

  const canMutate = () => {
    const binding = pageData()?.binding;
    return binding !== undefined && canMutateTracker(binding);
  };
  const colorOf = (name: string): string | undefined =>
    labelList()?.find((label) => label.name === name)?.color;

  const issues = () => pageData()?.issues ?? [];
  const visible = () => filterByLabel(issues(), labelFilter());
  const chipNames = () => {
    const active = labelFilter();
    return availableLabelNames(issues(), active === null ? [] : [active]);
  };

  const chipStyle = (name: string) => {
    const color = colorOf(name);
    if (color === undefined) return undefined;
    const style = labelChipStyle(color);
    return { background: style.background, color: style.color, 'border-color': 'transparent' };
  };

  const emptyText = () => {
    const base = state() === 'all' ? 'No issues' : `No ${state()} issues`;
    const active = labelFilter();
    return active === null ? `${base}.` : `${base} labeled "${active}".`;
  };

  const crumbs = (): Crumb[] => [
    { label: 'Repos', href: '/repos' },
    { label: repoData()?.name ?? 'Repository', href: `/repos/${params.id}/issues` },
    { label: 'Issues' },
  ];

  // Only forge-bound repos show the note, and only when the remote parses into
  // a web URL: otherwise the sentence stays plain text (no dead link).
  const forgeIssuesUrl = (): string | null => {
    const r = repoData();
    if (r === undefined) return null;
    const base = forgeWebUrl(r.remote_url, r.forge_kind);
    return base === null ? null : `${base}/issues`;
  };

  return (
    <main class="page">
      <Crumbs segments={crumbs()} />
      <div class="section-head">
        <h2>{repoData()?.name ?? 'Repository'} · Issues</h2>
        <Show when={canMutate()}>
          <div class="head-actions">
            <A href={`/repos/${params.id}/labels`} class="card-link">
              Labels
            </A>
            <A href={`/repos/${params.id}/issues/new`} class="card-link">
              + New issue
            </A>
          </div>
        </Show>
      </div>
      <Show when={pageData() !== undefined && !canMutate()}>
        <p class="muted forge-note">
          <Show
            when={forgeIssuesUrl()}
            fallback="Managed on the forge — lab shows a read-only view."
          >
            {(href) => (
              <a href={href()} target="_blank" rel="noreferrer">
                Managed on the forge — lab shows a read-only view.
              </a>
            )}
          </Show>
        </p>
      </Show>

      <Show when={(readyData()?.length ?? 0) > 0}>
        <section class="card ready-queue">
          <h2>Ready for agent ({readyData()!.length})</h2>
          <ul class="ready-list">
            <For each={readyData()}>
              {(issue) => (
                <li>
                  <A href={`/repos/${params.id}/issues/${issue.number}`} class="ready-row">
                    <span class="mono muted issue-number">#{issue.number}</span>
                    <span class="ready-title">{issue.title}</span>
                  </A>
                </li>
              )}
            </For>
          </ul>
        </section>
      </Show>

      <div class="filter-row" role="group" aria-label="Filter by state">
        <For each={STATE_FILTERS}>
          {(option) => (
            <button
              type="button"
              classList={{ seg: true, active: state() === option }}
              onClick={() => setQuery({ state: option === 'open' ? undefined : option })}
            >
              {option}
            </button>
          )}
        </For>
      </div>
      <Show when={chipNames().length > 0}>
        <div class="chip-row label-filter" role="group" aria-label="Filter by label">
          <For each={chipNames()}>
            {(name) => (
              <button
                type="button"
                classList={{ 'chip-toggle': true, on: labelFilter() === name }}
                style={chipStyle(name)}
                aria-pressed={labelFilter() === name}
                onClick={() => setQuery({ label: labelFilter() === name ? undefined : name })}
              >
                {name}
              </button>
            )}
          </For>
        </div>
      </Show>

      <Switch>
        <Match when={page.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(page.error)}</span>
          </div>
        </Match>
        <Match when={page() !== undefined && visible().length === 0}>
          <EmptyState>
            {emptyText()}
            <Show when={canMutate()}>
              {' '}
              <A href={`/repos/${params.id}/issues/new`}>New issue</A>
            </Show>
          </EmptyState>
        </Match>
        <Match when={page()}>
          <div class="card-list">
            <For each={visible()}>
              {(issue) => <IssueCard repoID={params.id} issue={issue} colorOf={colorOf} />}
            </For>
          </div>
        </Match>
      </Switch>
    </main>
  );
}

function IssueCard(props: {
  repoID: string;
  issue: IssueSummary;
  colorOf: (name: string) => string | undefined;
}) {
  return (
    <A
      href={`/repos/${props.repoID}/issues/${props.issue.number}`}
      classList={{
        card: true,
        'issue-card': true,
        ready: props.issue.state === 'open' && hasReadyLabel(props.issue.labels),
      }}
    >
      <div class="card-head">
        <span class="mono muted issue-number">#{props.issue.number}</span>
        <span class="card-title">{props.issue.title}</span>
        <span class="spacer" />
        <span class={`chip state-${props.issue.state}`}>{props.issue.state}</span>
      </div>
      <div class="chip-row">
        <For each={props.issue.labels}>
          {(name) => <LabelChip name={name} color={props.colorOf(name)} />}
        </For>
        <span class="muted issue-meta">
          {props.issue.comments_count} comment{props.issue.comments_count === 1 ? '' : 's'} ·{' '}
          {formatDateTime(props.issue.updated_at)}
        </span>
      </div>
    </A>
  );
}
