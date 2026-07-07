// Hand-rolled markdown → node tree for the embedded chat (issue #13). No
// library, no new npm dep: the parser emits a plain data tree the view maps to
// Solid JSX nodes (never an HTML string, never innerHTML), so rendering is
// XSS-safe by construction and needs no sanitizer. This matches the repo's
// twice-documented "no markdown engine — no heavy deps" ethos
// (web/src/routes/IssueDetail.tsx).
//
// Hard requirement (decision 6): transcript text arrives PARTIALLY during live
// streaming — an unclosed fence, a dangling `**`, `[text](` with no closing
// paren, a half-streamed table. Every branch must degrade to visible plain
// text: never throw, never drop a character. The parser is total — for any
// input string, concatenating the text of the emitted tree (plus the syntax it
// consumed) reproduces something the reader can read. The guard against silent
// loss is the block scanner always advancing and inline fallbacks emitting the
// literal delimiter rather than eating it.

/** Inline (span-level) nodes. */
export type Inline =
  | { type: 'text'; value: string }
  | { type: 'strong'; children: Inline[] }
  | { type: 'em'; children: Inline[] }
  | { type: 'code'; value: string }
  | { type: 'link'; href: string; children: Inline[] }
  | { type: 'break' };

export type Align = 'left' | 'center' | 'right' | 'none';

/** Block-level nodes. */
export type Block =
  | { type: 'heading'; level: number; children: Inline[] }
  | { type: 'paragraph'; children: Inline[] }
  | { type: 'code'; lang: string; text: string }
  | { type: 'list'; ordered: boolean; start: number; items: Block[][] }
  | { type: 'blockquote'; children: Block[] }
  | { type: 'table'; header: Inline[][]; align: Align[]; rows: Inline[][][] }
  | { type: 'hr' };

// Nesting is bounded by strictly-increasing content indentation, but a
// pathological input could still recurse deeply; cap it and degrade the tail to
// a paragraph rather than risk a stack blow-up.
const MAX_DEPTH = 12;

/** Parse markdown source into a block tree. Total: never throws. */
export function parseMarkdown(src: string): Block[] {
  if (src === '') return [];
  // Normalise line endings; tabs → 4 spaces so indentation math is uniform.
  const lines = src.replace(/\r\n?/g, '\n').replace(/\t/g, '    ').split('\n');
  return parseBlocks(lines, 0);
}

function parseBlocks(lines: string[], depth: number): Block[] {
  const blocks: Block[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i] ?? '';

    // Blank lines separate blocks.
    if (line.trim() === '') {
      i += 1;
      continue;
    }

    if (depth >= MAX_DEPTH) {
      // Too deep to keep structuring — flush the remainder as one paragraph so
      // nothing is dropped.
      const rest = lines.slice(i).join('\n');
      blocks.push({ type: 'paragraph', children: parseInline(rest) });
      break;
    }

    // Fenced code block: ``` or ~~~ (>=3), matching closer or EOF.
    const fence = matchFenceOpen(line);
    if (fence !== null) {
      const body: string[] = [];
      let j = i + 1;
      let closed = false;
      for (; j < lines.length; j += 1) {
        if (matchFenceClose(lines[j] ?? '', fence.char, fence.len)) {
          closed = true;
          break;
        }
        body.push(lines[j] ?? '');
      }
      blocks.push({ type: 'code', lang: fence.lang, text: body.join('\n') });
      // Consume through the closing fence; an unterminated fence swallows the
      // rest (closed=false → j is at EOF), which is the graceful degrade.
      i = closed ? j + 1 : j;
      continue;
    }

    // Horizontal rule: a line of only -, *, or _ (>=3, spaces allowed).
    if (isHr(line)) {
      blocks.push({ type: 'hr' });
      i += 1;
      continue;
    }

    // ATX heading: 1–6 leading #, then a space (or end of line).
    const heading = matchHeading(line);
    if (heading !== null) {
      blocks.push({ type: 'heading', level: heading.level, children: parseInline(heading.text) });
      i += 1;
      continue;
    }

    // Blockquote: consecutive lines starting with `>`.
    if (/^ {0,3}>/.test(line)) {
      const inner: string[] = [];
      let j = i;
      for (; j < lines.length; j += 1) {
        const l = lines[j] ?? '';
        if (!/^ {0,3}>/.test(l)) break;
        inner.push(l.replace(/^ {0,3}>( ?)/, ''));
      }
      blocks.push({ type: 'blockquote', children: parseBlocks(inner, depth + 1) });
      i = j;
      continue;
    }

    // GFM table: a `|`-bearing header line immediately followed by a delimiter
    // row (---|:--:|...) with the SAME column count (GFM rejects a mismatch, so
    // `foo | bar` over a lone `---` stays prose).
    if (isTableHead(line, lines[i + 1] ?? '')) {
      const table = parseTable(lines, i);
      blocks.push(table.node);
      i = table.next;
      continue;
    }

    // List: a bullet (-, *, +) or ordered (1. / 1)) marker.
    if (matchMarker(line) !== null) {
      const list = parseList(lines, i, depth);
      blocks.push(list.node);
      i = list.next;
      continue;
    }

    // Paragraph: gather consecutive lines until a blank or a line that starts a
    // new block. Keep internal newlines as soft breaks.
    const para: string[] = [];
    let j = i;
    for (; j < lines.length; j += 1) {
      const l = lines[j] ?? '';
      if (l.trim() === '') break;
      if (j > i && startsNewBlock(l, lines[j + 1] ?? '')) break;
      para.push(l);
    }
    blocks.push({ type: 'paragraph', children: parseInline(para.join('\n')) });
    i = j;
  }
  return blocks;
}

