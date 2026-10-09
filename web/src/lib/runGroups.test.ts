// runGroups (issue #76): the Live grouping (Needs you / Working / Idle in
// orderRail's order, empty groups omitted, ended rows dropped), the compact
// age, the state phrases, the outcome words, the ended-run title, and the
// Ended side's by-day grouping (Today / Yesterday / short date, newest first).
// Dates are built from LOCAL components so the day boundaries hold in any TZ.

import { describe, expect, it } from 'vitest';
import type { Instance, Run } from '../api';
import {
  compactAge,
  dayLabel,
  endedAge,
  endedRunTitle,
  groupByDay,
  groupLive,
  liveAge,
  outcomeWord,
  spacedAge,
  statePhrase,
} from './runGroups';

function run(overrides: Partial<Run>): Run {
  return {
    id: 'run_1',
    repo_id: 'repo_1',
    kind: 'manual',
    provider: 'claude-code',
    issue_number: null,
    pull_number: null,
    branch: 'lab/x',
    worktree_path: '/wt/x',
    session_name: 'proj~dom-20260706-1500',
    title: null,
    model: 'opus[1m]',
    effort: 'max',
    remote: true,
    deep_link_url: null,
    started_at: '2026-07-06T15:00:00.000Z',
    budget_deadline: null,
    ended_at: null,
    outcome: 'active',
    failure_reason: null,
    ...overrides,
  };
}

function instance(overrides: Partial<Instance>): Instance {
  return { ...run({}), repo_name: 'proj', live: true, connecting: false, state: '', ...overrides };
}

/** ISO string for a LOCAL wall-clock time. */
function local(y: number, m: number, d: number, h = 12, min = 0): string {
  return new Date(y, m - 1, d, h, min).toISOString();
}

describe('groupLive', () => {
  it('groups Needs you, Working, Idle in that order, newest first within a group', () => {
    const groups = groupLive([
      instance({ id: 'idle_old', state: 'idle', started_at: '2026-07-06T10:00:00Z' }),
      instance({ id: 'work', state: 'working' }),
      instance({ id: 'q', state: 'question', started_at: '2026-07-06T11:00:00Z' }),
      instance({ id: 'idle_new', state: '', started_at: '2026-07-06T12:00:00Z' }),
      instance({ id: 'needs', state: 'needs_input', started_at: '2026-07-06T13:00:00Z' }),
    ]);
    expect(groups.map((g) => [g.key, g.label, g.instances.map((i) => i.id)])).toEqual([
      ['needs-you', 'Needs you', ['needs', 'q']],
      ['working', 'Working', ['work']],
      ['idle', 'Idle', ['idle_new', 'idle_old']],
    ]);
  });

  it('omits empty groups and drops ended runs', () => {
    const groups = groupLive([
      instance({ id: 'work', state: 'working' }),
      instance({ id: 'dead', state: 'needs_input', live: false }),
    ]);
    expect(groups.map((g) => g.key)).toEqual(['working']);
    expect(groupLive([])).toEqual([]);
  });
});

describe('statePhrase / outcomeWord', () => {
  it('names each conversational state', () => {
    expect(statePhrase('needs_input')).toBe('Waiting for you');
    expect(statePhrase('question')).toBe('Asking a question');
    expect(statePhrase('working')).toBe('Working');
    expect(statePhrase('idle')).toBe('Idle');
    expect(statePhrase('')).toBe('Idle');
  });

  it('maps every ended outcome to its chip word', () => {
    expect(outcomeWord('success')).toBe('done');
    expect(outcomeWord('death')).toBe('died');
    expect(outcomeWord('timeout')).toBe('timed out');
    expect(outcomeWord('stopped')).toBe('stopped');
    expect(outcomeWord('escalated')).toBe('escalated');
  });
});

