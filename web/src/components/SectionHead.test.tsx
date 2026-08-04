// SectionHead contract: a `<div class="section-head">` whose element children
// are, in this exact order, the title `<h2>`, the `.spacer`, and then whatever
// the optional action slot renders — action last, so the spacer pushes it to
// the right edge. The spacer is present with AND without an action: that is the
// whole point, since identical markup either way is what keeps the row's height
// from moving when a page has no action to offer. `title` being JSX (not a
// string like SectionCard's) is also pinned here: a heading may be a fragment
// carrying the detail routes' inline issue-number span, and an action may
// itself be several controls (History's two filter selects), which all land in
// the row, in order, after the spacer.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import SectionHead from './SectionHead';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mount(component: () => ReturnType<typeof SectionHead>): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(component, container);
}

function head(): HTMLElement {
  const found = container.querySelector<HTMLElement>('div.section-head');
  expect(found).not.toBeNull();
  return found!;
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('SectionHead', () => {
  it('renders the title as the h2 of a plain section-head row', () => {
    mount(() => <SectionHead title="Repositories" />);

    expect(head().className).toBe('section-head');
    expect(head().querySelector('h2')?.textContent).toBe('Repositories');
  });

  it('renders the action inside the head row, as its last element child', () => {
    mount(() => (
      <SectionHead
        title="Credentials"
        action={
          <button type="button" class="primary">
            + Add credential
          </button>
        }
      />
    ));

    expect(head().querySelector('button')?.textContent).toBe('+ Add credential');
    // Ordering, not just presence: the action is the trailing item, pushed
    // right by the spacer that precedes it.
    const children = Array.from(head().children);
    expect(children.map((el) => el.tagName)).toEqual(['H2', 'SPAN', 'BUTTON']);
    expect(children[children.length - 1]).toBe(head().querySelector('button'));
  });

  it('keeps the row to the h2 and the spacer when there is no action', () => {
    mount(() => <SectionHead title="API tokens" />);

    // Identical markup to the action case minus the action itself — the
    // acceptance criterion is that the row's height does not depend on whether
    // a page has an action.
    expect(Array.from(head().children).map((el) => el.tagName)).toEqual(['H2', 'SPAN']);
    expect(head().querySelector('.spacer')).not.toBeNull();
    expect(head().querySelector('button')).toBeNull();
  });

  it('renders a JSX title inside the single h2', () => {
    mount(() => (
      <SectionHead
        title={
          <>
            <span class="issue-number">#12</span> Fix it
          </>
        }
        action={<span class="chip state-open">open</span>}
      />
    ));

    const headings = head().querySelectorAll('h2');
    expect(headings.length).toBe(1);
    expect(headings[0]?.querySelector('.issue-number')?.textContent).toBe('#12');
    expect(headings[0]?.textContent).toBe('#12 Fix it');
    // The chip is a sibling of the heading, not swallowed into it.
    expect(head().querySelector('.chip')?.parentElement).toBe(head());
  });

  it('renders several controls passed as one fragment, in order after the spacer', () => {
    mount(() => (
      <SectionHead
        title="History"
        action={
          <>
            <label class="field runs-filter">
              <select name="outcome-filter" aria-label="Filter by outcome" />
            </label>
            <label class="field runs-filter">
              <select name="repo-filter" aria-label="Filter by repository" />
            </label>
          </>
        }
      />
    ));

    const children = Array.from(head().children);
    expect(children.map((el) => el.tagName)).toEqual(['H2', 'SPAN', 'LABEL', 'LABEL']);
    // Document order, so this pins which filter comes first as well.
    const selects = Array.from(head().querySelectorAll('select'));
    expect(selects.map((el) => el.name)).toEqual(['outcome-filter', 'repo-filter']);
  });
});
