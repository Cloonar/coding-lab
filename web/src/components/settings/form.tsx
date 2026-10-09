// The one-page settings form store, generically (issue #61, issue #85): ONE
// form state for a whole settings page, over a field table (fields.ts). The
// repo page (routes/repo-settings/form.tsx) and global Settings both build
// theirs here, and everything the pages share — the field vocabulary
// (Field.tsx), the save bar, the section navigation, the leave guard, the
// page's scroll logic — reads it through SettingsFormContext, typed only as
// far as the SettingsForm contract below.
//
// What it holds: the operator's EDITS, one per touched field. A field's draft
// is its edit, else whatever the page's saved snapshot holds — so the
// seed/resync rule of lib/seededDrafts.ts holds by construction:
//
//   - an untouched field has no edit and simply shows the saved snapshot, so
//     it follows every refresh of it;
//   - a dirty field keeps the operator's edit across a refresh — unless the
//     server caught up with it (our own save landing, the same value saved
//     elsewhere), which drops the edit and makes the field clean again;
//   - "changed" and the PATCH are always diffed against the saved snapshot,
//     so a field the operator never touched is never sent, and a server-side
//     change to it is never silently reverted.
//
// A snapshot that only ever moves through this store's own Save (global
// settings have no live event) simply never takes the refresh path.
//
// The one save rule lives here too: Save validates the changed fields in the
// browser, sends ONE PATCH with exactly the changed fields, applies the
// response as the saved snapshot at once (the marks clear without waiting for
// any refetch) and confirms in the page's toast. A refusal that names a field
// is shown at that field; one that names none, and a network error, in the
// save bar. Edits are kept on every failure.
//
// The store is built in two steps — createFieldDrafts(), then
// createSettingsFormStore() — so a page can compose what its wire forms read
// (the store's `context`, e.g. the repo's AFK option keys) out of the drafts
// before the store binds the fields.

import { useLocation, useNavigate } from '@solidjs/router';
import {
  batch,
  createComputed,
  createContext,
  createMemo,
  createSignal,
  on,
  onCleanup,
  untrack,
  useContext,
  type Accessor,
  type JSX,
  type Signal,
} from 'solid-js';
import { ApiError, errorMessage } from '../../api';
import type { ToastOptions } from '../Toast';
import type {
  DraftOf,
  FieldEdits,
  FieldKey,
  FieldProblems,
  FieldSpec,
  FieldTable,
  FormShape,
} from './fields';

/** One field's draft and state — what a section binds a control to. */
export interface FieldBinding<T extends FormShape, K extends FieldKey<T>> {
  key: K;
  /** The field's table entry (section, label, rules). */
  spec: FieldSpec<T, K>;
  /** The draft: the operator's edit, else what the saved snapshot holds. */
  value: Accessor<DraftOf<T, K>>;
  /** Records an edit. Setting the saved value back removes the edit. */
  set: (value: DraftOf<T, K>) => void;
  /** True while the draft would change the saved snapshot. */
  changed: Accessor<boolean>;
  /** The problem shown under the field: a browser check or a server refusal. */
  error: Accessor<string | null>;
  /** True for a field whose own value may be null, meaning "inherit". */
  overridable: boolean;
  /**
   * True while an overridable field's draft leaves it inherited ("inherited"
   * at its label); false once it is set here. Always false otherwise.
   */
  inherits: Accessor<boolean>;
  /**
   * What the field inherits, worded for the page, or null while that is not
   * known. Where it comes from is the page's business (the repo page asks the
   * server; global Settings words another draft on the same page).
   */
  inheritedText: Accessor<string | null>;
  /** Returns an overridable field to inherited (a change, saved as null). */
  reset: () => void;
}

/** Where on the settings page to go: a section, and optionally one field in it. */
export interface RevealTarget<K extends string = string> {
  section: string;
  field?: K;
}

