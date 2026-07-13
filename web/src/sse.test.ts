import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { EVENT_TYPES, connectEvents, type EventSourceLike, type LabEvent } from './sse';

class FakeEventSource implements EventSourceLike {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  private listeners = new Map<string, Array<(event: MessageEvent<string>) => void>>();

  constructor(public readonly url: string) {}

  addEventListener(type: string, listener: (event: MessageEvent<string>) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  close(): void {
    this.closed = true;
  }

  open(): void {
    this.onopen?.();
  }

  error(): void {
    this.onerror?.();
  }

  emit(type: string, data: string): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data } as MessageEvent<string>);
    }
  }
}

function makeFactory() {
  const instances: FakeEventSource[] = [];
  const factory = (url: string) => {
    const es = new FakeEventSource(url);
    instances.push(es);
    return es;
  };
  return { instances, factory };
}

function last(instances: FakeEventSource[]): FakeEventSource {
  return instances[instances.length - 1]!;
}

// v4 UUID shape (crypto.randomUUID); case-insensitive on the hex.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

/** The `conn` query param of a stream URL (relative → parsed against a stub base). */
function connOf(url: string): string {
  return new URL(url, 'http://lab.test').searchParams.get('conn') ?? '';
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('connectEvents backoff', () => {
  it('reconnects on the 1s,2s,4s,8s,16s schedule, caps at 30s and resets after open', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    expect(instances).toHaveLength(1);
    expect(instances[0]!.url).toMatch(/^\/api\/v1\/events\?conn=/);
    expect(connOf(instances[0]!.url)).toMatch(UUID_RE);

    // Walk the doubling schedule: each error schedules the next attempt.
    const schedule = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000];
    for (const [i, delay] of schedule.entries()) {
      last(instances).error();
      expect(instances).toHaveLength(i + 1); // nothing until the timer fires
      vi.advanceTimersByTime(delay - 1);
      expect(instances).toHaveLength(i + 1);
      vi.advanceTimersByTime(1);
      expect(instances).toHaveLength(i + 2);
    }

    // A successful open followed by a real event resets the backoff to 1s.
    last(instances).open();
    last(instances).emit('heartbeat', '');
    expect(conn.connected()).toBe(true);

    const count = instances.length;
    last(instances).error();
    expect(conn.connected()).toBe(false);
    vi.advanceTimersByTime(999);
    expect(instances).toHaveLength(count);
    vi.advanceTimersByTime(1);
    expect(instances).toHaveLength(count + 1);

    conn.close();
  });

  it('tracks the connected signal and closes the broken source before retrying', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    expect(conn.connected()).toBe(false);
    instances[0]!.open();
    expect(conn.connected()).toBe(true);

    instances[0]!.error();
    expect(conn.connected()).toBe(false);
    expect(instances[0]!.closed).toBe(true);

    conn.close();
  });

  it('close() stops the reconnect loop for good', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.error();
    conn.close();
    vi.advanceTimersByTime(120_000);
    expect(instances).toHaveLength(1);

    // An error after close never resurrects the connection.
    const conn2 = connectEvents({ newEventSource: factory });
    conn2.close();
    last(instances).error();
    vi.advanceTimersByTime(120_000);
    expect(instances).toHaveLength(2);
  });
});

