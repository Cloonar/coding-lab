// Queued first message (issue #41): when the New-run composer spawns an
// instance with text still in the box, the text is parked here, keyed by the
// new run's id, and the chat view auto-sends it as the run's first reply once
// the transcript is readable.
//
// Lifetime + guarantees:
// - Survives SPA route changes: this is module-level state, NOT component
//   state, so the composer can navigate('/runs/:id') and the fresh RunChat
//   mount reads the entry back out here.
// - Lost on a full tab close / reload — accepted (issue #41): the queued text
//   is a convenience hop, not durable state. A spawn that fails never queues,
//   so the operator's text stays in the composer instead of vanishing.
// - Only a SUCCESSFUL startInstance() queues, so a run id in the map always
//   names a real run.
//
// Reactivity: a version signal bumps on every mutation. A consumer reads
// queuedVersion() to subscribe, then peekQueued(id) for the value — the same
// signal-then-read shape stores/cloneProgress uses (the map itself is a plain
// Map, not a reactive store).

import { createSignal, type Accessor } from 'solid-js';

const queued = new Map<string, string>();
const [version, setVersion] = createSignal(0);

/** Reactive tick: read this in a tracking scope to re-run when the queue changes. */
export const queuedVersion: Accessor<number> = version;

/** Park `text` as run `runID`'s first message (replaces any prior entry). */
export function setQueued(runID: string, text: string): void {
  queued.set(runID, text);
  setVersion((v) => v + 1);
}

/** Non-consuming read — undefined when nothing is queued for the run. */
export function peekQueued(runID: string): string | undefined {
  return queued.get(runID);
}

/**
 * Consuming read: returns the text (or undefined) AND removes the entry. The
 * auto-send path takes BEFORE it POSTs, so an effect that re-fires while the
 * reply is in flight sees no queued text and cannot double-send.
 */
export function takeQueued(runID: string): string | undefined {
  const text = queued.get(runID);
  if (text !== undefined) {
    queued.delete(runID);
    setVersion((v) => v + 1);
  }
  return text;
}

/** Drop a run's entry without sending (run ended / transcript gone before it could). */
export function clearQueued(runID: string): void {
  if (queued.delete(runID)) setVersion((v) => v + 1);
}
