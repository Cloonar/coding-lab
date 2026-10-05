// The repo settings field table (issue #61): ONE entry per PATCHable repo
// field, and the page's single source for everything the one save rule needs
// to know about a field —
//
//   - which section it lives in (the save bar's section links, the chips' and
//     outline's "unsaved changes" marks, the `?field=` lookup),
//   - its label,
//   - how its draft seeds from a saved repo,
//   - how the draft normalises to the wire (the '' <-> null conventions of
//     shared.ts, trimmed branch fields, the AFK option bag rule),
//   - and the rule the browser checks before Save sends anything.
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
  /** Checked state per declared bool option (a null bag seeds all-unchecked). */
  afk_options: Record<string, boolean>;
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

export type RepoFieldKey = keyof RepoDrafts;

/** What a field's wire value may depend on besides its own draft. */
export interface FieldContext {
  /**
   * Keys of the bool spawn options the AFK runs' effective provider declares
   * (resolved against the drafted agents). The option bag is PATCHed as the
   * FULL declared bag once any of these differs from the saved repo.
   */
  afkOptionKeys: readonly string[];
}

export interface RepoFieldSpec<K extends RepoFieldKey> {
  key: K;
  section: FormSectionSlug;
  /** The label the page prints at the field. */
  label: string;
  /** The draft a saved repo shows. */
  seed: (repo: Repo) => RepoDrafts[K];
  /** Draft equality, for object-valued drafts; default `===`. */
  same?: (a: RepoDrafts[K], b: RepoDrafts[K]) => boolean;
  /** The draft as PATCH /repos/{id} takes it. */
  wire: (draft: RepoDrafts[K], context: FieldContext) => RepoPatch[K];
  /** Whether a draft differs from the saved one; default: their wire values differ. */
  differs?: (draft: RepoDrafts[K], saved: RepoDrafts[K], context: FieldContext) => boolean;
  /** What is wrong with a draft, in the operator's words; null = nothing. */
  validate?: (draft: RepoDrafts[K]) => string | null;
}

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
  }),
  model_default: field('model_default', {
    section: 'agents',
    label: 'Model',
    seed: (r) => r.model_default ?? '',
    wire: normText,
  }),
  effort_default: field('effort_default', {
    section: 'agents',
    label: 'Effort',
    seed: (r) => r.effort_default ?? '',
    wire: normText,
  }),
  // Tri-state (issue #163): null clears back to inherit, false is an explicit
  // off — both are values, so the plain wire comparison is the dirty check.
  remote_default: field('remote_default', {
    section: 'agents',
    label: 'Remote control',
    seed: (r) => boolDraft(r.remote_default),
    wire: normBool,
  }),
  // --- Agents: AFK runs ---------------------------------------------------------
  afk_provider_default: field('afk_provider_default', {
    section: 'agents',
    label: 'Agent',
    seed: (r) => r.afk_provider_default ?? '',
    wire: normText,
  }),
  afk_model_default: field('afk_model_default', {
    section: 'agents',
    label: 'Model',
    seed: (r) => r.afk_model_default ?? '',
    wire: normText,
  }),
  afk_effort_default: field('afk_effort_default', {
    section: 'agents',
    label: 'Effort',
    seed: (r) => r.afk_effort_default ?? '',
    wire: normText,
  }),
  afk_remote_default: field('afk_remote_default', {
    section: 'agents',
    label: 'Remote control',
    seed: (r) => boolDraft(r.afk_remote_default),
    wire: normBool,
  }),
  // The option bag (issue #19): a null bag seeds all-unchecked. Once any
  // DECLARED option differs from the saved repo, the full declared bag is
  // sent as the repo's explicit override. The draft is an object, so it
  // compares by its key-sorted JSON.
  afk_options: field('afk_options', {
    section: 'agents',
    label: 'Options',
    seed: (r) => toBoolMap(r.afk_options),
    same: (a, b) => optionsKey(a) === optionsKey(b),
    wire: (draft, context) =>
      Object.fromEntries(
        context.afkOptionKeys.map((key) => [key, (draft[key] ?? false) ? 'true' : 'false']),
      ),
    differs: (draft, saved, context) =>
      context.afkOptionKeys.some((key) => (draft[key] ?? false) !== (saved[key] ?? false)),
  }),
  afk_prompt: field('afk_prompt', {
    section: 'agents',
    label: 'Seed prompt',
    seed: (r) => r.afk_prompt ?? '',
    wire: normText,
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
  }),
  max_instances_override: field('max_instances_override', {
    section: 'agents',
    label: 'Max instances',
    seed: (r) => (r.max_instances_override === null ? '' : String(r.max_instances_override)),
    wire: normInt,
    validate: optionalWholeNumber(1),
  }),
  // --- Runner -------------------------------------------------------------------
  // '' = inherit the global runner default (null on the wire, issue #55).
  runner: field('runner', {
    section: 'runner',
    label: 'Runner',
    seed: (r) => r.runner ?? '',
    wire: (draft) => (draft === '' ? null : (draft as Runner)),
  }),
  // The server resolves and digest-pins the ref on save; a bad ref comes back
  // as a refusal naming this field.
  image_ref: field('image_ref', {
    section: 'runner',
    label: 'Dev image',
    seed: (r) => r.image_ref ?? '',
    wire: normText,
  }),
  container_memory: field('container_memory', {
    section: 'runner',
    label: 'Memory',
    seed: (r) => r.container_memory ?? '',
    wire: normText,
  }),
  container_pids: field('container_pids', {
    section: 'runner',
    label: 'Processes',
    seed: (r) => (r.container_pids === null ? '' : String(r.container_pids)),
    wire: normInt,
    validate: optionalWholeNumber(1),
  }),
  container_nofile: field('container_nofile', {
    section: 'runner',
    label: 'Open files',
    seed: (r) => (r.container_nofile === null ? '' : String(r.container_nofile)),
    wire: normInt,
    validate: optionalWholeNumber(1),
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
  }),
  lander_model: field('lander_model', {
    section: 'autoland',
    label: 'Model',
    seed: (r) => r.lander_model ?? '',
    wire: normText,
  }),
  lander_effort: field('lander_effort', {
    section: 'autoland',
    label: 'Effort',
    seed: (r) => r.lander_effort ?? '',
    wire: normText,
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
  }),
  git_author_email: field('git_author_email', {
    section: 'general',
    label: 'Git author email',
    seed: (r) => r.git_author_email ?? '',
    wire: normText,
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

/** Every field key, in page order. */
export const REPO_FIELD_KEYS = Object.keys(REPO_FIELDS) as RepoFieldKey[];

/** Narrows an arbitrary string (a `?field=` value, a refusal's field) to a field key. */
export function isRepoFieldKey(value: string | undefined | null): value is RepoFieldKey {
  return typeof value === 'string' && Object.hasOwn(REPO_FIELDS, value);
}

/** The table entry of one field. */
export function repoField<K extends RepoFieldKey>(key: K): RepoFieldSpec<K> {
  return REPO_FIELDS[key];
}

/** Draft equality under the field's own rule. */
export function sameDraft<K extends RepoFieldKey>(
  key: K,
  a: RepoDrafts[K],
  b: RepoDrafts[K],
): boolean {
  const spec = repoField(key);
  return spec.same !== undefined ? spec.same(a, b) : a === b;
}

/** Whether `draft` would change the saved repo if it were sent. */
export function draftDiffers<K extends RepoFieldKey>(
  key: K,
  draft: RepoDrafts[K],
  saved: Repo,
  context: FieldContext,
): boolean {
  const spec = repoField(key);
  const base = spec.seed(saved);
  return spec.differs !== undefined
    ? spec.differs(draft, base, context)
    : spec.wire(draft, context) !== spec.wire(base, context);
}

/** The operator's edits: a draft per touched field, absent for an untouched one. */
export type RepoEdits = { [K in RepoFieldKey]?: RepoDrafts[K] };

/** The edited fields that differ from the saved repo, in page order. */
export function changedFields(
  edits: RepoEdits,
  saved: Repo,
  context: FieldContext,
): RepoFieldKey[] {
  return REPO_FIELD_KEYS.filter((key) => isChanged(key, edits, saved, context));
}

function isChanged<K extends RepoFieldKey>(
  key: K,
  edits: RepoEdits,
  saved: Repo,
  context: FieldContext,
): boolean {
  const draft = edits[key];
  return draft !== undefined && draftDiffers(key, draft as RepoDrafts[K], saved, context);
}

/**
 * The one PATCH of a Save: exactly the changed fields, each in its wire form.
 * Diffed against the SAVED repo, so a field the operator never touched can
 * never be sent — and a server-side change to it never reverted.
 */
export function buildRepoPatch(edits: RepoEdits, saved: Repo, context: FieldContext): RepoPatch {
  const patch: RepoPatch = {};
  for (const key of REPO_FIELD_KEYS) assignChanged(patch, key, edits, saved, context);
  return patch;
}

function assignChanged<K extends RepoFieldKey>(
  patch: RepoPatch,
  key: K,
  edits: RepoEdits,
  saved: Repo,
  context: FieldContext,
): void {
  const draft = edits[key];
  if (draft === undefined || !draftDiffers(key, draft as RepoDrafts[K], saved, context)) return;
  patch[key] = repoField(key).wire(draft as RepoDrafts[K], context);
}

/** The problems of the CHANGED fields (an untouched field is never sent, so never checked). */
export function validateEdits(
  edits: RepoEdits,
  saved: Repo,
  context: FieldContext,
): Partial<Record<RepoFieldKey, string>> {
  const problems: Partial<Record<RepoFieldKey, string>> = {};
  for (const key of changedFields(edits, saved, context)) {
    const message = validateDraft(key, edits[key] as RepoDrafts[typeof key]);
    if (message !== null) problems[key] = message;
  }
  return problems;
}

/** One draft's problem under its field's rule, or null. */
export function validateDraft<K extends RepoFieldKey>(key: K, draft: RepoDrafts[K]): string | null {
  return repoField(key).validate?.(draft) ?? null;
}
