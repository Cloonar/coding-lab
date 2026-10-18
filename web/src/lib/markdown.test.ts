// parseMarkdown/parseInline contract (issues #13, #103): a hand-rolled,
// zero-dep parser that emits a node tree the chat and issue views map to Solid
// JSX. Two guarantees under test: (1) the supported constructs — including
// `#N` refs and opt-in task-list items — parse to the right shape, and (2) the
// parser is TOTAL — every adversarial / partially-streamed input degrades to
// visible text, never throws, never drops a character (decision 6).

import { describe, expect, it } from 'vitest';
import { parseInline, parseMarkdown, type Block, type Inline } from './markdown';

type List = Extract<Block, { type: 'list' }>;

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
          case 'ref':
            return `#${n.number}`;
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
        // An ordered item shows its number and a task item its checkbox;
        // render both as source markers.
        return b.items
          .map((it, k) => {
            const num = b.ordered ? `${b.start + k}. ` : '';
            const c = b.checked?.[k];
            const box = c === true ? '[x] ' : c === false ? '[ ] ' : '';
            return num + box + it.map(block).join('');
          })
          .join('');
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

describe('parseInline — issue references', () => {
  const ref = (number: number): Inline => ({ type: 'ref', number });
  const text = (value: string): Inline => ({ type: 'text', value });

  it('parses #N as a ref node, at the start of the string and mid-prose', () => {
    expect(parseInline('#5')).toEqual([ref(5)]);
    expect(parseInline('see #5 now')).toEqual([text('see '), ref(5), text(' now')]);
  });

  it('accepts punctuation boundaries on either side', () => {
    expect(parseInline('(#5)')).toEqual([text('('), ref(5), text(')')]);
    expect(parseInline('#5.')).toEqual([ref(5), text('.')]);
    expect(parseInline('#5, #6')).toEqual([ref(5), text(', '), ref(6)]);
  });

  it('takes the whole digit run (#70 is 70, never 7)', () => {
    expect(parseInline('#70')).toEqual([ref(70)]);
    expect(parseInline('#123456789')).toEqual([ref(123456789)]);
  });

  it('rejects a # preceded by a word char, underscore or slash', () => {
    for (const src of ['foo#5', 'owner/repo#5', 'a_#5', '9#5']) {
      expect(parseInline(src)).toEqual([text(src)]);
    }
  });

  it('rejects a digit run that is not right-bounded', () => {
    for (const src of ['#5a', '#5_', '#5x9']) {
      expect(parseInline(src)).toEqual([text(src)]);
    }
  });

  it('leaves leading-zero and over-long runs literal (text fidelity)', () => {
    expect(parseInline('#05')).toEqual([text('#05')]);
    expect(parseInline('#0')).toEqual([text('#0')]);
    expect(parseInline('#1234567890')).toEqual([text('#1234567890')]);
  });

  it('keeps a lone # (and # before a non-digit) literal', () => {
    expect(parseInline('#')).toEqual([text('#')]);
    expect(parseInline('# x')).toEqual([text('# x')]);
    expect(parseInline('#a')).toEqual([text('#a')]);
  });

  it('does not parse refs inside inline code or a fence', () => {
    expect(parseInline('`#5`')).toEqual([{ type: 'code', value: '#5' }]);
    expect(parseMarkdown('```\n#5\n```')).toEqual([{ type: 'code', lang: '', text: '#5' }]);
  });

  it('parses refs inside headings, list items and emphasis', () => {
    expect(parseMarkdown('## See #5')).toEqual([
      { type: 'heading', level: 2, children: [text('See '), ref(5)] },
    ]);
    // `#5 foo` has no space after `#`, so it is a paragraph, not a heading.
    expect(parseMarkdown('#5 foo')).toEqual([
      { type: 'paragraph', children: [ref(5), text(' foo')] },
    ]);
    expect(parseMarkdown('## Blocked by')[0]).toMatchObject({ type: 'heading', level: 2 });
    const list = parseMarkdown('- #3')[0] as List;
    expect(list.items[0]).toEqual([{ type: 'paragraph', children: [ref(3)] }]);
    expect(parseInline('**#5**')).toEqual([{ type: 'strong', children: [ref(5)] }]);
    expect(parseInline('*blocked by #7*')).toEqual([
      { type: 'em', children: [text('blocked by '), ref(7)] },
    ]);
  });

  it('degrades a ref inside a link label to text (no nested link)', () => {
    const nodes = parseInline('[fixes #5](https://x.com)');
    expect(nodes).toEqual([
      {
        type: 'link',
        href: 'https://x.com',
        children: [text('fixes '), text('#5')],
      },
    ]);
    // ... including one nested in emphasis inside the label.
    const em = parseInline('[*#5*](https://x.com)')[0] as Extract<Inline, { type: 'link' }>;
    expect(em.children).toEqual([{ type: 'em', children: [text('#5')] }]);
  });
});

