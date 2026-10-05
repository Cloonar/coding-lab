// The instance list for a page that shows live runs and who is waiting
// (issue #61: the repositories list and the repo home's Live runs block).
// run.changed (spawn, stop, outcome) refetches the whole list through
// createLiveResource, reconnects included. run.messages.changed fires about
// once a second per streaming agent and can only flip one run's
// conversational state, so it patches that row in place instead of
// refetching — the same policy as the side rail (components/AppShell.tsx).
// An event for an unknown run, or one that changes nothing, is a no-op.
//
// Must run in a component body inside <EventsProvider>, like
// createLiveResource.

import { onCleanup, type Resource } from 'solid-js';
import { listInstances, type ConversationState, type Instance } from '../api';
import { useEvents } from '../events';
import { createLiveResource } from './liveResource';

export interface LiveInstances {
  instances: Resource<Instance[]>;
  refetch: () => void;
}

export function createLiveInstances(): LiveInstances {
  const events = useEvents();
  const [instances, { refetch, mutate }] = createLiveResource(
    () => listInstances(),
    [{ type: 'run.changed' }],
  );

  onCleanup(
    events.subscribe('run.messages.changed', (event) => {
      if (typeof event.runID !== 'string' || typeof event.state !== 'string') return;
      const runID = event.runID;
      const state = event.state as ConversationState;
      mutate((prev) => {
        if (prev === undefined) return prev;
        const idx = prev.findIndex((row) => row.id === runID);
        if (idx === -1) return prev;
        const row = prev[idx]!;
        if (row.state === state) return prev;
        const next = prev.slice();
        next[idx] = { ...row, state };
        return next;
      });
    }),
  );

  return { instances, refetch: () => void refetch() };
}
