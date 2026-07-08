// Global settings (/settings): spawn defaults (model/effort from the
// provider catalog), the global instance cap, AFK budget minutes, reaper and
// scheduler ticks, sweep interval and git author identity. Saved as a
// dirty-fields-only PATCH; int fields validate per field client-side and the
// server's 400 {"error"} lands in the banner. Runtime loops re-read settings
// each tick, so saves apply without a restart.

import { For, Match, Show, Switch, createResource, createSignal } from 'solid-js';
import {
  errorMessage,
  getSettings,
  listProviders,
  updateSettings,
  type IntSettingKey,
  type Settings as SettingsPayload,
  type TextSettingKey,
} from '../api';
import CatalogSelect from '../components/CatalogSelect';
import ErrorBanner from '../components/ErrorBanner';
import RequireAuth from '../components/RequireAuth';
import { providerFor } from '../lib/spawn';

interface IntField {
  key: IntSettingKey;
  label: string;
  hint: string;
}

const INT_FIELDS: IntField[] = [
  {
    key: 'max_instances',
    label: 'Max instances',
    hint: 'Global cap on live sessions across all repos (login session exempt).',
  },
  {
    key: 'afk_budget_minutes',
    label: 'AFK budget (minutes)',
    hint: 'Wall-clock budget per AFK run before the reaper times it out.',
  },
  {
    key: 'afk_tick_seconds',
    label: 'Reaper tick (seconds)',
    hint: 'How often AFK runs are classified (success / death / timeout).',
  },
  {
    key: 'afk_schedule_seconds',
    label: 'Scheduler tick (seconds)',
    hint: 'How often auto-enabled repos are considered for a new AFK run.',
  },
  {
    key: 'sweep_interval_minutes',
    label: 'Sweep interval (minutes)',
    hint: 'Throttle for the merged-worktree/branch GC sweep.',
  },
];

export default function Settings() {
  return (
    <RequireAuth>
      <SettingsView />
    </RequireAuth>
  );
}

function SettingsView() {
  const [settings, { refetch }] = createResource(() => getSettings());
  const [providers] = createResource(() => listProviders());

  return (
    <main class="page">
      <div class="section-head">
        <h2>Settings</h2>
      </div>
      <Switch>
        <Match when={settings.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(settings.error)}</span>
          </div>
        </Match>
        <Match when={settings()}>
          {(current) => (
            <SettingsForm
              initial={current()}
              // Global spawn defaults have no repo context; the catalog comes
              // from the API's provider list (its first entry — providerFor's
              // fallback), never from a hardcoded provider id (issue #51).
              provider={providerFor(providers() ?? [], '')}
              onSaved={() => void refetch()}
            />
          )}
        </Match>
      </Switch>
    </main>
  );
}

/** String draft of one settings value ('' for an absent key). */
function seedDraft(initial: SettingsPayload, key: IntSettingKey | TextSettingKey): string {
  const value = initial[key];
  return value === undefined ? '' : String(value);
}

