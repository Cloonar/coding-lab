// The repo settings field table (issue #61): ONE entry per PATCHable repo
// field, and the page's single source for everything the one save rule needs
// to know about a field — its section, its label, how its draft seeds from a
// saved repo, how the draft normalises to the wire (the '' <-> null
// conventions of shared.ts, trimmed branch fields, the AFK option bag rule),
// the rule the browser checks before Save sends anything, and whether it is
// OVERRIDABLE (an `inherit` draft, saved as a null override). The shape of an
// entry and every rule derived from the table are the shared settings core's
// (components/settings/fields.ts); what is the repo's own lives here: the
// entries, the server's field PAIRS, and the one pair check the browser can
// judge by itself (Autoland needs the forge binding).
//
// The entries are in PAGE ORDER, so "the first problem" and the order of the
// changed sections fall out of the table. Sections are thin renderers over it
// (form.tsx binds a draft per key; Field.tsx draws label, mark, control, hint
// and error).
//
// Every function here is pure. A draft is always a control's own value type
// (a string for text, number and select fields, a boolean for a switch, a
// per-key boolean map for the option bag); the wire value is what
// PATCH /repos/{id} takes for that key.

import type { Repo, RepoPatch, Runner, TrackerBinding } from '../../api';
import {
  createFieldTable,
  type FieldEdits,
  type FieldKey,
  type FieldProblems,
  type FieldSpec,
} from '../../components/settings/fields';
import { boolDraft, normBool, normInt, normText, optionsKey, toBoolMap } from './shared';

/** The sections whose fields wait for Save (the other four act at once). */
export type FormSectionSlug =
  'agents' | 'runner' | 'autoland' | 'general' | 'integrations' | 'branches';

/** The draft type of every PATCHable field, keyed by its PATCH key. */
export interface RepoDrafts {
  // Agents — runs you start
  provider: string;
  model_default: string;
  effort_default: string;
  /** Tri-state as a string: '' = inherit, 'true' / 'false' = an explicit pick. */
  remote_default: string;
  // Agents — AFK runs
  afk_provider_default: string;
  afk_model_default: string;
  afk_effort_default: string;
  afk_remote_default: string;
  /**
   * The repo's own option bag as checked state per option; null = the repo
   * has no bag of its own and inherits the one below it.
   */
  afk_options: Record<string, boolean> | null;
  afk_prompt: string;
  // Agents — AFK capacity
  afk_auto_enabled: boolean;
  budget_minutes: string;
  max_instances_override: string;
  // Runner
  runner: string;
  image_ref: string;
  container_memory: string;
  container_pids: string;
  container_nofile: string;
  // Autoland
  autoland_enabled: boolean;
  auto_merge: boolean;
  max_fix_attempts: string;
  lander_provider: string;
  lander_model: string;
  lander_effort: string;
  // General
  name: string;
  git_author_name: string;
  git_author_email: string;
  incogni: boolean;
  // Integrations
  credential_id: string;
  tracker_binding: TrackerBinding;
  forge_credential_id: string;
  // Branches
  default_branch: string;
  afk_branch_pattern: string;
  manual_branch_prefix: string;
}

/** The repo page's form, as the shared settings core sees it. */
export interface RepoSettingsShape {
  drafts: RepoDrafts;
  patch: RepoPatch;
  saved: Repo;
  context: FieldContext;
  section: FormSectionSlug;
}

export type RepoFieldKey = FieldKey<RepoSettingsShape>;

/** What a field's wire value may depend on besides its own draft. */
export interface FieldContext {
  /**
   * Keys of the bool spawn options the AFK runs' effective provider declares.
   * The option bag is PATCHed as the FULL declared bag once it differs from
   * the saved repo. null = not known right now (the provider could not be
   * told — its inherited value is still loading, or failed to load): a
   * drafted bag then stands on its own keys, so a pending change to it is
   * neither lost nor rewritten.
   */
  afkOptionKeys: readonly string[] | null;
}

export type RepoFieldSpec<K extends RepoFieldKey> = FieldSpec<RepoSettingsShape, K>;

// --- validators ---------------------------------------------------------------

const required =
  (message: string) =>
  (draft: string): string | null =>
    draft.trim() === '' ? message : null;

/** A blank field inherits; anything else must be a whole number >= min. */
const optionalWholeNumber =
  (min: number) =>
  (draft: string): string | null => {
    const n = normInt(draft);
    return n === undefined || (n !== null && n < min)
      ? `Use a whole number, ${min} or more, or leave it empty.`
      : null;
  };

const wholeNumber =
  (min: number) =>
  (draft: string): string | null => {
    const n = normInt(draft);
    return n === undefined || n === null || n < min ? `Use a whole number, ${min} or more.` : null;
  };

