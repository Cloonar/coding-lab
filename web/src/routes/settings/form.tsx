// The global settings form store (issue #85): ONE form state for the whole
// /settings page. The form itself — the operator's edits over the saved
// settings, the one save rule (validate in the browser, ONE PATCH of exactly
// the changed keys, the response applied as the saved settings at once, a
// refusal shown at its field), Discard with Undo — is the shared settings
// core's (components/settings/form.tsx), over the global field table
// (fields.ts). This file adds what only global Settings has:
//
//   - The saved settings: GET /settings once, then only what this page's own
//     Save answers. Settings have no live event, so there is no resync, no
//     scope and no identity to follow; a failed load says so with a retry.
//   - The provider catalog the agent, model and effort picks list, and the
//     providers the drafts resolve to — the drafted agent of runs you start
//     (the first registered one while unset), and the drafted AFK agent
//     falling through to it — so the catalogs re-catalog as the operator
//     flips an agent, before anything is saved.
//   - What each AFK and lander override inherits, derived from the drafted
//     base field on this page, live (inherited.ts).
//   - The host switch. A Save whose PATCH switches `runner_default` to host
//     asks first, in an in-page dialog naming how many repos inherit the
//     default (the page renders it: HostSwitchDialog.tsx). Cancel sends
//     nothing and keeps every edit; the switch never asks in any other
//     direction. The question is the core's `confirm` hook, so it holds for
//     every Save — the save bar's and the leave dialog's "Save and leave".
//   - The toast: the page owns it, and the store confirms in it ("Saved 2
//     changes") and offers Undo after a Discard.

import {
  createContext,
  createMemo,
  createResource,
  createSignal,
  onCleanup,
  untrack,
  useContext,
  type Accessor,
  type JSX,
} from 'solid-js';
import {
  errorMessage,
  getSettings,
  listProviders,
  listRepos,
  updateSettings,
  type Provider,
  type ProviderOptionSpec,
  type Settings,
} from '../../api';
import {
  SettingsFormProvider,
  createFieldDrafts,
  createSettingsFormStore,
  plural,
  type SettingsForm,
} from '../../components/settings/form';
import type { ToastOptions } from '../../components/Toast';
import { resourceValue } from '../../lib/resource';
import { providerFor } from '../../lib/spawn';
import { GLOBAL_FIELD_TABLE, type GlobalFieldContext, type GlobalSettingsShape } from './fields';
import { inheritedText, type GlobalInheritedSource } from './inherited';

/** The settings page's path; a section's URL is `/settings/<slug>`. */
export const SETTINGS_BASE = '/settings';

/** The provider catalog, and the providers the drafts resolve to. */
export interface GlobalSettingsCatalog {
  /** Registered providers; empty until loaded. A failed reload keeps the last list. */
  providers: Accessor<Provider[]>;
  /** True once a list has arrived (an empty one included). */
  loaded: Accessor<boolean>;
  /** Why the catalog could not be loaded (or reloaded), or null. */
  error: Accessor<string | null>;
  /** Loads the catalog again after a failure. */
  retry: () => void;
  /** The provider runs you start resolve to; null while the catalog is empty. */
  baseProvider: Accessor<Provider | null>;
  /** The provider AFK runs resolve to; null while the catalog is empty. */
  afkProvider: Accessor<Provider | null>;
  /** The bool spawn options the AFK provider declares (the option bag's rows). */
  afkBoolOptions: Accessor<ProviderOptionSpec[]>;
}

/** The pending question of a Save that switches the runner default to host. */
export interface HostSwitch {
  /** True while the question is open (the page shows its dialog). */
  open: Accessor<boolean>;
  /** Answers it: true saves, false sends nothing and keeps every edit. */
  answer: (confirmed: boolean) => void;
}

export interface GlobalSettingsForm extends SettingsForm<GlobalSettingsShape> {
  /** Why the settings could not be loaded, or null. Only before a first load. */
  loadError: Accessor<string | null>;
  /** Loads the settings again after a failed first load. */
  retryLoad: () => void;
  catalog: GlobalSettingsCatalog;
  /**
   * How many repos inherit the runner default (their own Runner is unset);
   * null while the repo list is loading, or when it could not be loaded — the
   * count is never guessed and never blocks a Save.
   */
  inheritingRepos: Accessor<number | null>;
  hostSwitch: HostSwitch;
}

const FormContext = createContext<GlobalSettingsForm>();

/** The global settings form; only valid inside GlobalSettingsFormProvider. */
export function useGlobalSettingsForm(): GlobalSettingsForm {
  const form = useContext(FormContext);
  if (form === undefined) {
    throw new Error('useGlobalSettingsForm() must be used inside GlobalSettingsFormProvider');
  }
  return form;
}

