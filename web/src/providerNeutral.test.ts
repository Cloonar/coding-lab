// Provider-neutral grep-guard (issue #51 decision 9 / acceptance criterion):
// no SPA source file may hardcode a provider brand. Provider identity always
// flows from API data (run.provider / the providers list) and all copy from
// display_name / fallback_open — so any /claude/i match in a non-test source
// file under src/ is a regression. There is deliberately NO allowlist: a
// legitimate-looking hit must be refactored to provider metadata instead.

import { describe, expect, it } from 'vitest';

// Every .ts/.tsx under src/ as raw text, excluding tests (whose fixtures may
// legitimately name concrete providers) — and thereby this file itself. Also
// excludes shared test harnesses (issue #194): *.test.ts* files import fixture
// helpers from a sibling harness.tsx that carries the same provider fixtures
// (e.g. RunChat's runchat/harness.tsx) but isn't itself suffixed `.test.ts*`.
const sources = import.meta.glob<string>(
  ['./**/*.ts', './**/*.tsx', '!./**/*.test.ts*', '!./**/harness.tsx'],
  {
    eager: true,
    query: '?raw',
    import: 'default',
  },
);

describe('provider-neutral sources', () => {
  it('scans the real source tree (sanity: the guard must never go blind)', () => {
    const files = Object.keys(sources);
    expect(files.length).toBeGreaterThan(20);
    expect(files).toContain('./api.ts');
    expect(files).toContain('./routes/RunChat.tsx');
    expect(files.some((f) => /\.test\.tsx?$/.test(f))).toBe(false);
  });

  it('contains no hardcoded provider brand (/claude/i) in any non-test source file', () => {
    const hits: string[] = [];
    for (const [file, text] of Object.entries(sources)) {
      text.split('\n').forEach((line, i) => {
        if (/claude/i.test(line)) hits.push(`${file}:${i + 1}: ${line.trim()}`);
      });
    }
    expect(hits).toEqual([]);
  });
});
