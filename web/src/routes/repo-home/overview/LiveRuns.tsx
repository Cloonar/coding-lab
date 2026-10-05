// The Live runs block of the repo home's Overview (issue #61): this repo's
// live instances, titled the way the runs rail titles them, each with its
// conversational state in words (Working / Waiting for you / Idle; an AFK run
// adds the budget it has left) and each a link to its Chat. Waiting runs come
// first, as in the rail. Stop all lives here and asks in place before it acts;
// the toast says how many instances were stopped.

import { A } from '@solidjs/router';
import { For, Show, createSignal, createUniqueId, onCleanup } from 'solid-js';
import { errorMessage, stopAll, type ConversationState, type Instance } from '../../../api';
import Banner from '../../../components/Banner';
import Icon from '../../../components/Icon';
import InlineConfirm from '../../../components/InlineConfirm';
import { budgetRemaining, parseAFKLabel } from '../../../lib/afk';
import { stateBadge } from '../../../lib/conversation';
import { runDisplayTitle, sessionLabel } from '../../../lib/instanceLabel';
import { plural } from '../../../lib/readiness';
import { orderRail } from '../../../lib/railOrder';

/** A live run's conversational state, in words. */
export function liveStateWords(state: ConversationState): string {
  switch (stateBadge(state)?.cls) {
    case 'working':
      return 'Working';
    case 'needs-input':
    case 'question':
      return 'Waiting for you';
    default:
      return 'Idle';
  }
}

export default function LiveRuns(props: {
  repoID: string;
  repoName: string;
  /** Every instance lab knows (the caller's live list); undefined while loading. */
  instances: Instance[] | undefined;
  onStopped: () => void;
  notify: (message: string) => void;
}) {
  const live = () =>
    orderRail(
      (props.instances ?? []).filter(
        (instance) => instance.live && instance.repo_id === props.repoID,
      ),
    );

  // The budget countdown ticks without a refetch.
  const [now, setNow] = createSignal(Date.now());
  const ticker = setInterval(() => setNow(Date.now()), 30_000);
  onCleanup(() => clearInterval(ticker));

  const [error, setError] = createSignal<string | null>(null);
  const stop = async () => {
    setError(null);
    try {
      const res = await stopAll(props.repoID);
      props.notify(
        res.stopped === 0
          ? 'No runs were live.'
          : `Stopped ${plural(res.stopped, 'run')} in ${props.repoName}.`,
      );
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      props.onStopped();
    }
  };

  const headingId = `live-runs-${createUniqueId()}`;

  return (
    <section class="overview-card live-runs" aria-labelledby={headingId}>
      <div class="overview-card-head">
        <h2 id={headingId}>Live runs</h2>
        <span class="spacer" />
        <Show when={live().length > 0}>
          <InlineConfirm
            label={`Stop all (${live().length})`}
            confirmLabel={`Stop ${plural(live().length, 'run')}`}
            busyLabel="Stopping…"
            class="danger live-runs-stop"
            onConfirm={stop}
          />
        </Show>
      </div>
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Show when={live().length > 0} fallback={<p class="muted overview-empty">No live runs.</p>}>
        <ul class="live-runs-list">
          <For each={live()}>{(instance) => <LiveRunRow instance={instance} now={now()} />}</For>
        </ul>
      </Show>
    </section>
  );
}

function LiveRunRow(props: { instance: Instance; now: number }) {
  const badge = () => stateBadge(props.instance.state);
  // AFK runs carry their budget countdown, as in the rail.
  const budget = () =>
    parseAFKLabel(sessionLabel(props.instance.session_name)) === null
      ? null
      : budgetRemaining(props.instance.budget_deadline, props.now);
  const status = () => {
    const words = liveStateWords(props.instance.state);
    const b = budget();
    return b === null ? words : `${words} · ${b}`;
  };
  return (
    <li>
      <A href={`/runs/${props.instance.id}`} class="live-run">
        <span class={`run-dot ${badge()?.cls ?? 'idle'}`} aria-hidden="true" />
        <span class="live-run-text">
          <span class="live-run-title">{runDisplayTitle(props.instance)}</span>
          <span class="live-run-state">{status()}</span>
        </span>
        <Icon name="chevron-right" size={18} class="live-run-chevron" />
      </A>
    </li>
  );
}
