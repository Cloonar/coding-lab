// Agents section (issue #198): the monolith's Spawn defaults, AFK defaults
// and AFK cards as one per-section form — drafts seeded via
// createSeededDrafts (the AFK options bag rides its optionsKey equality),
// saved as a dirty-fields-only PATCH through useSettingsForm.

import { For, Show } from 'solid-js';
import type { Accessor } from 'solid-js';
import {
  updateRepo,
  type Provider,
  type Repo,
  type RepoPatch,
  type Settings,
} from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';
import Select, { type SelectOption } from '../../../components/Select';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import { createSeededDrafts } from '../../../lib/seededDrafts';
import { providerFor, resolveRemote } from '../../../lib/spawn';
import {
  REMOTE_OPTIONS,
  boolDraft,
  normBool,
  normInt,
  normText,
  onOff,
  optionsKey,
  remoteBlocker,
  toBoolMap,
} from '../shared';

export default function AgentsSection(props: {
  repo: Accessor<Repo>;
  providers: Provider[];
  settings: Settings;
  onSaved: () => void;
}) {
  const drafts = createSeededDrafts(() => props.repo());
  const [provider, setProvider] = drafts.field((r) => r.provider ?? '');
  const [model, setModel] = drafts.field((r) => r.model_default ?? '');
  const [effort, setEffort] = drafts.field((r) => r.effort_default ?? '');
  const [remote, setRemote] = drafts.field((r) => boolDraft(r.remote_default));
  const [afkProvider, setAfkProvider] = drafts.field((r) => r.afk_provider_default ?? '');
  const [afkModel, setAfkModel] = drafts.field((r) => r.afk_model_default ?? '');
  const [afkEffort, setAfkEffort] = drafts.field((r) => r.afk_effort_default ?? '');
  const [afkRemote, setAfkRemote] = drafts.field((r) => boolDraft(r.afk_remote_default));
  const [afkPrompt, setAfkPrompt] = drafts.field((r) => r.afk_prompt ?? '');

  // Effective providers, resolved LIVE against the DRAFT values (skip-layer,
  // ADR-0030) so the model/effort catalogs below re-catalog as the operator
  // flips the agent selects — before anything is saved. Stored values foreign
  // to the new catalog persist untouched and just show "(not in catalog)".
  const providerOptions = (): SelectOption[] =>
    props.providers.map((p) => ({ value: p.id, label: p.display_name }));
  const baseProvider = () =>
    providerFor(props.providers, provider(), props.settings.provider_default);
  const afkEffectiveProvider = () =>
    providerFor(
      props.providers,
      afkProvider(),
      props.settings.spawn_provider_default_afk,
      provider(),
      props.settings.provider_default,
    );

  // What "inherit" currently MEANS for each remote-control select (issue #163),
  // resolved LIVE against the drafts + the global settings — the same skip-layer
  // walk the server does, so the inherit row can name the effective value
  // instead of leaving the operator to guess it.
  //   manual: repo.remote_default → spawn_remote_default → false
  //   AFK:    repo.afk_remote_default → spawn_remote_default_afk
  //             → repo.remote_default → spawn_remote_default → false
  const inheritedRemote = () => resolveRemote(props.settings.spawn_remote_default);
  const inheritedAfkRemote = () =>
    resolveRemote(
      props.settings.spawn_remote_default_afk,
      normBool(remote()),
      props.settings.spawn_remote_default,
    );

  // Per-key checkbox state for the provider's bool options (null bag = inherit,
  // seeded to all-unchecked). Once any checkbox differs from the seed, the full
  // declared bag is PATCHed as the explicit repo override. The bag is an
  // object, so its draft resyncs by VALUE — the optionsKey equality below is
  // what lets an untouched draft follow the server and a dirty one keep the
  // operator's in-flight toggles.
  const [afkOptions, setAfkOptions] = drafts.field(
    (r) => toBoolMap(r.afk_options),
    (a, b) => optionsKey(a) === optionsKey(b),
  );
  const boolOptions = () =>
    (afkEffectiveProvider()?.options ?? []).filter((o) => o.type === 'bool');
  const [afkAuto, setAfkAuto] = drafts.field((r) => r.afk_auto_enabled);
  const [budget, setBudget] = drafts.field((r) =>
    r.budget_minutes === null ? '' : String(r.budget_minutes),
  );
  const [maxInstances, setMaxInstances] = drafts.field((r) =>
    r.max_instances_override === null ? '' : String(r.max_instances_override),
  );

  const buildPatch = (): RepoPatch | string => {
    // Diff against the seed the drafts came from — NOT the live props.repo().
    // Diffing against the live repo would mark a stale draft of a field the
    // operator never touched as "dirty" and PATCH the old value back.
    const current = drafts.seed();
    const patch: RepoPatch = {};

    if (normText(provider()) !== current.provider) patch.provider = normText(provider());
    if (normText(model()) !== current.model_default) patch.model_default = normText(model());
    if (normText(effort()) !== current.effort_default) patch.effort_default = normText(effort());
    // Tri-state (issue #163): null clears back to inherit, false is an explicit
    // off — both are values, so the dirty check is a plain !== against the seed.
    if (normBool(remote()) !== current.remote_default) patch.remote_default = normBool(remote());

    if (normText(afkProvider()) !== current.afk_provider_default) {
      patch.afk_provider_default = normText(afkProvider());
    }
    if (normText(afkModel()) !== current.afk_model_default) {
      patch.afk_model_default = normText(afkModel());
    }
    if (normText(afkEffort()) !== current.afk_effort_default) {
      patch.afk_effort_default = normText(afkEffort());
    }
    if (normBool(afkRemote()) !== current.afk_remote_default) {
      patch.afk_remote_default = normBool(afkRemote());
    }
    if (normText(afkPrompt()) !== current.afk_prompt) patch.afk_prompt = normText(afkPrompt());
    // Send the full declared bag once any bool option differs from its seed.
    const declared = boolOptions();
    if (declared.length > 0) {
      const seeded = toBoolMap(current.afk_options);
      const checked = (key: string) => afkOptions()[key] ?? false;
      if (declared.some((o) => checked(o.key) !== (seeded[o.key] ?? false))) {
        patch.afk_options = Object.fromEntries(
          declared.map((o) => [o.key, checked(o.key) ? 'true' : 'false']),
        );
      }
    }

    if (afkAuto() !== current.afk_auto_enabled) patch.afk_auto_enabled = afkAuto();
    const budgetMinutes = normInt(budget());
    if (budgetMinutes === undefined) return 'Budget must be a whole number of minutes.';
    if (budgetMinutes !== current.budget_minutes) patch.budget_minutes = budgetMinutes;
    const cap = normInt(maxInstances());
    if (cap === undefined) return 'Max instances must be a whole number.';
    if (cap !== current.max_instances_override) patch.max_instances_override = cap;

    return patch;
  };

  const dirty = () => {
    const p = buildPatch();
    return typeof p === 'string' || Object.keys(p).length > 0;
  };

  const form = useSettingsForm<RepoPatch>({
    dirty,
    buildPatch,
    submit: (patch) => updateRepo(props.repo().id, patch),
    onSaved: () => props.onSaved(),
  });

  return (
    <form onSubmit={(e) => void form.save(e)} class="stack">
      <ErrorBanner message={form.error()} onDismiss={() => form.setError(null)} />
      <Show when={form.note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{form.note()}</span>
        </div>
      </Show>

      <section class="card">
        <h2>Spawn defaults</h2>
        <Select
          skin="field"
          label="Agent"
          name="provider"
          value={provider()}
          options={providerOptions()}
          inheritLabel="Inherit global default"
          onChange={setProvider}
        />
        <Select
          skin="field"
          label="Model"
          name="model_default"
          value={model()}
          options={baseProvider()?.models ?? []}
          inheritLabel="Inherit global default"
          onChange={setModel}
        />
        <Select
          skin="field"
          label="Effort"
          name="effort_default"
          value={effort()}
          options={baseProvider()?.efforts ?? []}
          inheritLabel="Inherit global default"
          onChange={setEffort}
        />
        {/* Remote control (issue #163): a 3-way Inherit / On / Off over a
            tri-state column — the inherit row names what it currently resolves
            to, so "inherit" is never a guess. */}
        <Select
          skin="field"
          label="Remote control"
          name="remote_default"
          value={remote()}
          options={REMOTE_OPTIONS}
          inheritLabel={`Inherit global default — currently ${onOff(inheritedRemote())}`}
          disabled={remoteBlocker(baseProvider()) !== null}
          onChange={setRemote}
        />
        <Show
          when={remoteBlocker(baseProvider())}
          fallback={
            <small class="hint hint-block">
              Registers the session with the agent's web app so it can be opened and driven from
              there.
            </small>
          }
        >
          {(name) => <small class="hint hint-block">{name()} ignores this.</small>}
        </Show>
      </section>

      <section class="card">
        <h2>AFK defaults</h2>
        <small class="hint hint-block">
          Overrides for unattended AFK runs on this repo. Blank / unchecked inherits the global AFK
          default.
        </small>
        <Select
          skin="field"
          label="AFK agent"
          name="afk_provider_default"
          value={afkProvider()}
          options={providerOptions()}
          inheritLabel="Inherit global AFK default"
          onChange={setAfkProvider}
        />
        <Select
          skin="field"
          label="Model"
          name="afk_model_default"
          value={afkModel()}
          options={afkEffectiveProvider()?.models ?? []}
          inheritLabel="Inherit global AFK default"
          onChange={setAfkModel}
        />
        <Select
          skin="field"
          label="Effort"
          name="afk_effort_default"
          value={afkEffort()}
          options={afkEffectiveProvider()?.efforts ?? []}
          inheritLabel="Inherit global AFK default"
          onChange={setAfkEffort}
        />
        {/* The AFK remote override (issue #163): inherit here walks the AFK
            chain — global AFK override, then this repo's own manual default
            above, then the global base — which is what the row reports. */}
        <Select
          skin="field"
          label="Remote control"
          name="afk_remote_default"
          value={afkRemote()}
          options={REMOTE_OPTIONS}
          inheritLabel={`Inherit global AFK default — currently ${onOff(inheritedAfkRemote())}`}
          disabled={remoteBlocker(afkEffectiveProvider()) !== null}
          onChange={setAfkRemote}
        />
        <Show when={remoteBlocker(afkEffectiveProvider())}>
          {(name) => <small class="hint hint-block">{name()} ignores this.</small>}
        </Show>
        <For each={boolOptions()}>
          {(option) => (
            <label class="check">
              <input
                type="checkbox"
                name={`afk_options.${option.key}`}
                checked={afkOptions()[option.key] ?? false}
                onChange={(e) =>
                  setAfkOptions({ ...afkOptions(), [option.key]: e.currentTarget.checked })
                }
              />
              <span>{option.label}</span>
            </label>
          )}
        </For>
        <Show when={boolOptions().length > 0}>
          <small class="hint hint-block">A repo option bag overrides the global AFK options.</small>
        </Show>
        <label class="field">
          <span>Seed prompt</span>
          <textarea
            name="afk_prompt"
            rows="10"
            value={afkPrompt()}
            onInput={(e) => setAfkPrompt(e.currentTarget.value)}
            placeholder={props.repo().afk_prompt_effective}
          />
        </label>
        <Show when={afkPrompt() === ''}>
          <button
            type="button"
            class="small"
            onClick={() => setAfkPrompt(props.repo().afk_prompt_effective)}
          >
            Customize
          </button>
        </Show>
        <small class="hint hint-block">
          The run is detected as done only by an open PR on its branch — a prompt that never opens a
          PR burns its budget, counts as a failure, and three failures auto-pause the repo's AFK.
        </small>
      </section>

      <section class="card">
        <h2>AFK</h2>
        <label class="check">
          <input
            type="checkbox"
            name="afk_auto_enabled"
            checked={afkAuto()}
            onChange={(e) => setAfkAuto(e.currentTarget.checked)}
          />
          <span>Auto-spawn on ready-for-agent issues</span>
        </label>
        <label class="field">
          <span>Budget (minutes)</span>
          <input
            type="number"
            name="budget_minutes"
            min="1"
            step="1"
            autocomplete="off"
            placeholder="global default"
            value={budget()}
            onInput={(e) => setBudget(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Max instances override</span>
          <input
            type="number"
            name="max_instances_override"
            min="1"
            step="1"
            autocomplete="off"
            placeholder="global default"
            value={maxInstances()}
            onInput={(e) => setMaxInstances(e.currentTarget.value)}
          />
        </label>
      </section>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save changes'}
      </button>
    </form>
  );
}
