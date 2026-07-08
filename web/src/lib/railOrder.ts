// Attention-first ordering for the side rail's ACTIVE list (issue #41). Three
// groups sort in order: attention (needs_input / question — the accent dot),
// then working, then the rest; within a group the newest run (by started_at)
// comes first. The API exposes no per-run last-activity timestamp, so
// last-activity ordering is out of scope — started_at is the only stable cursor.

import type { ConversationState, Instance } from '../api';

/** 0 = attention (waiting on you), 1 = working, 2 = everything else. */
export function railGroup(state: ConversationState): 0 | 1 | 2 {
  if (state === 'needs_input' || state === 'question') return 0;
  if (state === 'working') return 1;
  return 2;
}

function startedAtMs(instance: Instance): number {
  const t = Date.parse(instance.started_at);
  return Number.isNaN(t) ? 0 : t;
}

/**
 * Orders instances for the rail: attention, then working, then rest; newest
 * started_at first within each group. Pure — returns a new array and never
 * mutates the input; the sort is stable, so equal-timestamp rows keep their
 * input order.
 */
export function orderRail(instances: Instance[]): Instance[] {
  return [...instances].sort((a, b) => {
    const group = railGroup(a.state) - railGroup(b.state);
    if (group !== 0) return group;
    return startedAtMs(b) - startedAtMs(a);
  });
}
