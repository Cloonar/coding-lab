// parseMarkdown/parseInline contract (issue #13): a hand-rolled, zero-dep
// parser that emits a node tree the chat view maps to Solid JSX. Two guarantees
// under test: (1) the supported constructs parse to the right shape, and (2)
// the parser is TOTAL — every adversarial / partially-streamed input degrades
// to visible text, never throws, never drops a character (decision 6).

import { describe, expect, it } from 'vitest';
import { parseInline, parseMarkdown, type Block, type Inline } from './markdown';

/** Concatenate every rendered character the tree would show (syntax aside). */
function visibleText(blocks: Block[]): string {
  const inline = (nodes: Inline[]): string =>
    nodes
      .map((n) => {
        switch (n.type) {
          case 'text':
          case 'code':
            return n.value;
          case 'break':
            return '\n';
          case 'strong':
          case 'em':
          case 'link':
            return inline(n.children);
        }
      })
      .join('');
  const block = (b: Block): string => {
    switch (b.type) {
      case 'heading':
      case 'paragraph':
        return inline(b.children);
      case 'code':
        return b.text;
      case 'hr':
        return '';
      case 'blockquote':
        return b.children.map(block).join('');
      case 'list':
        return b.items.map((it) => it.map(block).join('')).join('');
      case 'table':
        return [b.header, ...b.rows].map((row) => row.map(inline).join('')).join('');
    }
  };
  return blocks.map(block).join('');
}

describe('parseMarkdown — block constructs', () => {
  it('parses ATX headings by level', () => {
    const blocks = parseMarkdown('# One\n\n### Three');
    expect(blocks[0]).toMatchObject({ type: 'heading', level: 1 });
    expect(blocks[1]).toMatchObject({ type: 'heading', level: 3 });
    expect(visibleText([blocks[0]!])).toBe('One');
  });

  it('does not treat #hashtag (no space) as a heading', () => {
    expect(parseMarkdown('#hashtag')[0]).toMatchObject({ type: 'paragraph' });
    expect(parseMarkdown('####### seven')[0]).toMatchObject({ type: 'paragraph' });
  });

  it('parses a fenced code block and RETAINS the raw source with its language', () => {
    const blocks = parseMarkdown('```ts\nconst x = 1;\n**not bold**\n```');
    expect(blocks[0]).toEqual({ type: 'code', lang: 'ts', text: 'const x = 1;\n**not bold**' });
  });

  it('parses an unterminated fence as code to end of input (no throw, no loss)', () => {
    const blocks = parseMarkdown('```\nline one\nline two');
    expect(blocks[0]).toEqual({ type: 'code', lang: '', text: 'line one\nline two' });
  });

  it('parses bullet and ordered lists', () => {
    const bullet = parseMarkdown('- a\n- b')[0] as Extract<Block, { type: 'list' }>;
    expect(bullet.ordered).toBe(false);
    expect(bullet.items).toHaveLength(2);

    const ordered = parseMarkdown('3. first\n4. second')[0] as Extract<Block, { type: 'list' }>;
    expect(ordered.ordered).toBe(true);
    expect(ordered.start).toBe(3);
    expect(ordered.items).toHaveLength(2);
  });

  it('parses nested lists into nested list blocks', () => {
    const list = parseMarkdown('- outer\n  - inner')[0] as Extract<Block, { type: 'list' }>;
    const firstItem = list.items[0]!;
    expect(firstItem[0]).toMatchObject({ type: 'paragraph' });
    expect(firstItem[1]).toMatchObject({ type: 'list' });
    expect(visibleText([list])).toContain('inner');
  });

  it('parses a GFM table with alignments', () => {
    const table = parseMarkdown('| a | b | c |\n|:--|:-:|--:|\n| 1 | 2 | 3 |')[0] as Extract<
      Block,
      { type: 'table' }
    >;
    expect(table.type).toBe('table');
    expect(table.align).toEqual(['left', 'center', 'right']);
    expect(table.header).toHaveLength(3);
    expect(table.rows).toHaveLength(1);
    expect(visibleText([table])).toContain('123');
  });

  it('does NOT treat prose with a pipe over a lone --- as a table (column mismatch)', () => {
    const blocks = parseMarkdown('foo | bar\n---');
    expect(blocks.every((b) => b.type !== 'table')).toBe(true);
  });

  it('keeps every cell of a data row that has MORE columns than the header', () => {
    const table = parseMarkdown('| a | b |\n|---|---|\n| 1 | 2 | 3 |')[0] as Extract<
      Block,
      { type: 'table' }
    >;
    expect(table.rows[0]).toHaveLength(3); // the extra "3" cell is not dropped
    expect(visibleText([table])).toContain('123');
  });

  it('parses blockquotes recursively', () => {
    const bq = parseMarkdown('> quoted **bold**')[0] as Extract<Block, { type: 'blockquote' }>;
    expect(bq.type).toBe('blockquote');
    expect(bq.children[0]).toMatchObject({ type: 'paragraph' });
  });

  it('parses horizontal rules but not a 2-char dash run', () => {
    expect(parseMarkdown('---')[0]).toEqual({ type: 'hr' });
    expect(parseMarkdown('* * *')[0]).toEqual({ type: 'hr' });
    expect(parseMarkdown('--')[0]).toMatchObject({ type: 'paragraph' });
  });

  it('keeps soft line breaks inside a paragraph', () => {
    const p = parseMarkdown('line one\nline two')[0] as Extract<Block, { type: 'paragraph' }>;
    expect(p.children.some((n) => n.type === 'break')).toBe(true);
    expect(visibleText([p])).toBe('line one\nline two');
  });
});

