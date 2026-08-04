// Run history (/history, per-repo filter via ?repo=, outcome filter via
// ?outcome=): phone-first cards with kind, label/branch, model/effort,
// started time, duration and the outcome badge (active/success/death/
// timeout/stopped/escalated) plus failure_reason. AFK kinds title as 'AFK #N' from the
// run's issue number. Newest first, straight from GET /runs; the outcome
// filter narrows client-side; run.changed refetches. (Formerly /runs; /runs
// now redirects here, and /runs/:id stays the chat.)
//
// An escalated autoland run (kind lander/fix/escalate) carrying a
// pull_number also states the consequence — which PR autoland is ignoring —
// and a Re-arm action (issue #188) that POSTs
// .../autoland/pulls/{pull}/rearm and refetches the page on success. Before
// this, the suppression was invisible: a bare 'escalated' chip with no way
// back in.

import { A, useSearchParams } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal } from 'solid-js';
import {
  errorMessage,
  listRuns,
  listRepos,
  rearmPull,
  type Run,
  type RunKind,
  type RunOutcome,
} from '../api';
import EmptyState from '../components/EmptyState';
import ErrorBanner from '../components/ErrorBanner';
import RequireAuth from '../components/RequireAuth';
import { instanceTitle, sessionLabel } from '../lib/instanceLabel';
import { createLiveResource } from '../lib/liveResource';

const RUNS_LIMIT = 50;

const KIND_LABELS: Record<RunKind, string> = {
  manual: 'manual',
  afk_manual: 'AFK',
  afk_auto: 'AFK auto',
  lander: 'Lander',
  fix: 'Fix',
  escalate: 'Escalate',
  scheduled: 'Scheduled',
};

const OUTCOMES: RunOutcome[] = ['active', 'success', 'death', 'timeout', 'stopped', 'escalated'];

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

  // The outcome filter narrows the already-fetched page client-side.
  const visible = () => {
    const all = runs() ?? [];
    const outcome = outcomeFilter();
    return outcome === '' ? all : all.filter((run) => run.outcome === outcome);
  };

  return (
    <main class="page">
      <div class="section-head">
        <h2>History</h2>
        <label class="field runs-filter">
          <select
            name="outcome-filter"
            value={outcomeFilter()}
            onInput={(e) => setParams({ outcome: e.currentTarget.value || undefined })}
            aria-label="Filter by outcome"
          >
            <option value="">All outcomes</option>
            <For each={OUTCOMES}>{(outcome) => <option value={outcome}>{outcome}</option>}</For>
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
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(runs.error)}</span>
          </div>
        </Match>
        <Match when={runs()?.length === 0}>
          <EmptyState>No runs yet.</EmptyState>
        </Match>
        <Match when={runs() !== undefined && visible().length === 0}>
          <EmptyState>No {outcomeFilter()} runs.</EmptyState>
        </Match>
        <Match when={runs()}>
          <div class="card-list">
            <For each={visible()}>
              {(run) => <RunCard run={run} onRearmed={() => void refetch()} />}
            </For>
          </div>
        </Match>
      </Switch>
    </main>
  );
}

function RunCard(props: { run: Run; onRearmed: () => void }) {
  const title = () => {
    // A user-set title wins over everything (issue #111 rename overlay).
    const custom = props.run.title?.trim() ?? '';
    if (custom !== '') return custom;
    // AFK runs title from the persisted issue number (restart-proof); the
    // session label is the fallback for both AFK and manual runs. A scheduled
    // run (issue #247) is anchored to no issue, so issue_number is null and it
    // falls through to the label path below, which renders its
    // 'sched-<id>-<stamp>' label as '<schedule> · <firing time>'.
    if (props.run.kind !== 'manual' && props.run.issue_number !== null) {
      return `AFK #${props.run.issue_number}`;
    }
    const label = instanceTitle(sessionLabel(props.run.session_name));
    return label === '' ? props.run.branch : label;
  };

  // Escalation is terminal history on the run row itself (never rewritten —
  // see internal/httpapi/autoland.go), so a re-arm never changes this run's
  // own fields; `busy`/`rearmError` are local to the card, same shape as
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

  return (
    <article class="card run-card">
      <div class="card-head">
        <span class="card-title">{title()}</span>
        <span class="spacer" />
        <span class={`chip outcome-${props.run.outcome}`}>{props.run.outcome}</span>
      </div>
      <p class="muted card-sub mono">{props.run.branch}</p>
      <div class="chip-row">
        <span class="chip">{KIND_LABELS[props.run.kind] ?? props.run.kind}</span>
        <span class="chip mono">
          {props.run.model} · {props.run.effort}
        </span>
        <span class="muted run-times">
          {formatStarted(props.run.started_at)} · {formatDuration(props.run)}
        </span>
      </div>
      <Show when={props.run.failure_reason}>
        <p class="run-failure">{props.run.failure_reason}</p>
      </Show>
      {/* The chip alone only names the outcome; this states the consequence
          (which PR is suppressed) and the way back in (issue #188). */}
      <Show when={props.run.outcome === 'escalated' && props.run.pull_number !== null}>
        <div class="run-escalated">
          <p class="run-escalated-note">
            Autoland is ignoring PR #{props.run.pull_number} until it is re-armed.
          </p>
          <ErrorBanner message={rearmError()} onDismiss={() => setRearmError(null)} />
          <div class="card-actions">
            <button type="button" class="run-rearm" onClick={() => void rearm()} disabled={busy()}>
              {busy() ? 'Re-arming…' : 'Re-arm'}
            </button>
          </div>
        </div>
      </Show>
      <A href={`/runs/${props.run.id}`} class="card-link run-open" title="Open the chat">
        Open chat →
      </A>
    </article>
  );
}

function formatStarted(startedAt: string): string {
  const t = new Date(startedAt);
  if (Number.isNaN(t.getTime())) return startedAt;
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}`;
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
