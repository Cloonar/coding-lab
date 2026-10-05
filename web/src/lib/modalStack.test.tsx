// lib/modalStack.ts: only the modal on top traps focus and hears Escape, so
// two modals on one page never fight over the focus; they share the scroll
// lock; and each hands focus back to whatever had it when it opened.

import { Show, createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createModal } from './modalStack';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

function Modal(props: { name: string; onEscape: () => void }) {
  let panel: HTMLDivElement | undefined;
  let heading: HTMLHeadingElement | undefined;
  createModal({
    panel: () => panel,
    fallback: () => heading,
    onEscape: (event) => {
      event.preventDefault();
      props.onEscape();
    },
  });
  return (
    <div ref={panel} data-modal={props.name}>
      <h2 ref={heading} tabIndex={-1}>
        {props.name}
      </h2>
      <button type="button" data-first>
        first of {props.name}
      </button>
      <button type="button" data-last>
        last of {props.name}
      </button>
    </div>
  );
}

function mount() {
  container = document.createElement('div');
  document.body.appendChild(container);
  const escapes = { lower: vi.fn(), upper: vi.fn() };
  const [lower, setLower] = createSignal(false);
  const [upper, setUpper] = createSignal(false);
  dispose = render(
    () => (
      <>
        <button type="button" data-page>
          on the page
        </button>
        <Show when={lower()}>
          <Modal name="lower" onEscape={escapes.lower} />
        </Show>
        <Show when={upper()}>
          <Modal name="upper" onEscape={escapes.upper} />
        </Show>
      </>
    ),
    container,
  );
  const q = (selector: string): HTMLElement => {
    const el = container.querySelector<HTMLElement>(selector);
    if (el === null) throw new Error(`missing ${selector}`);
    return el;
  };
  return { escapes, setLower, setUpper, q };
}

const key = (name: string, init: KeyboardEventInit = {}): KeyboardEvent => {
  const event = new KeyboardEvent('keydown', {
    key: name,
    bubbles: true,
    cancelable: true,
    ...init,
  });
  (document.activeElement ?? document.body).dispatchEvent(event);
  return event;
};

describe('createModal', () => {
  it('moves focus in, traps Tab, pulls escaped focus back, and returns it on close', () => {
    const { setLower, q } = mount();
    q('[data-page]').focus();

    setLower(true);
    expect(document.activeElement).toBe(q('[data-modal="lower"] h2'));
    expect(document.body.style.overflow).toBe('hidden');

    q('[data-modal="lower"] [data-last]').focus();
    key('Tab');
    expect(document.activeElement).toBe(q('[data-modal="lower"] [data-first]'));
    key('Tab', { shiftKey: true });
    expect(document.activeElement).toBe(q('[data-modal="lower"] [data-last]'));

    q('[data-page]').focus();
    expect(document.activeElement).toBe(q('[data-modal="lower"] [data-first]'));

    setLower(false);
    expect(document.activeElement).toBe(q('[data-page]'));
    expect(document.body.style.overflow).toBe('');
  });

  it('two open modals do not fight: the upper one has the focus, the lower one waits', () => {
    const { setLower, setUpper, q } = mount();
    setLower(true);
    q('[data-modal="lower"] [data-first]').focus();

    // Two traps that both insisted would bounce the focus until the stack
    // overflowed. Opening the second one must simply work.
    expect(() => setUpper(true)).not.toThrow();
    expect(document.activeElement).toBe(q('[data-modal="upper"] h2'));

    // Focus sent into the LOWER modal comes back to the upper one — once.
    expect(() => q('[data-modal="lower"] [data-last]').focus()).not.toThrow();
    expect(document.activeElement).toBe(q('[data-modal="upper"] [data-first]'));

    // Tab cycles inside the upper modal only.
    q('[data-modal="upper"] [data-last]').focus();
    key('Tab');
    expect(document.activeElement).toBe(q('[data-modal="upper"] [data-first]'));
  });

  it('Escape reaches the upper modal alone; the lower one hears it once it is on top again', () => {
    const { escapes, setLower, setUpper, q } = mount();
    setLower(true);
    q('[data-modal="lower"] [data-first]').focus();
    setUpper(true);

    key('Escape');
    expect(escapes.upper).toHaveBeenCalledTimes(1);
    expect(escapes.lower).not.toHaveBeenCalled();

    // The upper one closes: focus goes back into the lower one, which traps
    // again and has the Escape key again. The page stays locked.
    setUpper(false);
    expect(document.activeElement).toBe(q('[data-modal="lower"] [data-first]'));
    expect(document.body.style.overflow).toBe('hidden');
    q('[data-page]').focus();
    expect(document.activeElement).toBe(q('[data-modal="lower"] [data-first]'));
    key('Escape');
    expect(escapes.lower).toHaveBeenCalledTimes(1);

    setLower(false);
    expect(document.body.style.overflow).toBe('');
  });

  it('leaves an Escape that a control inside already handled', () => {
    const { escapes, setLower, q } = mount();
    setLower(true);
    const inner = q('[data-modal="lower"] [data-first]');
    inner.addEventListener('keydown', (event) => event.preventDefault(), { once: true });
    inner.focus();

    key('Escape');
    expect(escapes.lower).not.toHaveBeenCalled();
    key('Escape');
    expect(escapes.lower).toHaveBeenCalledTimes(1);
  });
});
