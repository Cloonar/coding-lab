// AFK UI decision tables. parseAFKLabel rows are transcribed from the v0
// contract (afk-engine port spec §2.8); the hint/paused/budget rows pin the
// M5 SPA behavior (claimable-count hint, three-strikes banner, D12b budget
// countdown).

import { describe, expect, it } from 'vitest';
import {
  AFK_PAUSE_THRESHOLD,
  afkStartHint,
  budgetRemaining,
  isAFKPaused,
  parseAFKLabel,
} from './afk';

describe('parseAFKLabel (v0 §2.8: afk- prefix, optional auto- marker, n >= 1)', () => {
  const accepted: [string, number, boolean][] = [
    ['afk-7', 7, false],
    ['afk-63', 63, false],
    ['afk-auto-7', 7, true],
    ['afk-auto-12345', 12345, true],
    ['afk-007', 7, false], // Atoi tolerance, v0 parity
  ];

  for (const [label, issue, auto] of accepted) {
    it(`${JSON.stringify(label)} → issue ${issue}, auto ${auto}`, () => {
      expect(parseAFKLabel(label)).toEqual({ issue, auto });
    });
  }

  const rejected: string[] = [
    'afk-x', // non-numeric (user label)
    'afk-feature', // user label
    'afk-', // no number
    'afk-0', // n < 1
    'afk--3', // negative
    'afk-auto-', // marker without number
    'afk-auto-x', // marker with garbage
    'afk-auto-0', // marker with n < 1
    'afk-7/x', // not a bare number
    'lab-7', // wrong prefix
    'debug-20260608-1530', // manual label
    '20260608-1530', // bare timestamp
    'afk', // prefix incomplete
    '',
  ];

  for (const label of rejected) {
    it(`rejects ${JSON.stringify(label)}`, () => {
      expect(parseAFKLabel(label)).toBeNull();
    });
  }
});

describe('afkStartHint (claimable count → suffix + greyed hint)', () => {
  const rows: [number | null, string, boolean][] = [
    [3, ' (3 ready)', false],
    [1, ' (1 ready)', false],
    [0, ' (0 ready)', true], // greyed hint only — still a real enabled button (v0 morph lesson)
    [null, '', false], // unknown → plain, never blocked on a hint
  ];

  for (const [count, suffix, greyed] of rows) {
    it(`${String(count)} → ${JSON.stringify(suffix)} greyed=${String(greyed)}`, () => {
      expect(afkStartHint(count)).toEqual({ suffix, greyed });
    });
  }
});

describe('isAFKPaused (three strikes, boundary >=)', () => {
  it('threshold is 3', () => {
    expect(AFK_PAUSE_THRESHOLD).toBe(3);
  });

  const rows: [number, boolean][] = [
    [0, false],
    [1, false],
    [2, false], // threshold-1 still launches (v0 belowThresholdStillLaunches)
    [3, true], // the third failure pauses
    [4, true],
  ];

  for (const [failures, want] of rows) {
    it(`${failures} failures → paused=${String(want)}`, () => {
      expect(isAFKPaused(failures)).toBe(want);
    });
  }
});

describe('budgetRemaining (runs.budget_deadline countdown)', () => {
  const now = Date.parse('2026-07-06T12:00:00.000Z');
  const at = (offsetMinutes: number) =>
    new Date(now + offsetMinutes * 60_000).toISOString().replace(/\.\d{3}Z$/, '.000Z');

  it('no deadline (manual runs) → null', () => {
    expect(budgetRemaining(null, now)).toBeNull();
  });

  it('unparseable deadline → null', () => {
    expect(budgetRemaining('not-a-time', now)).toBeNull();
  });

  it('72 minutes left → "~1h 12m left"', () => {
    expect(budgetRemaining(at(72), now)).toBe('~1h 12m left');
  });

  it('exactly 2h left → "~2h 0m left"', () => {
    expect(budgetRemaining(at(120), now)).toBe('~2h 0m left');
  });

  it('12 minutes left → "~12m left"', () => {
    expect(budgetRemaining(at(12), now)).toBe('~12m left');
  });

  it('30 seconds left → "<1m left"', () => {
    expect(budgetRemaining(new Date(now + 30_000).toISOString(), now)).toBe('<1m left');
  });

  it('deadline hit or passed → "over budget" (reaper boundary is >=)', () => {
    expect(budgetRemaining(at(0), now)).toBe('over budget');
    expect(budgetRemaining(at(-5), now)).toBe('over budget');
  });
});
