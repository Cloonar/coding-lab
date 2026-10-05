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
  createMemo,
  createResource,
  createSignal,
  on,
  onCleanup,
  useContext,
  type Accessor,
  type JSX,
  type Signal,
} from 'solid-js';
import {
  ApiError,
  errorMessage,
  getSettings,
  listProviders,
  updateRepo,
  type Provider,
  type ProviderOptionSpec,
  type Repo,
  type Settings,
} from '../../api';
import { resourceValue } from '../../lib/resource';
import { providerFor } from '../../lib/spawn';
import { useRepoHome } from '../repo-home/context';
import {
  REPO_FIELD_KEYS,
  buildRepoPatch,
  draftDiffers,
  isRepoFieldKey,
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
}

/** The provider catalog and global settings the fields resolve against. */
export interface RepoSettingsCatalog {
  /**
   * Starts loading the catalog (idempotent). Only the Settings tab calls it,
   * so Overview and Issues never wait on — or even request — these reads.
   */
  load: () => void;
  /** Registered providers; empty until loaded. */
  providers: Accessor<Provider[]>;
  /** Global settings; undefined until loaded. */
  settings: Accessor<Settings | undefined>;
  /** The provider runs you start resolve to, against the DRAFTS. */
  baseProvider: Accessor<Provider | null>;
  /** The provider AFK runs resolve to, against the drafts. */
  afkProvider: Accessor<Provider | null>;
  /** The provider the lander run resolves to, against the drafts. */
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
  // Fetched on demand (load()), and held here so the option bag's rule — and
  // the edits it applies to — survive a tab switch.
  const [wanted, setWanted] = createSignal(false);
  const [providersResource] = createResource(wanted, () => listProviders());
  const [settingsResource] = createResource(wanted, () => getSettings());
  const providers = (): Provider[] => resourceValue(providersResource) ?? [];
  const settings = (): Settings | undefined => resourceValue(settingsResource);

  // Effective providers, resolved LIVE against the drafted agents (skip-layer,
  // ADR-0030), so the model/effort catalogs re-catalog as the operator flips
  // an agent — before anything is saved.
  const baseProvider = createMemo(() =>
    providerFor(providers(), draftOf('provider'), settings()?.provider_default),
  );
  const afkProvider = createMemo(() =>
    providerFor(
      providers(),
      draftOf('afk_provider_default'),
      settings()?.spawn_provider_default_afk,
      draftOf('provider'),
      settings()?.provider_default,
    ),
  );
  // The lander's provider: its own pick, else this repo's provider chain.
  const landerProvider = createMemo(() =>
    providerFor(
      providers(),
      draftOf('lander_provider'),
      draftOf('provider'),
      settings()?.provider_default,
    ),
  );
  const afkBoolOptions = createMemo(() =>
    (afkProvider()?.options ?? []).filter((option) => option.type === 'bool'),
  );
  const context = createMemo<FieldContext>(() => ({
    afkOptionKeys: afkBoolOptions().map((option) => option.key),
  }));

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
        // again and follows the server.
        const untouched = repo !== undefined && sameDraft(key, next, spec.seed(repo));
        setEdit(key, untouched ? undefined : next);
        // A problem stays under its field until the field is valid again; a
        // server refusal has no browser rule, so any edit clears it.
        if (errors()[key] !== undefined) {
          const message = validateDraft(key, next);
          if (message === null) dropError(key);
          else setErrors({ ...errors(), [key]: message });
        }
        setBarError(null);
      });
    };
    return {
      key,
      spec,
      // Sections render only once a repo has loaded, so a draft exists there.
      value: value as Accessor<RepoDrafts[K]>,
      set,
      changed,
      error: () => errors()[key] ?? null,
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

  // --- seed / resync ------------------------------------------------------------
  // Another repo: nothing drafted against the previous one applies.
  createComputed(on(home.id, () => drop(), { defer: true }));

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

  // --- navigation ---------------------------------------------------------------
  const settingsBase = (): string => `/repos/${home.id()}/settings`;
  const reveal = (target: RevealTarget): void => {
    const base = settingsBase();
    const path = `${base}/${target.section}${target.field !== undefined ? `?field=${target.field}` : ''}`;
    const onSettings = location.pathname === base || location.pathname.startsWith(`${base}/`);
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
    catalog: {
      load: () => void setWanted(true),
      providers,
      settings,
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