/** Builds the form store of the global settings page. Call it in the page. */
export function createGlobalSettingsForm(options: {
  /** Shows the page's toast. */
  notify: (message: string, options?: ToastOptions) => void;
}): GlobalSettingsForm {
  // --- the saved settings -------------------------------------------------------
  const [settings, { mutate, refetch }] = createResource(() => getSettings());
  // Latched: once loaded, the snapshot only moves through this page's Save.
  const saved = createMemo<Settings | undefined>((previous) => resourceValue(settings) ?? previous);
  const loadError = (): string | null =>
    saved() === undefined && settings.error !== undefined ? errorMessage(settings.error) : null;

  // One signal per field: the operator's edit, or undefined while untouched.
  // eslint-disable-next-line solid/reactivity -- the accessor itself is handed on, and read where tracked
  const drafts = createFieldDrafts(GLOBAL_FIELD_TABLE, saved);
  const { draftOf } = drafts;

  // --- the catalog --------------------------------------------------------------
  const [providersResource, { refetch: refetchProviders }] = createResource(() => listProviders());
  // Latched: a reload that fails keeps the list that was there.
  const providers = createMemo<Provider[] | undefined>(
    (previous) => resourceValue(providersResource) ?? previous,
  );
  const catalogError = (): string | null =>
    providersResource.error !== undefined ? errorMessage(providersResource.error) : null;
  const list = (): Provider[] => providers() ?? [];
  const baseProvider = createMemo(() => providerFor(list(), draftOf('provider_default')));
  const afkProvider = createMemo(() =>
    providerFor(list(), draftOf('spawn_provider_default_afk'), draftOf('provider_default')),
  );
  const afkBoolOptions = createMemo(() =>
    (afkProvider()?.options ?? []).filter((option) => option.type === 'bool'),
  );
  const context = createMemo<GlobalFieldContext>(() => ({
    afkOptionKeys: afkProvider() === null ? null : afkBoolOptions().map((option) => option.key),
  }));

  // --- inherited values ---------------------------------------------------------
  const source = (): GlobalInheritedSource => ({
    draft: {
      provider_default: draftOf('provider_default') ?? '',
      spawn_model_default: draftOf('spawn_model_default') ?? '',
      spawn_effort_default: draftOf('spawn_effort_default') ?? '',
      spawn_remote_default: draftOf('spawn_remote_default') ?? false,
    },
    baseProvider: baseProvider(),
    afkProvider: afkProvider(),
  });

  // --- the repos that inherit the runner default -------------------------------
  const [repos] = createResource(() => listRepos());
  const inheritingRepos = (): number | null => {
    const rows = resourceValue(repos);
    return rows === undefined ? null : rows.filter((repo) => repo.runner === null).length;
  };

  // --- the host switch ----------------------------------------------------------
  // The Save waiting for an answer resolves through this.
  const [pendingHost, setPendingHost] = createSignal<((confirmed: boolean) => void) | null>(null);
  const answerHost = (confirmed: boolean): void => {
    const resolve = untrack(pendingHost);
    setPendingHost(null);
    resolve?.(confirmed);
  };
  const askHost = (): Promise<boolean> =>
    new Promise<boolean>((resolve) => {
      // One question at a time: an older one still open is a Cancel.
      untrack(pendingHost)?.(false);
      setPendingHost(() => resolve);
    });
  onCleanup(() => answerHost(false));

  // --- the form -----------------------------------------------------------------
  const form = createSettingsFormStore<GlobalSettingsShape>({
    drafts,
    context,
    base: () => SETTINGS_BASE,
    inheritedText: (key) => inheritedText(key, source()),
    // Only a switch TO host asks: the PATCH carries runner_default only when
    // it differs from the saved one.
    confirm: (patch, current) =>
      patch.runner_default === 'host' && current.runner_default !== 'host' ? askHost() : true,
    send: (patch) => updateSettings(patch),
    apply: (next) => void mutate(() => next),
    notify: (message, toastOptions) => options.notify(message, toastOptions),
    savedMessage: (count) => `Saved ${plural(count, 'change')}`,
  });

  return {
    ...form,
    loadError,
    retryLoad: () => void refetch(),
    catalog: {
      providers: list,
      loaded: () => providers() !== undefined,
      error: catalogError,
      retry: () => void refetchProviders(),
      baseProvider,
      afkProvider,
      afkBoolOptions,
    },
    inheritingRepos,
    hostSwitch: {
      open: () => pendingHost() !== null,
      answer: answerHost,
    },
  };
}

/**
 * Makes `form` the page's form: for the global sections (useGlobalSettingsForm)
 * and for the shared settings components (SettingsFormContext) alike.
 */
export function GlobalSettingsFormProvider(props: {
  form: GlobalSettingsForm;
  children?: JSX.Element;
}) {
  return (
    // eslint-disable-next-line solid/reactivity -- a page provides one form for its lifetime
    <FormContext.Provider value={props.form}>
      <SettingsFormProvider form={props.form}>{props.children}</SettingsFormProvider>
    </FormContext.Provider>
  );
}