/** What every one-page settings form offers the shared components. */
export interface SettingsForm<T extends FormShape> {
  /**
   * The snapshot the drafts are diffed against. Undefined until it has
   * loaded: the page renders its sections only once it has.
   */
  saved: Accessor<T['saved'] | undefined>;
  /** The binding of one field. */
  field<K extends FieldKey<T>>(key: K): FieldBinding<T, K>;
  /** The changed fields, in page order. Their count is the save bar's number. */
  changed: Accessor<FieldKey<T>[]>;
  /** The sections that hold a changed field, in page order. */
  changedSections: Accessor<T['section'][]>;
  /** True while at least one field differs from the saved snapshot. */
  dirty: Accessor<boolean>;
  /** The fields with a problem shown under them, in page order. */
  problems: Accessor<FieldKey<T>[]>;
  /** The sections that hold a field with a problem, in page order. */
  problemSections: Accessor<T['section'][]>;
  /** A failed save that names no field (or never reached the server). */
  barError: Accessor<string | null>;
  /** True while the PATCH is in flight. */
  busy: Accessor<boolean>;
  /**
   * Validates, then sends ONE PATCH with the changed fields. Resolves true
   * when everything is saved. On a problem it sends nothing (a browser check)
   * or keeps the edits (a refusal), goes to the field, and resolves false.
   */
  save: () => Promise<boolean>;
  /** Restores the saved values and offers Undo in the page's toast. */
  discard: () => void;
  /** Drops every edit without a trace (the leave dialog's Discard). */
  drop: () => void;
  /**
   * Goes to a section — or a field — of the settings page: navigates to its
   * URL if the page does not show, then scrolls there (and focuses the field).
   */
  reveal: (target: RevealTarget<FieldKey<T>>) => void;
  /** Bumped by every reveal(), so the page repeats one for an unchanged URL. */
  revealTick: Accessor<number>;
  /**
   * The field the page is being sent to (a reveal(), a `?field=` URL), so a
   * section that folds fields away can unfold the one that is wanted.
   */
  pointedAt: Accessor<FieldKey<T> | undefined>;
  /** Says which field the URL points at (the page calls it; undefined = none). */
  pointAt: (field: FieldKey<T> | undefined) => void;
}

/**
 * The shape the shared components see a form through: every key a string,
 * every draft unknown. Pages hand their own typed forms in; the components
 * read only the contract.
 */
export interface LooseShape extends FormShape {
  drafts: Record<string, unknown>;
  patch: Record<string, unknown>;
}

const SettingsFormContext = createContext<SettingsForm<LooseShape>>();

/** Makes `form` the settings form every shared component below reads. */
export function SettingsFormProvider<T extends FormShape>(props: {
  form: SettingsForm<T>;
  children?: JSX.Element;
}) {
  return (
    // eslint-disable-next-line solid/reactivity -- a page provides one form for its lifetime
    <SettingsFormContext.Provider value={props.form as unknown as SettingsForm<LooseShape>}>
      {props.children}
    </SettingsFormContext.Provider>
  );
}

/** The settings form of the page; only valid inside a SettingsFormProvider. */
export function useSettingsFormContext<T extends FormShape = LooseShape>(): SettingsForm<T> {
  const form = useContext(SettingsFormContext);
  if (form === undefined) {
    throw new Error('useSettingsFormContext() must be used inside a SettingsFormProvider');
  }
  return form as unknown as SettingsForm<T>;
}

/** "1 change" / "3 changes". */
export function plural(count: number, noun: string): string {
  return `${count} ${noun}${count === 1 ? '' : 's'}`;
}

// --- step 1: the drafts ----------------------------------------------------------