describe('compactAge', () => {
  const now = Date.parse('2026-07-06T15:00:00Z');
  it('reads now / minutes / hours / days', () => {
    expect(compactAge('2026-07-06T14:59:30Z', now)).toBe('now');
    expect(compactAge('2026-07-06T15:05:00Z', now)).toBe('now'); // future: clock skew
    expect(compactAge('2026-07-06T14:56:00Z', now)).toBe('4m');
    expect(compactAge('2026-07-06T12:30:00Z', now)).toBe('2h');
    expect(compactAge('2026-07-03T14:00:00Z', now)).toBe('3d');
  });

  it('is empty for a missing or unparseable timestamp', () => {
    expect(compactAge(null, now)).toBe('');
    expect(compactAge('garbage', now)).toBe('');
    expect(spacedAge(null, now)).toBe('');
  });

  it('has a spaced form for the desktop table', () => {
    expect(spacedAge('2026-07-06T14:59:30Z', now)).toBe('now');
    expect(spacedAge('2026-07-06T14:58:00Z', now)).toBe('2 min');
    expect(spacedAge('2026-07-06T13:30:00Z', now)).toBe('1 h');
    expect(spacedAge('2026-07-03T14:00:00Z', now)).toBe('3 d');
  });

  it('measures a live row from started_at and an ended row from ended_at', () => {
    const r = run({ started_at: '2026-07-06T13:00:00Z', ended_at: '2026-07-06T14:50:00Z' });
    expect(liveAge(r, now)).toBe('2h');
    expect(endedAge(r, now)).toBe('10m');
    expect(endedAge(run({ started_at: '2026-07-06T14:00:00Z', ended_at: null }), now)).toBe('1h');
  });
});

describe('endedRunTitle', () => {
  it('prefers a user title, then AFK #N, then the session label, then the branch', () => {
    expect(endedRunTitle(run({ title: ' Fix it ' }))).toBe('Fix it');
    expect(endedRunTitle(run({ kind: 'afk_manual', issue_number: 7 }))).toBe('AFK #7');
    expect(endedRunTitle(run({}))).toBe('dom · 15:00');
    expect(endedRunTitle(run({ session_name: 'legacy', branch: 'lab/b' }))).toBe('lab/b');
  });
});

describe('groupByDay', () => {
  const now = Date.parse(local(2026, 7, 6, 15));

  it('labels Today, Yesterday, then a short date', () => {
    expect(dayLabel(Date.parse(local(2026, 7, 6, 0, 5)), now)).toBe('Today');
    expect(dayLabel(Date.parse(local(2026, 7, 5, 23, 59)), now)).toBe('Yesterday');
    expect(dayLabel(Date.parse(local(2026, 7, 1)), now)).toBe('Jul 1');
    expect(dayLabel(Date.parse(local(2025, 12, 31)), now)).toBe('Dec 31, 2025');
  });

  it('treats the last day of the previous month as Yesterday', () => {
    const firstOfMonth = Date.parse(local(2026, 8, 1, 9));
    expect(dayLabel(Date.parse(local(2026, 7, 31, 20)), firstOfMonth)).toBe('Yesterday');
  });

  it('groups by the local day a run ended, newest day and newest run first', () => {
    const groups = groupByDay(
      [
        run({ id: 'y', ended_at: local(2026, 7, 5, 9) }),
        run({ id: 't_early', ended_at: local(2026, 7, 6, 8) }),
        run({ id: 'old', ended_at: local(2026, 6, 30, 9) }),
        run({ id: 't_late', ended_at: local(2026, 7, 6, 14) }),
        // No ended_at: falls back to started_at.
        run({ id: 'y_started', started_at: local(2026, 7, 5, 18), ended_at: null }),
      ],
      now,
    );
    expect(groups.map((g) => [g.label, g.runs.map((r) => r.id)])).toEqual([
      ['Today', ['t_late', 't_early']],
      ['Yesterday', ['y_started', 'y']],
      ['Jun 30', ['old']],
    ]);
  });
});
