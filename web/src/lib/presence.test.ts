// presence reporter contract (issue #160). Fully dependency-injected: connID is
// a plain signal, report/beacon/deviceHash are spies, and document/window are
// fake event targets. Pins that a report follows every (re)open with fresh
// visibility, that a reconnect re-resolves the device hash, that
// visibilitychange and the synchronous pagehide beacon carry the right
// visibility, that a hash-less device sends nothing, that a connID that moved on
// mid-resolve drops the report, and that stop() unwires both listeners.

import { createRoot, createSignal } from 'solid-js';
import { describe, expect, it, vi } from 'vitest';
import { createPresenceReporter, type PresenceDeps } from './presence';

/** Microtask + macrotask flush: lets the connID effect run and the deviceHash
 *  promise settle (real timers — this suite drives no fake clock). */
const tick = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

class FakeTarget {
  private listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, fn: EventListener): void {
    const set = this.listeners.get(type) ?? new Set<EventListener>();
    set.add(fn);
    this.listeners.set(type, set);
  }
  removeEventListener(type: string, fn: EventListener): void {
    this.listeners.get(type)?.delete(fn);
  }
  fire(type: string): void {
    for (const fn of this.listeners.get(type) ?? []) (fn as () => void)();
  }
  count(type: string): number {
    return this.listeners.get(type)?.size ?? 0;
  }
}

class FakeDoc extends FakeTarget {
  visibilityState: DocumentVisibilityState = 'visible';
}

function setup(over: Partial<Pick<PresenceDeps, 'deviceHash'>> = {}) {
  const report = vi.fn<(conn: string, device: string, visible: boolean) => Promise<void>>(() =>
    Promise.resolve(),
  );
  const beacon = vi.fn<(body: string) => void>(() => undefined);
  const deviceHash = over.deviceHash ?? vi.fn(() => Promise.resolve<string | null>('DEVHASH'));
  const doc = new FakeDoc();
  const win = new FakeTarget();

  let stop!: () => void;
  let setConn!: (value: string | null) => void;
  const dispose = createRoot((d) => {
    const [connID, set] = createSignal<string | null>(null);
    setConn = set;
    stop = createPresenceReporter(connID, {
      report,
      beacon,
      deviceHash,
      doc: doc as unknown as Document,
      win: win as unknown as Window,
    });
    return d;
  });

  return { report, beacon, deviceHash, doc, win, setConn, stop, dispose };
}

describe('createPresenceReporter', () => {
  it('reports current visibility once the connID appears', async () => {
    const h = setup();
    h.setConn('conn-1');
    await tick();

    expect(h.deviceHash).toHaveBeenCalledTimes(1);
    expect(h.report).toHaveBeenCalledTimes(1);
    expect(h.report).toHaveBeenCalledWith('conn-1', 'DEVHASH', true);

    h.stop();
    h.dispose();
  });

  it('re-resolves the hash and re-reports on reconnect (a new connID)', async () => {
    const h = setup();
    h.setConn('conn-1');
    await tick();
    h.setConn(null); // socket died: no live conn to report for
    await tick();
    h.setConn('conn-2'); // reconnect with a fresh conn
    await tick();

    // The null transition resolves nothing; each real open re-resolves fresh.
    expect(h.deviceHash).toHaveBeenCalledTimes(2);
    expect(h.report).toHaveBeenCalledTimes(2);
    expect(h.report).toHaveBeenLastCalledWith('conn-2', 'DEVHASH', true);

    h.stop();
    h.dispose();
  });

  it('reports visible:false on a visibilitychange to hidden, reusing the cached hash', async () => {
    const h = setup();
    h.setConn('conn-1');
    await tick();
    h.report.mockClear();

    h.doc.visibilityState = 'hidden';
    h.doc.fire('visibilitychange');
    await tick();

    expect(h.report).toHaveBeenCalledWith('conn-1', 'DEVHASH', false);
    // The hash was cached on open — visibilitychange must not re-resolve it.
    expect(h.deviceHash).toHaveBeenCalledTimes(1);

    h.stop();
    h.dispose();
  });

  it('beacons visible:false with the cached device hash on pagehide', async () => {
    const h = setup();
    h.setConn('conn-1');
    await tick();

    h.win.fire('pagehide');

    expect(h.beacon).toHaveBeenCalledTimes(1);
    expect(JSON.parse(h.beacon.mock.calls[0]![0])).toEqual({
      conn: 'conn-1',
      device: 'DEVHASH',
      visible: false,
    });

    h.stop();
    h.dispose();
  });

  it('never beacons before a hash is cached (nothing to suppress yet)', async () => {
    const h = setup();
    // pagehide with no connID / no cached hash at all.
    h.win.fire('pagehide');
    expect(h.beacon).not.toHaveBeenCalled();

    h.stop();
    h.dispose();
  });

  it('sends nothing at all when the device has no push subscription (hash null)', async () => {
    const h = setup({ deviceHash: vi.fn(() => Promise.resolve<string | null>(null)) });
    h.setConn('conn-1');
    await tick();

    h.doc.visibilityState = 'hidden';
    h.doc.fire('visibilitychange');
    await tick();
    h.win.fire('pagehide');

    expect(h.report).not.toHaveBeenCalled();
    expect(h.beacon).not.toHaveBeenCalled();

    h.stop();
    h.dispose();
  });

  it('drops a report whose connID changed while the hash resolve was in flight', async () => {
    const resolvers: Array<(value: string | null) => void> = [];
    const deviceHash = vi.fn(
      () => new Promise<string | null>((resolve) => resolvers.push(resolve)),
    );
    const h = setup({ deviceHash });

    h.setConn('conn-1');
    await tick(); // effect runs, deviceHash() #1 pending
    h.setConn('conn-2');
    await tick(); // effect runs again, deviceHash() #2 pending
    expect(deviceHash).toHaveBeenCalledTimes(2);

    // Resolve the STALE (conn-1) lookup: connID is now conn-2, so it is dropped.
    resolvers[0]!('HASH1');
    await tick();
    expect(h.report).not.toHaveBeenCalled();

    // Resolve the current (conn-2) lookup: this one reports.
    resolvers[1]!('HASH2');
    await tick();
    expect(h.report).toHaveBeenCalledTimes(1);
    expect(h.report).toHaveBeenCalledWith('conn-2', 'HASH2', true);

    h.stop();
    h.dispose();
  });

  it('stop() removes both listeners', () => {
    const h = setup();
    expect(h.doc.count('visibilitychange')).toBe(1);
    expect(h.win.count('pagehide')).toBe(1);

    h.stop();

    expect(h.doc.count('visibilitychange')).toBe(0);
    expect(h.win.count('pagehide')).toBe(0);

    h.dispose();
  });
});
