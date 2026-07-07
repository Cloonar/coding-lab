// Live instance rows under a repo card: v0-style title ('label · 15:30',
// AFK runs as 'AFK #N'), a subtle connecting pulse until the deep link
// lands, the open affordance (exact deep link, the provider's generic web
// fallback, or a copyable tmux-attach for a link-less provider — ADR-0017),
// and Stop with the guarded outcome ("removed" vs "parked") surfaced as a
// toast. AFK rows add the auto chip and the budget countdown derived from
// runs.budget_deadline (30s display tick).

import { A } from '@solidjs/router';
import { For, Show, createSignal, onCleanup } from 'solid-js';
import { errorMessage, stopInstance, type Instance, type Provider } from '../api';
import { budgetRemaining, parseAFKLabel } from '../lib/afk';
import { stateBadge } from '../lib/conversation';
import { openState, providerOpen } from '../lib/deepLink';
import { instanceTitle, sessionLabel } from '../lib/instanceLabel';
import OpenAffordance from './OpenAffordance';

export default function InstanceList(props: {
  instances: Instance[];
  providers: Provider[] | undefined;
  onStopped: (outcome: 'removed' | 'parked') => void;
  onChanged: () => void;
  onError: (message: string) => void;
}) {
  const [stopping, setStopping] = createSignal<string | null>(null);

  // Display-only countdown tick for AFK budget hints.
  const [now, setNow] = createSignal(Date.now());
  const ticker = setInterval(() => setNow(Date.now()), 30_000);
  onCleanup(() => clearInterval(ticker));

  const stop = async (instance: Instance) => {
    setStopping(instance.session_name);
    try {
      const res = await stopInstance(instance.session_name);
      props.onStopped(res.outcome);
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setStopping(null);
      props.onChanged();
    }
  };

  return (
    <ul class="instance-list">
      <For each={props.instances}>
        {(instance) => (
          <InstanceRow
            instance={instance}
            providers={props.providers}
            stopping={stopping()}
            now={now()}
            onStop={stop}
          />
        )}
      </For>
    </ul>
  );
}

function InstanceRow(props: {
  instance: Instance;
  providers: Provider[] | undefined;
  stopping: string | null;
  now: number;
  onStop: (instance: Instance) => Promise<void>;
}) {
  const state = () =>
    openState(props.instance, providerOpen(props.providers, props.instance.provider));
  const isStopping = () => props.stopping === props.instance.session_name;
  const afk = () => parseAFKLabel(sessionLabel(props.instance.session_name));
  const budget = () =>
    afk() === null ? null : budgetRemaining(props.instance.budget_deadline, props.now);
  const badge = () => stateBadge(props.instance.state);

  return (
    <li classList={{ 'instance-row': true, afk: afk() !== null }}>
      <div class="instance-main">
        <span class="instance-title">
          {instanceTitle(sessionLabel(props.instance.session_name))}
        </span>
        <span class="muted mono instance-branch">{props.instance.branch}</span>
        <Show when={budget()}>
          <span
            classList={{ muted: true, 'budget-left': true, over: budget() === 'over budget' }}
            title="Time left on this AFK run's budget"
          >
            {budget()}
          </span>
        </Show>
      </div>
      <Show when={afk()?.auto}>
        <span class="chip afk-kind">auto</span>
      </Show>
      <Show when={!props.instance.live}>
        <span class="chip idle">not running</span>
      </Show>
      <Show when={badge()}>
        {(b) => (
          <span classList={{ chip: true, convo: true, [b().cls]: true }} title={b().title}>
            {b().label}
          </span>
        )}
      </Show>
      <A href={`/runs/${props.instance.id}`} class="card-link" title="Open the chat">
        Chat
      </A>
      <OpenAffordance state={state()} />
      <button
        type="button"
        class="danger instance-stop"
        onClick={() => void props.onStop(props.instance)}
        disabled={isStopping()}
      >
        {isStopping() ? 'Stopping…' : 'Stop'}
      </button>
    </li>
  );
}
