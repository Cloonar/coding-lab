// The Runs page at `/` (issue #76) — the launch page at every width, and the
// Live side of the Runs page (History at /history is the Ended side; the head's
// Live / Ended switch links the two). It reads live instances ONLY through the
// shell's instances context (lib/shellInstances): AppShell owns the single
// listInstances resource, patched in place by run.messages.changed, and this
// page never fetches GET /api/v1/instances a second time.
//
// Below 1024px the page is RunList's 'page' variant: 56px rows grouped Needs
// you / Working / Idle. At >=1024px it is a table — run (dot + title),
// repository, state, model · effort, branch, base (commits behind), last — in
// the same groups, one <tbody> per group under a group header row. A table row
// opens the chat: the title cell holds the row's <A> (the keyboard and
// screen-reader path), and a click anywhere else on the row navigates too.
// Rows carry no destructive action; Stop stays in the chat header and on the
// repo's Overview. "Last" is the age since started_at, spaced ("2 min") — the
// API has no last-activity field (see lib/runGroups). Model reads the provider
// catalog's labels ("Large · High", the chat header's runModelLabels) from a
// listProviders() resource — a catalog read, not a second instances fetch.
// The State cell wraps to two lines; every other cell stays one line.

import { A, useNavigate } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import { errorMessage, listProviders, type Instance } from '../api';
import Banner from '../components/Banner';
import EmptyState from '../components/EmptyState';
import RequireAuth from '../components/RequireAuth';
import RunList, { StateDot, afkBudget } from '../components/RunList';
import RunsHead from '../components/RunsHead';
import { runDisplayTitle } from '../lib/instanceLabel';
import { createMediaQuery } from '../lib/media';
import { railGroup } from '../lib/railOrder';
import { groupLive, spacedAge, statePhrase } from '../lib/runGroups';
import { useShellInstances } from '../lib/shellInstances';
import { modelLabelText, runModelLabels } from './runchat/RunDetails';

export default function Runs() {
  return (
    <RequireAuth>
      <RunsView />
    </RequireAuth>
  );
}

function RunsView() {
  const shell = useShellInstances();
  const desktop = createMediaQuery('(min-width: 1024px)');
  const live = () => shell.all().filter((instance) => instance.live);
  const failed = () => shell.error() !== undefined && shell.error() !== null;

  return (
    <main class="page page-wide runs-page">
      <RunsHead />
      <Show when={failed()}>
        <Banner message={errorMessage(shell.error())} />
      </Show>
      <Switch>
        <Match when={!shell.loaded()}>
          <p class="center-note">Loading…</p>
        </Match>
        <Match when={live().length === 0}>
          {/* With a failed load the banner above says why; "No live runs"
              would claim something the page does not know. */}
          <Show when={!failed()}>
            <EmptyState>
              <span class="runs-empty-title">No live runs</span>
              <span class="runs-empty-hint">
                {desktop()
                  ? 'Start one with New run, or let an AFK run pick up a ready issue.'
                  : 'Start one from the New tab, or let an AFK run pick up a ready issue.'}
              </span>
              <A href="/new" class="runs-empty-new">
                New run
              </A>
              {/* The mockup's "Last ended: <title> · <age>" needs ended runs,
                  which the shell's instances list (active runs only) does not
                  hold — so just the way to them. */}
              <span class="runs-empty-ended">
                <A href="/history">Ended runs</A>
              </span>
            </EmptyState>
          </Show>
        </Match>
        <Match when={desktop()}>
          <RunsTable instances={live()} />
        </Match>
        <Match when={true}>
          <RunList instances={live()} variant="page" />
        </Match>
      </Switch>
    </main>
  );
}

function RunsTable(props: { instances: Instance[] }) {
  const navigate = useNavigate();
  // The provider catalog, for the Model column's pretty labels (the chat
  // header's runModelLabels). Not an instances fetch; a failure just leaves
  // the raw ids.
  const [providers] = createResource(() => listProviders().catch(() => []));
  // Display-only tick for the ages and AFK budgets, same cadence as RunList.
  const [now, setNow] = createSignal(Date.now());
  const ticker = setInterval(() => setNow(Date.now()), 30_000);
  onCleanup(() => clearInterval(ticker));

  // A click on the row outside its link navigates like the link; a click on
  // the link itself is the router's, so it is left alone (no double push).
  const openRow = (event: MouseEvent, instance: Instance) => {
    if (event.target instanceof Element && event.target.closest('a') !== null) return;
    navigate(`/runs/${instance.id}`);
  };

  return (
    <div class="runs-table-wrap">
      <table class="runs-table">
        <thead>
          <tr>
            <th scope="col">Run</th>
            <th scope="col">Repository</th>
            <th scope="col">State</th>
            <th scope="col">Model</th>
            <th scope="col">Branch</th>
            <th scope="col">Base</th>
            <th scope="col" class="runs-last">
              Last
            </th>
          </tr>
        </thead>
        <For each={groupLive(props.instances)}>
          {(group) => (
            <tbody>
              <tr class="runs-table-group">
                <th scope="rowgroup" colSpan={7}>
                  {group.label}
                  <span classList={{ 'runlist-count': true, attn: group.key === 'needs-you' }}>
                    {group.instances.length}
                  </span>
                </th>
              </tr>
              <For each={group.instances}>
                {(instance) => {
                  const behind = () => instance.commits_behind ?? 0;
                  const budget = () => afkBudget(instance, now());
                  const model = () => {
                    const labels = runModelLabels(instance, providers());
                    return labels === null ? '—' : modelLabelText(labels);
                  };
                  return (
                    <tr class="runs-table-row" onClick={(event) => openRow(event, instance)}>
                      <td class="runs-table-runcell">
                        <A href={`/runs/${instance.id}`} class="runs-table-run">
                          <StateDot state={instance.state} />
                          <span class="runs-table-title">{runDisplayTitle(instance)}</span>
                        </A>
                      </td>
                      <td class="runs-table-muted">
                        <span class="runs-clamp" title={instance.repo_name}>
                          {instance.repo_name}
                        </span>
                      </td>
                      <td
                        classList={{
                          'runs-table-state': true,
                          attn: railGroup(instance.state) === 0,
                        }}
                      >
                        {statePhrase(instance.state)}
                        <Show when={budget()}>
                          {(b) => (
                            <span
                              classList={{ 'runlist-budget': true, over: b() === 'over budget' }}
                            >
                              {' · '}
                              {b()}
                            </span>
                          )}
                        </Show>
                      </td>
                      <td class="runs-table-muted">
                        <span class="runs-clamp" title={model()}>
                          {model()}
                        </span>
                      </td>
                      <td class="mono runs-table-branch">
                        <span class="runs-clamp" title={instance.branch}>
                          {instance.branch}
                        </span>
                      </td>
                      <td>
                        <Show
                          when={behind() > 0}
                          fallback={<span class="runs-table-muted">—</span>}
                        >
                          <span
                            class="chip runlist-behind"
                            title={`${behind()} commit${behind() === 1 ? '' : 's'} behind the base branch`}
                          >
                            {behind()} behind
                          </span>
                        </Show>
                      </td>
                      <td class="runs-last">{spacedAge(instance.started_at, now())}</td>
                    </tr>
                  );
                }}
              </For>
            </tbody>
          )}
        </For>
      </table>
    </div>
  );
}
