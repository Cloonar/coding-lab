// Markdown renderer contract (issues #13, #103): the caller owns the wrapper
// class; an issue reference `#N` links only when the caller's resolver says
// where (external → new tab, internal → router <A>), else stays plain text; and
// with task lists on, `[x]`/`[ ]` items render read-only checkboxes.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import type { JSX } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import { type Block, type Inline } from '../lib/markdown';
import { Markdown, MarkdownTree, inlineText, type RefResolver } from './Markdown';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mount(view: () => JSX.Element, withRouter = false): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  if (!withRouter) {
    dispose = render(view, container);
    return;
  }
  const history = createMemoryHistory();
  history.set({ value: '/' });
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route path="*" component={view} />
      </MemoryRouter>
    ),
    container,
  );
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('Markdown', () => {
  it('puts the caller-supplied class on the wrapper', () => {
    mount(() => <Markdown source="hello" class="issue-body md" />);
    const wrap = container.firstElementChild as HTMLElement;
    expect(wrap.className).toBe('issue-body md');
    expect(wrap.querySelector('p.md-p')?.textContent).toBe('hello');
  });

  it('renders #N as plain text without a resolver', () => {
    mount(() => <Markdown source="see #5 now" class="md" />);
    expect(container.querySelector('a')).toBeNull();
    expect(container.querySelector('p')?.textContent).toBe('see #5 now');
  });

  it('renders #N as plain text when the resolver returns null', () => {
    const resolve: RefResolver = () => null;
    mount(() => <Markdown source="see #5" class="md" resolveRef={resolve} />);
    expect(container.querySelector('a')).toBeNull();
    expect(container.querySelector('p')?.textContent).toBe('see #5');
  });

  it('links an external ref into a new tab', () => {
    const resolve: RefResolver = (n) => ({
      href: `https://github.com/o/r/issues/${n}`,
      external: true,
    });
    mount(() => <Markdown source="see #5" class="md" resolveRef={resolve} />);
    const a = container.querySelector('a.md-ref') as HTMLAnchorElement;
    expect(a).not.toBeNull();
    expect(a.textContent).toBe('#5');
    expect(a.getAttribute('href')).toBe('https://github.com/o/r/issues/5');
    expect(a.getAttribute('target')).toBe('_blank');
    expect(a.getAttribute('rel')).toBe('noopener noreferrer');
  });

  it('links an internal ref through the router, in the same tab', () => {
    const resolve: RefResolver = (n) => ({ href: `/repos/repo_1/issues/${n}`, external: false });
    mount(() => <Markdown source="see #5" class="md" resolveRef={resolve} />, true);
    const a = container.querySelector('a.md-ref') as HTMLAnchorElement;
    expect(a).not.toBeNull();
    expect(a.textContent).toBe('#5');
    expect(a.getAttribute('href')).toBe('/repos/repo_1/issues/5');
    expect(a.hasAttribute('target')).toBe(false);
  });

  it('renders task items as disabled checkboxes when task lists are on', () => {
    mount(() => <Markdown source={'- [x] a\n- [ ] b'} class="md" taskLists />);
    const list = container.querySelector('ul') as HTMLElement;
    expect(list.className).toBe('md-list md-task');
    const items = list.querySelectorAll('li');
    expect(items).toHaveLength(2);
    const boxes = list.querySelectorAll<HTMLInputElement>('input[type="checkbox"]');
    expect(boxes).toHaveLength(2);
    expect([...boxes].map((b) => b.disabled)).toEqual([true, true]);
    expect([...boxes].map((b) => b.checked)).toEqual([true, false]);
    expect([...boxes].map((b) => b.getAttribute('aria-label'))).toEqual(['done', 'open']);
    expect(items[0]?.classList.contains('md-task-item')).toBe(true);
    expect(items[0]?.classList.contains('done')).toBe(true);
    expect(items[1]?.classList.contains('done')).toBe(false);
    expect(items[0]?.querySelector('.md-task-body')?.textContent).toBe('a');
  });

  it('leaves task syntax as text when task lists are off', () => {
    mount(() => <Markdown source={'- [x] a'} class="md" />);
    expect(container.querySelector('input')).toBeNull();
    expect(container.querySelector('ul')?.className).toBe('md-list');
    expect(container.querySelector('li')?.textContent).toBe('[x] a');
  });

  it('renders a pre-parsed tree, keeping plain items of a mixed list as-is', () => {
    const blocks: Block[] = [
      {
        type: 'list',
        ordered: false,
        start: 1,
        items: [
          [{ type: 'paragraph', children: [{ type: 'text', value: 'task' }] }],
          [{ type: 'paragraph', children: [{ type: 'text', value: 'plain' }] }],
        ],
        checked: [false, null],
      },
    ];
    mount(() => <MarkdownTree blocks={blocks} class="md" />);
    const items = container.querySelectorAll('li');
    expect(items[0]?.className).toBe('md-task-item');
    expect(items[1]?.hasAttribute('class')).toBe(false);
    expect(items[1]?.querySelector('input')).toBeNull();
  });
});

describe('inlineText', () => {
  it('flattens inline nodes to plain text, refs as #N', () => {
    const nodes: Inline[] = [
      { type: 'strong', children: [{ type: 'text', value: 'Agent' }] },
      { type: 'text', value: ' Brief for ' },
      { type: 'ref', number: 7 },
      { type: 'code', value: ' x' },
    ];
    expect(inlineText(nodes)).toBe('Agent Brief for #7 x');
  });
});
