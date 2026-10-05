// Change requests for one repo (/repos/:id/crs): the lab-internal PRs of the
// built-in tracker. Server-side state filter (open/merged/closed/all) and
// phone-first CR cards — number, title, state chip, head → base branches,
// closes chips linking to the issues the merge will close. cr.changed
// (scoped to this repo) refetches the page. It is the repo home's CRs tab
// (issue #61, builtin-bound repos): the frame owns the page and the repo
// heading, so this tab root renders no crumb.

import { A, useParams, useSearchParams } from '@solidjs/router';
import { For, Match, Switch } from 'solid-js';
import { errorMessage, listCRs, type CRStateFilter, type CRSummary } from '../api';
import Banner from '../components/Banner';
import ClosesChips from '../components/ClosesChips';
import EmptyState from '../components/EmptyState';
import SectionHead from '../components/SectionHead';
import { formatDateTime } from '../lib/issues';
import { createLiveResource } from '../lib/liveResource';

const STATE_FILTERS: CRStateFilter[] = ['open', 'merged', 'closed', 'all'];

export default function RepoCRs() {
  const params = useParams<{ id: string }>();
  const [query, setQuery] = useSearchParams<{ state?: string }>();

  const state = (): CRStateFilter =>
    query.state === 'merged' || query.state === 'closed' || query.state === 'all'
      ? query.state
      : 'open';

  const [page] = createLiveResource(
    () => `${params.id}\n${state()}`,
    (key) => {
      const sep = key.indexOf('\n');
      return listCRs(key.slice(0, sep), key.slice(sep + 1) as CRStateFilter);
    },
    [{ type: 'cr.changed', match: (event) => event.repoID === params.id }],
  );

  const emptyText = () =>
    state() === 'all' ? 'No change requests.' : `No ${state()} change requests.`;

  return (
    <>
      <SectionHead
        title="Change requests"
        action={
          <div class="head-actions">
            <A href={`/repos/${params.id}/issues`} class="card-link">
              Issues
            </A>
          </div>
        }
      />

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

      <Switch>
        <Match when={page.error !== undefined}>
          <Banner message={errorMessage(page.error)} />
        </Match>
        <Match when={page() !== undefined && page()!.length === 0}>
          <EmptyState>{emptyText()}</EmptyState>
        </Match>
        <Match when={page()}>
          <div class="card-list">
            <For each={page()}>{(cr) => <CRCard repoID={params.id} cr={cr} />}</For>
          </div>
        </Match>
      </Switch>
    </>
  );
}

function CRCard(props: { repoID: string; cr: CRSummary }) {
  return (
    <article class="card cr-card">
      <A href={`/repos/${props.repoID}/crs/${props.cr.number}`} class="cr-card-link">
        <div class="card-head">
          <span class="mono muted issue-number">CR #{props.cr.number}</span>
          <span class="card-title">{props.cr.title}</span>
          <span class="spacer" />
          <span class={`chip state-${props.cr.state}`}>{props.cr.state}</span>
        </div>
        <p class="muted card-sub mono cr-branches">
          {props.cr.head_branch} → {props.cr.base_branch}
        </p>
      </A>
      <div class="chip-row">
        <ClosesChips repoID={props.repoID} closes={props.cr.closes} />
        <span class="muted issue-meta">
          {props.cr.state === 'merged' && props.cr.merged_at !== null
            ? `merged ${formatDateTime(props.cr.merged_at)}`
            : `opened ${formatDateTime(props.cr.created_at)}`}
        </span>
      </div>
    </article>
  );
}
