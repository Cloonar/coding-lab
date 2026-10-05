// The repo settings form store (issue #61): ONE form state for the whole repo
// page. The repo home frame mounts it (RepoSettingsFormProvider) around the
// tabs and the routed tab, so pending changes outlive the Settings tab — the
// save bar and the leave guard, both rendered by the frame, keep working on
// Overview and Issues while changes are pending.
//
// What it holds: the operator's EDITS, one per touched field (fields.ts is the
// table of fields). A field's draft is its edit, else whatever the frame's
// live repo holds — so the seed/resync rule of lib/seededDrafts.ts holds by
// construction:
//
//   - an untouched field has no edit and simply shows the live repo, so it
//     follows every refresh (repo.changed, the Overview's Auto switch);
//   - a dirty field keeps the operator's edit across a refresh — unless the
//     server caught up with it (our own save landing, the same value saved
//     elsewhere), which drops the edit and makes the field clean again;
//   - "changed" and the PATCH are always diffed against the live saved repo,
//     so a field the operator never touched is never sent, and a server-side
//     change to it is never silently reverted.
//
// Inheritance (issue #61 §6): what an overridable field resolves to while the
// repo's own value is null is the SERVER's answer (getRepoInherited — the
// spawn path's own resolvers), asked again whenever a saved value other
// fields' chains read changes and, debounced, whenever a draft of one does.
// A request that fails keeps the last good answer (the page says it may be
// out of date) and the next trigger asks again. The browser never walks a
// chain. Its one composition: a field's EFFECTIVE value is its own draft when
// set, else the inherited one — that picks the provider whose catalogs a
// select lists, the Runner the page folds by, and the options the AFK option
// bag declares. What acts at once on the SAVED repo (Schedules) reads the
// answer for the saved repo alone, never the one for the drafts.
//
// The one save rule lives here too: Save validates the changed fields in the
// browser, sends ONE PATCH with exactly the changed fields, applies the
// response to the frame's repo at once (the marks clear without waiting for
// the SSE refetch) and confirms in the frame's toast. A refusal that names a
// field is shown at that field; one that names none, and a network error, in
// the save bar. Edits are kept on every failure.

import { useLocation, useNavigate } from '@solidjs/router';
import {
  batch,
  createComputed,
  createContext,
  createEffect,
  createMemo,
  createResource,
  createSignal,
  on,
  onCleanup,
  untrack,
  useContext,
  type Accessor,
  type JSX,
  type Signal,
} from 'solid-js';
import {
  ApiError,
  errorMessage,
  getRepoInherited,
  listProviders,
  updateRepo,
  type Provider,
  type ProviderOptionSpec,
  type Repo,
  type RepoInherited,
  type RepoInheritedDrafts,
} from '../../api';
import { resourceValue } from '../../lib/resource';
import { useRepoHome } from '../repo-home/context';
import {
  REPO_FIELD_KEYS,
  buildRepoPatch,
  draftDiffers,
  draftInherits,
  isOverridable,
  isRepoFieldKey,
  pairedFields,
  repoField,
  sameDraft,
  validateDraft,
  validateEdits,
  type FieldContext,
  type FormSectionSlug,
  type RepoDrafts,
  type RepoEdits,
  type RepoFieldKey,
  type RepoFieldSpec,
} from './fields';
import { CHAIN_FIELD_KEYS, inheritedText, type ChainFieldKey } from './inherited';
import { normText } from './shared';

/**
 * How long an edit of a chain field waits before the inherited values are
 * asked for again — one request for a quick run of picks, not one each.
 */
export const INHERITED_DEBOUNCE_MS = 150;

