// FormCard contract: the `<form>` IS the card — one element with
// `class="card form-card"`, no wrapper around it — and its children are, in
// this exact order, the optional `<h2>`, the optional `intro`, the
// `<ErrorBanner>`, the caller's `children`, and then the submit button (bare,
// or inside a trailing `.card-actions` row when `actions` is passed).
//
// Order is the assertion that matters. Issue #281 promises no visual change,
// and the two prose slots straddle the banner deliberately: standing
// explanation above it, per-submit prose below it. Routing AddRepo's intro
// paragraph through `children` would reorder its card the moment an error
// showed — invisible until then — so the full ordered-shape assertions, with
// every slot populated at once, are what pin that down. The rest of the surface
// is the button's state — `busy` swaps the label and disables, `disabled`
// disables on top of busy, `wide` picks the class — and the event forwarding:
// `onSubmit` gets the submit event untouched, with no `preventDefault()` from
// the component.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import FormCard from './FormCard';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mount(component: () => ReturnType<typeof FormCard>): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(component, container);
}

function form(): HTMLFormElement {
  const found = container.querySelector('form');
  expect(found).not.toBeNull();
  return found!;
}

function submitButton(): HTMLButtonElement {
  const found = container.querySelector<HTMLButtonElement>('button[type="submit"]');
  expect(found).not.toBeNull();
  return found!;
}

/** Element children as `TAG` or `TAG.class` — the vocabulary of the order assertions. */
function shape(el: Element): string[] {
  return Array.from(el.children).map((child) => {
    const classes = child.className === '' ? '' : `.${child.className}`;
    return `${child.tagName}${classes}`;
  });
}

