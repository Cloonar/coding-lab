// Schedules section (issue #247 / ADR-0062): the per-repo cadences that fire
// scheduled runs. A hybrid of the two shapes this area already has — the
// Secrets list (immediate CRUD, one server call per action, no
// unsaved-changes guard) wrapped around an Autoland-style editor form for the
// one Schedule being written.
//
// The cadence editor is the piece worth reading twice. A Schedule stores one
// cron expression; the Daily/Weekly/Monthly modes are an editing skin over
// that string (lib/cronPreset owns both directions), and Advanced is the raw
// expression. Whatever the mode, the upcoming firings under the editor are
// SERVER-rendered: the SPA never computes a firing, so it can never disagree
// with the engine about when a Schedule runs.

import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createResource,
  createSignal,
  onCleanup,
  untrack,
} from 'solid-js';
import type { Accessor } from 'solid-js';
import {
  createRepoSchedule,
  deleteRepoSchedule,
  errorMessage,
  listRepoSchedules,
  listScheduleFlows,
  patchRepoSchedule,
  previewCron,
  reenableRepoSchedule,
  type Provider,
  type Repo,
  type Schedule,
  type ScheduleCreate,
  type ScheduleFlow,
  type SchedulePatch,
  type Settings,
} from '../../../api';
import EmptyState from '../../../components/EmptyState';
import ErrorBanner from '../../../components/ErrorBanner';
import ListRowCard from '../../../components/ListRowCard';
import SectionCard from '../../../components/SectionCard';
import Select, { type SelectOption } from '../../../components/Select';
import {
  MONTH_DAY_MAX,
  WEEKDAYS,
  cadenceSummary,
  cronToPreset,
  presetToCron,
  type CadenceMode,
} from '../../../lib/cronPreset';
import { createLiveResource } from '../../../lib/liveResource';
import { providerFor } from '../../../lib/spawn';
import { normInt, normText } from '../shared';

/** Cadence default for a brand-new Schedule: early enough to be done by morning. */
const DEFAULT_TIME = '06:00';
/** Weekly's default pick (cron Monday), so the mode is never illegal on arrival. */
const DEFAULT_WEEKDAYS = [1];
/**
 * How long the cadence editor waits before asking the server what a changed
 * expression fires. Long enough that typing a raw cron is not one request per
 * keystroke, short enough that the preview still feels attached to the field.
 */
const PREVIEW_DEBOUNCE_MS = 400;

/**
 * The prompt starters the example picker offers. UI-only (ADR-0062): they are
 * editable starting text about the INVESTIGATION, never about routing — what
 * happens to the findings is the flows' business, so no example here names a
 * label, an issue or a CLI verb.
 */
const PROMPT_EXAMPLES: readonly { key: string; label: string; text: string }[] = [
  {
    key: 'dependency-updates',
    label: 'Check for dependency updates',
    text: [
      "Investigate this repository's dependencies for available updates.",
      '',
      'For every dependency with a newer release, read its changelog and note:',
      '- what actually changed, and whether any of it breaks how this repo uses it',
      '- whether the release closes a security advisory that reaches us',
      '- how much of this repo an update would touch',
      '',
      'Then summarize which updates are worth doing now, which can wait, and why.',
    ].join('\n'),
  },
  {
    key: 'security-audit',
    label: 'Security audit',
    text: [
      'Run the security-review skill over the current state of the default branch.',
      '',
      'Cover at least: how untrusted input is validated at the process boundaries,',
      'the authentication and authorization checks, secret handling, and every path',
      'that writes to disk or shells out.',
      '',
      'Write up the findings that deserve action, worst first — each with the file',
      'it lives in and why it matters.',
    ].join('\n'),
  },
];

/**
 * The selection in CATALOG order, always — the order a firing appends the
 * flows' instruction blocks in, and the order the server normalizes to on
 * write, so the form shows what the run will read. Keys the catalog does not
 * know (a flow retired under a stored Schedule) survive at the end rather than
 * being silently dropped.
 */
function canonicalFlows(selected: readonly string[], catalog: ScheduleFlow[]): string[] {
  const known = catalog.map((flow) => flow.key).filter((key) => selected.includes(key));
  const unknown = selected.filter((key) => !catalog.some((flow) => flow.key === key));
  return [...known, ...unknown];
}

