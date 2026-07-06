// Live instance rows under a repo card: v0-style title ('label · 15:30'),
// a subtle connecting pulse until the deep link lands, Open as a deep-link
// anchor (generic claude.ai picker fallback on a missed capture), and Stop
// with the guarded outcome ("removed" vs "parked") surfaced as a toast.

import { For, Show, createSignal } from 'solid-js';
import { errorMessage, stopInstance, type Instance } from '../api';
import { openState } from '../lib/deepLink';
import { instanceTitle, sessionLabel } from '../lib/instanceLabel';

export default function InstanceList(props: {
  instances: Instance[];
  onStopped: (outcome: 'removed' | 'parked') => void;
  onChanged: () => void;
  onError: (message: string) => void;
}) {
  const [stopping, setStopping] = createSignal<string | null>(null);

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
        {(instance) => <InstanceRow instance={instance} stopping={stopping()} onStop={stop} />}
      </For>
    </ul>
  );
}

function InstanceRow(props: {
  instance: Instance;
  stopping: string | null;
  onStop: (instance: Instance) => Promise<void>;
}) {
  const state = () => openState(props.instance);
  const linkUrl = () => {
    const s = state();
    return s.kind === 'link' ? s.url : '';
  };
  const linkTitle = () => {
    const s = state();
    return s.kind === 'link' && !s.exact ? s.title : undefined;
  };
  const isStopping = () => props.stopping === props.instance.session_name;

  return (
    <li class="instance-row">
      <div class="instance-main">
        <span class="instance-title">
          {instanceTitle(sessionLabel(props.instance.session_name))}
        </span>
        <span class="muted mono instance-branch">{props.instance.branch}</span>
      </div>
      <Show when={!props.instance.live}>
        <span class="chip idle">not running</span>
      </Show>
      <Show
        when={state().kind === 'link'}
        fallback={
          <span class="chip connecting pulse" title="Waiting for the deep link">
            connecting…
          </span>
        }
      >
        <a href={linkUrl()} target="_blank" rel="noreferrer" class="card-link" title={linkTitle()}>
          Open ↗
        </a>
      </Show>
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
