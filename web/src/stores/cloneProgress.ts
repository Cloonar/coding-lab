// Per-repo clone progress driven by `clone.progress` SSE events
// ({repoID, phase, percent, line}, throttled server-side to ~4/s). The store
// keeps only the latest event per repo — cards re-render from the signal, no
// refetch storm. `repo.changed` handling (refetch) lives with the repo list;
// this store never fetches.

import { createSignal, type Accessor } from 'solid-js';
import type { LabEvent, LabEventHandler, LabEventType } from '../sse';

export interface CloneProgress {
  phase: string;
  /** 0–100, or null while the current phase reports no percentage. */
  percent: number | null;
  /** Latest raw progress line from git, for the muted mono readout. */
  line: string;
}

/** The one sliver of EventsConnection the store needs — injectable in tests. */
export interface ProgressEventSource {
  subscribe(type: LabEventType, handler: LabEventHandler): () => void;
}

export interface CloneProgressStore {
  /** Reactive: latest progress for a repo, or null before any event. */
  progress(repoID: string): CloneProgress | null;
  /** Drops a repo's entry (e.g. when the operator retries a failed clone). */
  clear(repoID: string): void;
  /** Unsubscribes from the event stream. */
  dispose(): void;
}

function toProgress(event: LabEvent): CloneProgress {
  return {
    phase: typeof event.phase === 'string' ? event.phase : '',
    percent: typeof event.percent === 'number' ? event.percent : null,
    line: typeof event.line === 'string' ? event.line : '',
  };
}

export function createCloneProgressStore(events: ProgressEventSource): CloneProgressStore {
  const [all, setAll] = createSignal<Record<string, CloneProgress>>({});

  const unsubscribe = events.subscribe('clone.progress', (event) => {
    const repoID = typeof event.repoID === 'string' ? event.repoID : '';
    if (repoID === '') return;
    setAll((prev) => ({ ...prev, [repoID]: toProgress(event) }));
  });

  const accessor: Accessor<Record<string, CloneProgress>> = all;

  return {
    progress(repoID) {
      return accessor()[repoID] ?? null;
    },
    clear(repoID) {
      setAll((prev) => {
        if (!(repoID in prev)) return prev;
        const next = { ...prev };
        delete next[repoID];
        return next;
      });
    },
    dispose: unsubscribe,
  };
}
