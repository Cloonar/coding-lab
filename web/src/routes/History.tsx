// The Ended side of the Runs page (/history; issue #76 — formerly the History
// page of cards). It keeps its own URL and filters — per repo via ?repo=, per
// outcome via ?outcome= — under the same head as the Live side (RunsHead, with
// Ended selected). Rows take RunList's row shape: title plus the outcome chip
// the run earned, a second line `repo · PR #n or branch · duration`, and the
// age on the right (since ended_at — no last-activity field, see
// lib/runGroups), grouped by the local day the run ended, newest first. Each
// row opens the chat at /runs/:id. Live runs (outcome 'active') are the Live
// side's, so they are excluded here and 'active' is no outcome filter option.
// The outcome chip keeps its `outcome-<outcome>` colour classes; its word
// comes from lib/runGroups (a Run carries no PR state, so no "merged").
//
// Rows come straight from GET /runs (newest first, RUNS_LIMIT per page); the
// outcome filter narrows client-side; run.changed refetches. Runs carry
// repo_id, not a repo name: the name comes from the listRepos() the repo
// filter already loads, falling back to the session name's repo part.
//
// An escalated autoland run (kind lander/fix/escalate) carrying a
// pull_number also states the consequence — which PR autoland is ignoring —
// and a Re-arm action (issue #188) that POSTs
// .../autoland/pulls/{pull}/rearm and refetches the page on success. Both
// render under the row, outside its link, so the button is never inside an
// <a>. A failure_reason renders there too.

import { A, useSearchParams } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import { errorMessage, listRuns, listRepos, rearmPull, type Run, type RunOutcome } from '../api';
import Banner from '../components/Banner';
import EmptyState from '../components/EmptyState';
import RequireAuth from '../components/RequireAuth';
import RunsHead from '../components/RunsHead';
import { sessionRepo } from '../lib/instanceLabel';
import { createLiveResource } from '../lib/liveResource';
import { endedAge, endedRunTitle, groupByDay, outcomeWord } from '../lib/runGroups';

const RUNS_LIMIT = 50;

/** The ended outcomes — 'active' runs are the Live side's. */
const OUTCOMES: RunOutcome[] = ['success', 'death', 'timeout', 'stopped', 'escalated'];

export default function History() {
  return (
    <RequireAuth>
      <HistoryView />
    </RequireAuth>
  );
}

function HistoryView() {
  const [params, setParams] = useSearchParams<{ repo?: string; outcome?: string }>();
  const repoFilter = () => (typeof params.repo === 'string' ? params.repo : '');
  const outcomeFilter = (): RunOutcome | '' => {
    const outcome = params.outcome;
    return typeof outcome === 'string' && (OUTCOMES as string[]).includes(outcome)
      ? (outcome as RunOutcome)
      : '';
  };

  const [repos] = createResource(() => listRepos());
  const [runs, { refetch }] = createLiveResource(
    repoFilter,
    (repo) => listRuns({ repo: repo === '' ? undefined : repo, limit: RUNS_LIMIT }),
    [{ type: 'run.changed' }],
  );

  // Ages and day labels move on without a refetch.
  const [now, setNow] = createSignal(Date.now());
  const ticker = setInterval(() => setNow(Date.now()), 60_000);
  onCleanup(() => clearInterval(ticker));

  const repoName = (run: Run): string =>
    repos()?.find((repo) => repo.id === run.repo_id)?.name ?? sessionRepo(run.session_name);

  const filterWord = () => {
    const outcome = outcomeFilter();
    return outcome === '' ? '' : outcomeWord(outcome);
  };

  const ended = () => (runs() ?? []).filter((run) => run.outcome !== 'active');
  // The outcome filter narrows the already-fetched page client-side.
  const visible = () => {
    const outcome = outcomeFilter();
    return outcome === '' ? ended() : ended().filter((run) => run.outcome === outcome);
  };

  return (
    <main class="page page-wide runs-page">
      <RunsHead />
      <div class="runs-filters">
        <label class="field runs-filter">
          <select
            name="outcome-filter"
            value={outcomeFilter()}
            onInput={(e) => setParams({ outcome: e.currentTarget.value || undefined })}
            aria-label="Filter by outcome"
          >
            <option value="">All outcomes</option>
            <For each={OUTCOMES}>
              {(outcome) => <option value={outcome}>{outcomeWord(outcome)}</option>}
            </For>
          </select>
        </label>
        <label class="field runs-filter">
          <select
            name="repo-filter"
            value={repoFilter()}
            onInput={(e) => setParams({ repo: e.currentTarget.value || undefined })}
            aria-label="Filter by repository"
          >
            <option value="">All repositories</option>
            <For each={repos() ?? []}>{(repo) => <option value={repo.id}>{repo.name}</option>}</For>
          </select>
        </label>
      </div>
      <Switch>
        <Match when={runs.error !== undefined}>
          <Banner message={errorMessage(runs.error)} />
        </Match>
        <Match when={runs() !== undefined && ended().length === 0}>
          <EmptyState>No ended runs yet.</EmptyState>
        </Match>
        {/* Only reachable with an outcome filter set: unfiltered, visible()
            is ended(), which the Match above already caught empty. */}
        <Match when={runs() !== undefined && visible().length === 0}>
          <EmptyState>No {filterWord()} runs.</EmptyState>
        </Match>
        <Match when={runs()}>
          <div class="runlist runlist-page ended-list">
            <For each={groupByDay(visible(), now())}>
              {(day) => (
                <section class="runlist-group">
                  <p class="runlist-label">{day.label}</p>
                  <ul class="runlist-rows">
                    <For each={day.runs}>
                      {(run) => (
                        <EndedRow
                          run={run}
                          repoName={repoName(run)}
                          now={now()}
                          onRearmed={() => void refetch()}
                        />
                      )}
                    </For>
                  </ul>
                </section>
              )}
            </For>
          </div>
        </Match>
      </Switch>
    </main>
  );
}

