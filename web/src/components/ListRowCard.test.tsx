// ListRowCard contract: an `<article class="card">` whose children are, in this
// exact order, the `.card-head` row, the optional `<p class="muted card-sub">`
// metadata line, and then the caller's body. Inside the head row the order is
// title span, badges, spacer, actions — the spacer is always there, and it is
// the thing that separates "chips hugging the title" from "buttons at the
// trailing edge". Order is the assertion that matters: issue #282 is a pure
// markup extraction, so a slot landing on the wrong side of the spacer, or a
// `card-sub` landing under the body, is a visual regression even though every
// piece still renders.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import ListRowCard from './ListRowCard';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mount(component: () => ReturnType<typeof ListRowCard>): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(component, container);
}

function article(): HTMLElement {
  const found = container.querySelector('article');
  expect(found).not.toBeNull();
  return found!;
}

function head(): HTMLElement {
  const found = container.querySelector<HTMLElement>('.card-head');
  expect(found).not.toBeNull();
  return found!;
}

/** The head row's element children in document order, as class (or tag when unclassed). */
function headOrder(): string[] {
  return Array.from(head().children).map((el) => el.className || el.tagName);
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('ListRowCard', () => {
  it('renders a bare row as a plain card with the title and the spacer only', () => {
    mount(() => <ListRowCard title="ci-bot" />);

    expect(article().className).toBe('card');
    expect(head().querySelector('.card-title')?.textContent).toBe('ci-bot');
    // Both head slots empty: the row is exactly the title span and the spacer.
    expect(Array.from(head().children).map((el) => el.tagName)).toEqual(['SPAN', 'SPAN']);
    expect(headOrder()).toEqual(['card-title', 'spacer']);
    expect(article().querySelector('p')).toBeNull();
  });

  it('appends an extra class to the article and leaves it as just card when omitted', () => {
    mount(() => <ListRowCard title="ci-bot" class="token-card" />);
    expect(article().className).toBe('card token-card');

    dispose?.();
    dispose = undefined;
    container.remove();

    mount(() => <ListRowCard title="ci-bot" />);
    expect(article().className).toBe('card');
  });

  it('renders the action after the spacer, as the last element child of the head', () => {
    mount(() => <ListRowCard title="ci-bot" actions={<button type="button">Delete</button>} />);

    const children = Array.from(head().children);
    expect(children.map((el) => el.tagName)).toEqual(['SPAN', 'SPAN', 'BUTTON']);
    expect(children[children.length - 1]).toBe(head().querySelector('button'));
    expect(head().querySelector('button')?.textContent).toBe('Delete');
  });

  it('keeps two actions after the spacer, in the order they were passed', () => {
    mount(() => (
      <ListRowCard
        title="nightly"
        actions={
          <>
            <button type="button" class="small">
              Edit
            </button>
            <button type="button" class="danger small">
              Delete
            </button>
          </>
        }
      />
    ));

    const order = headOrder();
    expect(order).toEqual(['card-title', 'spacer', 'small', 'danger small']);
    const buttons = Array.from(head().querySelectorAll('button'));
    expect(buttons.map((el) => el.textContent)).toEqual(['Edit', 'Delete']);
    // Both land past the spacer, adjacent and in the order they were passed.
    const children = Array.from(head().children);
    const spacerAt = children.findIndex((el) => el.className === 'spacer');
    expect(children.indexOf(buttons[0]!)).toBe(spacerAt + 1);
    expect(children.indexOf(buttons[1]!)).toBe(spacerAt + 2);
  });

  it('renders badges between the title and the spacer, ahead of the actions', () => {
    mount(() => (
      <ListRowCard
        title="gh-personal"
        badges={
          <>
            <span class="chip mono">GitHub token</span>
            <span class="chip in-use">in use</span>
          </>
        }
        actions={
          <button type="button" class="danger small">
            Delete
          </button>
        }
      />
    ));

    const order = headOrder();
    expect(order).toEqual(['card-title', 'chip mono', 'chip in-use', 'spacer', 'danger small']);
    expect(order.indexOf('spacer')).toBeGreaterThan(order.indexOf('chip in-use'));
    expect(order.indexOf('spacer')).toBeLessThan(order.indexOf('danger small'));
  });

  it('places the sub line between the head row and the children', () => {
    mount(() => (
      <ListRowCard title="ci-bot" sub="Created 4 Aug 2026 · never used">
        <p class="body-marker">Body.</p>
      </ListRowCard>
    ));

    const sub = container.querySelector('p.muted.card-sub');
    expect(sub).not.toBeNull();
    expect(sub!.parentElement).toBe(article());
    expect(sub!.textContent).toBe('Created 4 Aug 2026 · never used');
    expect(Array.from(article().children).map((el) => el.className || el.tagName)).toEqual([
      'card-head',
      'muted card-sub',
      'body-marker',
    ]);
  });

  it('still renders the sub line when the value passed is falsy', () => {
    // Omission is the only thing that hides it: a caller that passes a `sub`
    // rendered the line unconditionally before the migration, and an empty
    // string must not silently take it away.
    mount(() => <ListRowCard title="nightly" sub="" />);

    const sub = container.querySelector('p.muted.card-sub');
    expect(sub).not.toBeNull();
    expect(sub!.textContent).toBe('');
  });

  it('renders no sub element when the sub prop is omitted', () => {
    mount(() => (
      <ListRowCard title="ci-bot">
        <p class="body-marker">Body.</p>
      </ListRowCard>
    ));

    expect(container.querySelector('.card-sub')).toBeNull();
    expect(Array.from(article().children).map((el) => el.className || el.tagName)).toEqual([
      'card-head',
      'body-marker',
    ]);
  });

  it('renders an element-bearing title inside the card-title span, not beside it', () => {
    mount(() => (
      <ListRowCard
        title={
          <>
            Pixel 8<span class="muted"> · this device</span>
          </>
        }
      />
    ));

    const title = head().querySelector('.card-title')!;
    const decoration = title.querySelector('.muted');
    expect(decoration).not.toBeNull();
    expect(decoration!.parentElement).toBe(title);
    expect(title.textContent).toBe('Pixel 8 · this device');
    // Nested, not a sibling: the head row itself is still just title + spacer.
    expect(headOrder()).toEqual(['card-title', 'spacer']);
  });
});