describe('parseInline — spans', () => {
  it('parses bold, italic, and inline code', () => {
    expect(parseInline('**b**')[0]).toMatchObject({ type: 'strong' });
    expect(parseInline('*i*')[0]).toMatchObject({ type: 'em' });
    expect(parseInline('`c`')[0]).toEqual({ type: 'code', value: 'c' });
  });

  it('keeps intraword underscores literal', () => {
    const nodes = parseInline('some_var_name');
    expect(nodes.every((n) => n.type === 'text')).toBe(true);
    expect(nodes.map((n) => (n.type === 'text' ? n.value : '')).join('')).toBe('some_var_name');
  });

  it('does not parse markdown inside inline code', () => {
    expect(parseInline('`**not bold**`')[0]).toEqual({ type: 'code', value: '**not bold**' });
  });

  it('links only allowed schemes, degrading others to plain text', () => {
    expect(parseInline('[ok](https://x.com)')[0]).toMatchObject({
      type: 'link',
      href: 'https://x.com',
    });
    expect(parseInline('[mail](mailto:a@b.com)')[0]).toMatchObject({ type: 'link' });
    // javascript: / data: / relative → never a link node (no href-injection).
    for (const bad of [
      '[x](javascript:alert(1))',
      '[x](data:text/html,x)',
      '[x](/relative/path)',
    ]) {
      expect(parseInline(bad).every((n) => n.type !== 'link')).toBe(true);
    }
  });

  it('autolinks bare http(s) URLs and trims trailing punctuation', () => {
    const nodes = parseInline('see https://example.com/page.');
    const link = nodes.find((n) => n.type === 'link') as Extract<Inline, { type: 'link' }>;
    expect(link.href).toBe('https://example.com/page');
    // the trailing period survives as visible text
    expect(nodes.at(-1)).toEqual({ type: 'text', value: '.' });
  });

  it('parses nested emphasis', () => {
    const strong = parseInline('**bold _and italic_**')[0] as Extract<Inline, { type: 'strong' }>;
    expect(strong.type).toBe('strong');
    expect(strong.children.some((n) => n.type === 'em')).toBe(true);
  });

  it('degrades a nested link inside a link label to literal text (drops no URL)', () => {
    const nodes = parseInline('[outer [inner](http://x.com) tail](http://y.com)');
    // Exactly one real link (the outer one) — no nested <a>.
    const links = nodes.filter((n) => n.type === 'link');
    expect(links).toHaveLength(1);
    expect(links[0]).toMatchObject({ href: 'http://y.com' });
    // The inner URL survives as literal text in the label, not dropped.
    const vis = visibleText(parseMarkdown('[outer [inner](http://x.com) tail](http://y.com)'));
    expect(vis).toContain('[inner](http://x.com)');
  });
});

describe('robustness — partial / malformed input degrades to visible text', () => {
  const cases: string[] = [
    '**bold with no close',
    'dangling ` backtick',
    '[text](https://no-close.com',
    '[text](',
    '| a | b |\n| --- |', // half-streamed table (ragged)
    '```js\nunclosed fence',
    '***', // hr, not emphasis
    '> ',
    '- ',
    '#',
    '_a_b_c_d',
    'a *b **c* d** e',
    '[a](javascript:void)',
    '~~~\nno close',
    'text with * lone asterisk and _ lone underscore',
  ];

  for (const src of cases) {
    it(`never throws on: ${JSON.stringify(src)}`, () => {
      expect(() => parseMarkdown(src)).not.toThrow();
    });
  }

  it('drops no alphanumeric characters for a fully-adversarial blob', () => {
    const src =
      '**bold with no close\n' +
      'dangling ` backtick\n' +
      'a link [half](https://x.com and more\n' +
      'inline *em* and `code9` mixed\n' +
      '\n' +
      '| a | b |\n|---|---|\n| 1 | 2 | 3 |\n'; // ragged table row must not lose "3"
    // Every non-syntax character must reach the reader. We assert the
    // alphanumeric payload (letters AND digits) survives; syntax chars like *,
    // `, [, | may be consumed as markup.
    const keep = (s: string) => s.replace(/[^A-Za-z0-9]/g, '');
    expect(keep(visibleText(parseMarkdown(src)))).toBe(keep(src));
  });

  it('terminates on deeply nested markers without hanging', () => {
    const deep = '> '.repeat(50) + 'x';
    expect(() => parseMarkdown(deep)).not.toThrow();
    const deepList = Array.from({ length: 40 }, (_, i) => ' '.repeat(i * 2) + '- x').join('\n');
    expect(() => parseMarkdown(deepList)).not.toThrow();
  });

  it('handles the empty string', () => {
    expect(parseMarkdown('')).toEqual([]);
  });
});