/** The operator's edits, and the draft of every field. */
export interface FieldDrafts<T extends FormShape> {
  table: FieldTable<T>;
  saved: Accessor<T['saved'] | undefined>;
  /** The operator's edit of one field, or undefined while it is untouched. */
  editOf<K extends FieldKey<T>>(key: K): DraftOf<T, K> | undefined;
  /** Records (or, with undefined, removes) an edit as is — no rule applied. */
  setEdit<K extends FieldKey<T>>(key: K, value: DraftOf<T, K> | undefined): void;
  /** The draft of one field; undefined only before any snapshot has loaded. */
  draftOf<K extends FieldKey<T>>(key: K): DraftOf<T, K> | undefined;
  /** Every edit, as one plain object. */
  readEdits(): FieldEdits<T>;
}

/** One signal per field of `table`: the operator's edit, else nothing. */
export function createFieldDrafts<T extends FormShape>(
  table: FieldTable<T>,
  saved: Accessor<T['saved'] | undefined>,
): FieldDrafts<T> {
  type Key = FieldKey<T>;
  const edits = {} as { [K in Key]: Signal<DraftOf<T, K> | undefined> };
  for (const key of table.keys) {
    (edits as Record<string, Signal<unknown>>)[key] = createSignal<unknown>(undefined);
  }
  const editOf = <K extends Key>(key: K): DraftOf<T, K> | undefined => edits[key][0]();
  const setEdit = <K extends Key>(key: K, value: DraftOf<T, K> | undefined): void => {
    // Wrapped, so a draft can never be mistaken for a signal updater.
    edits[key][1](() => value);
  };
  const copyEdit = <K extends Key>(out: FieldEdits<T>, key: K): void => {
    const value = editOf(key);
    if (value !== undefined) out[key] = value;
  };
  return {
    table,
    saved,
    editOf,
    setEdit,
    draftOf: (key) => {
      const edit = editOf(key);
      if (edit !== undefined) return edit;
      const snapshot = saved();
      return snapshot !== undefined ? table.spec(key).seed(snapshot) : undefined;
    },
    readEdits: () => {
      const out: FieldEdits<T> = {};
      for (const key of table.keys) copyEdit(out, key);
      return out;
    },
  };
}

// --- step 2: the store -----------------------------------------------------------

export interface SettingsFormStoreOptions<T extends FormShape> {
  drafts: FieldDrafts<T>;
  /**
   * What the wire forms read besides the drafts. Read lazily, so it may be
   * composed out of the drafts themselves.
   */
  context: Accessor<T['context']>;
  /** The settings page's path; reveal() goes to `<base>/<section>`. */
  base: Accessor<string>;
  /**
   * The identity the drafts are about (the repo page's repo id). When it
   * changes, every edit is dropped (and `onReset` runs); a Save answer or a
   * Discard's Undo that arrives afterwards is ignored. Absent = one identity
   * for the store's lifetime.
   */
  scope?: Accessor<unknown>;
  /** Runs after the edits were dropped for a new scope. */
  onReset?: () => void;
  /**
   * What makes two snapshots the same object (the repo page: its id). A new
   * snapshot of another one is no refresh, and resyncs nothing. Default: one
   * object for the store's lifetime.
   */
  identity?: (saved: T['saved']) => unknown;
  /**
   * The other fields of the pairs `key` belongs to: a problem that is not a
   * field's own rule failing is dropped when another field of its pair is
   * edited. Default: no pairs.
   */
  pairedFields?: (key: FieldKey<T>) => readonly FieldKey<T>[];
  /** Save's browser check; default: each changed field's own rule. */
  validate?: (edits: FieldEdits<T>, saved: T['saved'], context: T['context']) => FieldProblems<T>;
  /** What an overridable field resolves to while inherited, worded; null = not known. */
  inheritedText?: (key: FieldKey<T>) => string | null;
  /**
   * Asked once the PATCH is built and before it is sent; false sends nothing,
   * keeps every edit, and Save resolves false (a confirm dialog's Cancel).
   */
  confirm?: (patch: T['patch'], saved: T['saved']) => boolean | Promise<boolean>;
  /** Sends the PATCH; resolves to the snapshot the server now holds. */
  send: (patch: T['patch'], saved: T['saved']) => Promise<T['saved']>;
  /** Makes a PATCH response the saved snapshot. */
  apply: (next: T['saved']) => void;
  /** Shows the page's toast. */
  notify: (message: string, options?: ToastOptions) => void;
  /** The toast after a Save of `count` changes ("Saved 2 changes to <repo>"). */
  savedMessage: (count: number, next: T['saved']) => string;
}

