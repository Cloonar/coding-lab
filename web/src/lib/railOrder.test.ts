// Rail ordering (issue #41): attention → working → rest, newest started_at
// first within a group, and a stable tiebreak on equal timestamps.

import { describe, expect, it } from 'vitest';
import type { ConversationState, Instance } from '../api';
import { orderRail, railGroup } from './railOrder';

function inst(id: string, state: ConversationState, startedAt: string): Instance {
  return {
    id,
    repo_id: 'repo_1',
    repo_name: 'proj',
    kind: 'manual',
    provider: 'claude-code',
    issue_number: null,
    branch: `lab/${id}`,
    worktree_path: `/wt/${id}`,
    session_name: `proj~${id}`,
    model: 'opus[1m]',
    effort: 'max',
    deep_link_url: null,
    started_at: startedAt,
    budget_deadline: null,
    ended_at: null,
    outcome: 'active',
    failure_reason: null,
    live: true,
    connecting: false,
    state,
  };
}

describe('railGroup', () => {
  it('buckets needs_input/question as attention, working next, the rest last', () => {
    expect(railGroup('needs_input')).toBe(0);
    expect(railGroup('question')).toBe(0);
    expect(railGroup('working')).toBe(1);
    expect(railGroup('idle')).toBe(2);
    expect(railGroup('ended')).toBe(2);
    expect(railGroup('')).toBe(2);
  });
});

describe('orderRail', () => {
  it('orders attention, then working, then the rest', () => {
    const rows = orderRail([
      inst('idle', 'idle', '2026-07-06T10:00:00Z'),
      inst('working', 'working', '2026-07-06T10:00:00Z'),
      inst('attn', 'needs_input', '2026-07-06T10:00:00Z'),
    ]);
    expect(rows.map((r) => r.id)).toEqual(['attn', 'working', 'idle']);
  });

  it('sorts the newest started_at first within a group', () => {
    const rows = orderRail([
      inst('old', 'working', '2026-07-06T09:00:00Z'),
      inst('new', 'working', '2026-07-06T11:00:00Z'),
      inst('mid', 'working', '2026-07-06T10:00:00Z'),
    ]);
    expect(rows.map((r) => r.id)).toEqual(['new', 'mid', 'old']);
  });

  it('keeps equal-timestamp rows in input order (stable tiebreak)', () => {
    const rows = orderRail([
      inst('a', 'needs_input', '2026-07-06T10:00:00Z'),
      inst('b', 'question', '2026-07-06T10:00:00Z'),
      inst('c', 'needs_input', '2026-07-06T10:00:00Z'),
    ]);
    expect(rows.map((r) => r.id)).toEqual(['a', 'b', 'c']);
  });

  it('does not mutate the input array', () => {
    const input = [
      inst('working', 'working', '2026-07-06T10:00:00Z'),
      inst('attn', 'needs_input', '2026-07-06T10:00:00Z'),
    ];
    const before = input.map((r) => r.id);
    orderRail(input);
    expect(input.map((r) => r.id)).toEqual(before);
  });
});