/** True when a line (as a paragraph continuation) actually opens a new block. */
function startsNewBlock(line: string, next: string): boolean {
  return (
    matchFenceOpen(line) !== null ||
    isHr(line) ||
    matchHeading(line) !== null ||
    /^ {0,3}>/.test(line) ||
    matchMarker(line) !== null ||
    isTableHead(line, next)
  );
}

// --- Block matchers -------------------------------------------------------

function matchFenceOpen(line: string): { char: '`' | '~'; len: number; lang: string } | null {
  const m = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(line);
  if (m === null) return null;
  const fence = m[1] ?? '';
  const info = (m[2] ?? '').trim();
  // A ``` info string may not itself contain a backtick (CommonMark) — guards
  // against `a ``` inline b` being read as a fence open.
  if (fence[0] === '`' && info.includes('`')) return null;
  return { char: fence[0] as '`' | '~', len: fence.length, lang: info.split(/\s+/)[0] ?? '' };
}

function matchFenceClose(line: string, char: '`' | '~', len: number): boolean {
  const m = new RegExp(`^ {0,3}(${char === '`' ? '`' : '~'}{${len},})\\s*$`).exec(line);
  return m !== null;
}

function isHr(line: string): boolean {
  const t = line.trim();
  // 3+ of a single -, *, or _, optionally space-separated (--- or * * *).
  return /^-( *-){2,} *$/.test(t) || /^\*( *\*){2,} *$/.test(t) || /^_( *_){2,} *$/.test(t);
}

function matchHeading(line: string): { level: number; text: string } | null {
  const m = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*#*[ \t]*$/.exec(line);
  if (m === null) return null;
  return { level: (m[1] ?? '').length, text: m[2] ?? '' };
}

interface Marker {
  indent: number;
  ordered: boolean;
  start: number;
  width: number; // marker + the single following space, from column `indent`
  content: string;
}

function matchMarker(line: string): Marker | null {
  const bullet = /^( *)([-*+])( +)(.*)$/.exec(line);
  if (bullet !== null) {
    const indent = (bullet[1] ?? '').length;
    const width = (bullet[2] ?? '').length + (bullet[3] ?? '').length;
    return { indent, ordered: false, start: 1, width, content: bullet[4] ?? '' };
  }
  const ordered = /^( *)(\d{1,9})([.)])( +)(.*)$/.exec(line);
  if (ordered !== null) {
    const indent = (ordered[1] ?? '').length;
    const width = (ordered[2] ?? '').length + (ordered[3] ?? '').length + (ordered[4] ?? '').length;
    return {
      indent,
      ordered: true,
      start: Number(ordered[2]),
      width,
      content: ordered[5] ?? '',
    };
  }
  return null;
}

