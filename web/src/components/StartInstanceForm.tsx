// Collapsible "Start instance" form on a repo card: optional label (≤32
// chars), model + effort selects from the repo provider's catalog (D14).
// Pre-selection layers per D12d: repo default → global settings default →
// first option. 409s (cap reached / claude logged out / repo not ready)
// surface verbatim in the inline banner.

import { For, Show, createSignal } from 'solid-js';
import { errorMessage, startInstance, type Provider, type Repo, type SpawnDefaults } from '../api';
import { resolveSpawnOption } from '../lib/spawn';
import ErrorBanner from './ErrorBanner';

export default function StartInstanceForm(props: {
  repo: Repo;
  provider: Provider | null;
  defaults: SpawnDefaults;
  onStarted: () => void;
}) {
  const models = () => props.provider?.models ?? [];
  const efforts = () => props.provider?.efforts ?? [];

  const [label, setLabel] = createSignal('');
  // '' = untouched → submit the resolved default. Tracking the operator's
  // pick separately keeps a late settings/providers load from clobbering it.
  const [modelPick, setModelPick] = createSignal('');
  const [effortPick, setEffortPick] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const model = () =>
    modelPick() !== ''
      ? modelPick()
      : resolveSpawnOption(models(), props.repo.model_default, props.defaults.model);
  const effort = () =>
    effortPick() !== ''
      ? effortPick()
      : resolveSpawnOption(efforts(), props.repo.effort_default, props.defaults.effort);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const req: { label?: string; model?: string; effort?: string } = {};
      const trimmed = label().trim();
      if (trimmed !== '') req.label = trimmed;
      if (model() !== '') req.model = model();
      if (effort() !== '') req.effort = effort();
      await startInstance(props.repo.id, req);
      setLabel('');
      props.onStarted();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <details class="start-instance">
      <summary>Start instance</summary>
      <form onSubmit={(e) => void submit(e)}>
        <ErrorBanner message={error()} onDismiss={() => setError(null)} />
        <label class="field">
          <span>Label (optional)</span>
          <input
            name="label"
            maxlength="32"
            value={label()}
            onInput={(e) => setLabel(e.currentTarget.value)}
            placeholder="debug"
            autocomplete="off"
          />
        </label>
        <div class="field-row">
          <label class="field">
            <span>Model</span>
            <select
              name="model"
              value={model()}
              onInput={(e) => setModelPick(e.currentTarget.value)}
            >
              <For each={models()}>
                {(option) => <option value={option.value}>{option.label}</option>}
              </For>
            </select>
          </label>
          <label class="field">
            <span>Effort</span>
            <select
              name="effort"
              value={effort()}
              onInput={(e) => setEffortPick(e.currentTarget.value)}
            >
              <For each={efforts()}>
                {(option) => <option value={option.value}>{option.label}</option>}
              </For>
            </select>
          </label>
        </div>
        <button type="submit" class="primary" disabled={busy() || props.provider === null}>
          {busy() ? 'Starting…' : 'Start'}
        </button>
        <Show when={props.provider === null}>
          <span class="hint-block">Provider catalog unavailable — try reloading.</span>
        </Show>
      </form>
    </details>
  );
}
