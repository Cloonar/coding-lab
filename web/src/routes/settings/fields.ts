// The global settings field table (issue #85): ONE entry per PATCHable key of
// `Settings` (api/settings.ts), and the page's single source for everything
// the one save rule needs to know about a field — its section, its label, how
// its draft seeds from the saved settings, how the draft normalises to the
// wire, the rule the browser checks before Save sends anything, and whether
// it is OVERRIDABLE. The shape of an entry and every rule derived from the
// table are the shared settings core's (components/settings/fields.ts); this
// file holds the entries.
//
// Never in the table: the two read-only keys every GET carries,
// `afk_prompt_default` (the built-in seed prompt) and `dev_image_fallback`
// (the deployed --container-image). The server refuses a PATCH that sends
// either, and a key without an entry can never be sent.
//
// The entries are in PAGE ORDER — Agents (runs you start, AFK runs, lander,
// capacity), Runner, General — so "the first problem" and the order of the
// changed sections fall out of the table. Notifications holds no field: its
// rows act at once.
//
// What "inherit" means here. The AFK and lander overrides inherit the spawn
// default on the same page. The text ones (AFK agent, model and effort, lander
// model and effort, the seed prompt) inherit while blank and are saved as ""
// — the settings PATCH has no null for a text key — so their entries say
// which drafts inherit (`inherits`) instead of the repo page's null. The AFK
// remote control is tri-state: '' / 'true' / 'false' drafts, saved as null /
// true / false. What each inherits is worded in inherited.ts.
//
// The whole-number fields check the server's floors (settingsIntMin in
// internal/httpapi/settings.go) and transcript retention its ceiling too, so
// a value the server would refuse is caught at the field before anything is
// sent. Every function here is pure.

import { TRANSCRIPT_RETENTION_MAX_DAYS, type Settings } from '../../api';
import { createFieldTable, type FieldKey, type FieldSpec } from '../../components/settings/fields';
import { boolDraft, normBool, normInt, optionsKey, toBoolMap } from '../repo-settings/shared';

/** The sections whose fields wait for Save (Notifications acts at once). */
export type GlobalFormSectionSlug = 'agents' | 'runner' | 'general';

/** The draft type of every PATCHable key, keyed by its PATCH key. */
export interface GlobalDrafts {
  // Agents — runs you start
  provider_default: string;
  spawn_model_default: string;
  spawn_effort_default: string;
  spawn_remote_default: boolean;
  dialog_timeout_minutes: string;
  // Agents — AFK runs
  spawn_provider_default_afk: string;
  spawn_model_default_afk: string;
  spawn_effort_default_afk: string;
  /** Tri-state as a string: '' = inherit, 'true' / 'false' = an explicit pick. */
  spawn_remote_default_afk: string;
  /** The stored option bag as checked state per option (absent = off). */
  spawn_options_afk: Record<string, boolean>;
  afk_prompt: string;
  // Agents — lander
  spawn_model_default_lander: string;
  spawn_effort_default_lander: string;
  // Agents — capacity
  max_instances: string;
  afk_budget_minutes: string;
  afk_tick_seconds: string;
  afk_schedule_seconds: string;
  sweep_interval_minutes: string;
  // Runner
  runner_default: string;
  dev_image_default: string;
  container_memory: string;
  container_pids: string;
  container_nofile: string;
  // General
  git_author_name: string;
  git_author_email: string;
  transcript_retention_days: string;
  merge_delete_head: boolean;
}

/** The read-only keys of every GET: never a field, never sent. */
export type ReadOnlySettingKey = 'afk_prompt_default' | 'dev_image_fallback';

/** The global page's form, as the shared settings core sees it. */
export interface GlobalSettingsShape {
  drafts: GlobalDrafts;
  patch: Settings;
  saved: Settings;
  context: GlobalFieldContext;
  section: GlobalFormSectionSlug;
}

export type GlobalFieldKey = FieldKey<GlobalSettingsShape>;

