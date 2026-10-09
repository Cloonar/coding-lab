// What a global AFK or lander override inherits, in the operator's words
// (issue #85). Unlike the repo page — whose overrides sit on multi-layer
// chains only the server resolves (POST /repos/{id}/inherited) — each override
// here inherits ONE field on the same page, so the browser derives it from
// that field's DRAFT, live as the operator edits it (ADR-0080):
//
//   AFK agent             the drafted agent of runs you start (provider_default;
//                         the first registered provider while that is unset)
//   AFK model / effort    the drafted spawn model / effort, as the AFK agent's
//                         catalog names it
//   AFK remote control    the drafted base remote control, as on / off
//   lander model / effort the drafted spawn model / effort, as the agent of
//                         runs you start names it (there is no global lander
//                         agent: the lander runs the base agent)
//   seed prompt           the built-in template (shown as its placeholder)
//
// Wording follows the repo page (repo-settings/inherited.ts): an id becomes
// its label from the provider catalog; an empty model or effort — or one the
// resolving agent's catalog does not carry, which the spawn path skips — is
// the agent's own default. While the catalog has not loaded, an id is shown as
// it is. One hop, and no chain is walked: if the spawn path ever puts another
// layer between the spawn default and these overrides, this belongs behind
// the server like the repo's.

import type { Provider, ProviderOption } from '../../api';
import { AGENT_DEFAULT } from '../repo-settings/inherited';
import { onOff } from '../repo-settings/shared';
import type { GlobalFieldKey } from './fields';

/** What the wording reads: the drafts it derives from, and the catalog. */
export interface GlobalInheritedSource {
  /** The draft of a base field. */
  draft: {
    provider_default: string;
    spawn_model_default: string;
    spawn_effort_default: string;
    spawn_remote_default: boolean;
  };
  /** The provider runs you start resolve to (null while the catalog is empty). */
  baseProvider: Provider | null;
  /** The provider AFK runs resolve to (null while the catalog is empty). */
  afkProvider: Provider | null;
}

/** What a blank seed prompt runs, as the Default line names it. */
export const BUILT_IN_PROMPT = 'the built-in seed prompt';

/**
 * A model or effort id as `provider`'s catalog names it: its label; the
 * agent's default for an empty id or one the catalog does not carry; the id
 * itself while the catalog is not known.
 */
function optionText(
  value: string,
  provider: Provider | null,
  options: (provider: Provider) => readonly ProviderOption[],
): string {
  if (value === '') return AGENT_DEFAULT;
  if (provider === null) return value;
  return options(provider).find((option) => option.value === value)?.label ?? AGENT_DEFAULT;
}

const models = (provider: Provider): readonly ProviderOption[] => provider.models;
const efforts = (provider: Provider): readonly ProviderOption[] => provider.efforts;

/**
 * The value an overridable global field inherits, worded for the page: the
 * first pick of a select or segmented control ("Inherited · <text>"), the
 * placeholder of a text field, the "Default: <text>" line under a field set
 * here. null for a field that inherits nothing, and for an agent that is not
 * known yet.
 */
export function inheritedText(key: GlobalFieldKey, source: GlobalInheritedSource): string | null {
  const { draft, baseProvider, afkProvider } = source;
  switch (key) {
    case 'spawn_provider_default_afk':
      return baseProvider?.display_name ?? (draft.provider_default.trim() || null);
    case 'spawn_model_default_afk':
      return optionText(draft.spawn_model_default.trim(), afkProvider, models);
    case 'spawn_effort_default_afk':
      return optionText(draft.spawn_effort_default.trim(), afkProvider, efforts);
    case 'spawn_remote_default_afk':
      return onOff(draft.spawn_remote_default);
    case 'spawn_model_default_lander':
      return optionText(draft.spawn_model_default.trim(), baseProvider, models);
    case 'spawn_effort_default_lander':
      return optionText(draft.spawn_effort_default.trim(), baseProvider, efforts);
    case 'afk_prompt':
      return BUILT_IN_PROMPT;
    default:
      // Not an overridable field: it inherits nothing.
      return null;
  }
}