function isTableDelimiter(line: string): boolean {
  if (!line.includes('-')) return false;
  const cells = splitRow(line);
  if (cells.length === 0) return false;
  return cells.every((c) => /^:?-+:?$/.test(c.trim()));
}

/** A header line + a matching delimiter row (equal column count) open a table. */
function isTableHead(header: string, delim: string): boolean {
  if (!header.includes('|')) return false;
  if (!isTableDelimiter(delim)) return false;
  return splitRow(header).length === splitRow(delim).length;
}

/** Split a table row on unescaped pipes, dropping the leading/trailing empties. */
function splitRow(line: string): string[] {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  const cells: string[] = [];
  let buf = '';
  for (let k = 0; k < s.length; k += 1) {
    const ch = s[k];
    if (ch === '\\' && s[k + 1] === '|') {
      buf += '|';
      k += 1;
    } else if (ch === '|') {
      cells.push(buf);
      buf = '';
    } else {
      buf += ch;
    }
  }
  cells.push(buf);
  return cells;
}

function parseTable(lines: string[], start: number): { node: Block; next: number } {
  const header = splitRow(lines[start] ?? '');
  const align = splitRow(lines[start + 1] ?? '').map<Align>((c) => {
    const t = c.trim();
    const left = t.startsWith(':');
    const right = t.endsWith(':');
    if (left && right) return 'center';
    if (right) return 'right';
    if (left) return 'left';
    return 'none';
  });
  const rows: Inline[][][] = [];
  let i = start + 2;
  for (; i < lines.length; i += 1) {
    const l = lines[i] ?? '';
    if (l.trim() === '' || !l.includes('|')) break;
    const cells = splitRow(l);
    // Pad a short row up to the header width (rectangular grid) but never DROP
    // cells a long row carries — dropping them would violate the totality
    // invariant (a streamed row with extra pipes must not lose content).
    const cols = Math.max(header.length, cells.length);
    const row: Inline[][] = [];
    for (let c = 0; c < cols; c += 1) row.push(parseInline((cells[c] ?? '').trim()));
    rows.push(row);
  }
  return {
    node: {
      type: 'table',
      header: header.map((c) => parseInline(c.trim())),
      align,
      rows,
    },
    next: i,
  };
}

function parseList(lines: string[], start: number, depth: number): { node: Block; next: number } {
  const first = matchMarker(lines[start] ?? '');
  // `first` is guaranteed non-null by the caller's guard.
  const base = first as Marker;
  const ordered = base.ordered;
  const items: string[][] = [];
  let current: { contentIndent: number; lines: string[] } | null = null;
  let i = start;

  const flush = () => {
    if (current !== null) items.push(current.lines);
    current = null;
  };

  while (i < lines.length) {
    const line = lines[i] ?? '';
    const mk = matchMarker(line);
    // A sibling marker of the same kind at (roughly) the base indent starts a
    // new item; a more-indented marker is nested and handled by re-parsing the
    // item's gathered lines.
    if (mk !== null && mk.ordered === ordered && mk.indent < base.indent + base.width) {
      flush();
      current = { contentIndent: mk.indent + mk.width, lines: [mk.content] };
      i += 1;
      continue;
    }
    if (line.trim() === '') {
      // A blank line stays inside the list only if the list continues after it;
      // otherwise it ends the list (left for the outer scanner to skip).
      const next = lines[i + 1] ?? '';
      const nm = matchMarker(next);
      const continues =
        next.trim() !== '' &&
        ((nm !== null && nm.ordered === ordered && nm.indent < base.indent + base.width) ||
          (current !== null && leadingSpaces(next) >= current.contentIndent));
      if (!continues) break;
      if (current !== null) current.lines.push('');
      i += 1;
      continue;
    }
    if (current !== null && leadingSpaces(line) >= current.contentIndent) {
      current.lines.push(line.slice(current.contentIndent));
      i += 1;
      continue;
    }
    // A dedented, non-marker line ends the list.
    break;
  }
  flush();

  return {
    node: {
      type: 'list',
      ordered,
      start: base.start,
      items: items.map((itemLines) => parseBlocks(itemLines, depth + 1)),
    },
    next: i,
  };
}