/** The AFK branch pattern names the issue number exactly once. */
export function validateAfkBranchPattern(draft: string): string | null {
  return (draft.match(/<N>/g) ?? []).length === 1
    ? null
    : 'The pattern needs <N> exactly once. It stands for the issue number.';
}

// --- wire helpers -------------------------------------------------------------

const trimmed = (draft: string): string => draft.trim();
/** '' = no pick: null on the wire. Ids are never trimmed or rewritten. */
const pickOrNull = (draft: string): string | null => (draft === '' ? null : draft);
const same = <T>(draft: T): T => draft;

/** The keys a drafted bag is sent with: the declared ones, else its own. */
function bagKeys(draft: Record<string, boolean>, context: FieldContext): readonly string[] {
  return context.afkOptionKeys ?? Object.keys(draft);
}

function field<K extends RepoFieldKey>(
  key: K,
  spec: Omit<RepoFieldSpec<K>, 'key'>,
): RepoFieldSpec<K> {
  return { key, ...spec };
}

/**
 * The table. Typed over the PATCH's own keys, so a PATCHable field without an
 * entry (or an entry that is no PATCH key) does not compile.
 */
export const REPO_FIELDS: { [K in keyof Required<RepoPatch>]: RepoFieldSpec<K> } = {
  // --- Agents: runs you start --------------------------------------------------
  provider: field('provider', {
    section: 'agents',
    label: 'Agent',
    seed: (r) => r.provider ?? '',
    wire: normText,
    inherit: '',
  }),
  model_default: field('model_default', {
    section: 'agents',
    label: 'Model',
    seed: (r) => r.model_default ?? '',
    wire: normText,
    inherit: '',
  }),
  effort_default: field('effort_default', {
    section: 'agents',
    label: 'Effort',
    seed: (r) => r.effort_default ?? '',
    wire: normText,
    inherit: '',
  }),
  // Tri-state (issue #163): null clears back to inherit, false is an explicit
  // off — both are values, so the plain wire comparison is the dirty check.
  remote_default: field('remote_default', {
    section: 'agents',
    label: 'Remote control',
    seed: (r) => boolDraft(r.remote_default),
    wire: normBool,
    inherit: '',
  }),
  // --- Agents: AFK runs ---------------------------------------------------------
  afk_provider_default: field('afk_provider_default', {
    section: 'agents',
    label: 'Agent',
    seed: (r) => r.afk_provider_default ?? '',
    wire: normText,
    inherit: '',
  }),
  afk_model_default: field('afk_model_default', {
    section: 'agents',
    label: 'Model',
    seed: (r) => r.afk_model_default ?? '',
    wire: normText,
    inherit: '',
  }),
  afk_effort_default: field('afk_effort_default', {
    section: 'agents',
    label: 'Effort',
    seed: (r) => r.afk_effort_default ?? '',
    wire: normText,
    inherit: '',
  }),
  afk_remote_default: field('afk_remote_default', {
    section: 'agents',
    label: 'Remote control',
    seed: (r) => boolDraft(r.afk_remote_default),
    wire: normBool,
    inherit: '',
  }),
  // The option bag (issue #19). null = the repo has no bag of its own and
  // inherits. A bag of its own is sent as the FULL declared bag once it
  // differs from the saved repo; a bag the operator did not touch is never
  // sent. The draft is an object (or null), so it compares by its key-sorted
  // JSON.
  afk_options: field('afk_options', {
    section: 'agents',
    label: 'Options',
    seed: (r) => (r.afk_options === null ? null : toBoolMap(r.afk_options)),
    same: (a, b) => (a === null || b === null ? a === b : optionsKey(a) === optionsKey(b)),
    wire: (draft, context) =>
      draft === null
        ? null
        : Object.fromEntries(
            bagKeys(draft, context).map((key) => [key, (draft[key] ?? false) ? 'true' : 'false']),
          ),
    differs: (draft, saved, context) => {
      // Reset, or the first bag of its own. A bag for a provider that
      // declares no options is nothing to send.
      if (draft === null) return saved !== null;
      const keys = bagKeys(draft, context);
      if (saved === null) return keys.length > 0;
      return keys.some((key) => (draft[key] ?? false) !== (saved[key] ?? false));
    },
    inherit: null,
  }),
  afk_prompt: field('afk_prompt', {
    section: 'agents',
    label: 'Seed prompt',
    seed: (r) => r.afk_prompt ?? '',
    wire: normText,
    inherit: '',
  }),
  // --- Agents: AFK capacity -----------------------------------------------------
  afk_auto_enabled: field('afk_auto_enabled', {
    section: 'agents',
    label: 'Auto-spawn',
    seed: (r) => r.afk_auto_enabled,
    wire: same,
  }),
  budget_minutes: field('budget_minutes', {
    section: 'agents',
    label: 'Budget, minutes',
    seed: (r) => (r.budget_minutes === null ? '' : String(r.budget_minutes)),
    wire: normInt,
    validate: optionalWholeNumber(1),
    inherit: '',
  }),
  max_instances_override: field('max_instances_override', {
    section: 'agents',
    label: 'Max instances',
    seed: (r) => (r.max_instances_override === null ? '' : String(r.max_instances_override)),
    wire: normInt,
    validate: optionalWholeNumber(1),
    inherit: '',
  }),
  // --- Runner -------------------------------------------------------------------
  // '' = inherit the global runner default (null on the wire, issue #55).
  runner: field('runner', {
    section: 'runner',
    label: 'Runner',
    seed: (r) => r.runner ?? '',
    wire: (draft) => (draft === '' ? null : (draft as Runner)),
    inherit: '',
  }),
  // The server resolves and digest-pins the ref on save; a bad ref comes back
  // as a refusal naming this field.
  image_ref: field('image_ref', {
    section: 'runner',
    label: 'Dev image',
    seed: (r) => r.image_ref ?? '',
    wire: normText,
    inherit: '',
  }),
  container_memory: field('container_memory', {
    section: 'runner',
    label: 'Memory',
    seed: (r) => r.container_memory ?? '',
    wire: normText,
    inherit: '',
  }),
  container_pids: field('container_pids', {
    section: 'runner',
    label: 'Processes',
    seed: (r) => (r.container_pids === null ? '' : String(r.container_pids)),
    wire: normInt,
    validate: optionalWholeNumber(1),
    inherit: '',
  }),
  container_nofile: field('container_nofile', {
    section: 'runner',
    label: 'Open files',
    seed: (r) => (r.container_nofile === null ? '' : String(r.container_nofile)),
    wire: normInt,
    validate: optionalWholeNumber(1),
    inherit: '',
  }),
  // --- Autoland -----------------------------------------------------------------
  autoland_enabled: field('autoland_enabled', {
    section: 'autoland',
    label: 'Autoland claim PRs',
    seed: (r) => r.autoland_enabled,
    wire: same,
  }),
  auto_merge: field('auto_merge', {
    section: 'autoland',
    label: 'Merge on clean PASS',
    seed: (r) => r.auto_merge,
    wire: same,
  }),
  // A plain (non-nullable) integer: a blank field is a problem, not "inherit".
  max_fix_attempts: field('max_fix_attempts', {
    section: 'autoland',
    label: 'Max fix attempts',
    seed: (r) => String(r.max_fix_attempts),
    wire: (draft) => normInt(draft) ?? undefined,
    validate: wholeNumber(0),
  }),
  lander_provider: field('lander_provider', {
    section: 'autoland',
    label: 'Agent',
    seed: (r) => r.lander_provider ?? '',
    wire: normText,
    inherit: '',
  }),
  lander_model: field('lander_model', {
    section: 'autoland',
    label: 'Model',
    seed: (r) => r.lander_model ?? '',
    wire: normText,
    inherit: '',
  }),
  lander_effort: field('lander_effort', {
    section: 'autoland',
    label: 'Effort',
    seed: (r) => r.lander_effort ?? '',
    wire: normText,
    inherit: '',
  }),
  // --- General ------------------------------------------------------------------
  name: field('name', {
    section: 'general',
    label: 'Name',
    seed: (r) => r.name,
    wire: trimmed,
    validate: required('Enter a name.'),
  }),
  git_author_name: field('git_author_name', {
    section: 'general',
    label: 'Git author name',
    seed: (r) => r.git_author_name ?? '',
    wire: normText,
    inherit: '',
  }),
  git_author_email: field('git_author_email', {
    section: 'general',
    label: 'Git author email',
    seed: (r) => r.git_author_email ?? '',
    wire: normText,
    inherit: '',
  }),
  incogni: field('incogni', {
    section: 'general',
    label: 'Incogni',
    seed: (r) => r.incogni,
    wire: same,
  }),
  // --- Integrations -------------------------------------------------------------
  credential_id: field('credential_id', {
    section: 'integrations',
    label: 'Git credential',
    seed: (r) => r.credential_id ?? '',
    wire: pickOrNull,
  }),
  tracker_binding: field('tracker_binding', {
    section: 'integrations',
    label: 'Tracker binding',
    seed: (r) => r.tracker_binding,
    wire: same,
  }),
  forge_credential_id: field('forge_credential_id', {
    section: 'integrations',
    label: 'Forge credential',
    seed: (r) => r.forge_credential_id ?? '',
    wire: pickOrNull,
  }),
  // --- Branches -----------------------------------------------------------------
  default_branch: field('default_branch', {
    section: 'branches',
    label: 'Default branch',
    seed: (r) => r.default_branch,
    wire: trimmed,
    validate: required('Enter the default branch.'),
  }),
  afk_branch_pattern: field('afk_branch_pattern', {
    section: 'branches',
    label: 'AFK branch pattern',
    seed: (r) => r.afk_branch_pattern,
    wire: trimmed,
    validate: validateAfkBranchPattern,
  }),
  manual_branch_prefix: field('manual_branch_prefix', {
    section: 'branches',
    label: 'Manual branch prefix',
    seed: (r) => r.manual_branch_prefix,
    wire: trimmed,
    validate: required('Enter a prefix, for example lab/.'),
  }),
};