describe('connectEvents staleness watchdog', () => {
  it('force-reconnects a silently dead connection after 65s without any event', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');
    expect(conn.connected()).toBe(true);

    // Silence just short of the threshold: still considered live.
    vi.advanceTimersByTime(64_999);
    expect(conn.connected()).toBe(true);
    expect(instances).toHaveLength(1);
    expect(instances[0]!.closed).toBe(false);

    // 65s of total silence: the watchdog closes the source and reconnects.
    vi.advanceTimersByTime(1);
    expect(instances[0]!.closed).toBe(true);
    expect(conn.connected()).toBe(false);

    // The heartbeat had reset the backoff, so the retry lands 1s later.
    vi.advanceTimersByTime(1_000);
    expect(instances).toHaveLength(2);

    conn.close();
  });

  it('heartbeat arrivals keep a quiet connection alive indefinitely', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    for (let i = 0; i < 6; i += 1) {
      vi.advanceTimersByTime(25_000); // the server's heartbeat cadence
      instances[0]!.emit('heartbeat', '');
    }
    vi.advanceTimersByTime(60_000); // < 65s since the last heartbeat

    expect(conn.connected()).toBe(true);
    expect(instances).toHaveLength(1);
    expect(instances[0]!.closed).toBe(false);

    conn.close();
  });

  it('resets the backoff only after the opened stream delivers an event', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    // Two failures: retries land at 1s then 2s, so the next delay is 4s.
    instances[0]!.error();
    vi.advanceTimersByTime(1_000);
    instances[1]!.error();
    vi.advanceTimersByTime(2_000);
    expect(instances).toHaveLength(3);

    // Open WITHOUT any event, then die: the backoff keeps doubling (4s) —
    // a server that accepts connections but stays silent is not "up".
    instances[2]!.open();
    instances[2]!.error();
    vi.advanceTimersByTime(3_999);
    expect(instances).toHaveLength(3);
    vi.advanceTimersByTime(1);
    expect(instances).toHaveLength(4);

    // Once an event arrives, the next failure retries at the 1s minimum.
    instances[3]!.open();
    instances[3]!.emit('heartbeat', '');
    instances[3]!.error();
    vi.advanceTimersByTime(1_000);
    expect(instances).toHaveLength(5);

    conn.close();
  });
});

describe('connectEvents wake events', () => {
  it("window 'online' skips the remaining backoff wait", () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.error();
    vi.advanceTimersByTime(1_000);
    instances[1]!.error(); // next retry scheduled 2s out
    vi.advanceTimersByTime(500);
    expect(instances).toHaveLength(2);

    window.dispatchEvent(new Event('online'));
    expect(instances).toHaveLength(3); // reconnected immediately

    // The abandoned backoff timer never fires a duplicate attempt.
    vi.advanceTimersByTime(120_000);
    expect(instances).toHaveLength(3);

    conn.close();
  });

  it('visibilitychange→visible force-cycles a stale "connected" stream', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');
    expect(conn.connected()).toBe(true);

    // Simulate device sleep/wake: the clock jumps far past the staleness
    // threshold while the suspended watchdog timer has not fired yet.
    vi.setSystemTime(Date.now() + 300_000);
    document.dispatchEvent(new Event('visibilitychange'));

    expect(instances[0]!.closed).toBe(true);
    expect(instances).toHaveLength(2); // reconnected immediately, no backoff wait

    conn.close();
  });

  it('visibilitychange on a fresh connection is a no-op', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');
    document.dispatchEvent(new Event('visibilitychange'));

    expect(instances).toHaveLength(1);
    expect(conn.connected()).toBe(true);
    expect(instances[0]!.closed).toBe(false);

    conn.close();
  });

  it('wake events after close() never resurrect the connection', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.error(); // reconnect pending
    conn.close();

    window.dispatchEvent(new Event('online'));
    document.dispatchEvent(new Event('visibilitychange'));
    vi.advanceTimersByTime(120_000);

    expect(instances).toHaveLength(1);
  });

  it('visibilitychange→visible force-cycles a stale-past-FRESH_MS connected stream', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');
    expect(conn.connected()).toBe(true);

    // Past FRESH_MS (10s) but well under the 65s watchdog: the watchdog has
    // not fired, yet a foreground resume must not trust the "connected" socket.
    vi.advanceTimersByTime(15_000);
    expect(instances).toHaveLength(1); // watchdog has not tripped

    document.dispatchEvent(new Event('visibilitychange'));

    expect(instances[0]!.closed).toBe(true);
    expect(instances).toHaveLength(2); // new source immediately, no backoff wait

    conn.close();
  });

  it('a wake within FRESH_MS leaves a nominally connected stream untouched', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');

    vi.advanceTimersByTime(5_000); // still inside the freshness window
    document.dispatchEvent(new Event('visibilitychange'));

    expect(instances).toHaveLength(1);
    expect(conn.connected()).toBe(true);
    expect(instances[0]!.closed).toBe(false);

    conn.close();
  });

  it('pageshow force-cycles a stream stale past FRESH_MS', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');
    vi.advanceTimersByTime(15_000);

    window.dispatchEvent(new Event('pageshow'));

    expect(instances[0]!.closed).toBe(true);
    expect(instances).toHaveLength(2);

    conn.close();
  });

  it('focus force-cycles a stream stale past FRESH_MS', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');
    vi.advanceTimersByTime(15_000);

    window.dispatchEvent(new Event('focus'));

    expect(instances[0]!.closed).toBe(true);
    expect(instances).toHaveLength(2);

    conn.close();
  });

  it('pageshow and focus after close() never resurrect the connection', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.error(); // reconnect pending
    conn.close();

    window.dispatchEvent(new Event('pageshow'));
    window.dispatchEvent(new Event('focus'));
    vi.advanceTimersByTime(120_000);

    expect(instances).toHaveLength(1);
  });
});