function leadingSpaces(line: string): number {
  let n = 0;
  while (line[n] === ' ') n += 1;
  return n;
}

// --- Inline parsing -------------------------------------------------------

const ALLOWED_SCHEME = /^(https?:|mailto:)/i;
// A bare URL run — stops at whitespace and the ASCII delimiters that usually
// bound a URL in prose; trailing sentence punctuation is trimmed afterwards.
const BARE_URL = /^https?:\/\/[^\s<>()[\]]+/i;

/** Parse an inline string into span nodes. Total: never throws. */
export function parseInline(text: string): Inline[] {
  const nodes: Inline[] = [];
  let buf = '';
  const flush = () => {
    if (buf !== '') {
      nodes.push({ type: 'text', value: buf });
      buf = '';
    }
  };
  let i = 0;
  while (i < text.length) {
    const ch = text[i] as string;

    if (ch === '\n') {
      flush();
      nodes.push({ type: 'break' });
      i += 1;
      continue;
    }

    // Backtick code span (highest precedence): a run of N backticks closes at
    // the next run of exactly N. No closer → the run is literal.
    if (ch === '`') {
      const span = matchCodeSpan(text, i);
      if (span !== null) {
        flush();
        nodes.push({ type: 'code', value: span.value });
        i = span.next;
        continue;
      }
    }

    // Explicit link [text](url) with an allowed scheme.
    if (ch === '[') {
      const link = matchLink(text, i);
      if (link !== null) {
        flush();
        nodes.push(link.node);
        i = link.next;
        continue;
      }
    }

    // Bare autolink at a word boundary.
    if ((ch === 'h' || ch === 'H') && (i === 0 || !isWordChar(text[i - 1]))) {
      const auto = matchAutolink(text, i);
      if (auto !== null) {
        flush();
        nodes.push(auto.node);
        i = auto.next;
        continue;
      }
    }

    // Emphasis / strong.
    if (ch === '*' || ch === '_') {
      const emph = matchEmphasis(text, i);
      if (emph !== null) {
        flush();
        nodes.push(emph.node);
        i = emph.next;
        continue;
      }
    }

    buf += ch;
    i += 1;
  }
  flush();
  return nodes;
}

function matchCodeSpan(text: string, start: number): { value: string; next: number } | null {
  let len = 0;
  while (text[start + len] === '`') len += 1;
  const open = start + len;
  // Find a closing run of exactly `len` backticks.
  let i = open;
  while (i < text.length) {
    if (text[i] === '`') {
      let run = 0;
      while (text[i + run] === '`') run += 1;
      if (run === len) {
        const raw = text.slice(open, i);
        // CommonMark: strip one leading+trailing space when the content is not
        // all spaces (lets `` `code` `` wrap a backtick).
        const value =
          raw.length > 1 && raw.startsWith(' ') && raw.endsWith(' ') && raw.trim() !== ''
            ? raw.slice(1, -1)
            : raw;
        return { value, next: i + run };
      }
      i += run;
      continue;
    }
    i += 1;
  }
  return null;
}

function matchLink(text: string, start: number): { node: Inline; next: number } | null {
  // Balanced-ish scan for the closing ] of the label.
  let depth = 0;
  let close = -1;
  for (let i = start; i < text.length; i += 1) {
    const c = text[i];
    if (c === '\\') {
      i += 1;
      continue;
    }
    if (c === '[') depth += 1;
    else if (c === ']') {
      depth -= 1;
      if (depth === 0) {
        close = i;
        break;
      }
    }
  }
  if (close === -1 || text[close + 1] !== '(') return null;
  // Destination: up to the matching close paren, no nested parens.
  const parenOpen = close + 1;
  let paren = -1;
  for (let i = parenOpen + 1; i < text.length; i += 1) {
    const c = text[i];
    if (c === '\\') {
      i += 1;
      continue;
    }
    if (c === ')') {
      paren = i;
      break;
    }
    // A URL never contains whitespace; bail early so a stray `](` in prose does
    // not swallow the rest of the paragraph.
    if (c === '\n') return null;
  }
  if (paren === -1) return null;
  const href = text.slice(parenOpen + 1, paren).trim();
  if (!ALLOWED_SCHEME.test(href)) return null;
  const label = text.slice(start + 1, close);
  return {
    node: { type: 'link', href, children: parseInlineNoLink(label) },
    next: paren + 1,
  };
}

