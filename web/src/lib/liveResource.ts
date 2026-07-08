// Live resources: createResource + SSE subscriptions + resync, in one place.
//
// Today every view hand-rolls createResource, then events.subscribe(...) with a
// refetch, and NONE of them refetch on reconnect — so state a phone lost while
// locked (issue #54) stays stale forever. createLiveResource owns the resource,
// its event subscriptions and resync handling together, so a future view cannot
// forget resync: give it the event specs and the wiring is guaranteed.
//
// Must run under a component root inside <EventsProvider> — it calls useEvents()
// and createResource and registers onCleanup, so it belongs in a component body
// (or a createRoot), exactly like a bare createResource.
//
// Per spec: subscribe to spec.type; on an event, refetch when `match` is absent
// or returns true. `match` runs per event (not once at setup) precisely so it
// can read props/params fresh — a repo-scoped view compares event.repoID to the
// CURRENT route param, not the one captured when the view mounted. `debounceMs`
// trailing-edge coalesces a burst from a chatty type (run.messages.changed) into
// one refetch; each spec owns its own timer.
//
// resync is the reconnect-recovery pseudo-event (see sse.ts): it means "events
// were lost, the current state is untrustworthy", so it cancels every pending
// debounce and refetches once, unconditionally — no match predicate and no
// debounce window may suppress it. Every subscription and timer is torn down via
// onCleanup, so a disposed view leaves nothing behind to fire.

import { createResource, onCleanup } from 'solid-js';
import type { ResourceFetcher, ResourceReturn, ResourceSource } from 'solid-js';
import { useEvents } from '../events';
import type { LabEvent, LabEventType } from '../sse';

export interface LiveEventSpec {
  type: LabEventType;
  /** Absent = every event of this type matches. May read props/params fresh per event. */
  match?: (event: LabEvent) => boolean;
  /** Trailing-edge coalesce for chatty event types. */
  debounceMs?: number;
}

export function createLiveResource<T, R = unknown>(
  fetcher: ResourceFetcher<true, T, R>,
  specs: LiveEventSpec[],
): ResourceReturn<T, R>;
export function createLiveResource<T, S, R = unknown>(
  source: ResourceSource<S>,
  fetcher: ResourceFetcher<S, T, R>,
  specs: LiveEventSpec[],
): ResourceReturn<T, R>;
export function createLiveResource(
  arg1: unknown,
  arg2: unknown,
  arg3?: unknown,
): ResourceReturn<unknown, unknown> {
  // Arity split: (fetcher, specs) carries the spec array second; (source,
  // fetcher, specs) carries it third — so the array's position is the tell.
  const withSource = !Array.isArray(arg2);
  const specs = (withSource ? arg3 : arg2) as LiveEventSpec[];
  const resource = withSource
    ? createResource(
        arg1 as ResourceSource<unknown>,
        arg2 as ResourceFetcher<unknown, unknown, unknown>,
      )
    : createResource(arg1 as ResourceFetcher<true, unknown, unknown>);
  const [, { refetch }] = resource;

  const events = useEvents();
  // One trailing-edge timer per spec, each reachable from the resync handler so
  // it can cancel every pending debounce at once.
  const cancels: Array<() => void> = [];
  for (const spec of specs) {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const cancel = (): void => {
      clearTimeout(timer);
      timer = undefined;
    };
    cancels.push(cancel);
    onCleanup(
      events.subscribe(spec.type, (event) => {
        if (spec.match && !spec.match(event)) return;
        if (spec.debounceMs === undefined) {
          void refetch();
          return;
        }
        cancel();
        timer = setTimeout(() => void refetch(), spec.debounceMs);
      }),
    );
    onCleanup(cancel);
  }
  // A reconnect gap means we may have missed the very event that would have
  // refetched us, so resync ignores match and debounce: cancel every pending
  // debounce (a queued trailing refetch would be redundant now) and refetch.
  onCleanup(
    events.subscribe('resync', () => {
      for (const cancel of cancels) cancel();
      void refetch();
    }),
  );

  return resource;
}