describe('connectEvents resync', () => {
  it('is a client-only pseudo-event, absent from the wire vocabulary', () => {
    expect(EVENT_TYPES).not.toContain('resync');
  });

  it("fires resync on reconnect opens but never on the session's first open", () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    const resyncs: LabEvent[] = [];
    conn.subscribe('resync', (e) => resyncs.push(e));

    // First open of the session: mount-time fetches already have the state,
    // so no resync — a burst here would double-fetch every view.
    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');
    expect(resyncs).toHaveLength(0);

    // Drop and reconnect: the second open synthesizes exactly one resync.
    instances[0]!.error();
    vi.advanceTimersByTime(1_000);
    expect(instances).toHaveLength(2);
    instances[1]!.open();

    expect(resyncs).toEqual([{ type: 'resync' }]);

    conn.close();
  });

  it('resync on a reconnect open does not reset the backoff', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    const resyncs: LabEvent[] = [];
    conn.subscribe('resync', (e) => resyncs.push(e));

    // Establish a real session: first open + a wire event resets backoff to 1s.
    instances[0]!.open();
    instances[0]!.emit('heartbeat', '');
    expect(resyncs).toHaveLength(0); // first open never resyncs

    // Drop; the retry lands 1s later and the reconnect open fires a resync.
    instances[0]!.error();
    vi.advanceTimersByTime(1_000);
    instances[1]!.open();
    expect(resyncs).toEqual([{ type: 'resync' }]);

    // That open carried no wire event, so it must NOT have reset the backoff:
    // the next failure retries at 2s, not the 1s minimum.
    instances[1]!.error();
    vi.advanceTimersByTime(1_999);
    expect(instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(instances).toHaveLength(3);

    conn.close();
  });
});

