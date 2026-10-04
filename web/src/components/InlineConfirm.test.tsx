// InlineConfirm contract (issue #61): the trigger is replaced in place by
// Cancel plus a solid danger button naming the action; focus moves to Cancel
// on open and back to the trigger on Cancel or Escape; nothing runs until the
// named action is confirmed; a pending promise disables both buttons and
// shows the busy label, and the trigger returns once it settles; the second
// click of a double-click never confirms.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import InlineConfirm, { type InlineConfirmProps } from './InlineConfirm';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

function mount(props: Partial<InlineConfirmProps> = {}): {
  onConfirm: ReturnType<typeof vi.fn>;
  onOpenChange: ReturnType<typeof vi.fn>;
} {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onConfirm = vi.fn();
  const onOpenChange = vi.fn();
  dispose = render(
    () => (
      <InlineConfirm
        label="Stop all (2)"
        confirmLabel="Stop 2 runs"
        onConfirm={onConfirm}
        onOpenChange={onOpenChange}
        {...props}
      />
    ),
    container,
  );
  return { onConfirm, onOpenChange };
}

const button = (text: string): HTMLButtonElement | undefined =>
  Array.from(container.querySelectorAll('button')).find((b) => b.textContent === text);
const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

describe('InlineConfirm', () => {
  it('starts as the trigger alone, outlined danger by default', () => {
    mount();
    expect(container.querySelectorAll('button')).toHaveLength(1);
    expect(button('Stop all (2)')?.className).toBe('danger');
  });

  it('opens in place with Cancel and the named action, focusing Cancel', () => {
    const { onConfirm, onOpenChange } = mount();
    button('Stop all (2)')!.click();

    expect(button('Stop all (2)')).toBeUndefined();
    const group = container.querySelector('[role="group"]');
    expect(group?.getAttribute('aria-label')).toBe('Stop 2 runs');
    expect(button('Cancel')).toBeDefined();
    expect(button('Stop 2 runs')?.className).toBe('solid-danger');
    expect(document.activeElement).toBe(button('Cancel'));
    expect(onOpenChange).toHaveBeenLastCalledWith(true);
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('Cancel closes without running and returns focus to the trigger', () => {
    const { onConfirm, onOpenChange } = mount();
    button('Stop all (2)')!.click();
    button('Cancel')!.click();

    expect(onConfirm).not.toHaveBeenCalled();
    expect(button('Cancel')).toBeUndefined();
    expect(document.activeElement).toBe(button('Stop all (2)'));
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
  });

  it('Escape cancels the same way', () => {
    const { onConfirm } = mount();
    button('Stop all (2)')!.click();
    button('Cancel')!.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    );

    expect(onConfirm).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(button('Stop all (2)'));
  });

  it('runs the action on confirm and returns to the trigger', async () => {
    const { onConfirm } = mount();
    button('Stop all (2)')!.click();
    button('Stop 2 runs')!.click();
    await flush();

    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(button('Stop all (2)')).toBeDefined();
    expect(document.activeElement).toBe(button('Stop all (2)'));
  });

  it('shows the busy state while the action is pending', async () => {
    let resolve!: () => void;
    const pending = new Promise<void>((r) => (resolve = r));
    mount({ onConfirm: () => pending, busyLabel: 'Stopping…' });
    button('Stop all (2)')!.click();
    button('Stop 2 runs')!.click();
    await flush();

    const busy = button('Stopping…');
    expect(busy?.disabled).toBe(true);
    expect(button('Cancel')?.disabled).toBe(true);
    expect(container.querySelector('[role="group"]')?.getAttribute('aria-busy')).toBe('true');
    // Escape cannot abandon a running action.
    busy!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(button('Stopping…')).toBeDefined();

    resolve();
    await flush();
    expect(button('Stop all (2)')).toBeDefined();
  });

  it('comes back to the trigger after a failed action too', async () => {
    const failing = vi.fn(() => Promise.reject(new Error('nope')));
    mount({
      onConfirm: () => failing().catch(() => undefined),
    });
    button('Stop all (2)')!.click();
    button('Stop 2 runs')!.click();
    await flush();
    await flush();
    expect(failing).toHaveBeenCalledTimes(1);
    expect(button('Stop all (2)')).toBeDefined();
  });

  it('ignores the second click of a double-click on the trigger spot', () => {
    const { onConfirm } = mount();
    button('Stop all (2)')!.click();
    button('Stop 2 runs')!.dispatchEvent(new MouseEvent('click', { bubbles: true, detail: 2 }));
    expect(onConfirm).not.toHaveBeenCalled();
    expect(button('Stop 2 runs')).toBeDefined();
  });

  it('names the group by its prompt and supports the compact size', () => {
    mount({ prompt: 'Discard your changes?', confirmLabel: 'Discard', small: true });
    button('Stop all (2)')!.click();
    const group = container.querySelector('[role="group"]')!;
    const promptId = group.getAttribute('aria-labelledby');
    expect(document.getElementById(promptId!)?.textContent).toBe('Discard your changes?');
    expect(button('Cancel')?.className).toBe('small');
    expect(button('Discard')?.className).toBe('solid-danger small');
  });

  it('keeps a disabled trigger closed', () => {
    const { onOpenChange } = mount({ disabled: true });
    button('Stop all (2)')!.click();
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(button('Cancel')).toBeUndefined();
  });
});