describe('parseMarkdown — task lists', () => {
  const tasks = { taskLists: true };
  const para = (value: string): Block => ({
    type: 'paragraph',
    children: [{ type: 'text', value }],
  });

  it('recognises [ ], [x] and [X] items and strips the marker', () => {
    const list = parseMarkdown('- [ ] a\n- [x] b\n- [X] c', tasks)[0] as List;
    expect(list).toEqual({
      type: 'list',
      ordered: false,
      start: 1,
      items: [[para('a')], [para('b')], [para('c')]],
      checked: [false, true, true],
    });
  });

  it('records null for a plain item in a mixed list', () => {
    const list = parseMarkdown('- [x] done\n- plain\n- [ ] todo', tasks)[0] as List;
    expect(list.checked).toEqual([true, null, false]);
    expect(list.items[1]).toEqual([para('plain')]);
  });

  it('accepts an empty task item ([ ] at end of line)', () => {
    const list = parseMarkdown('- [ ]\n- [x] b', tasks)[0] as List;
    expect(list.checked).toEqual([false, true]);
    expect(list.items[0]).toEqual([]);
  });

  it('supports ordered task items', () => {
    const list = parseMarkdown('1. [x] first\n2. [ ] second', tasks)[0] as List;
    expect(list.ordered).toBe(true);
    expect(list.checked).toEqual([true, false]);
    expect(list.items[0]).toEqual([para('first')]);
  });

  it('threads the option into nested lists and blockquotes', () => {
    const list = parseMarkdown('- [ ] outer\n  - [x] inner\n  - plain', tasks)[0] as List;
    expect(list.checked).toEqual([false]);
    const inner = list.items[0]![1] as List;
    expect(inner.type).toBe('list');
    expect(inner.checked).toEqual([true, null]);
    expect(inner.items[0]).toEqual([para('inner')]);

    const bq = parseMarkdown('> - [x] quoted', tasks)[0] as Extract<Block, { type: 'blockquote' }>;
    expect((bq.children[0] as List).checked).toEqual([true]);
  });

  it('does not treat near-miss markers as tasks', () => {
    for (const src of ['- []', '- [ x]', '- [xx]', '- [x]foo', '- [y] no']) {
      const list = parseMarkdown(src, tasks)[0] as List;
      expect(list).not.toHaveProperty('checked');
      expect(visibleText([list])).toBe(src.slice(2));
    }
  });

  it('omits checked for a list with no task items', () => {
    expect(parseMarkdown('- a\n- b', tasks)[0]).not.toHaveProperty('checked');
  });

  it('keeps refs working inside a task item', () => {
    const list = parseMarkdown('- [x] #3', tasks)[0] as List;
    expect(list.checked).toEqual([true]);
    expect(list.items[0]).toEqual([{ type: 'paragraph', children: [{ type: 'ref', number: 3 }] }]);
  });

  it('with task lists OFF (the default) leaves the tree untouched', () => {
    const src = '- [ ] a\n- [x] b\n  - [X] c';
    const off = parseMarkdown(src);
    expect(off).toEqual(parseMarkdown(src, {}));
    const list = off[0] as List;
    expect(list).not.toHaveProperty('checked');
    expect((list.items[1]![1] as List).checked).toBeUndefined();
    expect(list.items[0]).toEqual([para('[ ] a')]);
    expect(visibleText(off)).toBe('[ ] a[x] b[X] c');
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

  // Ref and task-list syntax, under both option sets: never throws and every
  // alphanumeric character reaches the reader (a ref shows as `#N`, a task
  // item's box as its source marker — case-folded, since `[X]` shows as
  // checked like `[x]`; a link's href is not visible text, so it is cut from
  // the expectation).
  const refTaskCases: string[] = [
    '#',
    '#12',
    'a#1',
    '#0',
    '#007',
    '#99999999999',
    '##5',
    '#5#6',
    'x/#5 y_#6 (#7)',
    '[ ]',
    '[x]',
    '- [ ]',
    '- [x]',
    '- [ ] ',
    '- [x] #3',
    '- [X] **#4** `#5`',
    '1. [ ] a\n2) [x] b',
    '- [ ] outer\n  - [x] inner #8\n    - [ ] deeper',
    '> - [x] quoted #9',
    '- [x\n- [',
    '[see #5](https://x.com) #6',
    '| #1 | [x] |\n|---|---|\n| a#2 | #3 |',
  ];
  const keepFolded = (s: string) => s.replace(/[^A-Za-z0-9]/g, '').toLowerCase();
  for (const opts of [{}, { taskLists: true }]) {
    for (const src of refTaskCases) {
      it(`never throws, drops nothing on ${JSON.stringify(src)} with ${JSON.stringify(opts)}`, () => {
        let blocks: Block[] = [];
        expect(() => {
          blocks = parseMarkdown(src, opts);
        }).not.toThrow();
        const shown = keepFolded(visibleText(blocks));
        expect(shown).toBe(keepFolded(src.replace(/\]\(https:\/\/x\.com\)/, '')));
      });
    }
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
