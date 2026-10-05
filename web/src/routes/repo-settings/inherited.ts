// What an overridable repo field inherits, in the operator's words (issue
// #61). The VALUES come from the server alone — getRepoInherited() answers,
// for every overridable field, what it resolves to while the repo's own value
// is null, computed by the spawn path's own resolvers. Nothing here resolves
// a chain: this module only words an answer the server already gave —
//
//   - an agent, model or effort id becomes its label from the provider
//     catalog (an id the catalog does not carry is shown as it is);
//   - an empty model or effort means the provider's own default;
//   - a boolean reads "on" / "off", a Runner by its name;
//   - an empty dev image means none is configured anywhere below the repo.
//
// The one composition the browser does lives in form.tsx: a field's
// EFFECTIVE value is its own draft when set, else the inherited one.

import type { Provider, ProviderOption, ProviderOptionSpec, RepoInherited } from '../../api';
import { runnerName } from '../../lib/runner';
import type { RepoFieldKey } from './fields';
import { onOff, toBoolMap } from './shared';

/**
 * The fields whose drafts other fields' inherited values depend on. An
 * unsaved edit of one of them is sent along with the question, so a
 * dependent field follows it before anything is saved (the AFK agent follows
 * the agent, a model follows its agent).
 */
export const CHAIN_FIELD_KEYS = [
  'provider',
  'model_default',
  'effort_default',
  'remote_default',
  'afk_provider_default',
  'afk_model_default',
  'afk_effort_default',
  'lander_provider',
] as const satisfies readonly RepoFieldKey[];

export type ChainFieldKey = (typeof CHAIN_FIELD_KEYS)[number];

/** How an empty inherited model or effort is worded: the provider picks. */
export const AGENT_DEFAULT = 'agent default';

/** What the page needs from the catalog to word an inherited value. */
export interface InheritedWording {
  /** Every registered provider (to name an agent). */
  providers: readonly Provider[];
  /** The provider each run class resolves to; null while it is not known. */
  baseProvider: Provider | null;
  afkProvider: Provider | null;
  landerProvider: Provider | null;
  /** The bool options the AFK provider declares (to word an option bag). */
  afkBoolOptions: readonly ProviderOptionSpec[];
}

function optionLabel(options: readonly ProviderOption[] | undefined, value: string): string {
  if (value === '') return AGENT_DEFAULT;
  return options?.find((option) => option.value === value)?.label ?? value;
}

function providerName(providers: readonly Provider[], id: string): string {
  return providers.find((provider) => provider.id === id)?.display_name ?? id;
}

/** "Ultracode on" for the checked options of a bag, "all off" for none. */
function bagText(bag: Record<string, string>, declared: readonly ProviderOptionSpec[]): string {
  const checked = toBoolMap(bag);
  const on = declared.filter((option) => checked[option.key] === true);
  return on.length === 0 ? 'all off' : on.map((option) => `${option.label} on`).join(', ');
}

/**
 * The value `key` inherits, worded for the page: the first pick of a select
 * ("Inherited · <text>"), the placeholder of a text or number field, the
 * "Default: <text>" line under a field set here. null = not known — the
 * values are still loading, the request failed, or the server could not
 * resolve this chain; the page then shows the state without a value. It never
 * shows a guessed one.
 */
export function inheritedText(
  key: RepoFieldKey,
  inherited: RepoInherited | undefined,
  wording: InheritedWording,
): string | null {
  // The seed prompt's inherited text rides the repo itself
  // (afk_prompt_effective, shown as the field's placeholder).
  if (key === 'afk_prompt') return 'the inherited seed prompt';
  if (inherited === undefined) return null;
  switch (key) {
    case 'provider':
    case 'afk_provider_default':
    case 'lander_provider': {
      const id = inherited[key];
      return id === null ? null : providerName(wording.providers, id);
    }
    case 'model_default':
      return modelText(inherited.model_default, wording.baseProvider);
    case 'effort_default':
      return effortText(inherited.effort_default, wording.baseProvider);
    case 'afk_model_default':
      return modelText(inherited.afk_model_default, wording.afkProvider);
    case 'afk_effort_default':
      return effortText(inherited.afk_effort_default, wording.afkProvider);
    case 'lander_model':
      return modelText(inherited.lander_model, wording.landerProvider);
    case 'lander_effort':
      return effortText(inherited.lander_effort, wording.landerProvider);
    case 'remote_default':
    case 'afk_remote_default': {
      const value = inherited[key];
      return value === null ? null : onOff(value);
    }
    case 'afk_options':
      return inherited.afk_options === null
        ? null
        : bagText(inherited.afk_options, wording.afkBoolOptions);
    case 'budget_minutes':
    case 'max_instances_override':
    case 'container_pids':
    case 'container_nofile': {
      const value = inherited[key];
      return value === null ? null : String(value);
    }
    case 'git_author_name':
    case 'git_author_email': {
      const value = inherited[key];
      if (value === null) return null;
      return value === '' ? 'none set' : value;
    }
    case 'runner':
      return inherited.runner === null ? null : (runnerName(inherited.runner) ?? inherited.runner);
    case 'image_ref':
      if (inherited.image_ref === null) return null;
      return inherited.image_ref === '' ? 'none configured' : inherited.image_ref;
    case 'container_memory':
      return inherited.container_memory;
    default:
      // Not an overridable field: it inherits nothing.
      return null;
  }
}

function modelText(value: string | null, provider: Provider | null): string | null {
  return value === null ? null : optionLabel(provider?.models, value);
}

function effortText(value: string | null, provider: Provider | null): string | null {
  return value === null ? null : optionLabel(provider?.efforts, value);
}