/** Both sides are already canonical, so equality is a plain ordered compare. */
function sameFlows(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((key, i) => key === b[i]);
}

export default function SchedulesSection(props: {
  repo: Accessor<Repo>;
  providers: Provider[];
  settings: Settings;
  onSaved: () => void;
}) {
  // Live on repo.changed: the engine publishes it when a Schedule's paused
  // state or failure counter moves (a three-strikes pause, a completion's
  // reset), so the paused banner and counts refresh without a reload.
  const [schedules, { refetch }] = createLiveResource(
    () => props.repo().id,
    (repoID) => listRepoSchedules(repoID),
    [{ type: 'repo.changed', match: (event) => event.repoID === props.repo().id }],
  );
  const [flows] = createResource(() => listScheduleFlows());
  const [showCreate, setShowCreate] = createSignal(false);
  const [editingID, setEditingID] = createSignal<string | null>(null);
  const [error, setError] = createSignal<string | null>(null);
  const [note, setNote] = createSignal<string | null>(null);

  // One editor at a time: opening the create form closes an open row, and
  // opening a row closes the create form.
  const toggleCreate = (): void => {
    setEditingID(null);
    setNote(null);
    setShowCreate(!showCreate());
  };
  const toggleRow = (id: string): void => {
    setShowCreate(false);
    setNote(null);
    setEditingID(editingID() === id ? null : id);
  };

  const saved = (message: string): void => {
    setShowCreate(false);
    setEditingID(null);
    setError(null);
    setNote(message);
    void refetch();
    props.onSaved();
  };

  const flowLabel = (key: string): string =>
    (flows() ?? []).find((flow) => flow.key === key)?.label ?? key;

  return (
    <SectionCard
      title="Schedules"
      action={
        <button type="button" class="primary small" onClick={toggleCreate}>
          {showCreate() ? 'Cancel' : '+ Add schedule'}
        </button>
      }
      hint={
        <>
          A schedule fires a scheduled run on its cadence — the prompt and the selected flows brief
          it, and it ends when its budget expires. Cadences run in the server's local time.
        </>
      }
    >
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{note()}</span>
        </div>
      </Show>
      <Show when={showCreate()}>
        <ScheduleEditor
          repo={props.repo}
          providers={props.providers}
          settings={props.settings}
          flows={flows() ?? []}
          schedule={null}
          onSaved={saved}
          onCancel={() => setShowCreate(false)}
        />
      </Show>
      <Switch>
        <Match when={schedules.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(schedules.error)}</span>
          </div>
        </Match>
        <Match when={schedules()?.length === 0}>
          <EmptyState>
            No schedules yet — add one to investigate something on a cadence instead of remembering
            to.
          </EmptyState>
        </Match>
        <Match when={schedules()}>
          <div class="card-list">
            <For each={schedules()}>
              {(schedule) => (
                <ScheduleRow
                  repo={props.repo}
                  providers={props.providers}
                  settings={props.settings}
                  flows={flows() ?? []}
                  schedule={schedule}
                  flowLabel={flowLabel}
                  open={editingID() === schedule.id}
                  onToggle={() => toggleRow(schedule.id)}
                  onSaved={saved}
                  onChanged={() => void refetch()}
                  onError={setError}
                />
              )}
            </For>
          </div>
        </Match>
      </Switch>
    </SectionCard>
  );
}