/** The rules derived from the table (components/settings/fields.ts). */
export const REPO_FIELD_TABLE = createFieldTable<RepoSettingsShape>(REPO_FIELDS);

/** Every field key, in page order. */
export const REPO_FIELD_KEYS = REPO_FIELD_TABLE.keys;

/** Narrows an arbitrary string (a `?field=` value, a refusal's field) to a field key. */
export const isRepoFieldKey = REPO_FIELD_TABLE.isKey;

/** The table entry of one field. */
export function repoField<K extends RepoFieldKey>(key: K): RepoFieldSpec<K> {
  return REPO_FIELDS[key];
}

/** Whether a field's own value may be null, meaning "inherit". */
export const isOverridable = REPO_FIELD_TABLE.isOverridable;

/** The overridable fields, in page order. */
export const OVERRIDABLE_FIELD_KEYS = REPO_FIELD_TABLE.overridableKeys;

/**
 * Whether a draft of an overridable field leaves it inherited: it would be
 * saved as null (a blank text field, the inherit pick, no bag of its own).
 * Always false for a field that cannot inherit.
 */
export const draftInherits = REPO_FIELD_TABLE.draftInherits;

/** The operator's edits: a draft per touched field, absent for an untouched one. */
export type RepoEdits = FieldEdits<RepoSettingsShape>;

