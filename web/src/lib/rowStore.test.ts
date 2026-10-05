// lib/rowStore.ts: rows that keep their identity across refetches, that only
// ever belong to the current owner, and the focus rescue for a row that left.

import { createRoot, createSignal } from 'solid-js';
import { afterEach, describe, expect, it } from 'vitest';
import { createRowStore, rescueRowFocus, type OwnedRows } from './rowStore';

interface Row {
  id: string;
  name: string;
  on: boolean;
}

function setup(initial?: OwnedRows<Row>, owner = 'a') {
  return createRoot((dispose) => {
    const [fetched, setFetched] = createSignal<OwnedRows<Row> | undefined>(initial);
    const [current, setOwner] = createSignal(owner);
    const store = createRowStore(fetched, current);
    return { store, setFetched, setOwner, dispose };
  });
}

describe('createRowStore', () => {
  it('is empty and not loaded until the first answer', () => {
    const { store, setFetched, dispose } = setup();
    expect(store.loaded()).toBe(false);
    expect(store.rows).toHaveLength(0);

    setFetched({ owner: 'a', rows: [{ id: '1', name: 'one', on: true }] });
    expect(store.loaded()).toBe(true);
    expect(store.rows.map((row) => row.name)).toEqual(['one']);
    dispose();
  });

  it('keeps one object per id across answers, and patches what changed in place', () => {
    const { store, setFetched, dispose } = setup({
      owner: 'a',
      rows: [
        { id: '1', name: 'one', on: true },
        { id: '2', name: 'two', on: true },
      ],
    });
    const [first, second] = store.rows;

    // A refetch returns fresh objects for every row.
    setFetched({
      owner: 'a',
      rows: [
        { id: '1', name: 'one', on: false },
        { id: '2', name: 'two', on: true },
        { id: '3', name: 'three', on: true },
      ],
    });

    expect(store.rows[0]).toBe(first);
    expect(store.rows[1]).toBe(second);
    expect(first?.on).toBe(false);
    expect(store.rows).toHaveLength(3);

    // Reordered (the list is sorted by name on the server): still the same objects.
    setFetched({
      owner: 'a',
      rows: [
        { id: '3', name: 'three', on: true },
        { id: '1', name: 'one', on: false },
      ],
    });
    expect(store.rows.map((row) => row.id)).toEqual(['3', '1']);
    expect(store.rows[1]).toBe(first);
    dispose();
  });

  it('keeps the rows it has when a refetch fails', () => {
    const { store, setFetched, dispose } = setup({
      owner: 'a',
      rows: [{ id: '1', name: 'one', on: true }],
    });
    setFetched(undefined); // the resource errored
    expect(store.loaded()).toBe(true);
    expect(store.rows).toHaveLength(1);
    dispose();
  });

  it("never shows another owner's rows", () => {
    const { store, setFetched, setOwner, dispose } = setup({
      owner: 'a',
      rows: [{ id: '1', name: "a's row", on: true }],
    });

    // The page moved to another owner; the resource still holds A's answer.
    setOwner('b');
    expect(store.loaded()).toBe(false);
    expect(store.rows).toHaveLength(0);

    // A late answer for A changes nothing.
    setFetched({ owner: 'a', rows: [{ id: '1', name: "a's row, again", on: true }] });
    expect(store.loaded()).toBe(false);
    expect(store.rows).toHaveLength(0);

    setFetched({ owner: 'b', rows: [{ id: '9', name: "b's row", on: true }] });
    expect(store.loaded()).toBe(true);
    expect(store.rows.map((row) => row.name)).toEqual(["b's row"]);
    dispose();
  });

  it('patch applies one server answer to its row; remove takes a row out', () => {
    const { store, dispose } = setup({
      owner: 'a',
      rows: [
        { id: '1', name: 'one', on: true },
        { id: '2', name: 'two', on: true },
      ],
    });
    const [first, second] = store.rows;

    store.patch({ id: '1', name: 'one', on: false });
    expect(store.rows[0]).toBe(first);
    expect(first?.on).toBe(false);
    expect(second?.on).toBe(true);

    store.remove('1');
    expect(store.rows).toHaveLength(1);
    expect(store.rows[0]).toBe(second);
    dispose();
  });
});

describe('rescueRowFocus', () => {
  const made: HTMLElement[] = [];
  const control = (): HTMLButtonElement => {
    const button = document.createElement('button');
    document.body.appendChild(button);
    made.push(button);
    return button;
  };
  afterEach(() => {
    for (const el of made.splice(0)) el.remove();
  });

  it('focuses the row that took the place, else the last row, else the fallback', () => {
    const [a, b, fallback] = [control(), control(), control()];

    (document.activeElement as HTMLElement | null)?.blur();
    expect(rescueRowFocus([a, b], 1, fallback)).toBe(true);
    expect(document.activeElement).toBe(b);

    // The LAST row left: the one before it is the last now.
    b.blur();
    expect(rescueRowFocus([a], 1, fallback)).toBe(true);
    expect(document.activeElement).toBe(a);

    // No row left at all.
    a.blur();
    expect(rescueRowFocus([], 0, fallback)).toBe(true);
    expect(document.activeElement).toBe(fallback);
  });

  it('never takes the focus from where the operator put it', () => {
    const [a, elsewhere] = [control(), control()];
    elsewhere.focus();
    expect(rescueRowFocus([a], 0, null)).toBe(false);
    expect(document.activeElement).toBe(elsewhere);
  });
});
