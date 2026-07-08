// SSE client for /api/v1/events.
//
// Owns its own reconnect loop (the native EventSource retry is disabled by
// closing the source on error): exponential backoff 1s, 2s, 4s, … capped at
// 30s, reset to 1s once a connection opens AND delivers an event. Exposes a
// Solid `connected` signal for the live dot and a subscribe(type, handler)
// registry. Payloads are small JSON envelopes `{type, repoID?, …}` — clients
// refetch on event, no state diffing over SSE.
//
// Silent-death detection: the server heartbeats every 25s, so a healthy
// stream never goes 65s without an event. A watchdog force-closes and
// reconnects when it does (NAT idle teardown, wifi→cellular switch, device
// sleep — cases where EventSource never fires onerror). Window `online` and
// visibilitychange→visible reconnect immediately when the stream is stale.

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

export interface LabEvent {
  type: LabEventType;
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
  /** Registers a handler for one event type; returns an unsubscribe fn. */
  subscribe(type: LabEventType, handler: LabEventHandler): () => void;
  /** Stops reconnecting and closes the stream for good. */
  close(): void;
}

const DEFAULT_URL = '/api/v1/events';
const DEFAULT_MIN_DELAY_MS = 1_000;
const DEFAULT_MAX_DELAY_MS = 30_000;
/** No event (heartbeats included) for this long ⇒ the stream is silently dead. */
const STALE_MS = 65_000; // > 2x the server's 25s heartbeat interval

export function connectEvents(options: ConnectOptions = {}): EventsConnection {
  const url = options.url ?? DEFAULT_URL;
  const minDelay = options.minDelayMs ?? DEFAULT_MIN_DELAY_MS;
  const maxDelay = options.maxDelayMs ?? DEFAULT_MAX_DELAY_MS;
  const newEventSource =
    options.newEventSource ?? ((u: string) => new EventSource(u) as unknown as EventSourceLike);

  const [connected, setConnected] = createSignal(false);
  const handlers = new Map<LabEventType, Set<LabEventHandler>>();

  let source: EventSourceLike | null = null;
  let delay = minDelay;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let watchdog: ReturnType<typeof setTimeout> | undefined;
  let lastEventAt = 0; // Date.now() of the current source's open or last event
  let closed = false;

  const dispatch = (type: LabEventType, raw: string): void => {
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
    const es = newEventSource(url);
    source = es;
    let gotEvent = false;
    es.onopen = () => {
      setConnected(true);
      lastEventAt = Date.now();
      armWatchdog();
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

  // Network back / tab visible again: skip the rest of the backoff wait, and
  // force-cycle a nominally connected stream that is stale (a slept device
  // fires visibilitychange before its suspended watchdog timer gets to run).
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
    if (Date.now() - lastEventAt > STALE_MS) fail(source, true);
  };
  const onVisibilityChange = (): void => {
    if (document.visibilityState === 'visible') wake();
  };

  window.addEventListener('online', wake);
  document.addEventListener('visibilitychange', onVisibilityChange);

  open();

  return {
    connected,
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
    },
  };
}
