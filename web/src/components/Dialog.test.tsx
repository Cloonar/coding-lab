// Dialog contract (issue #61): closed renders nothing; open renders a scrim
// and a role="dialog" (or alertdialog) panel with aria-modal, labelled by its
// title; focus moves in (the title, or initialFocus) and returns to the
// opener on close; Tab is trapped inside; scrim click and Escape close unless
// dismissable is false; the page behind does not scroll while it is open.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import Dialog, { type DialogProps } from './Dialog';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  document.body.style.overflow = '';
});

function mount(props: Partial<DialogProps> = {}): {
  onClose: ReturnType<typeof vi.fn>;
  open: () => void;
  close: () => void;
} {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onClose = vi.fn();
  let setOpen!: (v: boolean) => void;
  dispose = render(() => {
    const [open, set] = createSignal(false);
    setOpen = set;
    return (
      <>
        <button type="button" class="opener" onClick={() => set(true)}>
          Delete repository
        </button>
        <Dialog
          open={open()}
          onClose={() => {
            onClose();
            set(false);
          }}
          title="Delete coding-lab?"
          actions={
            <>
              <button type="button" class="first-action">
                Cancel
              </button>
              <button type="button" class="last-action">
                Delete repository
              </button>
            </>
          }
          {...props}
        >
          <p>The remote is not touched. This cannot be undone.</p>
          <label>
            Type coding-lab to confirm <input class="typed" type="text" />
          </label>
        </Dialog>
      </>
    );
  }, container);
  return {
    onClose,
    open: () => {
      const opener = container.querySelector<HTMLButtonElement>('.opener')!;
      opener.focus();
      opener.click();
    },
    close: () => setOpen(false),
  };
}

const dialog = () => container.querySelector<HTMLElement>('.dialog');
const key = (k: string, shiftKey = false) => {
  const event = new KeyboardEvent('keydown', { key: k, shiftKey, bubbles: true, cancelable: true });
  (document.activeElement ?? document.body).dispatchEvent(event);
  return event;
};

describe('Dialog', () => {
  it('renders nothing while closed', () => {
    mount();
    expect(dialog()).toBeNull();
    expect(container.querySelector('.dialog-scrim')).toBeNull();
  });

  it('is a labelled modal and moves focus in on open', () => {
    const { open } = mount();
    open();

    const panel = dialog()!;
    expect(panel.getAttribute('role')).toBe('dialog');
    expect(panel.getAttribute('aria-modal')).toBe('true');
    const title = document.getElementById(panel.getAttribute('aria-labelledby')!);
    expect(title?.textContent).toBe('Delete coding-lab?');
    expect(title?.tagName).toBe('H2');
    expect(document.activeElement).toBe(title);
    expect(panel.querySelector('.dialog-actions')?.textContent).toContain('Delete repository');
    expect(container.querySelector('.dialog-scrim')).not.toBeNull();
  });

  it('focuses initialFocus when given, and an alertdialog is described by its body', () => {
    const { open } = mount({
      role: 'alertdialog',
      initialFocus: () => container.querySelector<HTMLInputElement>('.typed'),
    });
    open();

    const panel = dialog()!;
    expect(panel.getAttribute('role')).toBe('alertdialog');
    expect(document.activeElement).toBe(container.querySelector('.typed'));
    const body = document.getElementById(panel.getAttribute('aria-describedby')!);
    expect(body?.textContent).toContain('This cannot be undone.');
  });

  it('traps Tab inside the panel', () => {
    const { open } = mount();
    open();
    const first = container.querySelector<HTMLInputElement>('.typed')!;
    const last = container.querySelector<HTMLButtonElement>('.last-action')!;

    last.focus();
    expect(key('Tab').defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(first);

    first.focus();
    expect(key('Tab', true).defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(last);

    // From the title (focused on open), Shift+Tab wraps to the last control.
    (dialog()!.querySelector('.dialog-title') as HTMLElement).focus();
    key('Tab', true);
    expect(document.activeElement).toBe(last);
  });

  it('pulls focus that escapes the panel back in', () => {
    const { open } = mount();
    open();
    container.querySelector<HTMLButtonElement>('.opener')!.focus();
    expect(dialog()!.contains(document.activeElement)).toBe(true);
  });

  it('closes on Escape and on a scrim click, returning focus to the opener', () => {
    const { open, onClose } = mount();
    open();
    key('Escape');
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(dialog()).toBeNull();
    expect(document.activeElement).toBe(container.querySelector('.opener'));

    open();
    container.querySelector<HTMLElement>('.dialog-scrim')!.click();
    expect(onClose).toHaveBeenCalledTimes(2);
    expect(dialog()).toBeNull();
  });

  it('a click inside the panel never closes it', () => {
    const { open, onClose } = mount();
    open();
    dialog()!.click();
    container.querySelector<HTMLButtonElement>('.first-action')!.click();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('ignores Escape and the scrim when not dismissable', () => {
    const { open, onClose } = mount({ dismissable: false });
    open();
    key('Escape');
    container.querySelector<HTMLElement>('.dialog-scrim')!.click();
    expect(onClose).not.toHaveBeenCalled();
    expect(dialog()).not.toBeNull();
  });

  it('locks the page scroll while open and restores it on close', () => {
    document.body.style.overflow = 'auto';
    const { open, close } = mount();
    open();
    expect(document.body.style.overflow).toBe('hidden');
    close();
    expect(document.body.style.overflow).toBe('auto');
  });
});