/** What a field's wire value may depend on besides its own draft. */
export interface GlobalFieldContext {
  /**
   * Keys of the bool spawn options the AFK runs' provider declares. The bag
   * is PATCHed as the FULL declared bag once it differs from the saved one.
   * null = not known right now (the catalog has not loaded): a drafted bag
   * then stands on its own keys.
   */
  afkOptionKeys: readonly string[] | null;
}

export type GlobalFieldSpec<K extends GlobalFieldKey> = FieldSpec<GlobalSettingsShape, K>;

// --- validators ---------------------------------------------------------------

/**
 * A whole number from `min` (the server's floor) — and up to `max` where the
 * server caps it. A blank is no number: an integer setting cannot be cleared.
 */
function wholeNumber(min: number, max?: number) {
  return (draft: string): string | null => {
    const trimmed = draft.trim();
    const n = /^\d+$/.test(trimmed) ? Number(trimmed) : NaN;
    if (Number.isInteger(n) && n >= min && (max === undefined || n <= max)) return null;
    return max === undefined
      ? `Use a whole number, ${min} or more.`
      : `Use a whole number from ${min} to ${max}.`;
  };
}

const required =
  (message: string) =>
  (draft: string): string | null =>
    draft.trim() === '' ? message : null;

// --- wire helpers -------------------------------------------------------------

const trimmed = (draft: string): string => draft.trim();
const same = <T>(draft: T): T => draft;
/** A blank text override inherits (saved as ""). */
const blank = (draft: string): boolean => draft.trim() === '';
/** A stored text value as its draft ('' for an absent key). */
const text = (value: string | undefined): string => value ?? '';
/** A stored integer as its draft ('' for an absent key: never set). */
const int = (value: number | undefined): string => (value === undefined ? '' : String(value));
/** An integer draft as the PATCH takes it; undefined for one Save never sends. */
const intWire = (draft: string): number | undefined => normInt(draft) ?? undefined;
/**
 * An integer field changed when its text did: a typo in a field that was
 * never set (no wire value on either side) is still a change, and a problem.
 */
const intDiffers = (draft: string, saved: string): boolean => draft.trim() !== saved.trim();

/** The keys a drafted bag is sent with: the declared ones, else its own. */
function bagKeys(draft: Record<string, boolean>, context: GlobalFieldContext): readonly string[] {
  return context.afkOptionKeys ?? Object.keys(draft);
}

function field<K extends GlobalFieldKey>(
  key: K,
  spec: Omit<GlobalFieldSpec<K>, 'key'>,
): GlobalFieldSpec<K> {
  return { key, ...spec };
}

/** A whole-number entry: the draft is the field's text. */
function intField<K extends GlobalFieldKey & IntDraftKey>(
  key: K,
  spec: { section: GlobalFormSectionSlug; label: string; min: number; max?: number },
): GlobalFieldSpec<K> {
  return field(key, {
    section: spec.section,
    label: spec.label,
    seed: (s) => int(s[key] as number | undefined),
    wire: intWire as GlobalFieldSpec<K>['wire'],
    differs: intDiffers,
    validate: wholeNumber(spec.min, spec.max),
  } as Omit<GlobalFieldSpec<K>, 'key'>);
}

type IntDraftKey =
  | 'dialog_timeout_minutes'
  | 'max_instances'
  | 'afk_budget_minutes'
  | 'afk_tick_seconds'
  | 'afk_schedule_seconds'
  | 'sweep_interval_minutes'
  | 'container_pids'
  | 'container_nofile'
  | 'transcript_retention_days';

/** An overridable text entry: blank inherits, saved as "". */
function overrideText<K extends GlobalFieldKey & OverrideTextKey>(
  key: K,
  spec: { section: GlobalFormSectionSlug; label: string },
): GlobalFieldSpec<K> {
  return field(key, {
    section: spec.section,
    label: spec.label,
    seed: (s) => text(s[key]),
    wire: trimmed,
    inherit: '',
    inherits: blank,
  } as Omit<GlobalFieldSpec<K>, 'key'>);
}

