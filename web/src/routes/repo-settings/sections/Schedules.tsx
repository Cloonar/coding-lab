// Schedules section (issue #61 §8, ADR-0062): the per-repo cadences that
// fire scheduled runs, as a list of rows that act at once. A row reads name,
// cadence in words, flows, the next firing (the server's own rendering) and
// the last run's outcome, and carries an on/off switch that applies the
// moment it is flipped; a paused row says so and offers Re-enable, the only
// path out of a three-strikes pause. Opening a row goes to the schedule
// editor at its own URL (ScheduleEditor.tsx); "+ New schedule" at the end
// does the same for a new one. The page owns the list — it refetches on
// repo.changed (a pause, a re-enable) and on run.changed (a run of a
// Schedule started or ended) into a store whose rows keep their identity —
// and the editor; this section only renders.
//
// Keyboard focus survives all of it: a row is patched in place by a refetch
// (never rebuilt), its switch stays enabled while its request is in flight (a
// second click meanwhile sends nothing), the server's answer is applied to
// the row before the list is asked again, and when Re-enable takes its own
// button off the row, focus moves to the switch that replaces it.

import { For, Show, createSignal } from 'solid-js';
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
import { rescueFocus } from '../../../lib/focus';
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
  /** This repo's Schedules, each a stable object per id (lib/rowStore.ts). */
  rows: Schedule[];
  /** False until this repo's list is here. */
  loaded: boolean;
  /** Why the list could not be loaded or refreshed, or null. */
  error: string | null;
  flows: readonly ScheduleFlow[];
  /** The editor URL of a Schedule id, or of 'new'. */
  editorHref: (id: string) => string;
  /** Opens the editor over the page. */
  onOpen: (id: string) => void;
  /** A row changed on the server: this is its answer (the page applies it, then reloads). */
  onChanged: (row: Schedule) => void;
  /** A problem the page reports here (a Run now from the toast that did not start). */
  notice: string | null;
  onDismissNotice: () => void;
}) {
  const home = useRepoHome();
  const [error, setError] = createSignal<string | null>(null);
  // The rows with a request in flight. Held here and checked before sending:
  // the control stays enabled (a disabled one drops the focus), and a second
  // click while one is pending sends nothing.
  const [busy, setBusy] = createSignal<ReadonlySet<string>>(new Set());
  const setRowBusy = (id: string, on: boolean): void => {
    const next = new Set(busy());
    if (on) next.add(id);
    else next.delete(id);
    setBusy(next);
  };
  // One server call per row action, from the row's handler: the repo id is
  // read then, never earlier. Resolves true when the server took it.
  const act = async (
    schedule: Schedule,
    action: (repoId: string) => Promise<Schedule>,
    said: string,
  ): Promise<boolean> => {
    const id = schedule.id;
    if (busy().has(id)) return false;
    setRowBusy(id, true);
    setError(null);
    try {
      const answer = await action(props.repoId);
      props.onChanged(answer);
      home.notify(said);
      return true;
    } catch (err) {
      setError(errorMessage(err));
      return false;
    } finally {
      setRowBusy(id, false);
    }
  };
  const setEnabled = (schedule: Schedule, enabled: boolean): void => {
    // Captured before the await: the row is a live object, and its name may
    // have changed by the time the answer is in.
    const name = schedule.name;
    void act(
      schedule,
      (repoId) => patchRepoSchedule(repoId, schedule.id, { enabled }),
      `"${name}" turned ${enabled ? 'on' : 'off'}`,
    );
  };
  // The only path out of a three-strikes pause (ADR-0062): the engine
  // strikes, a human clears — the editor deliberately cannot.
  const reenable = async (schedule: Schedule, row: HTMLElement | undefined): Promise<void> => {
    const name = schedule.name;
    const done = await act(
      schedule,
      (repoId) => reenableRepoSchedule(repoId, schedule.id),
      `"${name}" re-enabled`,
    );
    // The button is gone from the row; its switch took the place.
    if (done) rescueFocus(row?.querySelector<HTMLElement>('button[role="switch"]'));
  };

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
      <Banner message={props.error} />
      <Show when={props.loaded}>
        <Show
          when={props.rows.length > 0}
          fallback={
            <EmptyState>
              No schedules yet. A schedule starts a run on a cadence, with your prompt.
            </EmptyState>
          }
        >
          <ul class="schedule-rows">
            <For each={props.rows}>
              {(schedule) => {
                let row: HTMLLIElement | undefined;
                return (
                  <li ref={row} class="schedule-row" classList={{ paused: schedule.paused }}>
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
                      {/* Re-read with the clock whenever the row changes. */}
                      <span class="schedule-row-line">{statusLine(schedule, Date.now())}</span>
                    </a>
                    <div class="schedule-row-side">
                      <Show
                        when={schedule.paused}
                        fallback={
                          <ToggleSwitch
                            aria-label={`${schedule.name} enabled`}
                            name={`schedule-enabled-${schedule.id}`}
                            checked={schedule.enabled}
                            onChange={(next) => setEnabled(schedule, next)}
                          />
                        }
                      >
                        <button
                          type="button"
                          aria-label={`Re-enable ${schedule.name}`}
                          aria-busy={busy().has(schedule.id) ? 'true' : undefined}
                          onClick={() => void reenable(schedule, row)}
                        >
                          {busy().has(schedule.id) ? 'Re-enabling…' : 'Re-enable'}
                        </button>
                      </Show>
                    </div>
                  </li>
                );
              }}
            </For>
          </ul>
        </Show>
      </Show>
      <Show when={props.loaded || props.error === null}>
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