/** One field's draft and state — what a section binds a control to. */
export interface FieldBinding<K extends RepoFieldKey> {
  key: K;
  /** The field's table entry (section, label, rules). */
  spec: RepoFieldSpec<K>;
  /** The draft: the operator's edit, else what the saved repo holds. */
  value: Accessor<RepoDrafts[K]>;
  /** Records an edit. Setting the saved value back removes the edit. */
  set: (value: RepoDrafts[K]) => void;
  /** True while the draft would change the saved repo. */
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
   * What the field inherits, worded for the page — the server's answer, or
   * null while that is not known (loading, failed, unresolvable). Never a
   * value the browser derived.
   */
  inheritedText: Accessor<string | null>;
  /** Returns an overridable field to inherited (a change, saved as null). */
  reset: () => void;
}

/** The provider catalog, and the providers the fields resolve against. */
export interface RepoSettingsCatalog {
  /**
   * The Settings tab is showing: loads the provider catalog (once) and asks
   * for the inherited values (again on every call). Only that tab calls it,
   * so Overview and Issues never wait on — or even request — these reads.
   */
  load: () => void;
  /** The Settings tab went away: nothing follows the repo until the next load(). */
  idle: () => void;
  /** Registered providers; empty until loaded. A failed reload keeps the last list. */
  providers: Accessor<Provider[]>;
  /** Why the catalog could not be loaded (or reloaded), or null. */
  error: Accessor<string | null>;
  /** Loads the catalog again after a failure. */
  retry: () => void;
  /**
   * The provider runs you start resolve to: the drafted agent when one is
   * set here, else the inherited one. null while that is not known, and for
   * an id the catalog does not carry.
   */
  baseProvider: Accessor<Provider | null>;
  /** The provider AFK runs resolve to, composed the same way. */
  afkProvider: Accessor<Provider | null>;
  /** The provider the lander run resolves to, composed the same way. */
  landerProvider: Accessor<Provider | null>;
  /** The bool spawn options the AFK provider declares (the option bag's rows). */
  afkBoolOptions: Accessor<ProviderOptionSpec[]>;
}

/** Where on the settings page to go: a section, and optionally one field in it. */
export interface RevealTarget {
  section: string;
  field?: RepoFieldKey;
}

export interface RepoSettingsForm {
  /**
   * The repo the drafts are diffed against: the frame's live repo, kept
   * across a failed refresh so pending edits stay saveable and guarded.
   * Undefined until the first load, and while another repo's load is pending.
   */
  saved: Accessor<Repo | undefined>;
  /** The binding of one field. */
  field<K extends RepoFieldKey>(key: K): FieldBinding<K>;
  /** The changed fields, in page order. Their count is the save bar's number. */
  changed: Accessor<RepoFieldKey[]>;
  /** The sections that hold a changed field, in page order. */
  changedSections: Accessor<FormSectionSlug[]>;
  /** True while at least one field differs from the saved repo. */
  dirty: Accessor<boolean>;
  /** The fields with a problem shown under them, in page order. */
  problems: Accessor<RepoFieldKey[]>;
  /** The sections that hold a field with a problem, in page order. */
  problemSections: Accessor<FormSectionSlug[]>;
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
  /** Restores the saved values and offers Undo in the frame's toast. */
  discard: () => void;
  /** Drops every edit without a trace (the leave dialog's Discard). */
  drop: () => void;
  /**
   * Goes to a section — or a field — of the settings page: opens the Settings
   * tab if another tab shows, then scrolls there (and focuses the field).
   */
  reveal: (target: RevealTarget) => void;
  /** Bumped by every reveal(), so the page repeats one for an unchanged URL. */
  revealTick: Accessor<number>;
  /**
   * The field the page is being sent to (a reveal(), a `?field=` URL), so a
   * section that folds fields away can unfold the one that is wanted.
   */
  pointedAt: Accessor<RepoFieldKey | undefined>;
  /** Says which field the URL points at (the page calls it; undefined = none). */
  pointAt: (field: RepoFieldKey | undefined) => void;
  /**
   * What every overridable field resolves to while the repo's own value is
   * null — the server's answer for the current drafts. Undefined until the
   * first answer; a later request that fails leaves the last good one here
   * (see inheritedError).
   */
  inherited: Accessor<RepoInherited | undefined>;
  /**
   * Why the last request for the inherited values failed, or null. While it
   * is set, `inherited()` is the last good answer — possibly out of date —
   * or still undefined; the next trigger (or retryInherited) asks again.
   */
  inheritedError: Accessor<string | null>;
  /** Asks for the inherited values again, now. */
  retryInherited: () => void;
  /**
   * The same answer for the SAVED repo alone, no draft applied — what
   * anything that acts at once (Schedules) must read. Undefined while it is
   * not known for the repo as saved now.
   */
  inheritedSaved: Accessor<RepoInherited | undefined>;
  /**
   * The Runner that applies: the drafted pick when set here, else the
   * inherited one. null/undefined while that is not known.
   */
  effectiveRunner: Accessor<string | null | undefined>;
  catalog: RepoSettingsCatalog;
}

