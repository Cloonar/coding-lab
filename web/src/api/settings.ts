import { request } from './core';

// --- M5: settings ---

export const INT_SETTING_KEYS = [
  'max_instances',
  'afk_budget_minutes',
  'afk_tick_seconds',
  'afk_schedule_seconds',
  'sweep_interval_minutes',
  /**
   * Auto-dismiss timeout (minutes) for manual-session dialogs (issue #124).
   * Not seeded server-side: absent from GET means never set, and
   * Settings.tsx renders that as a blank input rather than a 0. 0 itself is a
   * valid, meaningful value ("never time out"), not a sentinel for unset.
   */
  'dialog_timeout_minutes',
  /**
   * The global container resource-limit defaults (issue #205), seeded
   * 4096/16384 — the floor under a repo's container_pids/container_nofile
   * override (the repo-settings Runner section's inherit hint reads these).
   */
  'container_pids',
  'container_nofile',
] as const;

export const TEXT_SETTING_KEYS = [
  /** Root agent-provider default (seeded); the base of every provider chain. */
  'provider_default',
  'spawn_model_default',
  'spawn_effort_default',
  /** AFK provider override ("" = inherit provider_default). */
  'spawn_provider_default_afk',
  'spawn_model_default_afk',
  'spawn_effort_default_afk',
  'git_author_name',
  'git_author_email',
  'afk_prompt',
  /**
   * The global container --memory default (issue #205), seeded "8g" — the
   * floor under a repo's container_memory override.
   */
  'container_memory',
  /**
   * Read-only, server-injected (issue #52): the built-in seed-prompt template
   * with literal <N>/<BRANCH> tokens un-interpolated. Listed here only so
   * normalizeSettings' loop below picks it up for Settings.tsx to read as the
   * textarea placeholder — callers MUST NOT include it in an updateSettings
   * patch (the server 400s). Settings.tsx's buildPatch iterates its own
   * explicit key list rather than this array, which keeps it out of PATCHes.
   */
  'afk_prompt_default',
] as const;

/**
 * The boolean settings (issue #163) — the first of their kind, hence their own
 * key list beside the int/text ones.
 *
 * `spawn_remote_default` is the base remote-control default (seeded false, so
 * always present on GET). `spawn_remote_default_afk` is the AFK OVERRIDE and is
 * therefore TRI-STATE: `null` means "inherit the base", distinct from an
 * explicit `false`. That is why these cannot reuse the text keys' ''-as-unset
 * convention — `false` is a legal value, so only null/absent can mean unset.
 */
export const BOOL_SETTING_KEYS = ['spawn_remote_default', 'spawn_remote_default_afk'] as const;

export type IntSettingKey = (typeof INT_SETTING_KEYS)[number];
export type TextSettingKey = (typeof TEXT_SETTING_KEYS)[number];
export type BoolSettingKey = (typeof BOOL_SETTING_KEYS)[number];

/**
 * The runtime-mutable settings (§3a keys). All fields optional: GET returns
 * whatever is stored, PATCH sends only the fields being changed.
 *
 * `spawn_options_afk` is the provider spawn-option bag for AFK runs. The server
 * stores it as a JSON string and returns it as one on GET; normalizeSettings
 * parses it to an object here, and PATCH sends it back as an object.
 *
 * `afk_prompt_default` is read-only (see TEXT_SETTING_KEYS above) — present on
 * every GET, must never be sent back in a PATCH.
 *
 * The bool keys carry `null` as a first-class value (an AFK override cleared
 * back to inherit), so their type is `boolean | null`, not just `boolean`.
 */
export type Settings = Partial<
  Record<IntSettingKey, number> &
    Record<TextSettingKey, string> &
    Record<BoolSettingKey, boolean | null>
> & {
  spawn_options_afk?: Record<string, string>;
};

/**
 * A settings bool: a real JSON boolean, or the "true"/"false" text the settings
 * table stores (same TEXT-store tolerance the int keys get below). Anything
 * else — including null — is not a boolean and yields undefined, leaving the
 * caller to distinguish inherit (null) from garbage itself.
 */
export function coerceBool(value: unknown): boolean | undefined {
  if (typeof value === 'boolean') return value;
  if (value === 'true') return true;
  if (value === 'false') return false;
  return undefined;
}

/**
 * Normalizes a settings payload: tolerates both a flat {key: value} object
 * and a {settings: {key: value}} envelope (like extractSpawnDefaults), and
 * coerces int keys that arrive as numeric strings (the settings table stores
 * TEXT). Unknown keys and garbage values are dropped.
 */
export function normalizeSettings(raw: unknown): Settings {
  if (typeof raw !== 'object' || raw === null) return {};
  let map = raw as Record<string, unknown>;
  const nested = map['settings'];
  if (typeof nested === 'object' && nested !== null) map = nested as Record<string, unknown>;
  const out: Settings = {};
  for (const key of TEXT_SETTING_KEYS) {
    const value = map[key];
    if (typeof value === 'string') out[key] = value;
  }
  // Bool keys carry null THROUGH (issue #163): for the AFK override null is the
  // inherit state, not a missing value, so it must survive the round trip —
  // dropping it would make an explicit `false` and an inherit indistinguishable.
  for (const key of BOOL_SETTING_KEYS) {
    const value = map[key];
    if (value === null) {
      out[key] = null;
      continue;
    }
    const coerced = coerceBool(value);
    if (coerced !== undefined) out[key] = coerced;
  }
  for (const key of INT_SETTING_KEYS) {
    const value = map[key];
    const n =
      typeof value === 'number'
        ? value
        : typeof value === 'string' && value.trim() !== ''
          ? Number(value)
          : NaN;
    if (Number.isInteger(n)) out[key] = n;
  }
  // spawn_options_afk arrives as a raw JSON string (GET) or an object (PATCH
  // echo); parse defensively and coerce values to strings. A missing key or
  // garbage (parse failure / non-object / array) is omitted entirely.
  const rawOptions = map['spawn_options_afk'];
  let parsed: unknown;
  if (typeof rawOptions === 'string') {
    try {
      parsed = JSON.parse(rawOptions);
    } catch {
      parsed = undefined;
    }
  } else if (typeof rawOptions === 'object' && rawOptions !== null) {
    parsed = rawOptions;
  }
  if (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)) {
    const options: Record<string, string> = {};
    for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
      options[key] = String(value);
    }
    out.spawn_options_afk = options;
  }
  return out;
}

export async function getSettings(): Promise<Settings> {
  return normalizeSettings(await request<unknown>('GET', '/settings'));
}

/**
 * PATCH /settings {key: value, …} → 200 with the updated settings. Ints go
 * as JSON numbers; the server validates (ints parse, model/effort against
 * the provider catalogs) and 400s {"error"} on a bad value. Runtime loops
 * re-read settings every tick, so saves apply without a restart.
 */
export async function updateSettings(patch: Settings): Promise<Settings> {
  return normalizeSettings(await request<unknown>('PATCH', '/settings', patch));
}
