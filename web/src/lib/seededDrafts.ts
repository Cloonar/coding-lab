// Generic seed/resync draft store (issue #198), extracted from the
// RepoSettings SettingsForm pattern so every settings category subpage reuses
// one implementation. Drafts seed from a SNAPSHOT of the model (`seed`); the
// consuming form's buildPatch diffs each draft against seed() — never against
// the live model() — so only fields the operator actually edited enter the
// PATCH and a server-side change to an untouched field (e.g. default-branch
// detection finishing mid-edit) is never silently reverted by an unrelated
// save. When a refresh lands (SSE-driven refetch), an untouched draft follows
// the server and a dirty draft keeps the operator's in-flight edit — unless
// the server caught up with the edit (our own save landing), which makes the
// field clean again — then the seed advances to the fresh model.

import { createEffect, createSignal, on } from 'solid-js';
import type { Accessor } from 'solid-js';

export interface SeededDrafts<TModel> {
  /**
   * Registers a draft signal seeded from read(seed). The optional `equals`
   * serves object-valued fields (the repo AFK options bag compares by
   * key-sorted JSON); the default is `===`.
   */
  field<T>(
    read: (model: TModel) => T,
    equals?: (a: T, b: T) => boolean,
  ): [Accessor<T>, (value: T) => void];
  /** The snapshot the drafts were last seeded from — buildPatch diffs against THIS. */
  seed(): TModel;
}

export function createSeededDrafts<TModel extends object>(
  model: Accessor<TModel>,
): SeededDrafts<TModel> {
  // Snapshot by design (the RepoSettings `let seed = props.repo()` pattern,
  // sans the eslint-disable — solid/reactivity does not flag helpers); the
  // effect below advances it on every refresh.
  let seed = model();
  const fields: ((fresh: TModel) => void)[] = [];

  // ONE effect for all fields, later registrations included (the array is
  // read at run time). `on` runs the callback untracked, so operator
  // keystrokes never re-trigger it; defer skips the mount-time model the
  // drafts were already seeded from. The seed advances only AFTER the loop —
  // every field resyncs against the seed it was actually drafted from.
  createEffect(
    on(
      model,
      (fresh) => {
        for (const resync of fields) resync(fresh);
        seed = fresh;
      },
      { defer: true },
    ),
  );

  return {
    field<T>(
      read: (m: TModel) => T,
      equals: (a: T, b: T) => boolean = (a, b) => a === b,
    ): [Accessor<T>, (value: T) => void] {
      const [draft, set] = createSignal<T>(read(seed));
      // Wrap the raw setter so a function-typed draft value can never be
      // misinterpreted as a signal updater.
      const setDraft = (value: T): void => void set(() => value);
      // eslint-disable-next-line solid/reactivity -- runs untracked inside the on(model) effect above, by design
      fields.push((fresh) => {
        const current = draft();
        const next = read(fresh);
        // Untouched (still equal to its seeded value) OR the server caught up
        // with our edit — either way the draft follows fresh and reads clean.
        if (equals(current, read(seed)) || equals(current, next)) setDraft(next);
      });
      return [draft, setDraft];
    },
    seed: () => seed,
  };
}
