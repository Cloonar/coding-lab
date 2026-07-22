// Pure helpers the repo-settings section forms share (issue #198), moved from
// the RepoSettings monolith unchanged: the '' ↔ null draft normalizers, the
// tri-state remote-control vocabulary and the AFK options-bag utilities.

import type { Provider } from '../../api';
import type { SelectOption } from '../../components/Select';

/** The explicit picks of a tri-state override; Select prepends the inherit row. */
export const REMOTE_OPTIONS: SelectOption[] = [
  { value: 'true', label: 'On' },
  { value: 'false', label: 'Off' },
];

/** '' ↔ null for nullable text columns (blank field → global default). */
export function normText(value: string): string | null {
  const trimmed = value.trim();
  return trimmed === '' ? null : trimmed;
}

/**
 * '' ↔ null for the nullable BOOLEAN columns (issue #163). The tri-state
 * override rides the Select as a string: '' = inherit (null on the wire),
 * 'true'/'false' = an explicit pick. `false` is a value, never a sentinel — the
 * 2-state-checkbox-over-a-3-state-model bug (issue #21) is impossible by
 * construction here.
 */
export function normBool(value: string): boolean | null {
  return value === '' ? null : value === 'true';
}

/** The inverse: a stored boolean|null column → its tri-state select value. */
export function boolDraft(value: boolean | null): string {
  return value === null ? '' : String(value);
}

/** Operator-facing name of an effective on/off state (the inherit row's hint). */
export function onOff(value: boolean): string {
  return value ? 'on' : 'off';
}

/** '' ↔ null for nullable integer columns. Returns undefined on garbage. */
export function normInt(value: string): number | null | undefined {
  const trimmed = value.trim();
  if (trimmed === '') return null;
  const n = Number(trimmed);
  return Number.isInteger(n) && n >= 0 ? n : undefined;
}

/** Maps a repo afk_options bag (null = inherit) to per-key booleans. */
export function toBoolMap(options: Record<string, string> | null): Record<string, boolean> {
  const out: Record<string, boolean> = {};
  for (const [key, value] of Object.entries(options ?? {})) out[key] = value === 'true';
  return out;
}

/** Key-order-independent equality probe for two boolean option maps. */
export function optionsKey(options: Record<string, boolean>): string {
  return JSON.stringify(
    Object.fromEntries(
      Object.keys(options)
        .sort()
        .map((key) => [key, options[key]]),
    ),
  );
}

// Remote control is a provider CAPABILITY: a provider that has no such knob
// ignores the field, so it renders disabled and says so in that provider's own
// words (display_name) — never a hardcoded brand.
export const remoteBlocker = (provider: Provider | null): string | null =>
  provider !== null && !provider.supports_remote ? provider.display_name : null;
