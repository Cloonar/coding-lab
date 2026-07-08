// Queued first-message store (issue #41):
// - set/peek parks text per run id without consuming it;
// - take returns AND removes in one step (the double-send guard) and answers
//   undefined once the entry is gone;
// - clear drops an entry without sending;
// - entries are independent per run id;
// - every mutation bumps the reactive version so a tracking consumer re-runs,
//   while a no-op take/clear on an absent id does not.

import { createRenderEffect, createRoot } from 'solid-js';
import { afterEach, describe, expect, it } from 'vitest';
import { clearQueued, peekQueued, queuedVersion, setQueued, takeQueued } from './queuedMessage';

// Module-global map — clear the ids these tests touch so nothing leaks across.
afterEach(() => {
  clearQueued('run_a');
  clearQueued('run_b');
});

describe('queuedMessage store', () => {
  it('parks text and reads it back without consuming', () => {
    setQueued('run_a', 'first message');
    expect(peekQueued('run_a')).toBe('first message');
    // Peek does not consume — a second peek still sees it.
    expect(peekQueued('run_a')).toBe('first message');
  });

  it('take returns the text and removes the entry', () => {
    setQueued('run_a', 'ship it');
    expect(takeQueued('run_a')).toBe('ship it');
    expect(peekQueued('run_a')).toBeUndefined();
    // A second take is empty — this is the idempotence the auto-send relies on.
    expect(takeQueued('run_a')).toBeUndefined();
  });

  it('take on an absent id answers undefined', () => {
    expect(takeQueued('run_a')).toBeUndefined();
  });

  it('clear drops an entry without returning it', () => {
    setQueued('run_a', 'discard me');
    clearQueued('run_a');
    expect(peekQueued('run_a')).toBeUndefined();
  });

  it('keeps entries independent per run id', () => {
    setQueued('run_a', 'for A');
    setQueued('run_b', 'for B');
    expect(takeQueued('run_a')).toBe('for A');
    // Taking A leaves B untouched.
    expect(peekQueued('run_b')).toBe('for B');
  });

  it('bumps the reactive version on mutation and re-runs a tracking consumer', () => {
    const seen: Array<string | undefined> = [];

    // Mutations live OUTSIDE the root body (they are batched while it runs);
    // createRenderEffect (not createEffect) tracks synchronously, like the
    // cloneProgress store's reactivity test.
    const dispose = createRoot((dispose) => {
      createRenderEffect(() => {
        queuedVersion(); // subscribe
        seen.push(peekQueued('run_a'));
      });
      return dispose;
    });

    setQueued('run_a', 'live');
    takeQueued('run_a');
    // A no-op clear (nothing queued) must NOT bump the version → no extra run.
    clearQueued('run_a');

    expect(seen).toEqual([undefined, 'live', undefined]);
    dispose();
  });
});
