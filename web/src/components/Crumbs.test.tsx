// Crumbs contract: renders a "/"-separated trail where every segment carrying
// an href is an <A> link and the final (current-page) segment is inert text;
// the separators are the component's own — callers pass only labels + hrefs.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import Crumbs, { type Crumb } from './Crumbs';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mount(segments: Crumb[]): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/' });
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route path="*" component={() => <Crumbs segments={segments} />} />
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

describe('Crumbs', () => {
  it('links every segment with an href and leaves the final one inert', () => {
    mount([
      { label: 'Repos', href: '/repos' },
      { label: 'coding-lab', href: '/repos/repo_1/issues' },
      { label: 'Issues', href: '/repos/repo_1/issues' },
      { label: '#42' },
    ]);

    const links = Array.from(container.querySelectorAll('a'));
    expect(links.map((a) => [a.textContent, a.getAttribute('href')])).toEqual([
      ['Repos', '/repos'],
      ['coding-lab', '/repos/repo_1/issues'],
      ['Issues', '/repos/repo_1/issues'],
    ]);
    // The leaf is text, never a link.
    expect(container.querySelector('a[href="/repos/repo_1/issues/42"]')).toBeNull();
    expect(container.textContent).toContain('#42');
  });

  it('joins the trail with " / " separators (n-1 for n segments)', () => {
    mount([
      { label: 'Repos', href: '/repos' },
      { label: 'coding-lab', href: '/repos/repo_1/issues' },
      { label: 'Labels' },
    ]);

    expect(container.querySelector('p.crumb')?.textContent).toBe('Repos / coding-lab / Labels');
  });
});
