// Rows that keep their identity (issue #61): a list that is refetched on
// every event returns fresh objects each time, and a `<For>` over fresh
// objects rebuilds every row — the switch or button a keyboard user was on is
// replaced under them, and focus drops to <body>. createRowStore() holds the
// rows in a store reconciled by id, so a refetch patches what changed in
// place and a row's DOM (and the focus in it) survives.
//
// It also knows WHOSE rows it holds: a resource keeps its last answer while
// the next one loads, so after a move to another owner (another repo) the
// old owner's rows would show — and act — under the new one. The store only
// answers for the owner its rows were fetched for.

import { createComputed, createSignal, type Accessor } from 'solid-js';
import { createStore, reconcile } from 'solid-js/store';
import { rescueFocus } from './focus';

/** A list as it was fetched: for whom, and what. */
export interface OwnedRows<T> {
  owner: string;
  rows: T[];
}

export interface RowStore<T extends { id: string }> {
  /** The rows of the current owner, each a stable object per id. Empty until loaded. */
  rows: T[];
  /** True once the current owner's rows are here (a failed refetch keeps them). */
  loaded: Accessor<boolean>;
  /** Applies one row as the server answered it, without waiting for a refetch. */
  patch: (row: T) => void;
  /** Takes a row out at once (it was deleted). */
  remove: (id: string) => void;
}

/**
 * @param fetched the latest answer, or undefined while there is none (loading, failed)
 * @param owner   whose rows the page shows right now
 */
export function createRowStore<T extends { id: string }>(
  fetched: Accessor<OwnedRows<T> | undefined>,
  owner: Accessor<string>,
): RowStore<T> {
  const [rows, setRows] = createStore<T[]>([]);
  const [loadedFor, setLoadedFor] = createSignal<string | undefined>(undefined);
  createComputed(() => {
    const current = owner();
    const answer = fetched();
    if (answer === undefined || answer.owner !== current) {
      // Nothing for this owner yet: never another owner's rows.
      if (loadedFor() !== current) setRows(reconcile([] as T[], { key: 'id' }));
      return;
    }
    setRows(reconcile(answer.rows, { key: 'id' }));
    setLoadedFor(current);
  });
  return {
    rows,
    loaded: () => loadedFor() === owner(),
    patch: (row) => setRows((candidate) => candidate.id === row.id, reconcile(row)),
    remove: (id) => setRows((current) => current.filter((row) => row.id !== id)),
  };
}

/**
 * After a row left a list with the control that had focus: focuses the row
 * that took its place (same position), else the last row, else `fallback` —
 * and only when focus really was lost (lib/focus.ts).
 *
 * @param controls the list's per-row controls, in row order, as they are now
 * @param index    where the row that left used to be
 */
export function rescueRowFocus(
  controls: readonly HTMLElement[],
  index: number,
  fallback: HTMLElement | null | undefined,
): boolean {
  return rescueFocus(controls[index], controls[controls.length - 1], fallback);
}
