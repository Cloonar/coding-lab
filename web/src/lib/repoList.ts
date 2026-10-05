// The repositories list (issue #61), as pure functions: the order rule, the
// filter, the per-repo run counts, the relative "Last run" time and the
// Needs you entries. routes/Repos.tsx renders what these return.

import type { Instance, ReadinessCheck, Repo } from '../api';
import { AFK_PAUSE_THRESHOLD, isAFKPaused } from './afk';
import { plural } from './readiness';
import { remoteLabel } from './repoName';

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

/** True when the repo's readiness report names a failing check. */
export function isNotReady(repo: Repo): boolean {
  const readiness = repo.summary?.readiness;
  if (readiness === undefined) return false;
  return readiness.state === 'failing' || readiness.checks.some((c) => c.state === 'failing');
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * "just now", "4 min ago", "3 h ago", "yesterday", "6 days ago", "2 months
 * ago", "1 year ago" — the list's Last run column. null for no time at all
 * (a repo that never had a run) or an unparseable one. A time slightly in
 * the future (clock skew) reads "just now".
 */
export function relativeTime(iso: string | null | undefined, nowMs: number): string | null {
  const t = timeMs(iso);
  if (t === null) return null;
  const ago = nowMs - t;
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

/** One problem in the Needs you block, with the action that fixes it. */
export type NeedsYouEntry =
  | { kind: 'clone'; repo: Repo; message: string }
  | { kind: 'paused'; repo: Repo; message: string }
  | { kind: 'readiness'; repo: Repo; check: ReadinessCheck; message: string };

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
    const checks = repo.summary?.readiness.checks ?? [];
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
        kind: 'clone',
        repo,
        message: reason === '' ? 'Clone failed.' : `Clone failed: ${reason}`,
      });
    }
    for (const check of checks) {
      if (check.state !== 'failing' || check === cloneCheck) continue;
      entries.push({ kind: 'readiness', repo, check, message: check.detail });
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
        kind: 'paused',
        repo,
        message: `AFK paused after ${AFK_PAUSE_THRESHOLD} failed runs.${waiting}`,
      });
    }
  }
  return entries;
}