/** The edited fields that differ from the saved repo, in page order. */
export const changedFields = REPO_FIELD_TABLE.changedFields;

/**
 * The one PATCH of a Save: exactly the changed fields, each in its wire form.
 * Diffed against the SAVED repo, so a field the operator never touched can
 * never be sent — and a server-side change to it never reverted.
 */
export const buildRepoPatch = REPO_FIELD_TABLE.buildPatch;

/**
 * Fields the server checks as a PAIR (reposvc.UpdateSettings): the value that
 * would result on one side is only valid with the other. A problem with the
 * pair may be shown at either half, and an edit of either half answers it —
 * so a problem that is not a field's own rule failing is dropped when another
 * field of its group is edited (form.tsx).
 */
export const FIELD_PAIRS: readonly (readonly RepoFieldKey[])[] = [
  ['tracker_binding', 'forge_credential_id', 'autoland_enabled'],
  ['afk_branch_pattern', 'manual_branch_prefix'],
];

/** The other fields of the pairs `key` belongs to. */
export function pairedFields(key: RepoFieldKey): RepoFieldKey[] {
  return FIELD_PAIRS.filter((group) => group.includes(key))
    .flat()
    .filter((other) => other !== key);
}

/** Autoland is forge-only; what Save says instead of sending that pair. */
export const AUTOLAND_NEEDS_FORGE = 'Turn Autoland off before switching to the built-in tracker.';

/**
 * The problems of the CHANGED fields (an untouched field is never sent, so
 * never checked) — each field's own rule, then the one pair the browser can
 * judge by itself: Autoland on under the builtin tracker binding, which the
 * server refuses. It is reported at the Autoland switch, where it is fixed.
 */
export function validateEdits(
  edits: RepoEdits,
  saved: Repo,
  context: FieldContext,
): FieldProblems<RepoSettingsShape> {
  const problems = REPO_FIELD_TABLE.validateEdits(edits, saved, context);
  const changed = changedFields(edits, saved, context);
  if (changed.includes('tracker_binding') || changed.includes('autoland_enabled')) {
    const binding = edits.tracker_binding ?? saved.tracker_binding;
    const autoland = edits.autoland_enabled ?? saved.autoland_enabled;
    if (autoland && binding !== 'forge') problems.autoland_enabled = AUTOLAND_NEEDS_FORGE;
  }
  return problems;
}

/** One draft's problem under its field's rule, or null. */
export const validateDraft = REPO_FIELD_TABLE.validateDraft;
