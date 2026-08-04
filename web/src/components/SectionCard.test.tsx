// SectionCard contract: a `<section class="card">` whose children are, in this
// exact order, the `.card-head.section-card-head` row (title `<h2>` then the
// spacer then the optional action, action last), the optional
// `<small class="hint hint-block">`, and then the caller's body. Both optional
// slots vanish entirely when omitted, and `class` appends to `card` on the
// section. Order is the assertion that matters — the spacing contract of issue
// #275 is only correct if the hint sits between the head row and the body.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import SectionCard from './SectionCard';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mount(component: () => ReturnType<typeof SectionCard>): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(component, container);
}

function section(): HTMLElement {
  const found = container.querySelector('section');
  expect(found).not.toBeNull();
  return found!;
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('SectionCard', () => {
  it('renders the title as the h2 of the head row inside a plain card section', () => {
    mount(() => <SectionCard title="Imports" />);

    expect(section().className).toBe('card');
    const head = container.querySelector('.card-head.section-card-head');
    expect(head).not.toBeNull();
    expect(head!.querySelector('h2')?.textContent).toBe('Imports');
  });

  it('renders the action inside the head row, as its last element child', () => {
    mount(() => (
      <SectionCard title="Imports" action={<button type="button">+ Add import</button>}>
        <p class="empty">No imports yet.</p>
      </SectionCard>
    ));

    const head = container.querySelector('.section-card-head')!;
    expect(head.querySelector('button')?.textContent).toBe('+ Add import');
    // Ordering, not just presence: the action is the trailing item, pushed
    // right by the spacer that precedes it.
    const children = Array.from(head.children);
    expect(children.map((el) => el.tagName)).toEqual(['H2', 'SPAN', 'BUTTON']);
    expect(children[children.length - 1]).toBe(head.querySelector('button'));
  });

  it('places the hint between the head row and the children', () => {
    mount(() => (
      <SectionCard
        title="Imports"
        action={<button type="button">+ Add import</button>}
        hint={
          <>
            Snapshotted at spawn under <code>imports/</code>.
          </>
        }
      >
        <p class="body-marker">Body.</p>
      </SectionCard>
    ));

    const hint = container.querySelector('small.hint.hint-block');
    expect(hint).not.toBeNull();
    expect(hint!.parentElement).toBe(section());
    expect(hint!.textContent).toBe('Snapshotted at spawn under imports/.');
    expect(Array.from(section().children).map((el) => el.className || el.tagName)).toEqual([
      'card-head section-card-head',
      'hint hint-block',
      'body-marker',
    ]);
  });

  it('renders no hint element when the hint prop is omitted', () => {
    mount(() => (
      <SectionCard title="Imports">
        <p class="body-marker">Body.</p>
      </SectionCard>
    ));

    expect(container.querySelector('.hint-block')).toBeNull();
    expect(Array.from(section().children).map((el) => el.className || el.tagName)).toEqual([
      'card-head section-card-head',
      'body-marker',
    ]);
  });

  it('keeps the head row to the h2 and the spacer when there is no action', () => {
    mount(() => (
      <SectionCard title="Imports">
        <p class="body-marker">Body.</p>
      </SectionCard>
    ));

    const head = container.querySelector('.section-card-head')!;
    expect(Array.from(head.children).map((el) => el.tagName)).toEqual(['H2', 'SPAN']);
    expect(head.querySelector('.spacer')).not.toBeNull();
    expect(head.querySelector('button')).toBeNull();
    expect(container.querySelector('.body-marker')?.textContent).toBe('Body.');
  });

  it('appends an extra class to the section and leaves it as just card when omitted', () => {
    mount(() => <SectionCard title="Danger zone" class="danger-zone" />);
    expect(section().className).toBe('card danger-zone');

    dispose?.();
    dispose = undefined;
    container.remove();

    mount(() => <SectionCard title="Imports" />);
    expect(section().className).toBe('card');
  });
});
