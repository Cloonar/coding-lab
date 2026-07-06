import { createRenderEffect, createRoot } from 'solid-js';
import { describe, expect, it } from 'vitest';
import type { LabEvent, LabEventHandler, LabEventType } from '../sse';
import { createCloneProgressStore } from './cloneProgress';

/** Minimal in-memory stand-in for the SSE connection's subscribe registry. */
function fakeEvents() {
  const handlers = new Map<LabEventType, Set<LabEventHandler>>();
  return {
    subscribe(type: LabEventType, handler: LabEventHandler): () => void {
      let set = handlers.get(type);
      if (!set) {
        set = new Set();
        handlers.set(type, set);
      }
      set.add(handler);
      return () => {
        set.delete(handler);
      };
    },
    emit(type: LabEventType, event: LabEvent): void {
      for (const handler of handlers.get(type) ?? []) handler(event);
    },
    handlerCount(type: LabEventType): number {
      return handlers.get(type)?.size ?? 0;
    },
  };
}

describe('createCloneProgressStore', () => {
  it('tracks the latest clone.progress event per repo, independently', () => {
    createRoot((dispose) => {
      const events = fakeEvents();
      const store = createCloneProgressStore(events);

      expect(store.progress('repo_a')).toBeNull();

      events.emit('clone.progress', {
        type: 'clone.progress',
        repoID: 'repo_a',
        phase: 'receiving objects',
        percent: 42,
        line: 'Receiving objects:  42% (84/200)',
      });
      events.emit('clone.progress', {
        type: 'clone.progress',
        repoID: 'repo_b',
        phase: 'counting objects',
        percent: 7,
        line: 'remote: Counting objects:   7%',
      });

      expect(store.progress('repo_a')).toEqual({
        phase: 'receiving objects',
        percent: 42,
        line: 'Receiving objects:  42% (84/200)',
      });
      expect(store.progress('repo_b')?.percent).toBe(7);

      // A newer event replaces only its own repo's entry.
      events.emit('clone.progress', {
        type: 'clone.progress',
        repoID: 'repo_a',
        phase: 'resolving deltas',
        percent: 90,
        line: 'Resolving deltas:  90%',
      });
      expect(store.progress('repo_a')?.phase).toBe('resolving deltas');
      expect(store.progress('repo_b')?.percent).toBe(7);

      dispose();
    });
  });

  it('is reactive: a card effect re-runs on each event for its repo', () => {
    const events = fakeEvents();
    const seen: Array<number | null> = [];

    // Updates are batched while the createRoot body runs, so the emits live
    // outside it; createRenderEffect (not createEffect) tracks synchronously.
    const dispose = createRoot((dispose) => {
      const store = createCloneProgressStore(events);
      createRenderEffect(() => {
        seen.push(store.progress('repo_a')?.percent ?? null);
      });
      return dispose;
    });

    events.emit('clone.progress', {
      type: 'clone.progress',
      repoID: 'repo_a',
      phase: 'receiving objects',
      percent: 10,
      line: '',
    });
    events.emit('clone.progress', {
      type: 'clone.progress',
      repoID: 'repo_a',
      phase: 'receiving objects',
      percent: 55,
      line: '',
    });

    expect(seen).toEqual([null, 10, 55]);
    dispose();
  });

  it('treats a missing percent as null (indeterminate) and tolerates junk fields', () => {
    createRoot((dispose) => {
      const events = fakeEvents();
      const store = createCloneProgressStore(events);

      events.emit('clone.progress', {
        type: 'clone.progress',
        repoID: 'repo_a',
        phase: 'compressing',
        percent: 'not-a-number' as unknown as number,
        line: 42 as unknown as string,
      });

      expect(store.progress('repo_a')).toEqual({ phase: 'compressing', percent: null, line: '' });

      // No repoID → dropped, nothing crashes.
      events.emit('clone.progress', { type: 'clone.progress' });
      expect(store.progress('')).toBeNull();

      dispose();
    });
  });

  it('clear() drops one repo entry; dispose() unsubscribes from the stream', () => {
    createRoot((dispose) => {
      const events = fakeEvents();
      const store = createCloneProgressStore(events);
      expect(events.handlerCount('clone.progress')).toBe(1);

      events.emit('clone.progress', {
        type: 'clone.progress',
        repoID: 'repo_a',
        phase: 'receiving objects',
        percent: 99,
        line: '',
      });
      store.clear('repo_a');
      expect(store.progress('repo_a')).toBeNull();
      store.clear('repo_a'); // idempotent

      store.dispose();
      expect(events.handlerCount('clone.progress')).toBe(0);
      dispose();
    });
  });
});
