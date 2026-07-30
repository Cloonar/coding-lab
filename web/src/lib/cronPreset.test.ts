// Cadence preset ↔ cron (issue #247 / ADR-0062). The two directions have to
// agree byte-for-byte, because a Schedule stores only the cron string: a
// decomposition that does not round-trip would rewrite a cadence the operator
// never touched on the next save.

import { describe, expect, it } from 'vitest';
import {
  MONTH_DAY_MAX,
  WEEKDAYS,
  cadenceSummary,
  cronToPreset,
  normalizeWeekdays,
  presetToCron,
  weekdayLabel,
  type CadencePreset,
} from './cronPreset';

describe('presetToCron', () => {
  it('renders a daily preset', () => {
    expect(presetToCron({ mode: 'daily', time: '06:00' })).toBe('0 6 * * *');
    expect(presetToCron({ mode: 'daily', time: '23:59' })).toBe('59 23 * * *');
    expect(presetToCron({ mode: 'daily', time: '00:00' })).toBe('0 0 * * *');
  });

  it('renders a weekly preset with ascending cron weekday numbers', () => {
    expect(presetToCron({ mode: 'weekly', time: '06:30', weekdays: [1, 4] })).toBe('30 6 * * 1,4');
    // Sunday is cron 0, so a Sun+Mon pick leads with 0 — display order (Monday
    // first) is a rendering concern, never the expression's.
    expect(presetToCron({ mode: 'weekly', time: '06:30', weekdays: [1, 0] })).toBe('30 6 * * 0,1');
  });

  it('renders a monthly preset', () => {
    expect(presetToCron({ mode: 'monthly', time: '09:15', day: 1 })).toBe('15 9 1 * *');
    expect(presetToCron({ mode: 'monthly', time: '09:15', day: 28 })).toBe('15 9 28 * *');
  });

  it('renders nothing for a preset that cannot make an expression', () => {
    expect(presetToCron({ mode: 'daily', time: '' })).toBe('');
    expect(presetToCron({ mode: 'daily', time: '25:00' })).toBe('');
    expect(presetToCron({ mode: 'weekly', time: '06:00', weekdays: [] })).toBe('');
    expect(presetToCron({ mode: 'monthly', time: '06:00', day: 31 })).toBe('');
  });
});

describe('cronToPreset', () => {
  it('decomposes the three preset shapes', () => {
    expect(cronToPreset('0 6 * * *')).toEqual({ mode: 'daily', time: '06:00' });
    expect(cronToPreset('30 6 * * 1,4')).toEqual({
      mode: 'weekly',
      time: '06:30',
      weekdays: [1, 4],
    });
    expect(cronToPreset('15 9 1 * *')).toEqual({ mode: 'monthly', time: '09:15', day: 1 });
  });

  it('returns null for an expression no preset renders', () => {
    // The Advanced escape hatch's reason to exist.
    expect(cronToPreset('*/15 * * * *')).toBeNull();
    expect(cronToPreset('0 6 * * 1-5')).toBeNull();
    expect(cronToPreset('0 6 1 1 *')).toBeNull(); // a month restriction
    expect(cronToPreset('0 6 1 * 1')).toBeNull(); // day-of-month AND weekday
    expect(cronToPreset('0 6 29 * *')).toBeNull(); // past the monthly range
    expect(cronToPreset('0 6 * * *  extra')).toBeNull();
    expect(cronToPreset('0 6 * *')).toBeNull();
    expect(cronToPreset('')).toBeNull();
  });

  it('keeps a hand-written expression in Advanced rather than normalizing it', () => {
    // Both of these MEAN a preset, but neither is what presetToCron emits, so
    // recognizing them would silently rewrite the stored cadence on save.
    expect(cronToPreset('05 6 * * *')).toBeNull(); // padded minute
    expect(cronToPreset('30 6 * * 4,1')).toBeNull(); // weekdays out of order
    expect(cronToPreset('30 6 * * 1,1')).toBeNull(); // a repeated weekday
  });

  it('rejects out-of-range numbers', () => {
    expect(cronToPreset('60 6 * * *')).toBeNull();
    expect(cronToPreset('0 24 * * *')).toBeNull();
    expect(cronToPreset('0 6 0 * *')).toBeNull();
    expect(cronToPreset('0 6 * * 7')).toBeNull();
  });
});

describe('preset round-trip', () => {
  const presets: CadencePreset[] = [
    { mode: 'daily', time: '00:00' },
    { mode: 'daily', time: '06:00' },
    { mode: 'daily', time: '23:59' },
    { mode: 'weekly', time: '06:30', weekdays: [1] },
    { mode: 'weekly', time: '06:30', weekdays: [1, 4] },
    { mode: 'weekly', time: '18:05', weekdays: [0, 1, 2, 3, 4, 5, 6] },
    { mode: 'monthly', time: '09:15', day: 1 },
    { mode: 'monthly', time: '09:15', day: MONTH_DAY_MAX },
  ];

  it('preset → cron → the same preset', () => {
    for (const preset of presets) {
      expect(cronToPreset(presetToCron(preset))).toEqual(preset);
    }
  });

  it('cron → preset → the same cron', () => {
    for (const preset of presets) {
      const expr = presetToCron(preset);
      const decomposed = cronToPreset(expr);
      expect(decomposed).not.toBeNull();
      expect(presetToCron(decomposed as CadencePreset)).toBe(expr);
    }
  });
});

describe('normalizeWeekdays', () => {
  it('sorts, de-duplicates and drops values off the week', () => {
    expect(normalizeWeekdays([4, 1, 4])).toEqual([1, 4]);
    expect(normalizeWeekdays([0, 6])).toEqual([0, 6]);
    expect(normalizeWeekdays([7, -1, 2.5, 3])).toEqual([3]);
  });
});

describe('weekdayLabel', () => {
  it('names the cron weekday numbers and nothing else', () => {
    expect(weekdayLabel(0)).toBe('Sun');
    expect(weekdayLabel(1)).toBe('Mon');
    expect(weekdayLabel(9)).toBe('');
    expect(WEEKDAYS.map((day) => day.value)).toEqual([1, 2, 3, 4, 5, 6, 0]);
  });
});

describe('cadenceSummary', () => {
  it('reads a recognized cadence back in words, weekdays in display order', () => {
    expect(cadenceSummary('0 6 * * *')).toBe('Daily at 06:00');
    expect(cadenceSummary('30 6 * * 1,4')).toBe('Weekly on Mon, Thu at 06:30');
    // Cron leads with Sunday; the summary leads with Monday.
    expect(cadenceSummary('30 6 * * 0,1')).toBe('Weekly on Mon, Sun at 06:30');
    expect(cadenceSummary('15 9 1 * *')).toBe('Monthly on day 1 at 09:15');
  });

  it('falls back to the raw expression when nothing decomposes', () => {
    expect(cadenceSummary('*/15 * * * *')).toBe('*/15 * * * *');
    expect(cadenceSummary('  0 6 * * 1-5 ')).toBe('0 6 * * 1-5');
  });
});
