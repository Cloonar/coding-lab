// Pure issue-list logic: the ready-for-agent queue label, the client-side
// label refinement over an already-fetched state page, the chip-row label
// derivation, and the binding gate that decides whether lab may mutate the
// tracker at all. Kept here (not in components) so the filter and gate rules
// are unit-tested against the pinned M4 contract.

import type { TrackerBinding } from '../api';

/** The one label that puts an open issue in the agent's ready queue. */
export const READY_LABEL = 'ready-for-agent';

export function hasReadyLabel(labels: readonly string[]): boolean {
  return labels.includes(READY_LABEL);
}

/**
 * Sorted, de-duplicated union of every label across the issues, plus any
 * `extra` names (e.g. the active filter) so a chip never vanishes mid-use.
 * Derived from the full state page — never from the label-filtered subset —
 * so the operator can always switch to a different label.
 */
export function availableLabelNames(
  issues: readonly { labels: readonly string[] }[],
  extra: readonly string[] = [],
): string[] {
  const set = new Set<string>(extra);
  for (const issue of issues) for (const name of issue.labels) set.add(name);
  return [...set].sort((a, b) => a.localeCompare(b));
}

/**
 * Client-side label refinement: `null`/`''` keeps everything, otherwise only
 * issues carrying that exact label name. State filtering is server-side; this
 * narrows the already-fetched page instantly with no refetch.
 */
export function filterByLabel<T extends { labels: readonly string[] }>(
  issues: readonly T[],
  label: string | null,
): T[] {
  if (label === null || label === '') return [...issues];
  return issues.filter((issue) => issue.labels.includes(label));
}

/**
 * Only the builtin tracker accepts issue/label mutations from lab; a forge
 * repo is managed on the forge, so the UI hides every mutating control.
 */
export function canMutateTracker(binding: TrackerBinding): boolean {
  return binding === 'builtin';
}

/** 'YYYY-MM-DD HH:MM' in local time; unparseable input passes through. */
export function formatDateTime(iso: string): string {
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return iso;
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}`;
}