function SettingsForm(props: {
  initial: SettingsPayload;
  provider: ReturnType<typeof providerFor>;
  onSaved: () => void;
}) {
  // Drafts seed from the settings snapshot the form mounted with; buildPatch
  // diffs against that snapshot so only edited fields enter the PATCH
  // (settings have no SSE event, so no resync is needed).
  const initial = props.initial;
  const [drafts, setDrafts] = createSignal<Record<string, string>>({
    spawn_model_default: seedDraft(initial, 'spawn_model_default'),
    spawn_effort_default: seedDraft(initial, 'spawn_effort_default'),
    spawn_model_default_afk: seedDraft(initial, 'spawn_model_default_afk'),
    spawn_effort_default_afk: seedDraft(initial, 'spawn_effort_default_afk'),
    git_author_name: seedDraft(initial, 'git_author_name'),
    git_author_email: seedDraft(initial, 'git_author_email'),
    afk_prompt: seedDraft(initial, 'afk_prompt'),
    ...Object.fromEntries(INT_FIELDS.map((f) => [f.key, seedDraft(initial, f.key)])),
  });
  const draft = (key: string) => drafts()[key] ?? '';
  const setDraft = (key: string, value: string) => setDrafts({ ...drafts(), [key]: value });

  // AFK spawn-option checkboxes (bool provider options). The signal holds only
  // operator overrides; the seed reads the mounted snapshot, so a provider
  // catalog that loads after mount still resolves the right seeded state.
  const [optionDrafts, setOptionDrafts] = createSignal<Record<string, boolean>>({});
  const seedChecked = (key: string) => initial.spawn_options_afk?.[key] === 'true';
  const optionChecked = (key: string) => optionDrafts()[key] ?? seedChecked(key);
  const setOptionChecked = (key: string, value: boolean) =>
    setOptionDrafts({ ...optionDrafts(), [key]: value });
  const boolOptions = () => (props.provider?.options ?? []).filter((o) => o.type === 'bool');
  const optionsDirty = () => boolOptions().some((o) => optionChecked(o.key) !== seedChecked(o.key));

  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [note, setNote] = createSignal<string | null>(null);

  const intDirty = (key: IntSettingKey) => draft(key).trim() !== seedDraft(initial, key);

  /** Per-field validation: only dirty int fields can be in error. */
  const intError = (key: IntSettingKey): string | null => {
    if (!intDirty(key)) return null;
    const trimmed = draft(key).trim();
    if (!/^\d+$/.test(trimmed)) return 'Enter a whole number.';
    if (Number(trimmed) < 1) return 'Must be at least 1.';
    return null;
  };

  const textDirty = (key: TextSettingKey) => draft(key).trim() !== seedDraft(initial, key).trim();

  const buildPatch = (): SettingsPayload | null => {
    const patch: SettingsPayload = {};
    for (const key of [
      'spawn_model_default',
      'spawn_effort_default',
      'spawn_model_default_afk',
      'spawn_effort_default_afk',
      'git_author_name',
      'git_author_email',
      'afk_prompt',
    ] as TextSettingKey[]) {
      // afk_prompt_default is deliberately absent from this list: it is a
      // read-only, server-injected key (issue #52) that must never be PATCHed.
      if (textDirty(key)) patch[key] = draft(key).trim();
    }
    for (const field of INT_FIELDS) {
      if (!intDirty(field.key)) continue;
      if (intError(field.key) !== null) return null; // blocked by a field error
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

  const save = async (event: SubmitEvent) => {
    event.preventDefault();
    setError(null);
    setNote(null);
    const patch = buildPatch();
    if (patch === null) {
      setError('Fix the highlighted fields first.');
      return;
    }
    if (Object.keys(patch).length === 0) {
      setNote('Nothing to save.');
      return;
    }
    setBusy(true);
    try {
      await updateSettings(patch);
      setNote('Saved.');
      props.onSaved();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={(e) => void save(e)} class="stack">
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{note()}</span>
        </div>
      </Show>

      <section class="card">
        <h2>Spawn defaults</h2>
        <CatalogSelect
          label="Model"
          name="spawn_model_default"
          value={draft('spawn_model_default')}
          options={props.provider?.models ?? []}
          onChange={(value) => setDraft('spawn_model_default', value)}
        />
        <CatalogSelect
          label="Effort"
          name="spawn_effort_default"
          value={draft('spawn_effort_default')}
          options={props.provider?.efforts ?? []}
          onChange={(value) => setDraft('spawn_effort_default', value)}
        />
        <Show when={props.provider === null}>
          <small class="hint hint-block">
            Provider catalog unavailable — only the stored values are offered.
          </small>
        </Show>
      </section>

      <section class="card">
        <h2>AFK defaults</h2>
        <small class="hint hint-block">
          Used for unattended AFK runs. Leave a field on “Same as default” to inherit the spawn
          default above.
        </small>
        <CatalogSelect
          label="Model"
          name="spawn_model_default_afk"
          value={draft('spawn_model_default_afk')}
          options={props.provider?.models ?? []}
          inheritLabel="Same as default"
          onChange={(value) => setDraft('spawn_model_default_afk', value)}
        />
        <CatalogSelect
          label="Effort"
          name="spawn_effort_default_afk"
          value={draft('spawn_effort_default_afk')}
          options={props.provider?.efforts ?? []}
          inheritLabel="Same as default"
          onChange={(value) => setDraft('spawn_effort_default_afk', value)}
        />
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
          The run is detected as done only by an open PR on its branch — a prompt that never
          opens a PR burns its budget, counts as a failure, and three failures auto-pause the
          repo's AFK.
        </small>
      </section>

      <section class="card">
        <h2>Capacity & AFK</h2>
        <For each={INT_FIELDS}>
          {(field) => (
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
          )}
        </For>
      </section>

      <section class="card">
        <h2>Git author</h2>
        <label class="field">
          <span>Author name</span>
          <input
            type="text"
            name="git_author_name"
            autocomplete="off"
            value={draft('git_author_name')}
            onInput={(e) => setDraft('git_author_name', e.currentTarget.value)}
          />
          <small class="hint">Used for commits unless a repo overrides it.</small>
        </label>
        <label class="field">
          <span>Author email</span>
          <input
            type="text"
            name="git_author_email"
            autocomplete="off"
            spellcheck={false}
            value={draft('git_author_email')}
            onInput={(e) => setDraft('git_author_email', e.currentTarget.value)}
          />
        </label>
      </section>

      <button type="submit" class="primary wide" disabled={busy()}>
        {busy() ? 'Saving…' : 'Save settings'}
      </button>
    </form>
  );
}
