// sectionInView: the scroll-spy pick behind the repo settings chips and
// outline (issue #61) — pure arithmetic over section tops, no layout needed.

import { describe, expect, it } from 'vitest';
import { sectionInView } from './sectionSpy';

const tops = (...values: number[]) => values.map((top, i) => ({ id: `s${i + 1}`, top }));

describe('sectionInView', () => {
  it('answers undefined for no sections', () => {
    expect(sectionInView([], 100, false)).toBeUndefined();
    expect(sectionInView([], 100, true)).toBeUndefined();
  });

  it('is the first section until one reaches the line', () => {
    expect(sectionInView(tops(300, 900, 1500), 100, false)).toBe('s1');
  });

  it('is the last section whose top has reached the line', () => {
    expect(sectionInView(tops(-700, 90, 800), 100, false)).toBe('s2');
    expect(sectionInView(tops(-1400, -600, 100), 100, false)).toBe('s3');
  });

  it('counts a top exactly on the line as reached', () => {
    expect(sectionInView(tops(-500, 100, 700), 100, false)).toBe('s2');
    expect(sectionInView(tops(-500, 101, 700), 100, false)).toBe('s1');
  });

  it('is the last section once the page is scrolled to its end', () => {
    // The short final section never climbs to the line on its own.
    expect(sectionInView(tops(-900, -200, 400), 100, true)).toBe('s3');
  });
});
