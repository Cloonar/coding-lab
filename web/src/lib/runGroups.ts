// Pure grouping and wording helpers for the Runs page (issue #76): the Live
// side groups live instances into Needs you / Working / Idle in the rail's
// attention-first order (lib/railOrder), and the Ended side (/history) groups
// ended runs by local day. RunList (rail + phone page), the desktop Runs table
// and History all render from these, so the three surfaces never disagree on
// a group, a state phrase or an outcome word.
//
// "Last activity": the API exposes no per-run last-activity timestamp (adding
// one is an API change, out of scope for #76), so the age on a row is measured
// from started_at for a live run and from ended_at for an ended one.
//
// Outcome words: a Run row carries no PR state, so "merged" / "PR open" cannot
// be derived without an API change either; the chip names the run's own
// outcome (done, died, timed out, stopped, escalated) and the row's second line
// shows `PR #n` when pull_number is set.

import type { ConversationState, Instance, Run, RunOutcome } from '../api';
import { instanceTitle, sessionLabel } from './instanceLabel';
import { orderRail, railGroup } from './railOrder';

export type LiveGroupKey = 'needs-you' | 'working' | 'idle';

export interface LiveGroup {
  key: LiveGroupKey;
  label: string;
  instances: Instance[];
}

const LIVE_GROUPS: { key: LiveGroupKey; label: string }[] = [
  { key: 'needs-you', label: 'Needs you' },
  { key: 'working', label: 'Working' },
  { key: 'idle', label: 'Idle' },
];

/**
 * Live instances in Needs you / Working / Idle groups, in that order, each in
 * orderRail's order (newest started_at first). Ended/dead rows are dropped and
 * empty groups are omitted. Pure — never mutates the input.
 */
export function groupLive(instances: Instance[]): LiveGroup[] {
  const ordered = orderRail(instances.filter((instance) => instance.live));
  return LIVE_GROUPS.map((group, index) => ({
    ...group,
    instances: ordered.filter((instance) => railGroup(instance.state) === index),
  })).filter((group) => group.instances.length > 0);
}

/** A live run's state as the row's second line and the table's State column say it. */
export function statePhrase(state: ConversationState): string {
  switch (state) {
    case 'needs_input':
      return 'Waiting for you';
    case 'question':
      return 'Asking a question';
    case 'working':
      return 'Working';
    default:
      return 'Idle';
  }
}

/** The outcome chip's word; the chip keeps its `outcome-<outcome>` class for colour. */
export function outcomeWord(outcome: RunOutcome): string {
  switch (outcome) {
    case 'success':
      return 'done';
    case 'death':
      return 'died';
    case 'timeout':
      return 'timed out';
    case 'stopped':
      return 'stopped';
    case 'escalated':
      return 'escalated';
    case 'active':
      return 'active';
  }
}

type Age = { n: number; unit: 'm' | 'h' | 'd' } | 'now' | '';

function age(from: string | null, nowMs: number): Age {
  if (from === null) return '';
  const t = Date.parse(from);
  if (Number.isNaN(t)) return '';
  const minutes = Math.floor((nowMs - t) / 60_000);
  if (minutes < 1) return 'now';
  if (minutes < 60) return { n: minutes, unit: 'm' };
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return { n: hours, unit: 'h' };
  return { n: Math.floor(hours / 24), unit: 'd' };
}

/**
 * Compact age from an ISO timestamp to `nowMs`: "now" under a minute (and for
 * a timestamp in the future — clock skew), then "4m", "2h", "3d". "" when the
 * timestamp is missing or unparseable. The rows' form (phone page, History).
 */
export function compactAge(from: string | null, nowMs: number): string {
  const a = age(from, nowMs);
  return typeof a === 'string' ? a : `${a.n}${a.unit}`;
}

const SPACED_UNITS = { m: 'min', h: 'h', d: 'd' } as const;

/**
 * The same age spaced for the desktop Runs table's Last column, as the mockup
 * reads it: "now", "2 min", "1 h", "3 d" ("" when missing/unparseable).
 */
export function spacedAge(from: string | null, nowMs: number): string {
  const a = age(from, nowMs);
  return typeof a === 'string' ? a : `${a.n} ${SPACED_UNITS[a.unit]}`;
}

/** A live row's age: since started_at (no last-activity field — see header). */
export function liveAge(instance: Pick<Run, 'started_at'>, nowMs: number): string {
  return compactAge(instance.started_at, nowMs);
}

/** An ended row's age: since ended_at, falling back to started_at. */
export function endedAge(run: Pick<Run, 'started_at' | 'ended_at'>, nowMs: number): string {
  return compactAge(run.ended_at ?? run.started_at, nowMs);
}

/**
 * An ended run's title: a user-set title wins (issue #111); an AFK/autoland
 * run with an issue number titles 'AFK #N' (restart-proof, from the persisted
 * row); otherwise the session label, else the branch. A scheduled run carries
 * no issue number and falls through to its 'sched-<id>-<stamp>' label.
 */
export function endedRunTitle(run: Run): string {
  const custom = run.title?.trim() ?? '';
  if (custom !== '') return custom;
  if (run.kind !== 'manual' && run.issue_number !== null) return `AFK #${run.issue_number}`;
  const label = instanceTitle(sessionLabel(run.session_name));
  return label === '' ? run.branch : label;
}

export interface DayGroup<T> {
  /** Local calendar day, 'YYYY-MM-DD' — stable key for the group. */
  key: string;
  /** 'Today', 'Yesterday', else a short date ('Jul 6', or 'Jul 6, 2025' in another year). */
  label: string;
  runs: T[];
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

function pad(n: number): string {
  return String(n).padStart(2, '0');
}

function dayKey(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

function endedAtMs(run: Pick<Run, 'started_at' | 'ended_at'>): number {
  const t = Date.parse(run.ended_at ?? run.started_at);
  return Number.isNaN(t) ? 0 : t;
}

/** The day label for a timestamp, relative to `nowMs`'s local day. */
export function dayLabel(ms: number, nowMs: number): string {
  const day = new Date(ms);
  const today = new Date(nowMs);
  if (dayKey(day) === dayKey(today)) return 'Today';
  // setDate handles month/year rollover and DST without 24h arithmetic.
  const yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1);
  if (dayKey(day) === dayKey(yesterday)) return 'Yesterday';
  const short = `${MONTHS[day.getMonth()] ?? ''} ${day.getDate()}`;
  return day.getFullYear() === today.getFullYear() ? short : `${short}, ${day.getFullYear()}`;
}

/**
 * Ended runs grouped by the local day they ended (started_at when ended_at is
 * missing), newest day first and newest run first within a day. Pure; the sort
 * is stable, so equal timestamps keep their input order.
 */
export function groupByDay<T extends Pick<Run, 'started_at' | 'ended_at'>>(
  runs: T[],
  nowMs: number,
): DayGroup<T>[] {
  const sorted = [...runs].sort((a, b) => endedAtMs(b) - endedAtMs(a));
  const groups: DayGroup<T>[] = [];
  for (const run of sorted) {
    const ms = endedAtMs(run);
    const key = dayKey(new Date(ms));
    const last = groups[groups.length - 1];
    if (last !== undefined && last.key === key) {
      last.runs.push(run);
    } else {
      groups.push({ key, label: dayLabel(ms, nowMs), runs: [run] });
    }
  }
  return groups;
}
