// The readiness block (issue #61), the first block of the repo home's
// Overview: does a run start in this repo right now? The report is the
// server's (GET /repos/{id}/readiness, read live: this repo's repo.changed,
// provider.auth.changed, run.changed and every reconnect refetch it); until it
// arrives, and if it cannot be read, the repo summary's copy of the same
// report stands in.
//
// All checks passing → one line, "Ready to run", collapsed and expandable.
// Any failing or pending check → open, failing first, then pending, then
// passing. A failing check shows its one-sentence detail and ONE action:
// Retry clone (`action: 'retry_clone'`) or a button that opens the field that
// fixes it (`fix`). While the clone runs, the clone check shows the live
// progress from the clone progress store, and the head says runs can start
// when the clone has finished. A check absent from the report is not listed.
// Each check's state is an icon plus words for assistive tech, never colour
// alone.

import { useNavigate } from '@solidjs/router';
import { For, Show, createEffect, createMemo, createSignal, createUniqueId, on } from 'solid-js';
import {
  errorMessage,
  getRepoReadiness,
  retryClone,
  type Readiness,
  type ReadinessCheck,
  type ReadinessState,
  type Repo,
} from '../../../api';
import Banner from '../../../components/Banner';
import Icon, { type IconName } from '../../../components/Icon';
import { createLiveResource } from '../../../lib/liveResource';
import {
  checkTitle,
  fixHref,
  fixLabel,
  orderChecks,
  readinessHeadline,
} from '../../../lib/readiness';
import { resourceValue } from '../../../lib/resource';
import type { CloneProgress } from '../../../stores/cloneProgress';

const STATE_ICON: Record<ReadinessState, IconName> = {
  passing: 'check',
  failing: 'exclamation',
  pending: 'clock',
};

const STATE_WORD: Record<ReadinessState, string> = {
  passing: 'Passing',
  failing: 'Failing',
  pending: 'Pending',
};

export default function ReadinessBlock(props: {
  repo: Repo;
  /** Live clone progress for this repo, or null before any event. */
  progress: CloneProgress | null;
  /** The clone was restarted: drop the stale progress. */
  onCloneRetried: () => void;
  /** The repo row changed (retry): refetch it. */
  onRepoChanged: () => unknown;
  notify: (message: string) => void;
}) {
  const navigate = useNavigate();
  const [report, { refetch }] = createLiveResource(
    () => props.repo.id,
    (id) => getRepoReadiness(id),
    [
      { type: 'repo.changed', match: (event) => event.repoID === props.repo.id },
      { type: 'provider.auth.changed' },
      { type: 'run.changed' },
    ],
  );

  const readiness = (): Readiness | undefined =>
    resourceValue(report) ?? props.repo.summary?.readiness;
  const cloning = () => props.repo.clone_status === 'cloning';
  const headline = () => {
    const r = readiness();
    return r === undefined ? undefined : readinessHeadline(r, cloning());
  };
  const checks = () => orderChecks(readiness()?.checks ?? []);

  // Collapsed when everything passes, open otherwise — re-decided whenever
  // the roll-up changes, so a newly failing check opens the block.
  // A memo, so a refetch that leaves the roll-up as it was keeps the
  // operator's own open/closed choice.
  const [open, setOpen] = createSignal(false);
  const rollUp = createMemo(() => headline()?.state);
  createEffect(on(rollUp, (state) => setOpen(state !== undefined && state !== 'passing')));

  const [error, setError] = createSignal<string | null>(null);
  const [retrying, setRetrying] = createSignal(false);
  const retry = async () => {
    const repo = props.repo;
    setError(null);
    setRetrying(true);
    props.onCloneRetried();
    try {
      await retryClone(repo.id);
      props.notify(`Retrying the clone of ${repo.name}`);
      void refetch();
      await props.onRepoChanged();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setRetrying(false);
    }
  };

  const uid = createUniqueId();
  const listId = `readiness-${uid}-checks`;

  return (
    <Show
      when={headline()}
      fallback={
        <Show when={report.error !== undefined}>
          <Banner message={`Readiness could not be read: ${errorMessage(report.error)}`} />
        </Show>
      }
    >
      {(head) => (
        <section
          class="overview-card readiness"
          classList={{ [head().state]: true, open: open() }}
          aria-label="Readiness"
        >
          <h2 class="readiness-head">
            <button
              type="button"
              class="readiness-toggle"
              aria-expanded={open()}
              aria-controls={listId}
              onClick={() => setOpen(!open())}
            >
              <span class={`readiness-icon big ${head().state}`} aria-hidden="true">
                <Icon name={STATE_ICON[head().state]} size={18} />
              </span>
              <span class="readiness-text">
                <span class="readiness-title">{head().title}</span>
                <span class="readiness-detail">{head().detail}</span>
              </span>
              <Icon name="chevron-right" size={20} class="readiness-chevron" />
            </button>
          </h2>
          <div class="readiness-checks" id={listId} hidden={!open()}>
            <Banner message={error()} onDismiss={() => setError(null)} />
            <Show
              when={checks().length > 0}
              fallback={<p class="muted readiness-none">No checks to report.</p>}
            >
              <ul class="readiness-list">
                <For each={checks()}>
                  {(check) => (
                    <CheckRow
                      check={check}
                      repoID={props.repo.id}
                      progress={check.id === 'clone' && cloning() ? props.progress : null}
                      showProgress={check.id === 'clone' && check.state === 'pending' && cloning()}
                      retrying={retrying()}
                      onRetry={() => void retry()}
                      onFix={(href) => navigate(href)}
                    />
                  )}
                </For>
              </ul>
            </Show>
          </div>
        </section>
      )}
    </Show>
  );
}

