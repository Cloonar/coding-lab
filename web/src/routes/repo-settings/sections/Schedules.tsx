// Schedules section (issue #61 §8, ADR-0062): the per-repo cadences that
// fire scheduled runs, as a list of rows that act at once. A row reads name,
// cadence in words, flows, the next firing (the server's own rendering) and
// the last run's outcome, and carries an on/off switch that applies the
// moment it is flipped; a paused row says so and offers Re-enable, the only
// path out of a three-strikes pause. Opening a row goes to the schedule
// editor at its own URL (ScheduleEditor.tsx); "+ New schedule" at the end
// does the same for a new one. The page owns the list resource — it refetches
// on repo.changed (a pause, a re-enable) and on run.changed (a run of a
// Schedule started or ended) — and the editor; this section only renders.

import { For, Match, Show, Switch, createSignal, type Resource } from 'solid-js';
import {
  errorMessage,
  patchRepoSchedule,
  reenableRepoSchedule,
  type Schedule,
  type ScheduleFlow,
  type ScheduleLastRun,
} from '../../../api';
import Banner from '../../../components/Banner';
import EmptyState from '../../../components/EmptyState';
import Icon from '../../../components/Icon';
import ToggleSwitch from '../../../components/Switch';
import { cadenceSummary } from '../../../lib/cronPreset';
import { relativeTime } from '../../../lib/repoList';
import { useRepoHome } from '../../repo-home/context';

/** "Autolander flow", "Autolander, Human triage flows", or "prompt only". */
export function flowsText(keys: readonly string[], catalog: readonly ScheduleFlow[]): string {
  if (keys.length === 0) return 'prompt only';
  const labels = keys.map((key) => catalog.find((flow) => flow.key === key)?.label ?? key);
  return `${labels.join(', ')} ${keys.length === 1 ? 'flow' : 'flows'}`;
}

/**
 * The last run in words: "never ran", "running now", or the outcome plus how
 * long ago it ended ("succeeded 2 h ago", "died yesterday", "timed out 3
 * days ago"). The outcome is the server's; only the "ago" is rendered here.
 */
export function lastRunText(last: ScheduleLastRun | null, nowMs: number): string {
  if (last === null) return 'never ran';
  if (last.outcome === 'active') return 'running now';
  const words: Record<Exclude<ScheduleLastRun['outcome'], 'active'>, string> = {
    success: 'succeeded',
    death: 'died',
    timeout: 'timed out',
    stopped: 'stopped',
    escalated: 'escalated',
  };
  const when = relativeTime(last.ended_at ?? last.started_at, nowMs);
  return when === null ? words[last.outcome] : `${words[last.outcome]} ${when}`;
}

/**
 * The row's second line: where the Schedule stands. A paused one says how it
 * got there; an enabled one names its next firing (server-rendered, or "no
 * next run" when the server has none); a switched-off one says "off". The
 * last run follows in every case.
 */
export function statusLine(schedule: Schedule, nowMs: number): string {
  const last = `last run ${lastRunText(schedule.last_run, nowMs)}`;
  if (schedule.paused) {
    const n = schedule.consecutive_failures;
    return `Paused after ${n} failed ${n === 1 ? 'run' : 'runs'} · ${last}`;
  }
  if (!schedule.enabled) return `Off · ${last}`;
  const next =
    schedule.next_run_display !== null ? `Next ${schedule.next_run_display}` : 'No next run';
  return `${next} · ${last}`;
}