const noop = (): void => {};

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('FormCard', () => {
  it('renders the form itself as the card, with no wrapper element around it', () => {
    mount(() => (
      <FormCard
        title="Add repository"
        error={null}
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        submitLabel="Add"
        busyLabel="Adding…"
      />
    ));

    expect(form().className).toBe('card form-card');
    // The card is the form: nothing wraps it, and it is the only `.card`.
    expect(container.firstElementChild).toBe(form());
    expect(container.querySelectorAll('.card')).toHaveLength(1);
    expect(container.querySelector('.card')).toBe(form());
  });

  it('orders the heading, the banner, the caller body and the button', () => {
    mount(() => (
      <FormCard
        title="Add repository"
        error="remote unreachable"
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        submitLabel="Add"
        busyLabel="Adding…"
      >
        <p class="body-marker">Fields.</p>
      </FormCard>
    ));

    expect(shape(form())).toEqual(['H2', 'DIV.banner error', 'P.body-marker', 'BUTTON.primary']);
    expect(form().querySelector('h2')?.textContent).toBe('Add repository');
    expect(form().lastElementChild).toBe(submitButton());
  });

  it('places the intro between the heading and the banner', () => {
    mount(() => (
      <FormCard
        title="Add repository"
        intro={<p class="muted">lab clones a bare mirror it owns.</p>}
        error="remote unreachable"
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        submitLabel="Add"
        busyLabel="Adding…"
      >
        <p class="body-marker">Fields.</p>
      </FormCard>
    ));

    // Every slot populated at once — the only arrangement that catches an
    // intro routed through `children`, which would land it under the banner
    // and reorder AddRepo's card whenever an error was showing.
    expect(shape(form())).toEqual([
      'H2',
      'P.muted',
      'DIV.banner error',
      'P.body-marker',
      'BUTTON.primary',
    ]);
    expect(container.querySelector('.muted')?.textContent).toBe(
      'lab clones a bare mirror it owns.',
    );
  });

  it('emits no element for the intro when the prop is omitted', () => {
    mount(() => (
      <FormCard
        title="Add repository"
        error="remote unreachable"
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        submitLabel="Add"
        busyLabel="Adding…"
      >
        <p class="body-marker">Fields.</p>
      </FormCard>
    ));

    // Nothing sits in the gap — not even an empty wrapper: the banner is the
    // heading's immediate next sibling, so the card is unchanged by the slot.
    expect(form().querySelector('h2')?.nextElementSibling).toBe(container.querySelector('.banner'));
    expect(shape(form())).toEqual(['H2', 'DIV.banner error', 'P.body-marker', 'BUTTON.primary']);
  });

  it('renders no h2 at all when the title is omitted', () => {
    mount(() => (
      <FormCard
        error={null}
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        submitLabel="Create"
        busyLabel="Creating…"
      >
        <p class="body-marker">Fields.</p>
      </FormCard>
    ));

    expect(container.querySelector('h2')).toBeNull();
    expect(shape(form())).toEqual(['P.body-marker', 'BUTTON.primary']);
  });

  it('shows the error message, hides the banner when the error is null', () => {
    const [error, setError] = createSignal<string | null>('remote unreachable');
    mount(() => (
      <FormCard
        error={error()}
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        submitLabel="Add"
        busyLabel="Adding…"
      >
        <p class="body-marker">Fields.</p>
      </FormCard>
    ));

    expect(container.querySelector('.banner-text')?.textContent).toBe('remote unreachable');

    setError(null);
    expect(container.querySelector('.banner')).toBeNull();
    expect(shape(form())).toEqual(['P.body-marker', 'BUTTON.primary']);
  });

  it("calls onDismissError from the banner's dismiss control", () => {
    const onDismissError = vi.fn();
    mount(() => (
      <FormCard
        error="remote unreachable"
        onDismissError={onDismissError}
        onSubmit={noop}
        busy={false}
        submitLabel="Add"
        busyLabel="Adding…"
      />
    ));

    const dismiss = container.querySelector<HTMLButtonElement>('.banner-dismiss');
    expect(dismiss).not.toBeNull();
    dismiss!.click();
    expect(onDismissError).toHaveBeenCalledTimes(1);
  });

  it('swaps the button label and disables it while busy', () => {
    const [busy, setBusy] = createSignal(false);
    mount(() => (
      <FormCard
        error={null}
        onDismissError={noop}
        onSubmit={noop}
        busy={busy()}
        submitLabel="Add"
        busyLabel="Adding…"
      />
    ));

    expect(submitButton().textContent).toBe('Add');
    expect(submitButton().disabled).toBe(false);

    setBusy(true);
    expect(submitButton().textContent).toBe('Adding…');
    expect(submitButton().disabled).toBe(true);
  });

  it('disables the button on the extra disabled condition even when not busy', () => {
    const [disabled, setDisabled] = createSignal(true);
    mount(() => (
      <FormCard
        error={null}
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        disabled={disabled()}
        submitLabel="Add import"
        busyLabel="Adding…"
      />
    ));

    // Not busy, so the label is the resting one — but the button is still off.
    expect(submitButton().textContent).toBe('Add import');
    expect(submitButton().disabled).toBe(true);

    setDisabled(false);
    expect(submitButton().disabled).toBe(false);
  });

  it('toggles the button class between primary and primary wide', () => {
    const [wide, setWide] = createSignal(false);
    mount(() => (
      <FormCard
        error={null}
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        wide={wide()}
        submitLabel="Add"
        busyLabel="Adding…"
      />
    ));

    expect(submitButton().className).toBe('primary');

    setWide(true);
    expect(submitButton().className).toBe('primary wide');
  });

  it('leaves the button bare and last, with no card-actions row, when actions are omitted', () => {
    mount(() => (
      <FormCard
        error={null}
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        submitLabel="Add"
        busyLabel="Adding…"
      >
        <p class="body-marker">Fields.</p>
      </FormCard>
    ));

    expect(container.querySelector('.card-actions')).toBeNull();
    expect(form().lastElementChild).toBe(submitButton());
    expect(submitButton().parentElement).toBe(form());
  });

  it('wraps the button and the extra actions in a trailing card-actions row', () => {
    mount(() => (
      <FormCard
        error={null}
        onDismissError={noop}
        onSubmit={noop}
        busy={false}
        submitLabel="Create"
        busyLabel="Creating…"
        actions={<button type="button">Cancel</button>}
      >
        <p class="body-marker">Fields.</p>
      </FormCard>
    ));

    const actions = container.querySelector('.card-actions');
    expect(actions).not.toBeNull();
    expect(actions!.parentElement).toBe(form());
    expect(form().lastElementChild).toBe(actions);
    expect(shape(form())).toEqual(['P.body-marker', 'DIV.card-actions']);
    // Submit first, then the caller's action.
    expect(Array.from(actions!.children).map((el) => el.textContent)).toEqual(['Create', 'Cancel']);
    expect(actions!.firstElementChild).toBe(submitButton());
    expect(actions!.lastElementChild?.getAttribute('type')).toBe('button');
  });

  it('forwards the submit event to onSubmit, undefaulted', () => {
    let received: Event | undefined;
    const onSubmit = vi.fn((event: SubmitEvent) => {
      received = event;
    });
    mount(() => (
      <FormCard
        error={null}
        onDismissError={noop}
        onSubmit={onSubmit}
        busy={false}
        submitLabel="Add"
        busyLabel="Adding…"
      />
    ));

    // jsdom does not navigate on submit, so nothing here needs preventDefault —
    // which is the point: FormCard must not call it either.
    form().dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));

    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(received?.type).toBe('submit');
    expect(received?.defaultPrevented).toBe(false);
  });
});
