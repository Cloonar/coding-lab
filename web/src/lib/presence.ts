// Presence reporter for push suppression (issue #160).
//
// The SSE connection IS the presence heartbeat (see sse.ts): the server
// registers a conn's presence when the stream opens and deletes it on
// disconnect. This controller rides that connection's `connID` signal and tells
// the server, per open, whether the app is visible and which push device this
// browser is — so broadcast Web Push to a visible device is suppressed at send
// time (the service worker still shows every push it is handed, ADR-0039; all
// suppression is server-side).
//
// Fully dependency-injected so the whole truth table runs under jsdom. It must
// be created inside a reactive root (the caller uses Solid's component context):
// the connID effect is the driver.

import { createEffect, type Accessor } from 'solid-js';

export interface PresenceDeps {
  /** api.reportPresence — the fetch path (carries the CSRF header). */
  report: (conn: string, device: string, visible: boolean) => Promise<void>;
  /** navigator.sendBeacon('/api/v1/presence', Blob([body], 'application/json')). */
  beacon: (body: string) => void;
  /** lib/deviceHash.currentDeviceHash — this browser's push device key or null. */
  deviceHash: () => Promise<string | null>;
  /** Event targets; default the real document/window. */
  doc?: Document;
  win?: Window;
}

/**
 * Wires visibility reporting to `connID`. Returns stop(), which removes the
 * listeners. The connID effect it registers is owned by the caller's reactive
 * root and disposed with it.
 */
export function createPresenceReporter(
  connID: Accessor<string | null>,
  deps: PresenceDeps,
): () => void {
  const doc = deps.doc ?? document;
  const win = deps.win ?? window;

  // The cached device hash. `resolved` tells a resolved-to-null (no push
  // subscription — send nothing, ever) apart from not-yet-resolved. The connID
  // effect refreshes this FRESH on every (re)open so a rotated subscription or
  // a newly enabled push self-heals; visibilitychange reuses the cache.
  let hash: string | null = null;
  let resolved = false;

  const visible = (): boolean => doc.visibilityState === 'visible';

  // Fetch-path report of the current visibility. No subscription (hash null) →
  // nothing to suppress, so send nothing. Errors are swallowed: presence is
  // best-effort and the server errs toward notifying, so a dropped report only
  // risks a redundant push, never a lost one.
  const send = (conn: string): void => {
    const device = hash;
    if (device === null) return;
    void deps.report(conn, device, visible()).catch(() => {
      // best-effort — a failed presence report is not worth surfacing
    });
  };

  // Resolve the hash fresh, then report. Drops the whole thing if connID moved
  // on while the resolve was in flight: that report would name a superseded
  // connection the server may already have reaped.
  const resolveThenSend = async (conn: string): Promise<void> => {
    const fresh = await deps.deviceHash();
    if (connID() !== conn) return; // stale: a reconnect superseded this open mid-resolve
    hash = fresh;
    resolved = true;
    send(conn);
  };

  // Every stream (re)open re-resolves the hash FRESH and reports visibility.
  createEffect(() => {
    const conn = connID();
    if (conn === null) return; // no live stream to attribute presence to
    void resolveThenSend(conn);
  });

  const onVisibility = (): void => {
    const conn = connID();
    if (conn === null) return; // no live stream yet — the next open will report
    if (resolved) {
      send(conn);
      return;
    }
    void resolveThenSend(conn);
  };

  // The synchronous laptop-lid / app-swipe close: no await is possible, so this
  // can only fire when a hash is already cached (a resolved-null hash sends
  // nothing, same as the fetch path). sendBeacon survives the page teardown a
  // fetch would not — it closes the "went away without a visibilitychange" gap.
  const onPageHide = (): void => {
    const conn = connID();
    if (conn === null || hash === null) return;
    deps.beacon(JSON.stringify({ conn, device: hash, visible: false }));
  };

  doc.addEventListener('visibilitychange', onVisibility);
  win.addEventListener('pagehide', onPageHide);

  return () => {
    doc.removeEventListener('visibilitychange', onVisibility);
    win.removeEventListener('pagehide', onPageHide);
  };
}
