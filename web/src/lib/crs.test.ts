// Diff-line classifier contract: header/meta/hunk/add/remove/context rows
// from a sample unified diff, the in-hunk '---' removal trap, rename labels,
// and the per-file grouping the diff view renders from.

import { describe, expect, it } from 'vitest';
import { classifyDiff, fileHeaderLabel, groupDiffFiles, type DiffRowKind } from './crs';

const SAMPLE_DIFF = [
  'diff --git a/main.go b/main.go',
  'index 1111111..2222222 100644',
  '--- a/main.go',
  '+++ b/main.go',
  '@@ -1,4 +1,5 @@',
  ' package main',
  '',
  '-func old() {}',
  '+func new() {}',
  '+// added',
  '\\ No newline at end of file',
  'diff --git a/docs/old.md b/docs/new.md',
  'similarity index 96%',
  'rename from docs/old.md',
  'rename to docs/new.md',
  '@@ -3,2 +3,2 @@ heading',
  ' context line',
  '--- looks like a header but is a removal',
  '+++ looks like a header but is an addition',
].join('\n');

describe('classifyDiff', () => {
  it('classifies every row of a two-file sample diff', () => {
    const kinds = classifyDiff(SAMPLE_DIFF).map((row) => row.kind);
    const expected: DiffRowKind[] = [
      'file', // diff --git a/main.go b/main.go
      'meta', // index …
      'meta', // --- a/main.go (header, NOT a removal)
      'meta', // +++ b/main.go (header, NOT an addition)
      'hunk', // @@ -1,4 +1,5 @@
      'context', // ' package main'
      'context', // '' (blank context line survives)
      'remove', // -func old() {}
      'add', // +func new() {}
      'add', // +// added
      'meta', // \ No newline at end of file
      'file', // diff --git a/docs/old.md b/docs/new.md
      'meta', // similarity index
      'meta', // rename from
      'meta', // rename to
      'hunk', // @@ -3,2 +3,2 @@ heading
      'context', // ' context line'
      'remove', // '--- …' INSIDE a hunk is a removal, not a header
      'add', // '+++ …' INSIDE a hunk is an addition, not a header
    ];
    expect(kinds).toEqual(expected);
  });

  it('keeps each line text verbatim', () => {
    const rows = classifyDiff(SAMPLE_DIFF);
    expect(rows[0]!.text).toBe('diff --git a/main.go b/main.go');
    expect(rows[7]!.text).toBe('-func old() {}');
  });

  it('answers no rows for an empty diff and drops the trailing newline', () => {
    expect(classifyDiff('')).toEqual([]);
    const rows = classifyDiff('diff --git a/x b/x\n');
    expect(rows).toHaveLength(1);
    expect(rows[0]).toEqual({ kind: 'file', text: 'diff --git a/x b/x' });
  });
});

describe('fileHeaderLabel', () => {
  it('shows the single path when both sides match', () => {
    expect(
      fileHeaderLabel('diff --git a/internal/gitx/crmerge.go b/internal/gitx/crmerge.go'),
    ).toBe('internal/gitx/crmerge.go');
  });

  it('shows old → new for renames', () => {
    expect(fileHeaderLabel('diff --git a/docs/old.md b/docs/new.md')).toBe(
      'docs/old.md → docs/new.md',
    );
  });

  it('passes an unparseable header through raw', () => {
    expect(fileHeaderLabel('diff --git "weird"')).toBe('diff --git "weird"');
  });
});

describe('groupDiffFiles', () => {
  it('groups rows per file with friendly names, headers excluded from rows', () => {
    const groups = groupDiffFiles(classifyDiff(SAMPLE_DIFF));

    expect(groups.map((g) => g.name)).toEqual(['main.go', 'docs/old.md → docs/new.md']);
    expect(groups[0]!.rows).toHaveLength(10);
    expect(groups[1]!.rows).toHaveLength(7);
    expect(groups[0]!.rows.every((row) => row.kind !== 'file')).toBe(true);
    expect(groups[1]!.rows.map((r) => r.kind)).toContain('remove');
  });

  it('tolerates rows before any header via a nameless group', () => {
    const groups = groupDiffFiles([{ kind: 'meta', text: 'stray' }]);
    expect(groups).toEqual([{ name: '', rows: [{ kind: 'meta', text: 'stray' }] }]);
  });

  it('answers no groups for no rows', () => {
    expect(groupDiffFiles([])).toEqual([]);
  });
});
