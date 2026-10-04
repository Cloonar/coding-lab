// Toast contract: show(message) renders the text alone in a status region for
// 4s, and a new message replaces the old and restarts the timer (the behavior
// every existing caller relies on). Issue #61 adds an optional action — one
// button beside the text (Undo) — that hides the toast before running, a
// longer 6.5s lifetime for action toasts, a per-call duration, a timer that
// pauses while the pointer or focus is inside, and dismiss().

import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createToast, type ToastHandle } from './Toast';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;
let toast: ToastHandle;

beforeEach(() => {
  vi.useFakeTimers();
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(() => {
    toast = createToast();
    return <>{toast.Toast()}</>;
  }, container);
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.useRealTimers();
});

const el = () => container.querySelector<HTMLElement>('.toast');

describe('Toast (plain)', () => {
  it('renders the message alone in a status region and hides after 4s', () => {
    toast.show('Stopped 2 instances');
    expect(el()?.getAttribute('role')).toBe('status');
    expect(el()?.textContent).toBe('Stopped 2 instances');
    expect(el()?.querySelector('button')).toBeNull();
    expect(container.innerHTML).toBe('<div class="toast" role="status">Stopped 2 instances</div>');

    vi.advanceTimersByTime(3_999);
    expect(el()).not.toBeNull();
    vi.advanceTimersByTime(1);
    expect(el()).toBeNull();
  });

  it('replaces the message and restarts the timer', () => {
    toast.show('first');
    vi.advanceTimersByTime(3_000);
    toast.show('second');
    expect(el()?.textContent).toBe('second');
    vi.advanceTimersByTime(3_000);
    expect(el()).not.toBeNull();
    vi.advanceTimersByTime(1_000);
    expect(el()).toBeNull();
  });

  it('honours a per-call duration and dismiss()', () => {
    toast.show('quick', { durationMs: 1_000 });
    vi.advanceTimersByTime(1_000);
    expect(el()).toBeNull();

    toast.show('gone at once');
    toast.dismiss();
    expect(el()).toBeNull();
  });
});

describe('Toast (with an action)', () => {
  it('renders one action button and stays up 6.5s', () => {
    const run = vi.fn();
    toast.show('Changes discarded', { action: { label: 'Undo', run } });
    expect(el()?.querySelector('.toast-text')?.textContent).toBe('Changes discarded');
    const buttons = el()?.querySelectorAll('button');
    expect(buttons).toHaveLength(1);
    expect(buttons?.[0]?.textContent).toBe('Undo');
    expect(buttons?.[0]?.getAttribute('type')).toBe('button');

    vi.advanceTimersByTime(6_499);
    expect(el()).not.toBeNull();
    vi.advanceTimersByTime(1);
    expect(el()).toBeNull();
    expect(run).not.toHaveBeenCalled();
  });

  it('hides the toast, then runs the action', () => {
    const run = vi.fn(() => {
      expect(el()).toBeNull(); // already hidden when the action runs
    });
    toast.show('Removed the import of other-repo', { action: { label: 'Undo', run } });
    el()!.querySelector('button')!.click();
    expect(run).toHaveBeenCalledTimes(1);
    expect(el()).toBeNull();
  });

  it('pauses while focus or the pointer is inside, and resumes after', () => {
    toast.show('Changes discarded', { action: { label: 'Undo', run: vi.fn() } });
    const button = el()!.querySelector('button')!;

    button.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    vi.advanceTimersByTime(10_000);
    expect(el()).not.toBeNull();
    button.dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    vi.advanceTimersByTime(6_500);
    expect(el()).toBeNull();

    toast.show('Changes discarded', { action: { label: 'Undo', run: vi.fn() } });
    el()!.dispatchEvent(new MouseEvent('mouseenter'));
    vi.advanceTimersByTime(10_000);
    expect(el()).not.toBeNull();
    el()!.dispatchEvent(new MouseEvent('mouseleave'));
    vi.advanceTimersByTime(6_500);
    expect(el()).toBeNull();
  });
});
