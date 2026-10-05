// The repositories list (issue #61), as pure functions: the order rule, the
// filter, the per-repo run counts, the relative "Last run" time and the
// Needs you entries. routes/Repos.tsx renders what these return.

import type { Instance, ReadinessCheck, Repo } from '../api';
import type { LabEvent } from '../sse';
import { AFK_PAUSE_THRESHOLD, isAFKPaused } from './afk';
import type { LiveEventSpec } from './liveResource';
import { checksOf, plural, readinessState } from './readiness';
import { remoteLabel } from './repoName';

/** The coalescing window for summaryRefreshSpecs: a burst is one re-read. */
export const SUMMARY_REFRESH_MS = 250;

/**
 * The events, besides repo.changed, that move what a repo response carries.
 * The server computes a builtin-bound repo's `summary.claimable` and
 * `summary.open_issues` fresh on every read, but an issue or label edit only
 * publishes issue.changed; a run starting or ending publishes run.changed (and
 * moves `last_opened_at` and the claims); a parked branch publishes
 * parked.changed (a claim); an agent login flip publishes
 * provider.auth.changed (the readiness agent check). Each re-reads only lab's
 * own GET /repos or GET /repos/{id} — never a forge — trailing-edge debounced
 * so a burst is one request.
 *
 * `repoID` scopes the repo-tagged events to one repo (the repo home); an
 * event that carries no repoID still counts. Omitted, every event counts (the
 * list, which shows every repo).
 */
export function summaryRefreshSpecs(repoID?: () => string): LiveEventSpec[] {
  const concerns = (event: LabEvent): boolean =>
    repoID === undefined || event.repoID === undefined || event.repoID === repoID();
  return [
    { type: 'issue.changed', match: concerns, debounceMs: SUMMARY_REFRESH_MS },
    { type: 'run.changed', match: concerns, debounceMs: SUMMARY_REFRESH_MS },
    { type: 'parked.changed', match: concerns, debounceMs: SUMMARY_REFRESH_MS },
    { type: 'provider.auth.changed', debounceMs: SUMMARY_REFRESH_MS },
  ];
}

function timeMs(iso: string | null | undefined): number | null {
  if (iso === null || iso === undefined || iso === '') return null;
  const t = Date.parse(iso);
  return Number.isNaN(t) ? null : t;
}

/**
 * The list order: latest run first (by `last_opened_at`), then the repos that
 * never had a run, newest first (by `created_at`). Pure and stable — ties
 * keep their input order.
 */
export function orderRepos(repos: readonly Repo[]): Repo[] {
  return repos
    .map((repo, index) => ({ repo, index, opened: timeMs(repo.last_opened_at) }))
    .sort((a, b) => {
      if (a.opened !== null && b.opened !== null) return b.opened - a.opened || a.index - b.index;
      if (a.opened !== null) return -1;
      if (b.opened !== null) return 1;
      const created = (timeMs(b.repo.created_at) ?? 0) - (timeMs(a.repo.created_at) ?? 0);
      return created || a.index - b.index;
    })
    .map((entry) => entry.repo);
}

/** True when the filter field holds anything but whitespace. */
export function isFilterActive(query: string): boolean {
  return query.trim() !== '';
}

/**
 * Narrows the list as the operator types: a case-insensitive substring match
 * over the repo name and its remote as host plus path. An empty (or
 * whitespace-only) query keeps every repo.
 */
export function filterRepos(repos: readonly Repo[], query: string): Repo[] {
  const q = query.trim().toLowerCase();
  if (q === '') return [...repos];
  return repos.filter(
    (repo) =>
      repo.name.toLowerCase().includes(q) || remoteLabel(repo.remote_url).toLowerCase().includes(q),
  );
}

/** A live instance the agent is waiting on the operator in. */
export function isWaiting(instance: Instance): boolean {
  return instance.live && (instance.state === 'needs_input' || instance.state === 'question');
}

export interface RunCounts {
  /** Live instances in the repo. */
  live: number;
  /** Of those, the ones waiting for the operator. */
  waiting: number;
}

/** Live and waiting instance counts for one repo. */
export function runCounts(instances: readonly Instance[], repoID: string): RunCounts {
  let live = 0;
  let waiting = 0;
  for (const instance of instances) {
    if (!instance.live || instance.repo_id !== repoID) continue;
    live += 1;
    if (isWaiting(instance)) waiting += 1;
  }
  return { live, waiting };
}

/** The repo's AFK state as the list shows it. */
export type AFKState = 'paused' | 'on' | 'off';

