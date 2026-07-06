// One SSE connection per authenticated app instance, shared via context so
// navigation between pages never tears the stream down (clone progress keeps
// flowing while the operator wanders off to Credentials and back).

import { createContext, onCleanup, useContext, type ParentProps } from 'solid-js';
import { connectEvents, type EventsConnection } from './sse';

const EventsContext = createContext<EventsConnection>();

export function EventsProvider(props: ParentProps) {
  const events = connectEvents();
  onCleanup(() => events.close());
  return <EventsContext.Provider value={events}>{props.children}</EventsContext.Provider>;
}

export function useEvents(): EventsConnection {
  const ctx = useContext(EventsContext);
  if (!ctx) throw new Error('useEvents must be used inside <EventsProvider>');
  return ctx;
}