function CheckRow(props: {
  check: ReadinessCheck;
  repoID: string;
  progress: CloneProgress | null;
  showProgress: boolean;
  retrying: boolean;
  onRetry: () => void;
  onFix: (href: string) => void;
}) {
  const failing = () => props.check.state === 'failing';
  return (
    <li class={`readiness-check ${props.check.state}`}>
      <span class={`readiness-icon ${props.check.state}`} aria-hidden="true">
        <Icon name={STATE_ICON[props.check.state]} size={14} />
      </span>
      <span class="readiness-text">
        <span class="readiness-check-title">
          <span class="visually-hidden">{STATE_WORD[props.check.state]}: </span>
          {checkTitle(props.check.id)}
        </span>
        <span class="readiness-detail">{props.check.detail}</span>
        <Show when={props.showProgress}>
          <CloneProgressBar progress={props.progress} />
        </Show>
      </span>
      <Show when={failing() && props.check.action === 'retry_clone'}>
        <button
          type="button"
          class="readiness-action"
          disabled={props.retrying}
          onClick={() => props.onRetry()}
        >
          {props.retrying ? 'Retrying…' : 'Retry clone'}
        </button>
      </Show>
      <Show when={failing() && props.check.action === undefined ? props.check.fix : undefined}>
        {(fix) => (
          <button
            type="button"
            class="readiness-action"
            onClick={() => props.onFix(fixHref(props.repoID, fix()))}
          >
            {fixLabel(fix())}
          </button>
        )}
      </Show>
    </li>
  );
}

/** The running clone's live progress: phase and percent, as a progressbar. */
function CloneProgressBar(props: { progress: CloneProgress | null }) {
  const percent = () => props.progress?.percent ?? null;
  const phase = () => {
    const p = props.progress?.phase ?? '';
    return p === '' ? 'Starting' : p.charAt(0).toUpperCase() + p.slice(1);
  };
  return (
    <span class="clone-progress readiness-progress">
      <span class="progress-meta">
        <span class="muted">{phase()}</span>
        <span class="spacer" />
        <Show when={percent() !== null}>
          <span class="muted">{percent()}%</span>
        </Show>
      </span>
      <span
        class="progress-track"
        role="progressbar"
        aria-label="Clone progress"
        aria-valuemin="0"
        aria-valuemax="100"
        aria-valuenow={percent() ?? undefined}
        aria-valuetext={percent() !== null ? `${percent()}%, ${phase()}` : phase()}
      >
        <span
          classList={{ 'progress-fill': true, indeterminate: percent() === null }}
          style={percent() !== null ? { width: `${percent()}%` } : undefined}
        />
      </span>
    </span>
  );
}
