// SSE client for /api/v1/events.
//
// Owns its own reconnect loop (the native EventSource retry is disabled by
// closing the source on error): exponential backoff 1s, 2s, 4s, … capped at
// 30s, reset to 1s once a connection opens AND delivers an event. Exposes a
// Solid `connected` signal for the live dot and a subscribe(type, handler)
// registry. Payloads are small JSON envelopes `{type, repoID?, …}` — clients
// refetch on event, no state diffing over SSE.
//
// Resync on reconnect: the event bus is in-process fire-and-forget with no
// replay, so every event emitted while the socket was down is lost for good.
// Each reconnect therefore synthesizes a client-only `resync` pseudo-event
// (never sent over the wire, deliberately absent from EVENT_TYPES) so views
// refetch and recover whatever they missed. The session's first open is
// skipped: components already fetch their initial resources on mount, so a
// resync there would double-fetch every view. Resync is not a wire event and
// never resets the backoff.
//
// Silent-death detection: the server heartbeats every 25s, so a healthy
// stream never goes 65s without an event. A watchdog force-closes and
// reconnects when it does (NAT idle teardown, wifi→cellular switch, device
// sleep — cases where EventSource never fires onerror).
//
// Always-fresh wake: iOS silently kills a backgrounded PWA's socket, and a
// nominally-"connected" dead socket can otherwise sit green until the 65s
// watchdog fires. So a wake — window `online`/`pageshow`/`focus` and
// visibilitychange→visible — force-cycles the stream unless an event landed
// within the last 10s (FRESH_MS; heartbeats count). That short window is the
// anti-flap guard: a fresh open stamps lastEventAt, so rapid app-switcher
// peeks don't thrash the socket. It is deliberately stricter than the 65s
// STALE_MS watchdog, which stays the silent-death backstop for streams no
// wake event touched.
//
// Presence heartbeat: each open() mints a fresh `crypto.randomUUID()` and
// appends it as the `conn` query param, and the `connID` signal publishes that
// id to the presence reporter (issue #160). The SSE connection IS the presence
// heartbeat — the server registers a conn's presence when the stream opens and
// deletes it on disconnect — so a NEW UUID per connection is load-bearing: a
// reused id could resurrect a presence entry the server already reaped for the
// dead socket, defeating push suppression. connID goes non-null only once THAT
// source's onopen fires (server-side registration is guaranteed only after the
// headers went out), and back to null the instant the current source dies or
// the stream is closed for good.

import { createSignal, type Accessor } from 'solid-js';

export const EVENT_TYPES = [
  'repo.changed',
  'run.changed',
  'parked.changed',
  'clone.progress',
  'provider.auth.changed',
  'issue.changed',
  'cr.changed',
  'run.messages.changed',
  'heartbeat',
] as const;

export type LabEventType = (typeof EVENT_TYPES)[number];

/**
 * One SSE envelope. Beyond `type`/`repoID`, event types carry small scoping
 * fields read through the index signature: `run.messages.changed` carries
 * `runID`, the run's conversational `state`, and — when a tailer read
 * back-patched earlier content — `backpatchSeq`, the LOWEST seq whose content
 * changed since the previous read (absent on pure appends, state-only flips,
 * the first tick, and rotation ticks — issue #175); `run.changed` carries
 * `runID` when the event concerns exactly one run and omits it for genuinely
 * repo-scoped transitions (stop-all, AFK reaper, CR merge, parked cleanup).
 */
export interface LabEvent {
  type: LabEventType | 'resync';
  repoID?: string;
  [key: string]: unknown;
}

export type LabEventHandler = (event: LabEvent) => void;

/** The subset of EventSource we use — injectable for tests. */
export interface EventSourceLike {
  onopen: (() => void) | null;
  onerror: (() => void) | null;
  addEventListener(type: string, listener: (event: MessageEvent<string>) => void): void;
  close(): void;
}

export interface ConnectOptions {
  url?: string;
  /** Initial reconnect delay in ms (default 1000). */
  minDelayMs?: number;
  /** Reconnect delay cap in ms (default 30000). */
  maxDelayMs?: number;
  /** EventSource factory — tests inject a fake here. */
  newEventSource?: (url: string) => EventSourceLike;
}

export interface EventsConnection {
  /** Solid signal: true while the stream is open, false while reconnecting. */
  connected: Accessor<boolean>;
  /**
   * Solid signal: the current connection's `conn` UUID while its stream is open
   * (set on that source's onopen, when server-side presence registration is
   * guaranteed), null before the first open and whenever the source dies or the
   * stream is closed (issue #160). The presence reporter keys its reports on it.
   */
  connID: Accessor<string | null>;
  /**
   * Registers a handler for one event type; returns an unsubscribe fn. The
   * client-only `resync` pseudo-event fires on every reconnect (see header) so
   * views can refetch state lost while the socket was down.
   */
  subscribe(type: LabEventType | 'resync', handler: LabEventHandler): () => void;
  /** Stops reconnecting and closes the stream for good. */
  close(): void;
}

const DEFAULT_URL = '/api/v1/events';
const DEFAULT_MIN_DELAY_MS = 1_000;
const DEFAULT_MAX_DELAY_MS = 30_000;
/** No event (heartbeats included) for this long ⇒ the stream is silently dead. */
const STALE_MS = 65_000; // > 2x the server's 25s heartbeat interval
/** On wake, force-cycle a "connected" stream unless an event landed this recently. */
const FRESH_MS = 10_000; // anti-flap: a fresh open stamps lastEventAt (see header)

