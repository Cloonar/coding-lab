// Global settings › Agents (issue #198): spawn defaults (model/effort from the
// provider catalog), the AFK defaults override, the AFK seed prompt, and the
// global capacity/AFK loop tuning. Saved as a dirty-fields-only PATCH; int
// fields validate per field client-side and the server's 400 {"error"} lands
// in the banner. Runtime loops re-read settings each tick, so saves apply
// without a restart. Ported verbatim from the old Settings monolith's
// SettingsForm minus the git author card (now the General section), onto the
// shared useSettingsForm primitive — where the monolith's buildPatch returned
// null on a field error, this returns the 'Fix the highlighted fields first.'
// string useSettingsForm surfaces as the error banner (same save() behavior).

import { For, Show, createSignal } from 'solid-js';
import {
  updateSettings,
  type BoolSettingKey,
  type IntSettingKey,
  type Provider,
  type Settings,
  type TextSettingKey,
} from '../../../api';
import Select, { type SelectOption } from '../../../components/Select';
import ErrorBanner from '../../../components/ErrorBanner';
import SectionCard from '../../../components/SectionCard';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import { providerFor } from '../../../lib/spawn';

/** The AFK remote override's explicit picks; the inherit row is Select's own. */
const REMOTE_OPTIONS: SelectOption[] = [
  { value: 'true', label: 'On' },
  { value: 'false', label: 'Off' },
];

interface IntField {
  key: IntSettingKey;
  label: string;
  hint: string;
  /** Minimum accepted value. Every existing operational field requires >=1;
   *  dialog_timeout_minutes (issue #124) means "never" at 0, so it needs its
   *  own floor. */
  min: number;
  /** Which card this field renders in. Most int fields live in "Capacity &
   *  AFK"; dialog_timeout_minutes belongs in "Spawn defaults" instead
   *  (issue #124) — buildPatch/intError/seedDraft still iterate INT_FIELDS as
   *  one flat list regardless of placement. */
  card: 'capacity' | 'spawn';
}

const INT_FIELDS: IntField[] = [
  {
    key: 'max_instances',
    label: 'Max instances',
    hint: 'Global cap on live sessions across all repos (login session exempt).',
    min: 1,
    card: 'capacity',
  },
  {
    key: 'afk_budget_minutes',
    label: 'AFK budget (minutes)',
    hint: 'Wall-clock budget per AFK run before the reaper times it out.',
    min: 1,
    card: 'capacity',
  },
  {
    key: 'afk_tick_seconds',
    label: 'Reaper tick (seconds)',
    hint: 'How often AFK runs are classified (success / death / timeout).',
    min: 1,
    card: 'capacity',
  },
  {
    key: 'afk_schedule_seconds',
    label: 'Scheduler tick (seconds)',
    hint: 'How often auto-enabled repos are considered for a new AFK run.',
    min: 1,
    card: 'capacity',
  },
  {
    key: 'sweep_interval_minutes',
    label: 'Sweep interval (minutes)',
    hint: 'Throttle for the merged-worktree/branch GC sweep.',
    min: 1,
    card: 'capacity',
  },
  {
    key: 'dialog_timeout_minutes',
    label: 'Dialog auto-dismiss (minutes)',
    hint: '0 = never. Applies to manual sessions at the next spawn; running sessions keep their spawn-time value.',
    min: 0,
    card: 'spawn',
  },
];

/** INT_FIELDS partitioned by card — filtered once at module scope since the
 *  split is static, not reactive. */
const SPAWN_INT_FIELDS = INT_FIELDS.filter((f) => f.card === 'spawn');
const CAPACITY_INT_FIELDS = INT_FIELDS.filter((f) => f.card === 'capacity');

/**
 * String draft of one settings value ('' for an absent key). Bool keys (issue
 * #163) draft as 'true'/'false', and null — the AFK override's inherit state —
 * drafts as '' exactly like an absent key: both mean "no explicit pick here".
 */
function seedDraft(
  initial: Settings,
  key: IntSettingKey | TextSettingKey | BoolSettingKey,
): string {
  const value = initial[key];
  return value === undefined || value === null ? '' : String(value);
}

