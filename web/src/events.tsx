// One SSE connection per authenticated app instance, shared via context so
// navigation between pages never tears the stream down (clone progress keeps
// flowing while the operator wanders off to Credentials and back).

import { createContext, onCleanup, useContext, type ParentProps } from 'solid-js';
import { connectEvents, type EventsConnection } from './sse';
import { reportPresence } from './api';
import { currentDeviceHash } from './lib/deviceHash';
import { createPresenceReporter } from './lib/presence';

const EventsContext = createContext<EventsConnection>();

export function EventsProvider(props: ParentProps) {
  const events = connectEvents();
  onCleanup(() => events.close());

  // Presence-based push suppression (issue #160): the reporter rides the
  // app-wide singleton connection — the one SSE stream every page shares IS the
  // presence heartbeat — telling the server whether this device is visible.
  const stopPresence = createPresenceReporter(events.connID, {
    report: reportPresence,
    beacon: (body) => {
      navigator.sendBeacon('/api/v1/presence', new Blob([body], { type: 'application/json' }));
    },
    deviceHash: currentDeviceHash,
  });
  onCleanup(stopPresence);

  return <EventsContext.Provider value={events}>{props.children}</EventsContext.Provider>;
}

export function useEvents(): EventsConnection {
  const ctx = useContext(EventsContext);
  if (!ctx) throw new Error('useEvents must be used inside <EventsProvider>');
  return ctx;
}
