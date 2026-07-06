// Issues-list filter logic and the binding gate, tested against the pinned
// M4 contract: ready queue = the ready-for-agent label, label narrowing is
// client-side over the fetched state page, and only the builtin tracker
// accepts mutations from lab.

import { describe, expect, it } from 'vitest';
import {
  READY_LABEL,
  availableLabelNames,
  canMutateTracker,
  filterByLabel,
  formatDateTime,
  hasReadyLabel,
} from './issues';

describe('hasReadyLabel', () => {
  it('matches only the exact ready-for-agent label name', () => {
    expect(READY_LABEL).toBe('ready-for-agent');
    expect(hasReadyLabel(['bug', 'ready-for-agent'])).toBe(true);
    expect(hasReadyLabel(['ready-for-human'])).toBe(false);
    expect(hasReadyLabel(['Ready-For-Agent'])).toBe(false); // labels are case-sensitive
    expect(hasReadyLabel([])).toBe(false);
  });
});

describe('availableLabelNames', () => {
  const issues = [
    { labels: ['bug', 'ui'] },
    { labels: ['bug'] },
    { labels: [] },
    { labels: ['ready-for-agent'] },
  ];

  it('unions, de-duplicates and sorts every label across the issues', () => {
    expect(availableLabelNames(issues)).toEqual(['bug', 'ready-for-agent', 'ui']);
  });

  it('keeps the extra names so an active filter chip never vanishes', () => {
    // The 'wontfix' filter matched zero issues on this page — its chip must
    // survive so the operator can un-toggle it.
    expect(availableLabelNames(issues, ['wontfix'])).toEqual([
      'bug',
      'ready-for-agent',
      'ui',
      'wontfix',
    ]);
    // ...without duplicating names that are present anyway.
    expect(availableLabelNames(issues, ['bug'])).toEqual(['bug', 'ready-for-agent', 'ui']);
  });

  it('yields nothing for label-less pages', () => {
    expect(availableLabelNames([])).toEqual([]);
    expect(availableLabelNames([{ labels: [] }])).toEqual([]);
  });
});

describe('filterByLabel', () => {
  const issues = [
    { number: 1, labels: ['bug', 'ready-for-agent'] },
    { number: 2, labels: ['ui'] },
    { number: 3, labels: [] },
  ];

  it('keeps everything on null or empty filter', () => {
    expect(filterByLabel(issues, null).map((i) => i.number)).toEqual([1, 2, 3]);
    expect(filterByLabel(issues, '').map((i) => i.number)).toEqual([1, 2, 3]);
  });

  it('narrows to issues carrying the exact label', () => {
    expect(filterByLabel(issues, 'bug').map((i) => i.number)).toEqual([1]);
    expect(filterByLabel(issues, 'ui').map((i) => i.number)).toEqual([2]);
  });

  it('matches nothing for an unknown label', () => {
    expect(filterByLabel(issues, 'wontfix')).toEqual([]);
  });

  it('returns a copy — the source array is never mutated', () => {
    const copy = filterByLabel(issues, null);
    expect(copy).not.toBe(issues);
    expect(copy).toEqual(issues);
  });
});

describe('canMutateTracker (the binding gate)', () => {
  it('only the builtin tracker accepts lab-side mutations', () => {
    expect(canMutateTracker('builtin')).toBe(true);
    expect(canMutateTracker('forge')).toBe(false);
  });
});

describe('formatDateTime', () => {
  it('renders local YYYY-MM-DD HH:MM', () => {
    const iso = '2026-07-06T09:05:00.000Z';
    const t = new Date(iso);
    const pad = (n: number) => String(n).padStart(2, '0');
    const expected = `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}`;
    expect(formatDateTime(iso)).toBe(expected);
  });

  it('passes unparseable input through untouched', () => {
    expect(formatDateTime('not-a-date')).toBe('not-a-date');
    expect(formatDateTime('')).toBe('');
  });
});