describe('connectEvents subscriptions', () => {
  it('delivers parsed envelopes to type-scoped handlers', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    const repoEvents: LabEvent[] = [];
    const heartbeats: LabEvent[] = [];
    conn.subscribe('repo.changed', (e) => repoEvents.push(e));
    conn.subscribe('heartbeat', (e) => heartbeats.push(e));

    instances[0]!.open();
    instances[0]!.emit('repo.changed', '{"type":"repo.changed","repoID":"repo_abc"}');
    instances[0]!.emit('run.changed', '{"type":"run.changed"}'); // no subscriber — ignored
    instances[0]!.emit('heartbeat', '');

    expect(repoEvents).toEqual([{ type: 'repo.changed', repoID: 'repo_abc' }]);
    expect(heartbeats).toEqual([{ type: 'heartbeat' }]);

    conn.close();
  });

  it('registers the provider-generic auth event and delivers its provider id (issue #51)', () => {
    // The rename is a clean break: claude.auth.changed is gone from the wire
    // vocabulary; provider.auth.changed carries the provider id.
    expect(EVENT_TYPES).toContain('provider.auth.changed');
    expect(EVENT_TYPES).not.toContain('claude.auth.changed');

    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    const seen: LabEvent[] = [];
    conn.subscribe('provider.auth.changed', (e) => seen.push(e));

    instances[0]!.open();
    instances[0]!.emit(
      'provider.auth.changed',
      '{"type":"provider.auth.changed","provider":"claude-code"}',
    );
    expect(seen).toEqual([{ type: 'provider.auth.changed', provider: 'claude-code' }]);

    conn.close();
  });

  it('survives malformed payloads and honors unsubscribe', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    const seen: LabEvent[] = [];
    const unsubscribe = conn.subscribe('issue.changed', (e) => seen.push(e));

    instances[0]!.emit('issue.changed', '{not json');
    expect(seen).toEqual([{ type: 'issue.changed' }]);

    unsubscribe();
    instances[0]!.emit('issue.changed', '{"type":"issue.changed"}');
    expect(seen).toHaveLength(1);

    conn.close();
  });

  it('keeps subscriptions across reconnects', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    const seen: LabEvent[] = [];
    conn.subscribe('cr.changed', (e) => seen.push(e));

    instances[0]!.error();
    vi.advanceTimersByTime(1_000);
    expect(instances).toHaveLength(2);

    instances[1]!.open();
    instances[1]!.emit('cr.changed', '{"type":"cr.changed","repoID":"repo_x"}');
    expect(seen).toEqual([{ type: 'cr.changed', repoID: 'repo_x' }]);

    conn.close();
  });
});

describe('connectEvents presence conn (issue #160)', () => {
  it('appends a UUID conn param and gives every reconnect a different one', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    const first = connOf(instances[0]!.url);
    expect(first).toMatch(UUID_RE);

    // Drop and reconnect: the fresh source carries a NEW conn (a reused id could
    // resurrect a presence entry the server already reaped for the dead socket).
    instances[0]!.error();
    vi.advanceTimersByTime(1_000);
    expect(instances).toHaveLength(2);
    const second = connOf(instances[1]!.url);
    expect(second).toMatch(UUID_RE);
    expect(second).not.toBe(first);

    conn.close();
  });

  it('tracks connID: null before open, the UUID after, null after error, fresh UUID next open', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    // Registration is guaranteed server-side only once the headers are out, so
    // connID stays null until THIS source's onopen fires.
    expect(conn.connID()).toBeNull();

    instances[0]!.open();
    const firstID = conn.connID();
    expect(firstID).toMatch(UUID_RE);
    expect(firstID).toBe(connOf(instances[0]!.url)); // the id on the wire, exactly

    // The source dies: presence for it is gone, so connID drops back to null.
    instances[0]!.error();
    expect(conn.connID()).toBeNull();

    // The reconnect open publishes the new source's own (different) conn.
    vi.advanceTimersByTime(1_000);
    instances[1]!.open();
    const secondID = conn.connID();
    expect(secondID).toMatch(UUID_RE);
    expect(secondID).toBe(connOf(instances[1]!.url));
    expect(secondID).not.toBe(firstID);

    conn.close();
  });

  it('resets connID to null on close()', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ newEventSource: factory });

    instances[0]!.open();
    expect(conn.connID()).not.toBeNull();

    conn.close();
    expect(conn.connID()).toBeNull();
  });

  it('appends with & when the configured url already carries a query string', () => {
    const { instances, factory } = makeFactory();
    const conn = connectEvents({ url: '/api/v1/events?tenant=acme', newEventSource: factory });

    expect(instances[0]!.url).toMatch(/^\/api\/v1\/events\?tenant=acme&conn=/);
    expect(connOf(instances[0]!.url)).toMatch(UUID_RE);

    conn.close();
  });
});
