// createLiveResource contract: it owns a createResource plus its SSE
// subscriptions and the reconnect resync, so a view can never forget to
// refetch state lost while the socket was down (issue #54).
//
// Harness: the real EventsProvider drives the real connectEvents over a stubbed
// global EventSource (the FakeEventSource shape from RepoIssues.test.tsx, plus
// open()/error() to drive the reconnect). No fetch stub — the fetchers are
// local vi.fn spies. resync has no wire event: it is synthesized by sse.ts on a
// reconnect open, so we drive error → advance the 1s backoff → open the fresh
// source (never on the session's first open, which the mount consumes).

import { render } from 'solid-js/web';
import type { ResourceReturn } from 'solid-js';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { EventsProvider } from '../events';
import { createLiveResource } from './liveResource';

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((event: { data: string }) => void)[]>();

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: { data: string }) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  close(): void {}

  open(): void {
    this.onopen?.();
  }

  error(): void {
    this.onerror?.();
  }

  emit(type: string, payload: Record<string, unknown> = {}): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data: JSON.stringify(payload) });
    }
  }
}

let container: HTMLDivElement;
let dispose: (() => void) | undefined;
// Typed as an unknown-returning fetcher so the resource is Resource<unknown>,
// matching the pass-through tuple; the spies ignore their args and count calls.
let fetcher: Mock<(source: unknown, info: unknown) => unknown>;
let setupResource: () => ResourceReturn<unknown, unknown>;
let live: ResourceReturn<unknown, unknown>;

function Harness() {
  live = setupResource();
  return <div class="val">{String(live[0]() ?? '')}</div>;
}

function lastSource(): FakeEventSource {
  const list = FakeEventSource.instances;
  const es = list[list.length - 1];
  if (!es) throw new Error('no EventSource instance yet');
  return es;
}

// Mount under the real provider, then consume the session's FIRST open so a
// later error→reconnect open synthesizes a resync (sse.ts skips the first).
function mount(): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(
    () => (
      <EventsProvider>
        <Harness />
      </EventsProvider>
    ),
    container,
  );
  lastSource().open();
}

function emit(type: string, payload: Record<string, unknown> = {}): void {
  lastSource().emit(type, payload);
}

