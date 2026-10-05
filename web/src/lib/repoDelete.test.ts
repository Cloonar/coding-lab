import { describe, expect, it } from 'vitest';
import type { ParkedEntry } from '../api';
import { deleteConsequences, needsForce, plural, type DeleteFacts } from './repoDelete';

const parked = (branch: string, worktree_path: string): ParkedEntry => ({
  branch,
  worktree_path,
  dirty: false,
  commits_ahead: 1,
  unpushed: 1,
});

function facts(over: Partial<DeleteFacts> = {}): DeleteFacts {
  return {
    cloneStatus: 'ready',
    trackerBinding: 'forge',
    live: 0,
    parked: [],
    schedules: 0,
    secrets: 0,
    ...over,
  };
}

const ALWAYS = "Removes lab's clone, this repository's settings and its run history.";

describe('deleteConsequences', () => {
  it('lists only the clone line for a repo with nothing else attached', () => {
    expect(deleteConsequences(facts())).toEqual([ALWAYS]);
  });

  it('lists every applicable consequence with its count, most disruptive first', () => {
    expect(
      deleteConsequences(
        facts({
          cloneStatus: 'cloning',
          live: 2,
          parked: [parked('afk/57', '/wt/a')],
          schedules: 3,
          secrets: 1,
        }),
      ),
    ).toEqual([
      'Abandons the running clone.',
      'Stops 2 live instances.',
      "Deletes 1 parked branch with lab's clone; its worktree folder stays on disk.",
      'Deletes 3 Schedules and 1 secret.',
      ALWAYS,
    ]);
  });

  it('words parked work truthfully: branches go with the clone, worktree folders stay', () => {
    const line = (p: ParkedEntry[]) => deleteConsequences(facts({ parked: p }))[0];
    expect(line([parked('a', '/wt/a'), parked('b', '/wt/b')])).toBe(
      "Deletes 2 parked branches with lab's clone; their worktree folders stay on disk.",
    );
    expect(line([parked('a', ''), parked('b', '')])).toBe(
      "Deletes 2 parked branches with lab's clone.",
    );
    expect(line([parked('a', '/wt/a'), parked('b', ''), parked('c', '')])).toBe(
      "Deletes 3 parked branches with lab's clone; 1 worktree folder stays on disk.",
    );
  });

  it('names Schedules or secrets alone when only one applies', () => {
    expect(deleteConsequences(facts({ schedules: 1 }))[0]).toBe('Deletes 1 Schedule.');
    expect(deleteConsequences(facts({ secrets: 4 }))[0]).toBe('Deletes 4 secrets.');
  });

  it('leaves out a count that could not be loaded instead of showing 0', () => {
    const lines = deleteConsequences(
      facts({ live: null, parked: null, schedules: null, secrets: null }),
    );
    expect(lines).toEqual([ALWAYS]);
    expect(lines.join(' ')).not.toMatch(/\b0\b/);
  });

  it('mentions the built-in tracker issues for a builtin-bound repo', () => {
    expect(deleteConsequences(facts({ trackerBinding: 'builtin' }))).toEqual([
      "Deletes the issues and change requests kept in lab's built-in tracker.",
      ALWAYS,
    ]);
  });
});

describe('needsForce', () => {
  it('forces when live instances or a running clone would refuse the plain delete', () => {
    expect(needsForce({ cloneStatus: 'ready', live: 0 })).toBe(false);
    expect(needsForce({ cloneStatus: 'ready', live: null })).toBe(false);
    expect(needsForce({ cloneStatus: 'ready', live: 1 })).toBe(true);
    expect(needsForce({ cloneStatus: 'cloning', live: 0 })).toBe(true);
    expect(needsForce({ cloneStatus: 'error', live: null })).toBe(false);
  });
});

describe('plural', () => {
  it('picks the singular for exactly one', () => {
    expect(plural(1, 'secret')).toBe('1 secret');
    expect(plural(0, 'secret')).toBe('0 secrets');
    expect(plural(2, 'parked branch', 'parked branches')).toBe('2 parked branches');
  });
});
