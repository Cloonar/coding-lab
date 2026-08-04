// EmptyState contract: exactly one `<p>` whose class is exactly `empty`,
// wrapping whatever the caller passed and nothing else — no container element,
// no extra nodes, no conditionals. The class is asserted by equality rather
// than by `contains` because it is the whole styling contract with `.empty` in
// banners.css; a second class would mean the component had started styling on
// its own. The children assertions pin the slot as JSX and not text: an
// embedded `<a>` has to arrive as a real element, and a message built from a
// signal has to keep tracking it, because History's per-filter line and
// RepoCRs' `emptyText()` depend on exactly those two things.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import EmptyState from './EmptyState';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mount(component: () => ReturnType<typeof EmptyState>): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(component, container);
}

function paragraph(): HTMLParagraphElement {
  const found = container.querySelector('p');
  expect(found).not.toBeNull();
  return found!;
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('EmptyState', () => {
  it('renders one paragraph carrying exactly the empty class', () => {
    mount(() => <EmptyState>No runs yet.</EmptyState>);

    expect(container.querySelectorAll('p').length).toBe(1);
    expect(paragraph().className).toBe('empty');
    expect(paragraph().parentElement).toBe(container);
  });

  it('renders static text children verbatim', () => {
    mount(() => (
      <EmptyState>No API tokens yet — create one for scripts and automation.</EmptyState>
    ));

    expect(paragraph().textContent).toBe(
      'No API tokens yet — create one for scripts and automation.',
    );
  });

  it('renders JSX children as real elements, not as escaped text', () => {
    // A plain `<a>` stands in for the `<A>` the Repos route actually passes —
    // routing is beside the point here, element-ness is the point.
    mount(() => (
      <EmptyState>
        No repositories yet — <a href="/repos/new">add one</a> now to get started.
      </EmptyState>
    ));

    const link = paragraph().querySelector('a');
    expect(link).not.toBeNull();
    expect(link!.getAttribute('href')).toBe('/repos/new');
    expect(link!.textContent).toBe('add one');
    expect(paragraph().textContent).toBe('No repositories yet — add one now to get started.');
  });

  it('keeps reactive children live after the first render', () => {
    const [filter, setFilter] = createSignal('failed');
    mount(() => <EmptyState>No {filter()} runs.</EmptyState>);

    expect(paragraph().textContent).toBe('No failed runs.');

    setFilter('parked');

    // History's per-filter message and RepoCRs' `emptyText()` are computed, so
    // the slot has to stay a tracked expression rather than a value snapshotted
    // once at mount.
    expect(paragraph().textContent).toBe('No parked runs.');
  });

  it('adds no element around or inside the content it was given', () => {
    mount(() => (
      <EmptyState>
        Ready — <a href="/runs/new">start a run</a>.
      </EmptyState>
    ));

    expect(Array.from(container.children).map((el) => el.tagName)).toEqual(['P']);
    expect(Array.from(paragraph().children).map((el) => el.tagName)).toEqual(['A']);
  });

  it('renders an empty paragraph when no children are passed at all', () => {
    mount(() => <EmptyState />);

    expect(paragraph().className).toBe('empty');
    expect(paragraph().textContent).toBe('');
    expect(paragraph().children.length).toBe(0);
  });
});
