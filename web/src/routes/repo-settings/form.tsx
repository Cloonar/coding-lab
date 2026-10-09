// The repo settings form store (issue #61): ONE form state for the whole repo
// page. The repo home frame mounts it (RepoSettingsFormProvider) around the
// tabs and the routed tab, so pending changes outlive the Settings tab — the
// save bar and the leave guard, both rendered by the frame, keep working on
// Overview and Issues while changes are pending.
//
// The form itself — the operator's edits over the frame's live repo, the
// lib/seededDrafts.ts seed/resync rule, the one save rule (validate in the
// browser, ONE PATCH of exactly the changed fields, the response applied to
// the frame's repo at once, a refusal shown at its field) — is the shared
// settings core's (components/settings/form.tsx), over the repo field table
// (fields.ts). Moving to another repo drops every edit; a Save answer or an
// Undo that arrives after such a move is ignored. This file adds what only
// the repo page has: the latched saved repo, and the inheritance layer.
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

import {
  batch,
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
} from 'solid-js';
import {
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
import {
  SettingsFormProvider,
  createFieldDrafts,
  createSettingsFormStore,
  plural,
  type FieldBinding as SettingsFieldBinding,
  type SettingsForm,
} from '../../components/settings/form';
import { resourceValue } from '../../lib/resource';
import { useRepoHome } from '../repo-home/context';
import {
  REPO_FIELD_TABLE,
  pairedFields,
  repoField,
  validateEdits,
  type FieldContext,
  type RepoFieldKey,
  type RepoSettingsShape,
} from './fields';
import { CHAIN_FIELD_KEYS, inheritedText, type ChainFieldKey } from './inherited';
import { normText } from './shared';

/**
 * How long an edit of a chain field waits before the inherited values are
 * asked for again — one request for a quick run of picks, not one each.
 */
export const INHERITED_DEBOUNCE_MS = 150;

/** One repo field's draft and state — what a section binds a control to. */
export type FieldBinding<K extends RepoFieldKey> = SettingsFieldBinding<RepoSettingsShape, K>;

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

export interface RepoSettingsForm extends SettingsForm<RepoSettingsShape> {
  /**
   * The repo the drafts are diffed against: the frame's live repo, kept
   * across a failed refresh so pending edits stay saveable and guarded.
   * Undefined until the first load, and while another repo's load is pending.
   */
  saved: Accessor<Repo | undefined>;
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

/** For the fields whose wire form reads no context (every chain field). */
const NO_CONTEXT: FieldContext = { afkOptionKeys: null };

function createRepoSettingsForm(): RepoSettingsForm {
  const home = useRepoHome();

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
  // eslint-disable-next-line solid/reactivity -- the accessor itself is handed on, and read where tracked
  const drafts = createFieldDrafts(REPO_FIELD_TABLE, saved);
  const { editOf, draftOf } = drafts;

  const [inherited, setInherited] = createSignal<RepoInherited | undefined>(undefined);
  const [inheritedError, setInheritedError] = createSignal<string | null>(null);

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

  // --- the form -----------------------------------------------------------------
  const form = createSettingsFormStore<RepoSettingsShape>({
    drafts,
    context,
    base: () => `/repos/${home.id()}/settings`,
    // Another repo: nothing drafted against the previous one applies, and
    // nothing it inherited either.
    scope: home.id,
    onReset: () => forgetInherited(),
    identity: (repo) => repo.id,
    pairedFields,
    validate: validateEdits,
    inheritedText: (key) => inheritedText(key, inherited(), wording()),
    send: (patch, repo) => updateRepo(repo.id, patch),
    apply: (next) => home.mutate(next),
    notify: (message, options) => home.notify(message, options),
    savedMessage: (count, next) => `Saved ${plural(count, 'change')} to ${next.name}`,
  });

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
    if (edit === undefined || !form.field(key).changed()) return;
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

  return {
    ...form,
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
 * tab, the save bar and the leave guard. The shared settings components read
 * the same form through SettingsFormContext.
 */
export function RepoSettingsFormProvider(props: { children?: JSX.Element }) {
  const form = createRepoSettingsForm();
  return (
    <FormContext.Provider value={form}>
      <SettingsFormProvider form={form}>{props.children}</SettingsFormProvider>
    </FormContext.Provider>
  );
}
