// Banner contract: one `.banner` row whose class and ARIA role are picked
// together by `variant`, whose message is text and only ever text, and whose
// dismiss button exists only when a handler was passed for it.
//
// Variant and role are asserted as one pair in every case because the component
// treats them as one decision, not two: 'error' is the destructive alert that
// interrupts a screen reader, 'notice' and 'success' announce politely. Split
// across separate assertions they would still pass while drifting apart — a
// fourth variant could pick up a palette and forget a role, which is precisely
// the per-copy mistake the ~33 hand-rolled banners were free to make one at a
// time and this component exists to make impossible.
//
// The text-not-HTML case is the single most important assertion here. Banner
// bodies carry strings the app did not write — git remote errors, agent output,
// server replies — so a message containing markup has to land in the DOM as
// characters. It is pinned by asking for the element (`querySelector('b')` is
// null) rather than by reading `textContent` alone, because an escaped `<b>`
// and a live `<b>` are indistinguishable from `textContent` on the row.
//
// Non-dismissible means the button is absent, not disabled. Most banners in the
// app are driven by a resource's error or by a `paused` flag and have nowhere
// to dismiss to — the next render brings them straight back — so a disabled
// button would still take up the row, still be announced, and still lie about
// being interactive. `querySelector('.banner-dismiss')` returning null is the
// contract, and the ordered-shape assertions are what prove nothing was left
// standing in its place.
//
// Class strings are asserted by equality rather than by `contains`, because the
// order is the contract: `afk-strip-paused` (select.css) and `clone-error`
// (chips.css) are real styling hooks on two migrated sites, and they have to
// land after the variant, with the no-class case rendering exactly
// `banner error` and no trailing space.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import Banner from './Banner';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mount(component: () => ReturnType<typeof Banner>): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(component, container);
}

function banner(): HTMLDivElement {
  const found = container.querySelector<HTMLDivElement>('.banner');
  expect(found).not.toBeNull();
  return found!;
}

function text(): HTMLSpanElement {
  const found = container.querySelector<HTMLSpanElement>('.banner-text');
  expect(found).not.toBeNull();
  return found!;
}

function dismissButton(): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>('.banner-dismiss');
}