export function afkState(repo: Repo): AFKState {
  if (isAFKPaused(repo.consecutive_failures)) return 'paused';
  return repo.afk_auto_enabled ? 'on' : 'off';
}

/**
 * True when the repo's readiness report names a failing check (or rolls up
 * to failing). A repo without a summary or report is not called not ready.
 */
export function isNotReady(repo: Repo): boolean {
  const readiness = repo.summary?.readiness;
  if (readiness === undefined || readiness === null) return false;
  return readinessState(readiness) === 'failing';
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** How far ahead of this clock a time may be and still read "just now". */
export const FUTURE_SKEW_MS = 5 * MINUTE;

/**
 * "just now", "4 min ago", "3 h ago", "yesterday", "6 days ago", "2 months
 * ago", "1 year ago" — the list's Last run column. null for no time at all
 * (a repo that never had a run) or an unparseable one. A time slightly in
 * the future (clock skew, up to FUTURE_SKEW_MS) reads "just now"; one further
 * ahead reads as its plain date, since no relative wording would be true.
 */
export function relativeTime(iso: string | null | undefined, nowMs: number): string | null {
  const t = timeMs(iso);
  if (t === null) return null;
  const ago = nowMs - t;
  if (ago < -FUTURE_SKEW_MS) return new Date(t).toLocaleDateString();
  if (ago < MINUTE) return 'just now';
  if (ago < HOUR) return `${Math.floor(ago / MINUTE)} min ago`;
  if (ago < DAY) return `${Math.floor(ago / HOUR)} h ago`;
  if (ago < 2 * DAY) return 'yesterday';
  const days = Math.floor(ago / DAY);
  if (days < 60) return `${days} days ago`;
  if (days < 365) return `${Math.floor(days / 30)} months ago`;
  const years = Math.floor(days / 365);
  return years === 1 ? '1 year ago' : `${years} years ago`;
}

/**
 * One problem in the Needs you block, with the action that fixes it. `key`
 * names the problem stably across refetches ("clone:<repo id>",
 * "paused:<repo id>", "readiness:<repo id>:<check id>"): the block keys its
 * rows and their busy state on it.
 */
export type NeedsYouEntry =
  | { key: string; kind: 'clone'; repo: Repo; message: string }
  | { key: string; kind: 'paused'; repo: Repo; message: string }
  | { key: string; kind: 'readiness'; repo: Repo; check: ReadinessCheck; message: string };

function firstLine(text: string): string {
  return (
    text
      .split('\n')
      .map((line) => line.trim())
      .find((line) => line !== '') ?? ''
  );
}

/**
 * Every problem the operator has to act on, in list order, one entry each:
 * - a failed clone (action Retry) — ONE entry, even though its readiness
 *   report also carries the failing clone check;
 * - a three-strikes pause (action Reset), with the waiting count when the
 *   claimable count is known;
 * - each other failing readiness check (action Fix), worded by the server.
 */
export function needsYou(repos: readonly Repo[]): NeedsYouEntry[] {
  const entries: NeedsYouEntry[] = [];
  for (const repo of repos) {
    const checks = checksOf(repo.summary?.readiness);
    const cloneFailed =
      repo.clone_status === 'error' ||
      checks.some((check) => check.state === 'failing' && check.action === 'retry_clone');
    // The readiness check that reports the same failed clone, folded into the
    // clone entry below rather than listed a second time.
    const cloneCheck = cloneFailed
      ? checks.find(
          (check) =>
            check.state === 'failing' && (check.action === 'retry_clone' || check.id === 'clone'),
        )
      : undefined;
    if (cloneFailed) {
      const reason = firstLine(repo.clone_error ?? '') || firstLine(cloneCheck?.detail ?? '');
      entries.push({
        key: `clone:${repo.id}`,
        kind: 'clone',
        repo,
        message: reason === '' ? 'Clone failed.' : `Clone failed: ${reason}`,
      });
    }
    for (const check of checks) {
      if (check.state !== 'failing' || check === cloneCheck) continue;
      entries.push({
        key: `readiness:${repo.id}:${check.id}`,
        kind: 'readiness',
        repo,
        check,
        message: check.detail,
      });
    }
    if (isAFKPaused(repo.consecutive_failures)) {
      const claimable = repo.summary?.claimable ?? null;
      const waiting =
        claimable === null
          ? ''
          : claimable === 0
            ? ' No issues waiting.'
            : ` ${plural(claimable, 'issue')} waiting.`;
      entries.push({
        key: `paused:${repo.id}`,
        kind: 'paused',
        repo,
        message: `AFK paused after ${AFK_PAUSE_THRESHOLD} failed runs.${waiting}`,
      });
    }
  }
  return entries;
}
