// The repo delete dialog's copy (issue #61): what deleting one repository
// does, line by line, built from what the dialog could load. Every sentence
// states what the server actually does (reposvc.Delete):
//
// - live instances are stopped and a running clone is abandoned — only by the
//   forced delete, which the dialog sends when either applies;
// - the repo row goes, and with it its settings, run history, Schedules and
//   secrets, and on a builtin-bound repo the built-in tracker's issues and
//   change requests;
// - lab's bare clone is removed, which deletes every parked branch; parked
//   worktree folders are NOT removed — they stay on disk;
// - the remote is never touched.
//
// A count the dialog could not load is null: its line is left out, never
// shown as 0. Only what applies to this repo is listed.

import type { CloneStatus, ParkedEntry, TrackerBinding } from '../api';

/** What the delete dialog loaded about one repo; null = could not be loaded. */
export interface DeleteFacts {
  cloneStatus: CloneStatus;
  trackerBinding: TrackerBinding;
  /** Live instances of this repo. */
  live: number | null;
  parked: ParkedEntry[] | null;
  schedules: number | null;
  secrets: number | null;
}

/** "1 schedule" / "3 schedules". */
export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** Whether confirming must send the forced delete: something live would refuse the plain one. */
export function needsForce(facts: Pick<DeleteFacts, 'cloneStatus' | 'live'>): boolean {
  return facts.cloneStatus === 'cloning' || (facts.live ?? 0) > 0;
}

function parkedLine(parked: ParkedEntry[]): string {
  const branches = parked.length;
  const folders = parked.filter((p) => p.worktree_path !== '').length;
  const head = `Deletes ${plural(branches, 'parked branch', 'parked branches')} with lab's clone`;
  if (folders === 0) return `${head}.`;
  if (folders === branches) {
    return branches === 1
      ? `${head}; its worktree folder stays on disk.`
      : `${head}; their worktree folders stay on disk.`;
  }
  return `${head}; ${plural(folders, 'worktree folder')} ${folders === 1 ? 'stays' : 'stay'} on disk.`;
}

/** The consequence lines that apply, most disruptive first. */
export function deleteConsequences(facts: DeleteFacts): string[] {
  const lines: string[] = [];
  if (facts.cloneStatus === 'cloning') lines.push('Abandons the running clone.');
  if (facts.live !== null && facts.live > 0) {
    lines.push(`Stops ${plural(facts.live, 'live instance')}.`);
  }
  if (facts.parked !== null && facts.parked.length > 0) lines.push(parkedLine(facts.parked));
  const schedules = facts.schedules ?? 0;
  const secrets = facts.secrets ?? 0;
  if (schedules > 0 && secrets > 0) {
    lines.push(`Deletes ${plural(schedules, 'Schedule')} and ${plural(secrets, 'secret')}.`);
  } else if (schedules > 0) {
    lines.push(`Deletes ${plural(schedules, 'Schedule')}.`);
  } else if (secrets > 0) {
    lines.push(`Deletes ${plural(secrets, 'secret')}.`);
  }
  if (facts.trackerBinding === 'builtin') {
    lines.push("Deletes the issues and change requests kept in lab's built-in tracker.");
  }
  lines.push("Removes lab's clone, this repository's settings and its run history.");
  return lines;
}