function matchAutolink(text: string, start: number): { node: Inline; next: number } | null {
  const m = BARE_URL.exec(text.slice(start));
  if (m === null) return null;
  let url = m[0];
  // Trim trailing sentence punctuation; keep a `)` only if balanced within.
  let trimmed = url.replace(/[.,;:!?'"]+$/, '');
  while (trimmed.endsWith(')') && countChar(trimmed, '(') < countChar(trimmed, ')')) {
    trimmed = trimmed.slice(0, -1);
  }
  url = trimmed;
  if (url.length <= 'https://'.length) return null;
  return {
    node: { type: 'link', href: url, children: [{ type: 'text', value: url }] },
    next: start + url.length,
  };
}

function matchEmphasis(text: string, start: number): { node: Inline; next: number } | null {
  const marker = text[start] as string;
  // Underscore emphasis is intraword-safe: `foo_bar_baz` stays literal.
  if (marker === '_' && isWordChar(text[start - 1])) return null;

  let run = 0;
  while (text[start + run] === marker) run += 1;
  const markerLen = run >= 2 ? 2 : 1;
  const contentStart = start + markerLen;
  // An opener must be followed by a non-space (no `** foo`).
  if (isSpace(text[contentStart])) return null;

  // Scan for the closer, skipping code spans so `*a `b*c` d*` is not fooled.
  let i = contentStart;
  while (i < text.length) {
    const c = text[i];
    if (c === '`') {
      const span = matchCodeSpan(text, i);
      if (span !== null) {
        i = span.next;
        continue;
      }
    }
    if (c === '\n') break; // emphasis does not span a hard line break
    if (c === marker) {
      let crun = 0;
      while (text[i + crun] === marker) crun += 1;
      if (crun >= markerLen) {
        // A valid closer is preceded by a non-space and (for `_`) not inside a
        // word.
        const prev = text[i - 1];
        const okClose = !isSpace(prev) && !(marker === '_' && isWordChar(text[i + markerLen]));
        if (okClose && i > contentStart) {
          const inner = text.slice(contentStart, i);
          const node: Inline =
            markerLen === 2
              ? { type: 'strong', children: parseInline(inner) }
              : { type: 'em', children: parseInline(inner) };
          return { node, next: i + markerLen };
        }
      }
      i += crun;
      continue;
    }
    i += 1;
  }
  return null;
}

/** parseInline variant that never opens links (used inside a link label). */
function parseInlineNoLink(text: string): Inline[] {
  // Neutralise `[` so a nested link can't form, then parse normally; the
  // brackets still render as literal text.
  return parseInline(text).map(stripLinks);
}

function stripLinks(node: Inline): Inline {
  if (node.type === 'link') {
    // A link nested inside a link label degrades to LITERAL source text (never
    // a nested <a>), reconstructed so no character is dropped — but an autolink
    // (label === href) needs no surrounding brackets.
    const label = flattenText(node.children);
    return { type: 'text', value: label === node.href ? label : `[${label}](${node.href})` };
  }
  if (node.type === 'strong' || node.type === 'em') {
    return { ...node, children: node.children.map(stripLinks) };
  }
  return node;
}

function flattenText(nodes: Inline[]): string {
  let out = '';
  for (const n of nodes) {
    if (n.type === 'text' || n.type === 'code') out += n.value;
    else if (n.type === 'break') out += '\n';
    else if (n.type === 'strong' || n.type === 'em') out += flattenText(n.children);
    else if (n.type === 'link') out += flattenText(n.children);
  }
  return out;
}

function isWordChar(ch: string | undefined): boolean {
  return ch !== undefined && /[A-Za-z0-9]/.test(ch);
}
function isSpace(ch: string | undefined): boolean {
  return ch === undefined || ch === ' ' || ch === '\t' || ch === '\n';
}
function countChar(s: string, ch: string): number {
  let n = 0;
  for (const c of s) if (c === ch) n += 1;
  return n;
}