/** Element children as `TAG` or `TAG.class` — the vocabulary of the order assertions. */
function shape(el: Element): string[] {
  return Array.from(el.children).map((child) => {
    const classes = child.className === '' ? '' : `.${child.className}`;
    return `${child.tagName}${classes}`;
  });
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

const VARIANTS: { variant: 'error' | 'notice' | 'success'; className: string; role: string }[] = [
  { variant: 'error', className: 'banner error', role: 'alert' },
  { variant: 'notice', className: 'banner notice', role: 'status' },
  { variant: 'success', className: 'banner success', role: 'status' },
];

describe('Banner', () => {
  it.each(VARIANTS)(
    'gives the $variant variant class $className and role $role together',
    ({ variant, className, role }) => {
      mount(() => <Banner message="remote unreachable" variant={variant} />);

      // Asserted in one breath: the palette and the announcement are one
      // decision, and a variant that got only half of it would be a bug the
      // hand-rolled copies used to ship.
      expect(banner().className).toBe(className);
      expect(banner().getAttribute('role')).toBe(role);
    },
  );

  it('defaults to the error variant, alert role included, when variant is omitted', () => {
    mount(() => <Banner message="remote unreachable" />);

    expect(banner().className).toBe('banner error');
    expect(banner().getAttribute('role')).toBe('alert');
  });

  it('renders nothing at all when the message is null', () => {
    mount(() => <Banner message={null} onDismiss={() => {}} />);

    // Not an empty row, not a hidden one — no `.banner` element exists, so a
    // resource with no error contributes no markup to its card.
    expect(container.querySelector('.banner')).toBeNull();
    expect(container.querySelector('.banner-text')).toBeNull();
    expect(dismissButton()).toBeNull();
    expect(container.children.length).toBe(0);
  });

  it('appears, updates and disappears as the message signal flips', () => {
    const [message, setMessage] = createSignal<string | null>(null);
    mount(() => <Banner message={message()} />);

    expect(container.querySelector('.banner')).toBeNull();

    setMessage('remote unreachable');
    expect(text().textContent).toBe('remote unreachable');

    // Same row, new text: the message is a tracked expression, not a value
    // snapshotted when the banner first appeared.
    setMessage('authentication failed');
    expect(text().textContent).toBe('authentication failed');

    setMessage(null);
    expect(container.querySelector('.banner')).toBeNull();
    expect(container.children.length).toBe(0);
  });

  it('renders a message containing markup as text, never as HTML', () => {
    const hostile = '<b>boom</b> & "quotes"';
    mount(() => <Banner message={hostile} />);

    // The v0 sticky-banner property: server strings reach the row as
    // characters. `textContent` alone cannot tell an escaped `<b>` from a live
    // one, so the element query is the assertion that matters.
    expect(text().textContent).toBe(hostile);
    expect(text().querySelector('b')).toBeNull();
    expect(text().children.length).toBe(0);
    expect(banner().querySelector('b')).toBeNull();
  });

  it('renders the dismiss button, labelled, and calls onDismiss exactly once per click', () => {
    const onDismiss = vi.fn();
    mount(() => <Banner message="remote unreachable" onDismiss={onDismiss} />);

    const dismiss = dismissButton();
    expect(dismiss).not.toBeNull();
    expect(dismiss!.getAttribute('type')).toBe('button');
    // The svg is aria-hidden, so the control's name lives on the button.
    expect(dismiss!.getAttribute('aria-label')).toBe('Dismiss');
    expect(dismiss!.getAttribute('title')).toBe('Dismiss');

    dismiss!.click();
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it('renders no dismiss button at all when onDismiss is omitted', () => {
    mount(() => <Banner message="clone failed" />);

    // Absent, not disabled: a banner with nowhere to dismiss to must not offer
    // a control that a screen reader would still announce.
    expect(dismissButton()).toBeNull();
    expect(banner().querySelector('button')).toBeNull();
    expect(shape(banner())).toEqual(['SPAN.banner-text']);
  });

  it('appends the caller class after the variant, in that exact order', () => {
    mount(() => <Banner message="Paused" class="afk-strip-paused" />);

    // Byte-for-byte what the AFK strip rendered inline; select.css keys off the
    // modifier sitting last, so equality — not `contains` — is the assertion.
    expect(banner().className).toBe('banner error afk-strip-paused');
  });

  it('keeps the variant between the base class and a caller class on a non-default variant', () => {
    mount(() => <Banner message="Cloned" variant="success" class="clone-error" />);

    expect(banner().className).toBe('banner success clone-error');
    expect(banner().getAttribute('role')).toBe('status');
  });

  it('renders exactly "banner error" with no trailing space when class is omitted', () => {
    mount(() => <Banner message="remote unreachable" />);

    // A `banner error ` with a stray space is invisible in the browser and
    // would break any test or selector that compares the class string.
    expect(banner().className).toBe('banner error');
    expect(banner().getAttribute('class')).toBe('banner error');
  });

  it('places the action between the text and the dismiss button', () => {
    mount(() => (
      <Banner
        message="Schedule paused"
        variant="notice"
        action={
          <button type="button" class="banner-action">
            Re-enable
          </button>
        }
        onDismiss={() => {}}
      />
    ));

    // Order is the assertion: the three migrated sites (AFK Reset, paused
    // Re-enable, clone Retry) each had their control sitting exactly here.
    expect(shape(banner())).toEqual([
      'SPAN.banner-text',
      'BUTTON.banner-action',
      'BUTTON.banner-dismiss',
    ]);
    const action = container.querySelector('.banner-action');
    expect(action).not.toBeNull();
    expect(action!.parentElement).toBe(banner());
    expect(text().nextElementSibling).toBe(action);
    expect(action!.nextElementSibling).toBe(dismissButton());
    expect(action!.textContent).toBe('Re-enable');
  });

  it('emits no element in the action slot when the action is omitted', () => {
    mount(() => <Banner message="remote unreachable" onDismiss={() => {}} />);

    // Not even an empty wrapper stands in the gap: the dismiss button is the
    // text's immediate next sibling, so an omitted slot leaves the row as the
    // migrated sites already had it.
    expect(shape(banner())).toEqual(['SPAN.banner-text', 'BUTTON.banner-dismiss']);
    expect(text().nextElementSibling).toBe(dismissButton());
    expect(banner().lastElementChild).toBe(dismissButton());
  });

  it('renders the action with no dismiss button when only the action is passed', () => {
    mount(() => (
      <Banner
        message="Clone failed"
        action={
          <button type="button" class="banner-action">
            Retry
          </button>
        }
      />
    ));

    expect(shape(banner())).toEqual(['SPAN.banner-text', 'BUTTON.banner-action']);
    expect(dismissButton()).toBeNull();
  });
});