export default function SchedulesSection(props: {
  repoId: string;
  schedules: Resource<Schedule[]>;
  flows: readonly ScheduleFlow[];
  /** The editor URL of a Schedule id, or of 'new'. */
  editorHref: (id: string) => string;
  /** Opens the editor over the page. */
  onOpen: (id: string) => void;
  /** Reloads the list after a row changed on the server. */
  onChanged: () => void;
  /** A problem the page reports here (a Run now from the toast refused). */
  notice: string | null;
  onDismissNotice: () => void;
}) {
  const home = useRepoHome();
  const [error, setError] = createSignal<string | null>(null);
  const [busy, setBusy] = createSignal<string | null>(null);
  // The clock the "ago" words read; refreshed whenever the list does.
  const now = (): number => {
    props.schedules();
    return Date.now();
  };

  // One server call per row action, from the row's handler: the repo id is
  // read then, never earlier.
  const act = async (
    schedule: Schedule,
    action: (repoId: string) => Promise<unknown>,
    said: string,
  ): Promise<void> => {
    setBusy(schedule.id);
    setError(null);
    try {
      await action(props.repoId);
      props.onChanged();
      home.notify(said);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };
  const setEnabled = (schedule: Schedule, enabled: boolean): Promise<void> =>
    act(
      schedule,
      (repoId) => patchRepoSchedule(repoId, schedule.id, { enabled }),
      `"${schedule.name}" turned ${enabled ? 'on' : 'off'}`,
    );
  // The only path out of a three-strikes pause (ADR-0062): the engine
  // strikes, a human clears — the editor deliberately cannot.
  const reenable = (schedule: Schedule): Promise<void> =>
    act(
      schedule,
      (repoId) => reenableRepoSchedule(repoId, schedule.id),
      `"${schedule.name}" re-enabled`,
    );

  const open = (event: MouseEvent, id: string): void => {
    // A plain click opens the editor over the page; a modified one (a new
    // tab) keeps the link's own behaviour.
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return;
    }
    event.preventDefault();
    props.onOpen(id);
  };

  return (
    <section class="card schedules-list" aria-label="Schedules">
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Banner message={props.notice} onDismiss={props.onDismissNotice} />
      <Switch>
        <Match when={props.schedules.error !== undefined}>
          <Banner message={errorMessage(props.schedules.error)} />
        </Match>
        <Match when={props.schedules()?.length === 0}>
          <EmptyState>
            No schedules yet. A schedule starts a run on a cadence, with your prompt.
          </EmptyState>
        </Match>
        <Match when={props.schedules()}>
          {(list) => (
            <ul class="schedule-rows">
              <For each={list()}>
                {(schedule) => (
                  <li class="schedule-row" classList={{ paused: schedule.paused }}>
                    <a
                      class="schedule-row-main"
                      href={props.editorHref(schedule.id)}
                      onClick={(event) => open(event, schedule.id)}
                    >
                      <span class="schedule-row-title">
                        <strong>{schedule.name}</strong>
                        <Icon name="chevron-right" size={16} />
                        <Show when={schedule.paused}>
                          <span class="chip status-warn">paused</span>
                        </Show>
                      </span>
                      <span class="schedule-row-line">
                        {cadenceSummary(schedule.cadence)} ·{' '}
                        {flowsText(schedule.flows, props.flows)}
                      </span>
                      <span class="schedule-row-line">{statusLine(schedule, now())}</span>
                    </a>
                    <div class="schedule-row-side">
                      <Show
                        when={schedule.paused}
                        fallback={
                          <ToggleSwitch
                            aria-label={`${schedule.name} enabled`}
                            name={`schedule-enabled-${schedule.id}`}
                            checked={schedule.enabled}
                            disabled={busy() === schedule.id}
                            onChange={(next) => void setEnabled(schedule, next)}
                          />
                        }
                      >
                        <button
                          type="button"
                          onClick={() => void reenable(schedule)}
                          disabled={busy() === schedule.id}
                        >
                          {busy() === schedule.id ? 'Re-enabling…' : 'Re-enable'}
                        </button>
                      </Show>
                    </div>
                  </li>
                )}
              </For>
            </ul>
          )}
        </Match>
      </Switch>
      <Show when={props.schedules.error === undefined}>
        <a
          class="schedule-new"
          href={props.editorHref('new')}
          onClick={(event) => open(event, 'new')}
        >
          <Icon name="plus" size={16} />
          New schedule
        </a>
      </Show>
    </section>
  );
}