const FormContext = createContext<RepoSettingsForm>();

/** The repo settings form; only valid inside the /repos/:id frame. */
export function useRepoSettingsForm(): RepoSettingsForm {
  const form = useContext(FormContext);
  if (form === undefined) {
    throw new Error('useRepoSettingsForm() must be used inside the repo home frame (/repos/:id)');
  }
  return form;
}

/** "1 change" / "3 changes". */
export function plural(count: number, noun: string): string {
  return `${count} ${noun}${count === 1 ? '' : 's'}`;
}

/** For the fields whose wire form reads no context (every chain field). */
const NO_CONTEXT: FieldContext = { afkOptionKeys: null };

type EditSignals = { [K in RepoFieldKey]: Signal<RepoDrafts[K] | undefined> };
type Bindings = { [K in RepoFieldKey]: FieldBinding<K> };
type FieldErrors = Partial<Record<RepoFieldKey, string>>;

function sectionsOf(keys: RepoFieldKey[]): FormSectionSlug[] {
  const sections: FormSectionSlug[] = [];
  for (const key of keys) {
    const section = repoField(key).section;
    if (!sections.includes(section)) sections.push(section);
  }
  return sections;
}

function createRepoSettingsForm(): RepoSettingsForm {
  const home = useRepoHome();
  const location = useLocation();
  const navigate = useNavigate();

  // The saved repo, latched: a refresh that fails (the frame then reports no
  // repo) must not make pending edits unsaveable or unguarded, so the last
  // good repo of THIS id stands in until the next good one.
  const saved = createMemo<Repo | undefined>((previous) => {
    const id = home.id();
    const live = home.repo();
    if (live !== undefined) return live;
    return previous !== undefined && previous.id === id ? previous : undefined;
  });

  // One signal per field: the operator's edit, or undefined while untouched.
  const edits = {} as EditSignals;
  for (const key of REPO_FIELD_KEYS) {
    (edits as Record<string, Signal<unknown>>)[key] = createSignal<unknown>(undefined);
  }
  const editOf = <K extends RepoFieldKey>(key: K): RepoDrafts[K] | undefined => edits[key][0]();
  const setEdit = <K extends RepoFieldKey>(key: K, value: RepoDrafts[K] | undefined): void => {
    // Wrapped, so a draft can never be mistaken for a signal updater.
    edits[key][1](() => value);
  };
  const readEdits = (): RepoEdits => {
    const out: RepoEdits = {};
    for (const key of REPO_FIELD_KEYS) copyEdit(out, key);
    return out;
  };
  const copyEdit = <K extends RepoFieldKey>(out: RepoEdits, key: K): void => {
    const value = editOf(key);
    if (value !== undefined) out[key] = value;
  };

  const [errors, setErrors] = createSignal<FieldErrors>({});
  const [barError, setBarError] = createSignal<string | null>(null);
  const [busy, setBusy] = createSignal(false);
  const [revealTick, setRevealTick] = createSignal(0);
  const [pointedAt, setPointedAt] = createSignal<RepoFieldKey | undefined>(undefined);
  const [inherited, setInherited] = createSignal<RepoInherited | undefined>(undefined);
  const [inheritedError, setInheritedError] = createSignal<string | null>(null);
  // What the Save in flight submitted, while it is in flight: an edit made
  // meanwhile is measured against THAT, not against the repo it replaces.
  let inFlight: RepoEdits | undefined;

  const dropError = (key: RepoFieldKey): void => {
    if (errors()[key] === undefined) return;
    const next = { ...errors() };
    delete next[key];
    setErrors(next);
  };
  const clearEdit = (key: RepoFieldKey): void => {
    setEdit(key, undefined);
    dropError(key);
  };
  const dropPairError = <K extends RepoFieldKey>(key: K): void => {
    if (errors()[key] === undefined) return;
    const draft = draftOf(key);
    if (draft === undefined || validateDraft(key, draft) === null) dropError(key);
  };
  const drop = (): void =>
    batch(() => {
      for (const key of REPO_FIELD_KEYS) setEdit(key, undefined);
      setErrors({});
      setBarError(null);
    });

  // The draft of one field; undefined only before any repo has loaded.
  const draftOf = <K extends RepoFieldKey>(key: K): RepoDrafts[K] | undefined => {
    const edit = editOf(key);
    if (edit !== undefined) return edit;
    const repo = saved();
    return repo !== undefined ? repoField(key).seed(repo) : undefined;
  };

  // --- catalog ------------------------------------------------------------------
  // The provider catalog is fetched on demand (load()), and held here so the
  // option bag's rule — and the edits it applies to — survive a tab switch.
  const [wanted, setWanted] = createSignal(false);
  // True while the Settings tab shows: only then do inherited values follow
  // the repo and the drafts.
  const [active, setActive] = createSignal(false);
  const [providersResource, { refetch: refetchProviders }] = createResource(wanted, () =>
    listProviders(),
  );
  // Latched: a reload that fails keeps the list that was there.
  const providers = createMemo<Provider[]>(
    (previous) => resourceValue(providersResource) ?? previous,
    [],
  );
  const catalogError = (): string | null =>
    providersResource.error !== undefined ? errorMessage(providersResource.error) : null;
  const providerByID = (id: string | null | undefined): Provider | null =>
    id == null ? null : (providers().find((provider) => provider.id === id) ?? null);

  // The EFFECTIVE value of a field other fields depend on: its own draft when
  // set here, else what the server says it inherits. This is the only
  // composition the browser does — no chain is walked here.
  const effectiveOf = (
    key: 'provider' | 'afk_provider_default' | 'lander_provider' | 'runner',
  ): string | null | undefined => {
    const own = normText(draftOf(key) ?? '');
    return own !== null ? own : inherited()?.[key];
  };
  const baseProvider = createMemo(() => providerByID(effectiveOf('provider')));
  const afkProvider = createMemo(() => providerByID(effectiveOf('afk_provider_default')));
  const landerProvider = createMemo(() => providerByID(effectiveOf('lander_provider')));
  const effectiveRunner = (): string | null | undefined => effectiveOf('runner');
  const afkBoolOptions = createMemo(() =>
    (afkProvider()?.options ?? []).filter((option) => option.type === 'bool'),
  );
  const context = createMemo<FieldContext>(() => ({
    // Not known while the AFK provider is not: a drafted bag then stands on
    // its own keys (fields.ts).
    afkOptionKeys: afkProvider() === null ? null : afkBoolOptions().map((option) => option.key),
  }));
  const wording = () => ({
    providers: providers(),
    baseProvider: baseProvider(),
    afkProvider: afkProvider(),
    landerProvider: landerProvider(),
    afkBoolOptions: afkBoolOptions(),
  });

  // --- bindings -----------------------------------------------------------------
  const bind = <K extends RepoFieldKey>(key: K): FieldBinding<K> => {
    const spec = repoField(key);
    const value = createMemo<RepoDrafts[K] | undefined>(() => draftOf(key), undefined, {
      equals: (a, b) => a === b || (a !== undefined && b !== undefined && sameDraft(key, a, b)),
    });
    const changed = createMemo(() => {
      const edit = editOf(key);
      const repo = saved();
      return edit !== undefined && repo !== undefined && draftDiffers(key, edit, repo, context());
    });
    const set = (next: RepoDrafts[K]): void => {
      const repo = saved();
      batch(() => {
        // An edit back to the saved value is no edit: the field is untouched
        // again and follows the server. Not while a Save that carries this
        // field is in flight: the repo is about to hold what was submitted,
        // so going back to the value it holds NOW is an edit, and stays one.
        const untouched =
          inFlight?.[key] === undefined &&
          repo !== undefined &&
          sameDraft(key, next, spec.seed(repo));
        setEdit(key, untouched ? undefined : next);
        if (untouched) {
          // An untouched field is never sent: nothing about it is a problem.
          dropError(key);
        } else if (errors()[key] !== undefined) {
          // A problem stays under its field until the field is valid again; a
          // server refusal has no browser rule, so any edit clears it.
          const message = validateDraft(key, next);
          if (message === null) dropError(key);
          else setErrors({ ...errors(), [key]: message });
        }
        // A problem with a PAIR (a refusal the server pinned to the other
        // half, Autoland under the builtin binding) is answered from either
        // half: it goes when its own field's rule does not hold it.
        for (const other of pairedFields(key)) dropPairError(other);
        setBarError(null);
      });
    };
    const overridable = isOverridable(key);
    const inherits = createMemo(() => {
      const draft = value();
      return draft !== undefined && draftInherits(key, draft, context());
    });
    const inheritedValue = createMemo(() =>
      overridable ? inheritedText(key, inherited(), wording()) : null,
    );
    return {
      key,
      spec,
      // Sections render only once a repo has loaded, so a draft exists there.
      value: value as Accessor<RepoDrafts[K]>,
      set,
      changed,
      error: () => errors()[key] ?? null,
      overridable,
      inherits,
      inheritedText: inheritedValue,
      reset: () => {
        if (overridable) set(spec.inherit as RepoDrafts[K]);
      },
    };
  };
  const bindings = {} as Bindings;
  for (const key of REPO_FIELD_KEYS) {
    (bindings as Record<string, FieldBinding<RepoFieldKey>>)[key] = bind(key);
  }

  const changed = createMemo(() => REPO_FIELD_KEYS.filter((key) => bindings[key].changed()));
  const changedSections = createMemo(() => sectionsOf(changed()));
  const dirty = (): boolean => changed().length > 0;
  const problems = createMemo(() => REPO_FIELD_KEYS.filter((key) => errors()[key] !== undefined));
  const problemSections = createMemo(() => sectionsOf(problems()));
  // A problem is about a pending change, and is counted in the save bar. Once
  // nothing is pending the bar is gone — and so is every problem.
  createComputed(() => {
    if (dirty() || busy()) return;
    if (Object.keys(untrack(errors)).length > 0) setErrors({});
    if (untrack(barError) !== null) setBarError(null);
  });

  // --- seed / resync ------------------------------------------------------------
  // Another repo: nothing drafted against the previous one applies, and
  // nothing it inherited either.
  createComputed(
    on(
      home.id,
      () => {
        drop();
        forgetInherited();
      },
      { defer: true },
    ),
  );

  // A refresh of the same repo (the lib/seededDrafts.ts rule): an edit the
  // server caught up with is dropped, so the field is clean and follows the
  // server from here on. So is an "edit" that never was a change (a stray
  // space) once the server changes that field — it must not turn into one.
  const resync = <K extends RepoFieldKey>(key: K, fresh: Repo, previous: Repo): void => {
    const edit = editOf(key);
    if (edit === undefined) return;
    const spec = repoField(key);
    const next = spec.seed(fresh);
    if (sameDraft(key, edit, next)) {
      clearEdit(key);
    } else if (
      // Not for a field the Save in flight carries: the server changing it
      // is that Save landing, and an edit that differs from it is newer.
      inFlight?.[key] === undefined &&
      !sameDraft(key, spec.seed(previous), next) &&
      !draftDiffers(key, edit, previous, context())
    ) {
      clearEdit(key);
    }
  };
  createComputed(
    on(
      saved,
      (fresh, previous) => {
        if (fresh === undefined || previous === undefined || previous.id !== fresh.id) return;
        batch(() => {
          for (const key of REPO_FIELD_KEYS) resync(key, fresh, previous);
        });
      },
      { defer: true },
    ),
  );

  // --- inherited values ---------------------------------------------------------
  // Asked when the Settings tab mounts, when a SAVED value other fields'
  // chains read changes (the frame refetches the repo for every run and issue
  // event; none of those changes an answer) and, debounced, when a draft of
  // one changes. Each request carries only the chain drafts that are actually
  // edited (absent = the saved value, null = reset to inherit). Requests are
  // numbered: an answer that is not the latest one's is dropped, so a slow
  // older request can never overwrite a newer answer. A request that fails
  // leaves the last good answer in place — withdrawing it would empty every
  // catalog and unfold what the Runner folds — and says so (inheritedError);
  // the next trigger, a refresh of the repo included, asks again.
  let asked = 0;
  let debounce: ReturnType<typeof setTimeout> | undefined;
  // What the last request asked about, to skip a repeat of it.
  let lastAsked: { saved: string; drafts: string } | undefined;
  // The answer for the SAVED repo, no draft applied, with the saved chain
  // values it was asked about: it holds only while those are the repo's.
  const [savedAnswer, setSavedAnswer] = createSignal<
    { saved: string; answer: RepoInherited } | undefined
  >(undefined);
  // The saved chain values a no-draft request is under way (or answered) for.
  let savedAskedFor: string | undefined;
  const forgetInherited = (): void => {
    asked += 1;
    lastAsked = undefined;
    savedAskedFor = undefined;
    clearTimeout(debounce);
    batch(() => {
      setInherited(undefined);
      setInheritedError(null);
      setSavedAnswer(undefined);
    });
  };
  // The saved repo, as far as any chain reads it.
  const savedWire = <K extends ChainFieldKey>(key: K, repo: Repo): unknown => {
    const spec = repoField(key);
    return spec.wire(spec.seed(repo), NO_CONTEXT);
  };
  const savedChainKey = createMemo<string | undefined>(() => {
    const repo = saved();
    if (repo === undefined) return undefined;
    return JSON.stringify([repo.id, ...CHAIN_FIELD_KEYS.map((key) => savedWire(key, repo))]);
  });
  const inheritedSaved = (): RepoInherited | undefined => {
    const held = savedAnswer();
    return held !== undefined && held.saved === savedChainKey() ? held.answer : undefined;
  };
  const chainDrafts = (): RepoInheritedDrafts => {
    const drafts: RepoInheritedDrafts = {};
    for (const key of CHAIN_FIELD_KEYS) addChainDraft(drafts, key);
    return drafts;
  };
  const addChainDraft = <K extends ChainFieldKey>(drafts: RepoInheritedDrafts, key: K): void => {
    const edit = editOf(key);
    if (edit === undefined || !bindings[key].changed()) return;
    // A chain field's wire form depends on nothing but its own draft.
    drafts[key] = repoField(key).wire(edit, NO_CONTEXT) as RepoInheritedDrafts[K];
  };
  // A memo, so the debounce below starts only when the drafts to send really
  // change — not whenever something they were read through does.
  const chainKey = createMemo(() => JSON.stringify(chainDrafts()));
  const NO_DRAFTS = '{}';
  // The answer for the saved repo alone. Any answer to a question without
  // drafts is one — also one a newer question has overtaken.
  const keepSavedAnswer = (savedKey: string, answer: RepoInherited): void => {
    if (untrack(savedChainKey) === savedKey) setSavedAnswer({ saved: savedKey, answer });
  };
  const askInherited = (always: boolean): void => {
    clearTimeout(debounce);
    const repo = untrack(saved);
    const savedKey = untrack(savedChainKey);
    if (!untrack(active) || repo === undefined || savedKey === undefined) return;
    const drafts = untrack(chainDrafts);
    const key = untrack(chainKey);
    if (
      !always &&
      lastAsked !== undefined &&
      lastAsked.saved === savedKey &&
      lastAsked.drafts === key
    ) {
      return;
    }
    lastAsked = { saved: savedKey, drafts: key };
    asked += 1;
    const number = asked;
    if (key === NO_DRAFTS) savedAskedFor = savedKey;
    getRepoInherited(repo.id, drafts).then(
      (answer) => {
        batch(() => {
          if (key === NO_DRAFTS) keepSavedAnswer(savedKey, answer);
          if (number !== asked) return;
          setInherited(answer);
          setInheritedError(null);
        });
      },
      (err: unknown) => {
        if (key === NO_DRAFTS && savedAskedFor === savedKey) savedAskedFor = undefined;
        if (number !== asked) return;
        // The last good answer stands — possibly out of date, which the page
        // says — and nothing is guessed. The next trigger asks again.
        lastAsked = undefined;
        setInheritedError(errorMessage(err));
      },
    );
    // With chain drafts pending, the answer above is for the DRAFTS. What
    // acts at once reads the saved repo's: ask for that too, unless it is
    // known or already on its way.
    if (key !== NO_DRAFTS && savedAskedFor !== savedKey) {
      savedAskedFor = savedKey;
      getRepoInherited(repo.id, {}).then(
        (answer) => keepSavedAnswer(savedKey, answer),
        () => {
          if (savedAskedFor === savedKey) savedAskedFor = undefined;
        },
      );
    }
  };
  createEffect(
    on([active, savedChainKey, saved], (now, before) => {
      // The tab came up, or a saved value some chain reads changed: ask. Any
      // other refresh of the repo asks only to retry a request that failed.
      const moved = before === undefined || before[0] !== now[0] || before[1] !== now[1];
      if (moved || untrack(inheritedError) !== null) askInherited(true);
    }),
  );
  createEffect(
    on(
      chainKey,
      () => {
        clearTimeout(debounce);
        debounce = setTimeout(() => askInherited(false), INHERITED_DEBOUNCE_MS);
      },
      { defer: true },
    ),
  );
  onCleanup(() => clearTimeout(debounce));

  // --- navigation ---------------------------------------------------------------
  const settingsBase = (): string => `/repos/${home.id()}/settings`;
  const reveal = (target: RevealTarget): void => {
    const base = settingsBase();
    const path = `${base}/${target.section}${target.field !== undefined ? `?field=${target.field}` : ''}`;
    const onSettings = location.pathname === base || location.pathname.startsWith(`${base}/`);
    // First, so a section that folded the field away has it back on the page
    // by the time the page looks for it.
    setPointedAt(target.field);
    // The page does the scrolling itself, so the router must not jump to the
    // top. Inside the Settings tab the URL is replaced (no history spam).
    navigate(path, onSettings ? { replace: true, scroll: false } : { scroll: false });
    // After navigate(): the router is mid-navigation by now (when the URL
    // changes at all), so the page acts once, on the new URL.
    setRevealTick((tick) => tick + 1);
  };

  // --- save / discard -----------------------------------------------------------
  const save = async (): Promise<boolean> => {
    const repo = saved();
    if (repo === undefined || busy()) return false;
    const fieldContext = context();
    const submitted = readEdits();
    const found = validateEdits(submitted, repo, fieldContext);
    batch(() => {
      setBarError(null);
      setErrors(found);
    });
    const firstProblem = REPO_FIELD_KEYS.find((key) => found[key] !== undefined);
    if (firstProblem !== undefined) {
      // Nothing is sent while a field is wrong.
      reveal({ section: repoField(firstProblem).section, field: firstProblem });
      return false;
    }
    const patch = buildRepoPatch(submitted, repo, fieldContext);
    const count = Object.keys(patch).length;
    if (count === 0) return true;

    inFlight = submitted;
    setBusy(true);
    try {
      const next = await updateRepo(repo.id, patch);
      // Left for another repo while the request was in flight: the answer
      // belongs to a repo the drafts no longer describe.
      if (home.id() !== repo.id) return true;
      batch(() => {
        // The response IS the saved repo now — applied to the frame at once,
        // so the bar and the marks clear without waiting for the SSE refetch.
        home.mutate(next);
        // Everything that was submitted is saved, whatever the server
        // normalised it to (a sanitized name, a digest-pinned image). An
        // edit typed while the request was in flight is newer, and stays.
        for (const key of REPO_FIELD_KEYS) settle(key, submitted);
        setErrors({});
      });
      home.notify(`Saved ${plural(count, 'change')} to ${next.name}`);
      return true;
    } catch (err) {
      const field = err instanceof ApiError ? err.field : undefined;
      if (isRepoFieldKey(field)) {
        // The refusal names its field: show it there.
        setErrors({ [field]: errorMessage(err) });
        reveal({ section: repoField(field).section, field });
      } else {
        setBarError(errorMessage(err));
      }
      return false;
    } finally {
      inFlight = undefined;
      setBusy(false);
    }
  };
  const settle = <K extends RepoFieldKey>(key: K, submitted: RepoEdits): void => {
    const sent = submitted[key];
    const current = editOf(key);
    if (sent === undefined || current === undefined) return;
    if (sameDraft(key, current, sent as RepoDrafts[K])) setEdit(key, undefined);
  };

  const discard = (): void => {
    const id = home.id();
    const droppedEdits = readEdits();
    const droppedErrors = errors();
    drop();
    home.notify('Changes discarded', {
      action: {
        label: 'Undo',
        run: () => {
          // The toast can outlive a move to another repo; the edits cannot.
          if (home.id() !== id) return;
          batch(() => {
            for (const key of REPO_FIELD_KEYS) restoreEdit(key, droppedEdits);
            setErrors(droppedErrors);
          });
        },
      },
    });
  };
  const restoreEdit = <K extends RepoFieldKey>(key: K, from: RepoEdits): void => {
    const value = from[key];
    if (value !== undefined) setEdit(key, value as RepoDrafts[K]);
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
    pointAt: (field) => void setPointedAt(field),
    inherited,
    inheritedError,
    retryInherited: () => askInherited(true),
    inheritedSaved,
    effectiveRunner,
    catalog: {
      load: () =>
        batch(() => {
          setWanted(true);
          setActive(true);
        }),
      idle: () => void setActive(false),
      providers,
      error: catalogError,
      retry: () => void refetchProviders(),
      baseProvider,
      afkProvider,
      landerProvider,
      afkBoolOptions,
    },
  };
}

/**
 * Mounts the form store for the repo home frame. It reads the repo from
 * useRepoHome(), so it must sit inside the frame's context provider, and
 * around everything that shows or edits pending changes: the tabs, the routed
 * tab, the save bar and the leave guard.
 */
export function RepoSettingsFormProvider(props: { children?: JSX.Element }) {
  const form = createRepoSettingsForm();
  return <FormContext.Provider value={form}>{props.children}</FormContext.Provider>;
}