function ScheduleRow(props: {
  repo: Accessor<Repo>;
  providers: Provider[];
  settings: Settings;
  flows: ScheduleFlow[];
  schedule: Schedule;
  flowLabel: (key: string) => string;
  open: boolean;
  onToggle: () => void;
  onSaved: (message: string) => void;
  onChanged: () => void;
  onError: (message: string | null) => void;
}) {
  const [busy, setBusy] = createSignal<'delete' | 'reenable' | null>(null);

  const act = async (kind: 'delete' | 'reenable', action: () => Promise<void>): Promise<void> => {
    setBusy(kind);
    props.onError(null);
    try {
      await action();
      props.onChanged();
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const remove = (): Promise<void> => {
    const repoID = props.repo().id;
    const schedule = props.schedule;
    if (!window.confirm(`Delete schedule "${schedule.name}"?`)) return Promise.resolve();
    return act('delete', () => deleteRepoSchedule(repoID, schedule.id));
  };

  // The only path out of a three-strikes pause (ADR-0062): the engine strikes,
  // a human clears — the edit form deliberately cannot.
  const reenable = (): Promise<void> => {
    const repoID = props.repo().id;
    const scheduleID = props.schedule.id;
    return act('reenable', async () => {
      await reenableRepoSchedule(repoID, scheduleID);
    });
  };

  return (
    <ListRowCard
      title={props.schedule.name}
      actions={
        <>
          <button
            type="button"
            class="small"
            onClick={() => props.onToggle()}
            disabled={busy() !== null}
          >
            {props.open ? 'Close' : 'Edit'}
          </button>
          <button
            type="button"
            class="danger small"
            onClick={() => void remove()}
            disabled={busy() !== null}
          >
            {busy() === 'delete' ? 'Working…' : 'Delete'}
          </button>
        </>
      }
      sub={cadenceSummary(props.schedule.cadence)}
    >
      <div class="chip-row">
        <span class="chip">{props.schedule.enabled ? 'enabled' : 'disabled'}</span>
        <For each={props.schedule.flows}>
          {(key) => <span class="chip">{props.flowLabel(key)}</span>}
        </For>
      </div>
      <Show when={props.schedule.paused}>
        <div class="banner error" role="alert">
          <span class="banner-text">Paused after 3 failures</span>
          <button type="button" onClick={() => void reenable()} disabled={busy() !== null}>
            {busy() === 'reenable' ? 'Re-enabling…' : 'Re-enable'}
          </button>
        </div>
      </Show>
      <Show when={props.open}>
        <ScheduleEditor
          repo={props.repo}
          providers={props.providers}
          settings={props.settings}
          flows={props.flows}
          schedule={props.schedule}
          onSaved={props.onSaved}
          onCancel={() => props.onToggle()}
        />
      </Show>
    </ListRowCard>
  );
}

function ScheduleEditor(props: {
  repo: Accessor<Repo>;
  providers: Provider[];
  settings: Settings;
  flows: ScheduleFlow[];
  /** null = the create form; otherwise the row being edited, pre-filled. */
  schedule: Schedule | null;
  onSaved: (message: string) => void;
  onCancel: () => void;
}) {
  // The editor mounts fresh every time it opens and unmounts when it closes,
  // so seeding the drafts once here IS the pre-fill — there is no live row to
  // resync against mid-edit (an open editor and a refetch never overlap),
  // which is what the untracked read says out loud.
  const seed = untrack(() => props.schedule);
  const seedPreset = seed === null ? null : cronToPreset(seed.cadence);

  const [name, setName] = createSignal(seed?.name ?? '');
  const [prompt, setPrompt] = createSignal(seed?.prompt ?? '');
  const [picked, setPicked] = createSignal<string[]>(seed?.flows ?? []);
  // The selection the form SHOWS and SENDS: always the catalog's order, never
  // the click order, derived so it settles the moment the catalog arrives.
  const selected = (): string[] => canonicalFlows(picked(), props.flows);
  const [enabled, setEnabled] = createSignal(seed?.enabled ?? true);
  const [budget, setBudget] = createSignal(
    seed?.budget_minutes == null ? '' : String(seed.budget_minutes),
  );
  const [provider, setProvider] = createSignal(seed?.provider ?? '');
  const [model, setModel] = createSignal(seed?.model ?? '');
  const [effort, setEffort] = createSignal(seed?.effort ?? '');

  // Cadence drafts. A stored expression that decomposes opens in its preset;
  // one that does not opens in Advanced with the expression untouched.
  const [mode, setMode] = createSignal<CadenceMode>(
    seedPreset?.mode ?? (seed === null ? 'daily' : 'advanced'),
  );
  const [time, setTime] = createSignal(seedPreset?.time ?? DEFAULT_TIME);
  const [weekdays, setWeekdays] = createSignal(
    seedPreset !== null && seedPreset.mode === 'weekly' ? seedPreset.weekdays : DEFAULT_WEEKDAYS,
  );
  const [monthDay, setMonthDay] = createSignal(
    seedPreset !== null && seedPreset.mode === 'monthly' ? seedPreset.day : 1,
  );
  const [rawCron, setRawCron] = createSignal(seed?.cadence ?? '');

  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [note, setNote] = createSignal<string | null>(null);

  /** The one thing a Schedule stores about time: whatever the mode renders. */
  const cadence = (): string => {
    const current = mode();
    if (current === 'advanced') return rawCron().trim();
    if (current === 'daily') return presetToCron({ mode: 'daily', time: time() });
    if (current === 'weekly') {
      return presetToCron({ mode: 'weekly', time: time(), weekdays: weekdays() });
    }
    return presetToCron({ mode: 'monthly', time: time(), day: monthDay() });
  };

  // Debounced preview source: the first value is immediate (an editor that
  // just opened already knows its cadence), every later change waits out the
  // typing. The fetch itself is the server's — see the file header.
  const [previewExpr, setPreviewExpr] = createSignal(cadence());
  createEffect(() => {
    const expr = cadence();
    const timer = setTimeout(() => setPreviewExpr(expr), PREVIEW_DEBOUNCE_MS);
    onCleanup(() => clearTimeout(timer));
  });
  const [preview] = createResource(
    () => (previewExpr() === '' ? null : previewExpr()),
    (expr) => previewCron(expr),
  );

  const toggleFlow = (key: string): void => {
    setPicked(picked().includes(key) ? picked().filter((k) => k !== key) : [...picked(), key]);
  };

  const toggleWeekday = (value: number): void => {
    setWeekdays(
      weekdays().includes(value)
        ? weekdays().filter((day) => day !== value)
        : [...weekdays(), value].sort((a, b) => a - b),
    );
  };

  // Filling from an example never destroys work silently: an empty prompt just
  // fills, a written one asks first.
  const applyExample = (key: string): void => {
    const example = PROMPT_EXAMPLES.find((candidate) => candidate.key === key);
    if (example === undefined) return;
    const current = prompt().trim();
    if (
      current !== '' &&
      current !== example.text.trim() &&
      !window.confirm('Replace the prompt with this example?')
    ) {
      return;
    }
    setPrompt(example.text);
  };

  const providerOptions = (): SelectOption[] =>
    props.providers.map((p) => ({ value: p.id, label: p.display_name }));
  // A Schedule's override is one more default rung ABOVE the AFK layering
  // (ADR-0062), so the effective provider its model/effort catalogs come from
  // is: this Schedule's pick, then the repo's AFK chain, then the repo's base
  // chain. Resolved live against the draft so the catalogs re-catalog as the
  // operator flips the agent, and a stored value foreign to the new catalog
  // stays selected as "(not in catalog)" rather than silently changing.
  const effectiveProvider = () =>
    providerFor(
      props.providers,
      provider(),
      props.repo().afk_provider_default,
      props.settings.spawn_provider_default_afk,
      props.repo().provider,
      props.settings.provider_default,
    );

  const budgetMinutes = (): number | null => {
    const parsed = normInt(budget());
    return parsed === undefined ? null : parsed;
  };

  /** Client-side refusals, so an obviously-incomplete form costs no request. */
  const validate = (): string | null => {
    if (name().trim() === '') return 'Name is required.';
    if (prompt().trim() === '' && selected().length === 0) {
      return 'A schedule needs a prompt, a flow, or both.';
    }
    if (mode() === 'weekly' && weekdays().length === 0) return 'Pick at least one weekday.';
    if (cadence() === '') return 'Cadence is required.';
    const parsed = normInt(budget());
    if (parsed === undefined || (parsed !== null && parsed < 1)) {
      return 'Budget must be a whole number of minutes, 1 or more.';
    }
    return null;
  };

  const createBody = (): ScheduleCreate => {
    const body: ScheduleCreate = {
      name: name().trim(),
      cadence: cadence(),
      prompt: prompt().trim(),
      flows: selected(),
      enabled: enabled(),
    };
    // The overrides ride the create only when actually set — an unset override
    // is the absence of a key, not a null.
    const minutes = budgetMinutes();
    if (minutes !== null) body.budget_minutes = minutes;
    const providerOverride = normText(provider());
    if (providerOverride !== null) body.provider = providerOverride;
    const modelOverride = normText(model());
    if (modelOverride !== null) body.model = modelOverride;
    const effortOverride = normText(effort());
    if (effortOverride !== null) body.effort = effortOverride;
    return body;
  };

  /** Only what actually changed — a PATCH is a diff, never a re-send. */
  const patchBody = (current: Schedule): SchedulePatch => {
    const patch: SchedulePatch = {};
    if (name().trim() !== current.name) patch.name = name().trim();
    if (cadence() !== current.cadence) patch.cadence = cadence();
    if (prompt().trim() !== current.prompt) patch.prompt = prompt().trim();
    if (!sameFlows(selected(), current.flows)) patch.flows = selected();
    if (enabled() !== current.enabled) patch.enabled = enabled();
    if (budgetMinutes() !== current.budget_minutes) patch.budget_minutes = budgetMinutes();
    if (normText(provider()) !== current.provider) patch.provider = normText(provider());
    if (normText(model()) !== current.model) patch.model = normText(model());
    if (normText(effort()) !== current.effort) patch.effort = normText(effort());
    return patch;
  };

  const submit = async (event: SubmitEvent): Promise<void> => {
    event.preventDefault();
    setError(null);
    setNote(null);
    const problem = validate();
    if (problem !== null) {
      setError(problem);
      return;
    }
    const repoID = props.repo().id;
    const current = props.schedule;
    let send: () => Promise<unknown>;
    if (current === null) {
      const body = createBody();
      send = () => createRepoSchedule(repoID, body);
    } else {
      const patch = patchBody(current);
      if (Object.keys(patch).length === 0) {
        setNote('Nothing to save.');
        return;
      }
      send = () => patchRepoSchedule(repoID, current.id, patch);
    }
    setBusy(true);
    try {
      await send();
      props.onSaved(current === null ? 'Schedule created.' : 'Saved.');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="card form-card">
      <h2>{props.schedule === null ? 'New schedule' : 'Edit schedule'}</h2>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{note()}</span>
        </div>
      </Show>
      <form onSubmit={(e) => void submit(e)}>
        <label class="field">
          <span>Name</span>
          <input
            type="text"
            name="schedule-name"
            autocomplete="off"
            placeholder="Weekly dependency check"
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
          />
        </label>

        <label class="field">
          <span>Prompt</span>
          <textarea
            name="schedule-prompt"
            rows="6"
            value={prompt()}
            onInput={(e) => setPrompt(e.currentTarget.value)}
            placeholder="What should the run investigate?"
          />
        </label>
        {/* The example picker fills the prompt above and immediately falls back
            to its placeholder row — it is a starter, not a stored choice, so it
            never carries a selected value. */}
        <Select
          skin="field"
          label="Examples"
          name="schedule-example"
          value=""
          options={PROMPT_EXAMPLES.map((example) => ({
            value: example.key,
            label: example.label,
          }))}
          inheritLabel="Start from an example…"
          onChange={applyExample}
        />

        <div class="field">
          <span>Flows</span>
          <div class="label-picker" role="group" aria-label="Flows">
            <For each={props.flows}>
              {(flow) => (
                <button
                  type="button"
                  name={`flow-${flow.key}`}
                  classList={{ 'chip-toggle': true, on: selected().includes(flow.key) }}
                  aria-pressed={selected().includes(flow.key)}
                  title={flow.description}
                  onClick={() => toggleFlow(flow.key)}
                >
                  {selected().includes(flow.key) ? '✓ ' : ''}
                  {flow.label}
                </button>
              )}
            </For>
          </div>
          <small class="hint">
            Flows append routing instructions to the prompt, in catalog order. None is fine — that
            is a prompt-only schedule.
          </small>
        </div>

        <label class="field">
          <span>Cadence</span>
          <select
            name="cadence_mode"
            value={mode()}
            onChange={(e) => setMode(e.currentTarget.value as CadenceMode)}
          >
            <option value="daily">Daily</option>
            <option value="weekly">Weekly</option>
            <option value="monthly">Monthly</option>
            <option value="advanced">Advanced (cron)</option>
          </select>
        </label>
        <Show when={mode() !== 'advanced'}>
          <label class="field">
            <span>Time</span>
            <input
              type="time"
              name="cadence_time"
              value={time()}
              onInput={(e) => setTime(e.currentTarget.value)}
            />
          </label>
        </Show>
        <Show when={mode() === 'weekly'}>
          <div class="field">
            <span>Weekdays</span>
            <div class="label-picker" role="group" aria-label="Weekdays">
              <For each={WEEKDAYS}>
                {(day) => (
                  <button
                    type="button"
                    name={`weekday-${day.value}`}
                    classList={{ 'chip-toggle': true, on: weekdays().includes(day.value) }}
                    aria-pressed={weekdays().includes(day.value)}
                    onClick={() => toggleWeekday(day.value)}
                  >
                    {day.label}
                  </button>
                )}
              </For>
            </div>
          </div>
        </Show>
        <Show when={mode() === 'monthly'}>
          <label class="field">
            <span>Day of month</span>
            <select
              name="cadence_day"
              value={String(monthDay())}
              onChange={(e) => setMonthDay(Number(e.currentTarget.value))}
            >
              <For each={Array.from({ length: MONTH_DAY_MAX }, (_, i) => i + 1)}>
                {(day) => <option value={String(day)}>{day}</option>}
              </For>
            </select>
            <small class="hint">
              Days 29–31 skip the months that are too short — write those as a cron expression under
              Advanced.
            </small>
          </label>
        </Show>
        <Show when={mode() === 'advanced'}>
          <label class="field">
            <span>Cron expression</span>
            <input
              type="text"
              name="cadence_expr"
              class="mono"
              autocomplete="off"
              spellcheck={false}
              placeholder="30 6 * * 1"
              value={rawCron()}
              onInput={(e) => setRawCron(e.currentTarget.value)}
            />
            <small class="hint">
              Five fields, minute granularity: minute hour day-of-month month day-of-week.
            </small>
          </label>
        </Show>
        {/* Always visible, every mode: the server's own answer to "when does
            this actually fire", so a preset and a hand-written expression are
            checked by exactly the same parser the engine uses. */}
        <Show when={cadence() !== '' && preview()} keyed>
          {(fired) => (
            <small
              classList={{
                hint: true,
                'hint-block': true,
                'cadence-preview': true,
                invalid: !fired.valid,
              }}
            >
              {fired.valid
                ? `Next: ${(fired.next_display ?? []).join(', ')}`
                : (fired.error ?? 'This cadence never fires.')}
            </small>
          )}
        </Show>

        <label class="field">
          <span>Budget (minutes)</span>
          <input
            type="number"
            name="schedule_budget_minutes"
            min="1"
            step="1"
            autocomplete="off"
            placeholder="30"
            value={budget()}
            onInput={(e) => setBudget(e.currentTarget.value)}
          />
          <small class="hint">
            A scheduled run has no done-signal: the budget clock is what ends it, and expiry counts
            as a success.
          </small>
        </label>
        <Select
          skin="field"
          label="Agent"
          name="schedule_provider"
          value={provider()}
          options={providerOptions()}
          inheritLabel="Inherit repo AFK agent"
          onChange={setProvider}
        />
        <Select
          skin="field"
          label="Model"
          name="schedule_model"
          value={model()}
          options={effectiveProvider()?.models ?? []}
          inheritLabel="Inherit repo AFK default"
          onChange={setModel}
        />
        <Select
          skin="field"
          label="Effort"
          name="schedule_effort"
          value={effort()}
          options={effectiveProvider()?.efforts ?? []}
          inheritLabel="Inherit repo AFK default"
          onChange={setEffort}
        />
        <label class="check">
          <input
            type="checkbox"
            name="schedule_enabled"
            checked={enabled()}
            onChange={(e) => setEnabled(e.currentTarget.checked)}
          />
          <span>Enabled</span>
        </label>

        <div class="card-actions">
          <button type="submit" class="primary" disabled={busy()}>
            {busy() ? 'Saving…' : props.schedule === null ? 'Create schedule' : 'Save schedule'}
          </button>
          <button type="button" onClick={() => props.onCancel()} disabled={busy()}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