// Reconnect: kill the current source, let the 1s backoff elapse, open the new
// one. That second open is what makes sse.ts dispatch the resync pseudo-event.
function triggerResync(): void {
  lastSource().error();
  vi.advanceTimersByTime(1_000);
  lastSource().open();
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal('EventSource', FakeEventSource);
  FakeEventSource.instances = [];
  fetcher = vi.fn<(source: unknown, info: unknown) => unknown>(() => 'value');
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('createLiveResource event refetch', () => {
  it('refetches on a matching event', () => {
    setupResource = () =>
      createLiveResource(fetcher, [{ type: 'issue.changed', match: (e) => e.repoID === 'repo_1' }]);
    mount();
    expect(fetcher).toHaveBeenCalledTimes(1);

    emit('issue.changed', { repoID: 'repo_1' });
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it('does not refetch when match returns false', () => {
    setupResource = () =>
      createLiveResource(fetcher, [{ type: 'issue.changed', match: (e) => e.repoID === 'repo_1' }]);
    mount();
    expect(fetcher).toHaveBeenCalledTimes(1);

    emit('issue.changed', { repoID: 'repo_other' });
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it('refetches on every event of the type when no match is given', () => {
    setupResource = () => createLiveResource(fetcher, [{ type: 'repo.changed' }]);
    mount();
    expect(fetcher).toHaveBeenCalledTimes(1);

    emit('repo.changed', { repoID: 'a' });
    emit('repo.changed', { repoID: 'b' });
    expect(fetcher).toHaveBeenCalledTimes(3);
  });
});

describe('createLiveResource debounce', () => {
  it('coalesces a burst into a single trailing refetch', () => {
    setupResource = () =>
      createLiveResource(fetcher, [{ type: 'run.messages.changed', debounceMs: 50 }]);
    mount();
    expect(fetcher).toHaveBeenCalledTimes(1);

    emit('run.messages.changed');
    emit('run.messages.changed');
    emit('run.messages.changed');
    // Nothing fires while the window is still open.
    expect(fetcher).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(49);
    expect(fetcher).toHaveBeenCalledTimes(1);

    // One trailing refetch once the window elapses — the burst coalesced.
    vi.advanceTimersByTime(1);
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
});

describe('createLiveResource resync', () => {
  it('refetches immediately and unconditionally, ignoring match', () => {
    setupResource = () =>
      createLiveResource(fetcher, [{ type: 'repo.changed', match: () => false }]);
    mount();
    expect(fetcher).toHaveBeenCalledTimes(1);

    // The match blocks the normal event path...
    emit('repo.changed', { repoID: 'a' });
    expect(fetcher).toHaveBeenCalledTimes(1);

    // ...but resync means "state is untrustworthy" and refetches regardless.
    triggerResync();
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it('cancels a pending debounce so it fires once, not twice', () => {
    setupResource = () =>
      createLiveResource(fetcher, [{ type: 'run.messages.changed', debounceMs: 5_000 }]);
    mount();
    expect(fetcher).toHaveBeenCalledTimes(1);

    // Arm a debounce, then resync before the (long) window elapses.
    emit('run.messages.changed');
    expect(fetcher).toHaveBeenCalledTimes(1);

    triggerResync(); // advances 1s < 5s, so the debounce has not fired yet
    expect(fetcher).toHaveBeenCalledTimes(2); // exactly the one immediate resync refetch

    // The cancelled debounce timer never fires its now-redundant refetch.
    vi.advanceTimersByTime(5_000);
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
});

describe('createLiveResource cleanup', () => {
  it('unsubscribes and clears timers on dispose', () => {
    setupResource = () =>
      createLiveResource(fetcher, [
        { type: 'repo.changed' },
        { type: 'run.messages.changed', debounceMs: 50 },
      ]);
    mount();
    expect(fetcher).toHaveBeenCalledTimes(1);

    // Leave a debounce timer pending across the dispose.
    emit('run.messages.changed');
    dispose?.();
    dispose = undefined;

    // Nothing the disposed resource subscribed to can reach the fetcher again,
    // and the pending debounce timer was cleared rather than left to throw.
    emit('repo.changed', { repoID: 'a' });
    lastSource().error();
    lastSource().open();
    vi.advanceTimersByTime(120_000);
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});

describe('createLiveResource arities', () => {
  it('supports (fetcher, specs) with no source', () => {
    setupResource = () => createLiveResource(fetcher, [{ type: 'repo.changed' }]);
    mount();

    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(fetcher.mock.calls[0]?.[0]).toBe(true); // Solid passes `true` when sourceless
    emit('repo.changed');
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it('supports (source, fetcher, specs) and passes the source value to the fetcher', () => {
    setupResource = () => createLiveResource(() => 'src-key', fetcher, [{ type: 'repo.changed' }]);
    mount();

    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(fetcher.mock.calls[0]?.[0]).toBe('src-key');
    emit('repo.changed');
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
});

describe('createLiveResource return tuple', () => {
  it('exposes a working refetch and mutate', async () => {
    setupResource = () => createLiveResource(fetcher, [{ type: 'repo.changed' }]);
    mount();
    expect(fetcher).toHaveBeenCalledTimes(1);

    // refetch re-runs the fetcher.
    const { refetch, mutate } = live[1];
    void refetch();
    expect(fetcher).toHaveBeenCalledTimes(2);

    // mutate writes the value straight into the resource, no fetch.
    mutate('mutated');
    await Promise.resolve();
    expect(container.querySelector('.val')?.textContent).toBe('mutated');
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
});
