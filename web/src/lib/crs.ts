// Pure change-request view logic: a stateful unified-diff line classifier
// (git diff text → typed rows), per-file grouping for the phone-first diff
// view, and the friendly `diff --git` header label. Kept here (not in the
// component) so the row classification is unit-tested against real git
// output shapes — including the trap: a removed line whose content itself
// starts with '---' must tint as a removal, not read as a file header.

/** One rendered diff row. `file` rows get the distinct sticky-ish header. */
export type DiffRowKind = 'file' | 'hunk' | 'add' | 'remove' | 'context' | 'meta';

export interface DiffRow {
  kind: DiffRowKind;
  text: string;
}

/**
 * Classifies each line of a unified diff. Stateful on purpose: '+'/'-' only
 * mean add/remove INSIDE a hunk (after '@@'), so the '--- a/…' / '+++ b/…'
 * header lines stay meta while a removed '---…' content line inside a hunk
 * still classifies as a removal. Everything between a 'diff --git' header
 * and the first hunk (index, mode, rename/copy, Binary files) is meta;
 * '\ No newline at end of file' markers inside hunks are meta too.
 */
export function classifyDiff(diff: string): DiffRow[] {
  if (diff === '') return [];
  const lines = diff.split('\n');
  if (lines[lines.length - 1] === '') lines.pop(); // trailing newline
  const rows: DiffRow[] = [];
  let inHunk = false;
  for (const line of lines) {
    let kind: DiffRowKind;
    if (line.startsWith('diff --git ')) {
      kind = 'file';
      inHunk = false;
    } else if (line.startsWith('@@')) {
      kind = 'hunk';
      inHunk = true;
    } else if (!inHunk) {
      kind = 'meta';
    } else if (line.startsWith('+')) {
      kind = 'add';
    } else if (line.startsWith('-')) {
      kind = 'remove';
    } else if (line.startsWith('\\')) {
      kind = 'meta';
    } else {
      kind = 'context';
    }
    rows.push({ kind, text: line });
  }
  return rows;
}

/**
 * 'diff --git a/x b/y' → 'x', or 'x → y' when the paths differ (a rename or
 * copy under the server's -M detection). Unparseable headers pass through
 * raw so nothing is ever hidden.
 */
export function fileHeaderLabel(line: string): string {
  const m = /^diff --git a\/(.+) b\/(.+)$/.exec(line);
  if (m === null) return line;
  return m[1] === m[2] ? m[1]! : `${m[1]!} → ${m[2]!}`;
}

export interface DiffFileGroup {
  /** Friendly header label; '' when rows precede any 'diff --git' line. */
  name: string;
  /** The file's rows, header line excluded (it became `name`). */
  rows: DiffRow[];
}

/**
 * Splits classified rows into per-file groups at each 'file' row, so the
 * view can render one sticky-ish header plus one horizontal-scroll container
 * per file. Rows before the first header (never produced by `git diff`, but
 * tolerated) land in a nameless group.
 */
export function groupDiffFiles(rows: readonly DiffRow[]): DiffFileGroup[] {
  const groups: DiffFileGroup[] = [];
  let current: DiffFileGroup | null = null;
  for (const row of rows) {
    if (row.kind === 'file') {
      current = { name: fileHeaderLabel(row.text), rows: [] };
      groups.push(current);
      continue;
    }
    if (current === null) {
      current = { name: '', rows: [] };
      groups.push(current);
    }
    current.rows.push(row);
  }
  return groups;
}