function EndedRow(props: { run: Run; repoName: string; now: number; onRearmed: () => void }) {
  // Escalation is terminal history on the run row itself (never rewritten —
  // see internal/httpapi/autoland.go), so a re-arm never changes this run's
  // own fields; `busy`/`rearmError` are local to the row, same shape as
  // AFKStrip's reset. onRearmed() still refetches the page: the human's
  // gesture kicks an immediate autoland pass server-side, and the operator
  // should not have to wait for the next run.changed event to see it land.
  const [busy, setBusy] = createSignal(false);
  const [rearmError, setRearmError] = createSignal<string | null>(null);

  const rearm = async () => {
    const repoID = props.run.repo_id;
    const pull = props.run.pull_number;
    if (pull === null) return; // guarded by the Show below; keeps TS honest
    const onRearmed = props.onRearmed;
    setBusy(true);
    setRearmError(null);
    try {
      await rearmPull(repoID, pull);
      onRearmed();
    } catch (err) {
      setRearmError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const title = () => endedRunTitle(props.run);
  const where = () =>
    props.run.pull_number === null ? props.run.branch : `PR #${props.run.pull_number}`;
  const word = () => outcomeWord(props.run.outcome);

  return (
    <li class="ended-run">
      <A
        href={`/runs/${props.run.id}`}
        class="runlist-row"
        aria-label={`${title()} — ${props.repoName} — ${word()}`}
      >
        <span class="runlist-dot" />
        <span class="runlist-body">
          <span class="runlist-top">
            <span class="runlist-title">{title()}</span>
            <span class={`chip outcome-chip outcome-${props.run.outcome}`}>{word()}</span>
          </span>
          <span class="runlist-sub">
            {props.repoName} · {where()} · {formatDuration(props.run)}
          </span>
        </span>
        <span class="runlist-age">{endedAge(props.run, props.now)}</span>
      </A>
      <Show when={props.run.failure_reason}>
        <p class="run-failure ended-run-extra">{props.run.failure_reason}</p>
      </Show>
      {/* The chip alone only names the outcome; this states the consequence
          (which PR is suppressed) and the way back in (issue #188). */}
      <Show when={props.run.outcome === 'escalated' && props.run.pull_number !== null}>
        <div class="run-escalated ended-run-extra">
          <p class="run-escalated-note">
            Autoland is ignoring PR #{props.run.pull_number} until it is re-armed.
          </p>
          <Banner message={rearmError()} onDismiss={() => setRearmError(null)} />
          <div class="card-actions">
            <button type="button" class="run-rearm" onClick={() => void rearm()} disabled={busy()}>
              {busy() ? 'Re-arming…' : 'Re-arm'}
            </button>
          </div>
        </div>
      </Show>
    </li>
  );
}

function formatDuration(run: Run): string {
  const start = new Date(run.started_at).getTime();
  const end = run.ended_at === null ? Date.now() : new Date(run.ended_at).getTime();
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return '—';
  const totalSeconds = Math.floor((end - start) / 1000);
  const suffix = run.ended_at === null ? '…' : '';
  if (totalSeconds < 60) return `${totalSeconds}s${suffix}`;
  const minutes = Math.floor(totalSeconds / 60);
  if (minutes < 60) return `${minutes}m${suffix}`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m${suffix}`;
}