export function connectEvents(options: ConnectOptions = {}): EventsConnection {
  const url = options.url ?? DEFAULT_URL;
  const minDelay = options.minDelayMs ?? DEFAULT_MIN_DELAY_MS;
  const maxDelay = options.maxDelayMs ?? DEFAULT_MAX_DELAY_MS;
  const newEventSource =
    options.newEventSource ?? ((u: string) => new EventSource(u) as unknown as EventSourceLike);

  const [connected, setConnected] = createSignal(false);
  const [connID, setConnID] = createSignal<string | null>(null);
  const handlers = new Map<LabEventType | 'resync', Set<LabEventHandler>>();

  let source: EventSourceLike | null = null;
  let delay = minDelay;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let watchdog: ReturnType<typeof setTimeout> | undefined;
  let lastEventAt = 0; // Date.now() of the current source's open or last event
  let closed = false;
  let firstOpen = true; // resync fires on reconnect opens, never the first (see header)

  const dispatch = (type: LabEventType | 'resync', raw: string): void => {
    let event: LabEvent = { type };
    if (raw !== '') {
      try {
        const parsed: unknown = JSON.parse(raw);
        if (typeof parsed === 'object' && parsed !== null) {
          event = { ...(parsed as Record<string, unknown>), type };
        }
      } catch {
        // Malformed payload — deliver the bare event type.
      }
    }
    const set = handlers.get(type);
    if (!set) return;
    for (const handler of set) handler(event);
  };

  const clearWatchdog = (): void => {
    if (watchdog !== undefined) {
      clearTimeout(watchdog);
      watchdog = undefined;
    }
  };

  // Tears down `es` and, unless close()d, reconnects — immediately for wake
  // events, after the current backoff delay (which then doubles) otherwise.
  const fail = (es: EventSourceLike, immediate: boolean): void => {
    es.close(); // we own reconnection — never let EventSource self-retry
    if (source !== es) return; // stale source, already superseded
    source = null;
    clearWatchdog();
    setConnected(false);
    setConnID(null); // the current conn's presence entry dies with the socket
    if (closed) return;
    if (immediate) {
      open();
      return;
    }
    timer = setTimeout(() => {
      timer = undefined;
      open();
    }, delay);
    delay = Math.min(delay * 2, maxDelay);
  };

  // Heartbeat watchdog: STALE_MS of silence while "connected" means the
  // connection died without an error event — force-close and reconnect.
  const armWatchdog = (): void => {
    clearWatchdog();
    watchdog = setTimeout(() => {
      watchdog = undefined;
      if (source !== null) fail(source, false);
    }, STALE_MS);
  };

  const open = (): void => {
    // A fresh UUID per connection: the server keys presence on it and reaps the
    // entry on disconnect, so a reused id could resurrect a dead one (see header).
    const connUUID = crypto.randomUUID();
    const sep = url.includes('?') ? '&' : '?';
    const es = newEventSource(`${url}${sep}conn=${connUUID}`);
    source = es;
    let gotEvent = false;
    es.onopen = () => {
      setConnected(true);
      lastEventAt = Date.now();
      armWatchdog();
      // Publish the conn id only for the live source, and only now: the server
      // registers this conn's presence as the stream opens (once the headers
      // are out), so onopen is the earliest a report can't be dropped as stale.
      if (source === es) setConnID(connUUID);
      // Reconnect = a gap in the fire-and-forget bus, so nudge subscribers to
      // refetch. The session's first open is skipped: views fetch their initial
      // state on mount, and a resync there would just double-fetch every view.
      if (!firstOpen) dispatch('resync', '');
      firstOpen = false;
    };
    es.onerror = () => fail(es, false);
    for (const type of EVENT_TYPES) {
      es.addEventListener(type, (ev) => {
        if (source === es) {
          if (!gotEvent) {
            gotEvent = true;
            delay = minDelay; // backoff resets only after a genuine open + event
          }
          lastEventAt = Date.now();
          armWatchdog();
        }
        dispatch(type, typeof ev.data === 'string' ? ev.data : '');
      });
    }
  };

  // A wake (network back, tab visible, PWA foregrounded): skip the rest of the
  // backoff wait, and force-cycle a nominally-connected stream unless it proved
  // itself fresh within FRESH_MS. iOS kills backgrounded sockets silently, so
  // "connected" alone isn't trustworthy on resume — but a just-opened stream
  // stamps lastEventAt, so quick app-switcher peeks don't thrash the socket.
  const wake = (): void => {
    if (closed) return;
    if (source === null) {
      if (timer !== undefined) {
        clearTimeout(timer);
        timer = undefined;
        open();
      }
      return;
    }
    if (Date.now() - lastEventAt > FRESH_MS) fail(source, true);
  };
  const onVisibilityChange = (): void => {
    if (document.visibilityState === 'visible') wake();
  };

  window.addEventListener('online', wake);
  window.addEventListener('pageshow', wake);
  window.addEventListener('focus', wake);
  document.addEventListener('visibilitychange', onVisibilityChange);

  open();

  return {
    connected,
    connID,
    subscribe(type, handler) {
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
    close() {
      closed = true;
      window.removeEventListener('online', wake);
      window.removeEventListener('pageshow', wake);
      window.removeEventListener('focus', wake);
      document.removeEventListener('visibilitychange', onVisibilityChange);
      if (timer !== undefined) {
        clearTimeout(timer);
        timer = undefined;
      }
      clearWatchdog();
      const es = source;
      source = null;
      es?.close();
      setConnected(false);
      setConnID(null);
    },
  };
}