/** Builds the form store of one settings page over its drafts. */
export function createSettingsFormStore<T extends FormShape>(
  options: SettingsFormStoreOptions<T>,
): SettingsForm<T> {
  type Key = FieldKey<T>;
  const { drafts, context } = options;
  const { table, saved, editOf, setEdit, draftOf, readEdits } = drafts;
  const location = useLocation();
  const navigate = useNavigate();
  const pairedFields = options.pairedFields ?? (() => []);
  const validate = options.validate ?? table.validateEdits;

  const [errors, setErrors] = createSignal<FieldProblems<T>>({});
  const [barError, setBarError] = createSignal<string | null>(null);
  const [busy, setBusy] = createSignal(false);
  const [revealTick, setRevealTick] = createSignal(0);
  const [pointedAt, setPointedAt] = createSignal<Key | undefined>(undefined);
  // What the Save in flight submitted, while it is in flight: an edit made
  // meanwhile is measured against THAT, not against the snapshot it replaces.
  let inFlight: FieldEdits<T> | undefined;

  const dropError = (key: Key): void => {
    if (errors()[key] === undefined) return;
    const next = { ...errors() };
    delete next[key];
    setErrors(() => next);
  };
  const clearEdit = (key: Key): void => {
    setEdit(key, undefined);
    dropError(key);
  };
  const dropPairError = <K extends Key>(key: K): void => {
    if (errors()[key] === undefined) return;
    const draft = draftOf(key);
    if (draft === undefined || table.validateDraft(key, draft) === null) dropError(key);
  };
  const drop = (): void =>
    batch(() => {
      for (const key of table.keys) setEdit(key, undefined);
      setErrors({});
      setBarError(null);
    });

  // --- navigation ---------------------------------------------------------------
  const reveal = (target: RevealTarget<Key>): void => {
    const base = options.base();
    const path = `${base}/${target.section}${target.field !== undefined ? `?field=${target.field}` : ''}`;
    const onSettings = location.pathname === base || location.pathname.startsWith(`${base}/`);
    // First, so a section that folded the field away has it back on the page
    // by the time the page looks for it.
    setPointedAt(() => target.field);
    // The page does the scrolling itself, so the router must not jump to the
    // top. Inside the settings page the URL is replaced (no history spam).
    navigate(path, onSettings ? { replace: true, scroll: false } : { scroll: false });
    // After navigate(): the router is mid-navigation by now (when the URL
    // changes at all), so the page acts once, on the new URL.
    setRevealTick((tick) => tick + 1);
  };

  // --- bindings -----------------------------------------------------------------
  const bind = <K extends Key>(key: K): FieldBinding<T, K> => {
    const spec = table.spec(key);
    const value = createMemo<DraftOf<T, K> | undefined>(() => draftOf(key), undefined, {
      equals: (a, b) =>
        a === b || (a !== undefined && b !== undefined && table.sameDraft(key, a, b)),
    });
    const changed = createMemo(() => {
      const edit = editOf(key);
      const snapshot = saved();
      return (
        edit !== undefined &&
        snapshot !== undefined &&
        table.draftDiffers(key, edit, snapshot, context())
      );
    });
    const set = (next: DraftOf<T, K>): void => {
      const snapshot = saved();
      batch(() => {
        // An edit back to the saved value is no edit: the field is untouched
        // again and follows the server. Not while a Save that carries this
        // field is in flight: the snapshot is about to hold what was
        // submitted, so going back to the value it holds NOW is an edit, and
        // stays one.
        const untouched =
          inFlight?.[key] === undefined &&
          snapshot !== undefined &&
          table.sameDraft(key, next, spec.seed(snapshot));
        setEdit(key, untouched ? undefined : next);
        if (untouched) {
          // An untouched field is never sent: nothing about it is a problem.
          dropError(key);
        } else if (errors()[key] !== undefined) {
          // A problem stays under its field until the field is valid again; a
          // server refusal has no browser rule, so any edit clears it.
          const message = table.validateDraft(key, next);
          if (message === null) dropError(key);
          else setErrors(() => ({ ...errors(), [key]: message }));
        }
        // A problem with a PAIR (a refusal the server pinned to the other
        // half, a rule across fields) is answered from either half: it goes
        // when its own field's rule does not hold it.
        for (const other of pairedFields(key)) dropPairError(other);
        setBarError(null);
      });
    };
    const overridable = table.isOverridable(key);
    const inherits = createMemo(() => {
      const draft = value();
      return draft !== undefined && table.draftInherits(key, draft, context());
    });
    const inheritedText = createMemo(() =>
      overridable ? (options.inheritedText?.(key) ?? null) : null,
    );
    return {
      key,
      spec,
      // Sections render only once a snapshot has loaded, so a draft exists there.
      value: value as Accessor<DraftOf<T, K>>,
      set,
      changed,
      error: () => errors()[key] ?? null,
      overridable,
      inherits,
      inheritedText,
      reset: () => {
        if (overridable) set(spec.inherit as DraftOf<T, K>);
      },
    };
  };
  const bindings = {} as { [K in Key]: FieldBinding<T, K> };
  for (const key of table.keys) {
    (bindings as Record<string, unknown>)[key] = bind(key);
  }

  const changed = createMemo(() => table.keys.filter((key) => bindings[key].changed()));
  const changedSections = createMemo(() => table.sectionsOf(changed()));
  const dirty = (): boolean => changed().length > 0;
  const problems = createMemo(() => table.keys.filter((key) => errors()[key] !== undefined));
  const problemSections = createMemo(() => table.sectionsOf(problems()));
  // A problem is about a pending change, and is counted in the save bar. Once
  // nothing is pending the bar is gone — and so is every problem.
  createComputed(() => {
    if (dirty() || busy()) return;
    if (Object.keys(untrack(errors)).length > 0) setErrors({});
    if (untrack(barError) !== null) setBarError(null);
  });

  // --- seed / resync ------------------------------------------------------------
  // Another scope (another repo): nothing drafted against the previous one
  // applies.
  const scope = options.scope;
  if (scope !== undefined) {
    createComputed(
      on(
        scope,
        () => {
          drop();
          options.onReset?.();
        },
        { defer: true },
      ),
    );
  }

  // A refresh of the same snapshot (the lib/seededDrafts.ts rule): an edit the
  // server caught up with is dropped, so the field is clean and follows the
  // server from here on. So is an "edit" that never was a change (a stray
  // space) once the server changes that field — it must not turn into one.
  const identity = options.identity ?? (() => undefined);
  const resync = <K extends Key>(key: K, fresh: T['saved'], previous: T['saved']): void => {
    const edit = editOf(key);
    if (edit === undefined) return;
    const spec = table.spec(key);
    const next = spec.seed(fresh);
    if (table.sameDraft(key, edit, next)) {
      clearEdit(key);
    } else if (
      // Not for a field the Save in flight carries: the server changing it
      // is that Save landing, and an edit that differs from it is newer.
      inFlight?.[key] === undefined &&
      !table.sameDraft(key, spec.seed(previous), next) &&
      !table.draftDiffers(key, edit, previous, context())
    ) {
      clearEdit(key);
    }
  };
  createComputed(
    on(
      saved,
      (fresh, previous) => {
        if (fresh === undefined || previous === undefined) return;
        if (identity(previous) !== identity(fresh)) return;
        batch(() => {
          for (const key of table.keys) resync(key, fresh, previous);
        });
      },
      { defer: true },
    ),
  );

  // --- save / discard -----------------------------------------------------------
  const scopeOf = (): unknown => scope?.();
  const save = async (): Promise<boolean> => {
    const snapshot = saved();
    if (snapshot === undefined || busy()) return false;
    const fieldContext = context();
    const submitted = readEdits();
    const found = validate(submitted, snapshot, fieldContext);
    batch(() => {
      setBarError(null);
      setErrors(() => found);
    });
    const firstProblem = table.keys.find((key) => found[key] !== undefined);
    if (firstProblem !== undefined) {
      // Nothing is sent while a field is wrong.
      reveal({ section: table.spec(firstProblem).section, field: firstProblem });
      return false;
    }
    const patch = table.buildPatch(submitted, snapshot, fieldContext);
    const count = Object.keys(patch).length;
    if (count === 0) return true;
    if (options.confirm !== undefined && !(await options.confirm(patch, snapshot))) return false;

    const at = scopeOf();
    inFlight = submitted;
    setBusy(true);
    try {
      const next = await options.send(patch, snapshot);
      // Left for another scope while the request was in flight: the answer
      // belongs to a snapshot the drafts no longer describe.
      if (scopeOf() !== at) return true;
      batch(() => {
        // The response IS the saved snapshot now — applied at once, so the
        // bar and the marks clear without waiting for any refetch.
        options.apply(next);
        // Everything that was submitted is saved, whatever the server
        // normalised it to (a sanitized name, a digest-pinned image). An
        // edit typed while the request was in flight is newer, and stays.
        for (const key of table.keys) settle(key, submitted);
        setErrors({});
      });
      options.notify(options.savedMessage(count, next));
      return true;
    } catch (err) {
      const field = err instanceof ApiError ? err.field : undefined;
      if (table.isKey(field)) {
        // The refusal names its field: show it there.
        setErrors(() => ({ [field]: errorMessage(err) }) as FieldProblems<T>);
        reveal({ section: table.spec(field).section, field });
      } else {
        setBarError(errorMessage(err));
      }
      return false;
    } finally {
      inFlight = undefined;
      setBusy(false);
    }
  };
  const settle = <K extends Key>(key: K, submitted: FieldEdits<T>): void => {
    const sent = submitted[key];
    const current = editOf(key);
    if (sent === undefined || current === undefined) return;
    if (table.sameDraft(key, current, sent as DraftOf<T, K>)) setEdit(key, undefined);
  };

  const discard = (): void => {
    const at = scopeOf();
    const droppedEdits = readEdits();
    const droppedErrors = errors();
    drop();
    options.notify('Changes discarded', {
      action: {
        label: 'Undo',
        run: () => {
          // The toast can outlive a move to another scope; the edits cannot.
          if (scopeOf() !== at) return;
          batch(() => {
            for (const key of table.keys) restoreEdit(key, droppedEdits);
            setErrors(() => droppedErrors);
          });
        },
      },
    });
  };
  const restoreEdit = <K extends Key>(key: K, from: FieldEdits<T>): void => {
    const value = from[key];
    if (value !== undefined) setEdit(key, value as DraftOf<T, K>);
  };

  // A tab close or reload keeps the browser's own prompt while changes are
  // pending. Chrome's legacy contract: preventDefault AND set returnValue.
  const onBeforeUnload = (event: BeforeUnloadEvent): void => {
    if (!dirty()) return;
    event.preventDefault();
    event.returnValue = '';
  };
  window.addEventListener('beforeunload', onBeforeUnload);
  onCleanup(() => window.removeEventListener('beforeunload', onBeforeUnload));

  return {
    saved,
    field: (key) => bindings[key],
    changed,
    changedSections,
    dirty,
    problems,
    problemSections,
    barError,
    busy,
    save,
    discard,
    drop,
    reveal,
    revealTick,
    pointedAt,
    pointAt: (field) => void setPointedAt(() => field),
  };
}