export default function Agents(props: {
  initial: Settings;
  providers: Provider[];
  onSaved: () => void;
}) {
  // Drafts seed from the settings snapshot the form mounted with; buildPatch
  // diffs against that snapshot so only edited fields enter the PATCH
  // (settings have no SSE event, so no resync is needed).
  const initial = props.initial;
  const [drafts, setDrafts] = createSignal<Record<string, string>>({
    provider_default: seedDraft(initial, 'provider_default'),
    spawn_model_default: seedDraft(initial, 'spawn_model_default'),
    spawn_effort_default: seedDraft(initial, 'spawn_effort_default'),
    spawn_remote_default: seedDraft(initial, 'spawn_remote_default'),
    spawn_provider_default_afk: seedDraft(initial, 'spawn_provider_default_afk'),
    spawn_model_default_afk: seedDraft(initial, 'spawn_model_default_afk'),
    spawn_effort_default_afk: seedDraft(initial, 'spawn_effort_default_afk'),
    spawn_remote_default_afk: seedDraft(initial, 'spawn_remote_default_afk'),
    afk_prompt: seedDraft(initial, 'afk_prompt'),
    ...Object.fromEntries(INT_FIELDS.map((f) => [f.key, seedDraft(initial, f.key)])),
  });
  const draft = (key: string) => drafts()[key] ?? '';
  const setDraft = (key: string, value: string) => setDrafts({ ...drafts(), [key]: value });

  // Effective providers, resolved LIVE against the DRAFTS (skip-layer,
  // ADR-0030): the base catalogs follow the drafted provider_default, the AFK
  // catalogs the drafted AFK provider (falling through to the base draft) —
  // both re-catalog as the operator flips the agent selects, before save.
  const providerOptions = (): SelectOption[] =>
    props.providers.map((p) => ({ value: p.id, label: p.display_name }));
  const baseProvider = () => providerFor(props.providers, draft('provider_default'));
  const afkProvider = () =>
    providerFor(props.providers, draft('spawn_provider_default_afk'), draft('provider_default'));

  // AFK spawn-option checkboxes (bool provider options). The signal holds only
  // operator overrides; the seed reads the mounted snapshot, so a provider
  // catalog that loads after mount still resolves the right seeded state.
  const [optionDrafts, setOptionDrafts] = createSignal<Record<string, boolean>>({});
  const seedChecked = (key: string) => initial.spawn_options_afk?.[key] === 'true';
  const optionChecked = (key: string) => optionDrafts()[key] ?? seedChecked(key);
  const setOptionChecked = (key: string, value: boolean) =>
    setOptionDrafts({ ...optionDrafts(), [key]: value });
  const boolOptions = () => (afkProvider()?.options ?? []).filter((o) => o.type === 'bool');
  const optionsDirty = () => boolOptions().some((o) => optionChecked(o.key) !== seedChecked(o.key));

  // Remote control (issue #163) is a provider CAPABILITY, not a spawn option:
  // a provider without it ignores the setting, so the control is disabled and
  // says so — in the provider's own words (display_name), never a brand name.
  const remoteBlocker = (provider: Provider | null): string | null =>
    provider !== null && !provider.supports_remote ? provider.display_name : null;

  const intDirty = (key: IntSettingKey) => draft(key).trim() !== seedDraft(initial, key);

  /** Per-field validation: only dirty int fields can be in error. Each
   *  field's own `min` gates it — dialog_timeout_minutes (issue #124) allows
   *  0 ("never"), unlike the other int fields which require >=1. */
  const intError = (key: IntSettingKey): string | null => {
    if (!intDirty(key)) return null;
    const trimmed = draft(key).trim();
    if (!/^\d+$/.test(trimmed)) return 'Enter a whole number.';
    const min = INT_FIELDS.find((f) => f.key === key)?.min ?? 1;
    if (Number(trimmed) < min) return `Must be at least ${min}.`;
    return null;
  };

  const textDirty = (key: TextSettingKey) => draft(key).trim() !== seedDraft(initial, key).trim();

  const boolDirty = (key: BoolSettingKey) => draft(key) !== seedDraft(initial, key);
  /** The drafted tri-state: '' = inherit/unset (null on the wire), else the pick. */
  const boolValue = (key: BoolSettingKey): boolean | null =>
    draft(key) === '' ? null : draft(key) === 'true';

  const buildPatch = (): Settings | string => {
    const patch: Settings = {};
    for (const key of [
      'provider_default',
      'spawn_model_default',
      'spawn_effort_default',
      'spawn_provider_default_afk',
      'spawn_model_default_afk',
      'spawn_effort_default_afk',
      'afk_prompt',
    ] as TextSettingKey[]) {
      // afk_prompt_default is deliberately absent from this list: it is a
      // read-only, server-injected key (issue #52) that must never be PATCHed.
      if (textDirty(key)) patch[key] = draft(key).trim();
    }
    // Bool keys (issue #163): dirty-only, like every other field — an untouched
    // remote toggle is never sent, and `false` is a value, not an omission.
    for (const key of ['spawn_remote_default', 'spawn_remote_default_afk'] as BoolSettingKey[]) {
      if (boolDirty(key)) patch[key] = boolValue(key);
    }
    for (const field of INT_FIELDS) {
      if (!intDirty(field.key)) continue;
      // Where the monolith returned null (blocked), we hand useSettingsForm the
      // banner string it surfaces as the error — same behavior save() produced.
      if (intError(field.key) !== null) return 'Fix the highlighted fields first.';
      patch[field.key] = Number(draft(field.key).trim());
    }
    // Send the full declared bag ("true"/"false" per key) once any option
    // differs from its seed — a partial patch would be ambiguous server-side.
    if (optionsDirty()) {
      patch.spawn_options_afk = Object.fromEntries(
        boolOptions().map((o) => [o.key, optionChecked(o.key) ? 'true' : 'false']),
      );
    }
    return patch;
  };

  // One source of truth for the leave guard: dirty is derived straight from
  // buildPatch (a string result means "dirty but blocked by a field error"),
  // never a parallel bookkeeping signal.
  const dirty = () => {
    const patch = buildPatch();
    return typeof patch === 'string' || Object.keys(patch).length > 0;
  };

  const form = useSettingsForm<Settings>({
    dirty,
    buildPatch,
    submit: (patch) => updateSettings(patch),
    onSaved: () => props.onSaved(),
  });

  // Shared row markup for every int field, regardless of which card it
  // renders in (issue #124 needs one field split out of Capacity & AFK into
  // Spawn defaults, with identical behavior).
  const intFieldRow = (field: IntField) => (
    <label class="field">
      <span>{field.label}</span>
      <input
        type="text"
        inputmode="numeric"
        name={field.key}
        autocomplete="off"
        value={draft(field.key)}
        onInput={(e) => setDraft(field.key, e.currentTarget.value)}
        aria-invalid={intError(field.key) !== null}
      />
      <Show when={intError(field.key)} fallback={<small class="hint">{field.hint}</small>}>
        <small class="field-error" role="alert">
          {intError(field.key)}
        </small>
      </Show>
    </label>
  );

  return (
    <form onSubmit={(e) => void form.save(e)} class="stack">
      <ErrorBanner message={form.error()} onDismiss={() => form.setError(null)} />
      <Show when={form.note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{form.note()}</span>
        </div>
      </Show>

      <SectionCard title="Spawn defaults">
        <Select
          skin="field"
          label="Agent"
          name="provider_default"
          // The ROOT of the provider chain — there is nothing to inherit from,
          // so no inherit entry. An unseeded store shows the effective
          // fallback (the first registered provider).
          value={
            draft('provider_default') !== ''
              ? draft('provider_default')
              : (baseProvider()?.id ?? '')
          }
          options={providerOptions()}
          onChange={(value) => setDraft('provider_default', value)}
        />
        <Select
          skin="field"
          label="Model"
          name="spawn_model_default"
          value={draft('spawn_model_default')}
          options={baseProvider()?.models ?? []}
          onChange={(value) => setDraft('spawn_model_default', value)}
        />
        <Select
          skin="field"
          label="Effort"
          name="spawn_effort_default"
          value={draft('spawn_effort_default')}
          options={baseProvider()?.efforts ?? []}
          onChange={(value) => setDraft('spawn_effort_default', value)}
        />
        <Show when={baseProvider() === null}>
          <small class="hint hint-block">
            Provider catalog unavailable — only the stored values are offered.
          </small>
        </Show>
        {/* Remote control (issue #163): the BASE default every other layer
            falls back to, hence a plain on/off checkbox — there is nothing
            above it to inherit from. Off unless the operator turns it on. */}
        <label class="check">
          <input
            type="checkbox"
            name="spawn_remote_default"
            checked={draft('spawn_remote_default') === 'true'}
            disabled={remoteBlocker(baseProvider()) !== null}
            onChange={(e) =>
              setDraft('spawn_remote_default', e.currentTarget.checked ? 'true' : 'false')
            }
          />
          <span>Remote control</span>
        </label>
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
        {/* Not seeded server-side (issue #124): absent from GET renders as a
            blank input, distinct from an explicit 0 ("never"). */}
        <For each={SPAWN_INT_FIELDS}>{intFieldRow}</For>
      </SectionCard>

      <SectionCard
        title="AFK defaults"
        hint={
          <>
            Used for unattended AFK runs. Leave a field on “Same as default” to inherit the spawn
            default above.
          </>
        }
      >
        <Select
          skin="field"
          label="AFK agent"
          name="spawn_provider_default_afk"
          value={draft('spawn_provider_default_afk')}
          options={providerOptions()}
          inheritLabel="Same as default"
          onChange={(value) => setDraft('spawn_provider_default_afk', value)}
        />
        <Select
          skin="field"
          label="Model"
          name="spawn_model_default_afk"
          value={draft('spawn_model_default_afk')}
          options={afkProvider()?.models ?? []}
          inheritLabel="Same as default"
          onChange={(value) => setDraft('spawn_model_default_afk', value)}
        />
        <Select
          skin="field"
          label="Effort"
          name="spawn_effort_default_afk"
          value={draft('spawn_effort_default_afk')}
          options={afkProvider()?.efforts ?? []}
          inheritLabel="Same as default"
          onChange={(value) => setDraft('spawn_effort_default_afk', value)}
        />
        {/* The AFK remote OVERRIDE (issue #163) — three distinct states, so a
            checkbox would be a lie: "Same as default" (null), On and Off. `false`
            here is an explicit off that beats an on base default. */}
        <Select
          skin="field"
          label="Remote control"
          name="spawn_remote_default_afk"
          value={draft('spawn_remote_default_afk')}
          options={REMOTE_OPTIONS}
          inheritLabel="Same as default"
          disabled={remoteBlocker(afkProvider()) !== null}
          onChange={(value) => setDraft('spawn_remote_default_afk', value)}
        />
        <Show when={remoteBlocker(afkProvider())}>
          {(name) => <small class="hint hint-block">{name()} ignores this.</small>}
        </Show>
        <For each={boolOptions()}>
          {(option) => (
            <label class="check">
              <input
                type="checkbox"
                name={`spawn_options_afk.${option.key}`}
                checked={optionChecked(option.key)}
                onChange={(e) => setOptionChecked(option.key, e.currentTarget.checked)}
              />
              <span>{option.label}</span>
            </label>
          )}
        </For>
        <label class="field">
          <span>Seed prompt</span>
          <textarea
            name="afk_prompt"
            rows="10"
            value={draft('afk_prompt')}
            onInput={(e) => setDraft('afk_prompt', e.currentTarget.value)}
            placeholder={initial.afk_prompt_default ?? ''}
          />
        </label>
        <Show when={draft('afk_prompt') === ''}>
          <button
            type="button"
            class="small"
            onClick={() => setDraft('afk_prompt', initial.afk_prompt_default ?? '')}
          >
            Customize
          </button>
        </Show>
        <small class="hint hint-block">
          The run is detected as done only by an open PR on its branch — a prompt that never opens a
          PR burns its budget, counts as a failure, and three failures auto-pause the repo's AFK.
        </small>
      </SectionCard>

      <SectionCard title="Capacity & AFK">
        <For each={CAPACITY_INT_FIELDS}>{intFieldRow}</For>
      </SectionCard>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save settings'}
      </button>
    </form>
  );
}