type OverrideTextKey =
  | 'spawn_provider_default_afk'
  | 'spawn_model_default_afk'
  | 'spawn_effort_default_afk'
  | 'spawn_model_default_lander'
  | 'spawn_effort_default_lander'
  | 'afk_prompt';

/**
 * The table. Typed over the PATCHable keys of `Settings`, so a writable key
 * without an entry (or an entry that is no such key) does not compile.
 */
export const GLOBAL_FIELDS: {
  [K in Exclude<keyof Settings, ReadOnlySettingKey>]: GlobalFieldSpec<K>;
} = {
  // --- Agents: runs you start -------------------------------------------------
  // The root of every provider chain: there is nothing to inherit from, and
  // the server refuses a blank. An unseeded store shows the first registered
  // provider (sections/Agents.tsx) and saves it once picked.
  provider_default: field('provider_default', {
    section: 'agents',
    label: 'Agent',
    seed: (s) => text(s.provider_default),
    wire: trimmed,
  }),
  spawn_model_default: field('spawn_model_default', {
    section: 'agents',
    label: 'Model',
    seed: (s) => text(s.spawn_model_default),
    wire: trimmed,
  }),
  spawn_effort_default: field('spawn_effort_default', {
    section: 'agents',
    label: 'Effort',
    seed: (s) => text(s.spawn_effort_default),
    wire: trimmed,
  }),
  // The base remote-control default: a plain bool, seeded false server-side.
  spawn_remote_default: field('spawn_remote_default', {
    section: 'agents',
    label: 'Remote control',
    seed: (s) => s.spawn_remote_default === true,
    wire: same,
  }),
  // Not seeded server-side: absent means never set (a blank field); 0 is a
  // value ("never"), not a sentinel.
  dialog_timeout_minutes: intField('dialog_timeout_minutes', {
    section: 'agents',
    label: 'Dialog auto-dismiss, minutes',
    min: 0,
  }),
  // --- Agents: AFK runs ---------------------------------------------------------
  spawn_provider_default_afk: overrideText('spawn_provider_default_afk', {
    section: 'agents',
    label: 'Agent',
  }),
  spawn_model_default_afk: overrideText('spawn_model_default_afk', {
    section: 'agents',
    label: 'Model',
  }),
  spawn_effort_default_afk: overrideText('spawn_effort_default_afk', {
    section: 'agents',
    label: 'Effort',
  }),
  // Tri-state (issue #163): null inherits, false is an explicit off — both
  // are values, so the plain wire comparison is the dirty check.
  spawn_remote_default_afk: field('spawn_remote_default_afk', {
    section: 'agents',
    label: 'Remote control',
    seed: (s) => boolDraft(s.spawn_remote_default_afk ?? null),
    wire: normBool,
    inherit: '',
  }),
  // The option bag (issue #19): sent as the FULL declared bag once any
  // declared option differs from the stored one; an untouched bag is never
  // sent. There is no inherit state for it here — what is stored is the
  // global bag — so it is no override.
  spawn_options_afk: field('spawn_options_afk', {
    section: 'agents',
    label: 'Options',
    seed: (s) => toBoolMap(s.spawn_options_afk ?? null),
    same: (a, b) => optionsKey(a) === optionsKey(b),
    wire: (draft, context) =>
      Object.fromEntries(
        bagKeys(draft, context).map((key) => [key, (draft[key] ?? false) ? 'true' : 'false']),
      ),
    differs: (draft, saved, context) =>
      bagKeys(draft, context).some((key) => (draft[key] ?? false) !== (saved[key] ?? false)),
  }),
  // Blank runs the built-in seed prompt (afk_prompt_default), which the
  // field shows as its placeholder; Customize copies it in.
  afk_prompt: overrideText('afk_prompt', { section: 'agents', label: 'Seed prompt' }),
  // --- Agents: lander -----------------------------------------------------------
  spawn_model_default_lander: overrideText('spawn_model_default_lander', {
    section: 'agents',
    label: 'Model',
  }),
  spawn_effort_default_lander: overrideText('spawn_effort_default_lander', {
    section: 'agents',
    label: 'Effort',
  }),
  // --- Agents: capacity ---------------------------------------------------------
  max_instances: intField('max_instances', { section: 'agents', label: 'Max instances', min: 1 }),
  afk_budget_minutes: intField('afk_budget_minutes', {
    section: 'agents',
    label: 'AFK budget, minutes',
    min: 1,
  }),
  // The ticks floor at 5 seconds on the server: faster would hammer tmux and
  // the tracker.
  afk_tick_seconds: intField('afk_tick_seconds', {
    section: 'agents',
    label: 'Reaper tick, seconds',
    min: 5,
  }),
  afk_schedule_seconds: intField('afk_schedule_seconds', {
    section: 'agents',
    label: 'Scheduler tick, seconds',
    min: 5,
  }),
  sweep_interval_minutes: intField('sweep_interval_minutes', {
    section: 'agents',
    label: 'Sweep interval, minutes',
    min: 1,
  }),
  // --- Runner -------------------------------------------------------------------
  // Exactly host or container. Switching TO host asks first (form.tsx).
  runner_default: field('runner_default', {
    section: 'runner',
    label: 'Runner',
    seed: (s) => text(s.runner_default),
    wire: same,
  }),
  // Blank falls through to the deployed image; the server resolves and
  // digest-pins a ref on save, and refuses one that does not resolve —
  // naming this field.
  dev_image_default: field('dev_image_default', {
    section: 'runner',
    label: 'Dev image',
    seed: (s) => text(s.dev_image_default),
    wire: trimmed,
  }),
  // A concrete value, not an override: there is nothing to clear it to. The
  // grammar is the server's call.
  container_memory: field('container_memory', {
    section: 'runner',
    label: 'Memory',
    seed: (s) => text(s.container_memory),
    wire: trimmed,
    validate: required('Enter a memory limit, for example 8g.'),
  }),
  container_pids: intField('container_pids', { section: 'runner', label: 'Processes', min: 1 }),
  container_nofile: intField('container_nofile', {
    section: 'runner',
    label: 'Open files',
    min: 1,
  }),
  // --- General ------------------------------------------------------------------
  git_author_name: field('git_author_name', {
    section: 'general',
    label: 'Git author name',
    seed: (s) => text(s.git_author_name),
    wire: trimmed,
  }),
  git_author_email: field('git_author_email', {
    section: 'general',
    label: 'Git author email',
    seed: (s) => text(s.git_author_email),
    wire: trimmed,
  }),
  // 0 is the off switch (keep none); the server caps the window.
  transcript_retention_days: intField('transcript_retention_days', {
    section: 'general',
    label: 'Transcript retention, days',
    min: 0,
    max: TRANSCRIPT_RETENTION_MAX_DAYS,
  }),
  // A plain bool, seeded true server-side (issue #90, ADR-0081): an absent key
  // reads on, as the server's GetBool(key, true) does.
  merge_delete_head: field('merge_delete_head', {
    section: 'general',
    label: 'Delete head branch after merge',
    seed: (s) => s.merge_delete_head !== false,
    wire: same,
  }),
};

/** The rules derived from the table (components/settings/fields.ts). */
export const GLOBAL_FIELD_TABLE = createFieldTable<GlobalSettingsShape>(GLOBAL_FIELDS);

/** Every field key, in page order. */
export const GLOBAL_FIELD_KEYS = GLOBAL_FIELD_TABLE.keys;

/** Narrows an arbitrary string (a `?field=` value, a refusal's field) to a field key. */
export const isGlobalFieldKey = GLOBAL_FIELD_TABLE.isKey;

/** The table entry of one field. */
export function globalField<K extends GlobalFieldKey>(key: K): GlobalFieldSpec<K> {
  return GLOBAL_FIELDS[key];
}
