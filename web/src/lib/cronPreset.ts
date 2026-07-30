// Cadence presets ↔ cron (issue #247 / ADR-0062). A Schedule stores exactly
// one thing about when it fires: a five-field cron expression. The presets
// below are a phone-first EDITING skin over that single stored string, so the
// two directions have to line up — every preset renders to cron, and a stored
// cron decomposes back to the preset that produced it, or to nothing at all
// (which selects the raw-cron escape hatch).
//
// Recognition is deliberately EXACT rather than generous: only the three
// shapes presetToCron emits decompose. A cron the operator hand-wrote in a
// shape this file happens to half-understand ('05 6 * * *', '4,1' weekdays,
// '*/15') stays in Advanced and is preserved byte-for-byte, because silently
// re-rendering it as the "same" preset would rewrite a cadence nobody asked to
// change.
//
// Nothing here computes a firing. Upcoming firings are server-rendered
// (GET /cron/preview, one parser shared with the engine) so the SPA can never
// disagree with the server about when a Schedule actually runs.

/** The cadence editor's modes; 'advanced' is the raw-cron escape hatch. */
export type CadenceMode = 'daily' | 'weekly' | 'monthly' | 'advanced';

/**
 * A decomposed cadence. `time` is 24h "HH:MM" (an `<input type="time">`
 * value); `weekdays` are cron day-of-week numbers (Sun=0 … Sat=6), ascending;
 * `day` is a day of the month within the every-month-has-it range.
 */
export type CadencePreset =
  | { mode: 'daily'; time: string }
  | { mode: 'weekly'; time: string; weekdays: number[] }
  | { mode: 'monthly'; time: string; day: number };

/**
 * The weekday chips, in DISPLAY order (week starts Monday) carrying their cron
 * values — cron numbers Sunday 0, which is not where a week starts for a
 * reader. Rendering order and cron order are different things on purpose: the
 * expression always lists ascending cron numbers.
 */
export const WEEKDAYS: readonly { value: number; label: string }[] = [
  { value: 1, label: 'Mon' },
  { value: 2, label: 'Tue' },
  { value: 3, label: 'Wed' },
  { value: 4, label: 'Thu' },
  { value: 5, label: 'Fri' },
  { value: 6, label: 'Sat' },
  { value: 0, label: 'Sun' },
];

/**
 * The highest day-of-month the monthly preset offers. 29–31 are Advanced
 * territory: they skip the months that are too short, which is a real cron
 * behaviour but not one a "monthly" preset should imply.
 */
export const MONTH_DAY_MAX = 28;

const pad2 = (value: number): string => String(value).padStart(2, '0');

/** "HH:MM" → its parts; anything out of range or ill-formed → null. */
function parseTime(time: string): { hour: number; minute: number } | null {
  const match = /^(\d{1,2}):(\d{2})$/.exec(time.trim());
  if (match === null) return null;
  const hour = Number(match[1]);
  const minute = Number(match[2]);
  if (hour > 23 || minute > 59) return null;
  return { hour, minute };
}

/**
 * A bare decimal cron field within [min, max]. Leading zeros are rejected
 * ('05' is not what presetToCron emits) so such an expression falls to
 * Advanced instead of being rewritten on the next save.
 */
function plainNumber(field: string, min: number, max: number): number | null {
  if (!/^(?:0|[1-9]\d?)$/.test(field)) return null;
  const value = Number(field);
  return value >= min && value <= max ? value : null;
}

/** Ascending, de-duplicated cron weekday numbers; out-of-range values drop. */
export function normalizeWeekdays(weekdays: readonly number[]): number[] {
  const seen = new Set<number>();
  for (const day of weekdays) {
    if (Number.isInteger(day) && day >= 0 && day <= 6) seen.add(day);
  }
  return [...seen].sort((a, b) => a - b);
}

/** The display label of a cron weekday number ('' for a number off the week). */
export function weekdayLabel(value: number): string {
  return WEEKDAYS.find((day) => day.value === value)?.label ?? '';
}

/**
 * A preset → its cron expression. Returns '' for a preset that cannot render
 * one (no time yet, no weekday picked, a day of month out of range) — the
 * editor's own validation is what reports that; here '' just means "nothing to
 * preview".
 */
export function presetToCron(preset: CadencePreset): string {
  const parsed = parseTime(preset.time);
  if (parsed === null) return '';
  const head = `${parsed.minute} ${parsed.hour}`;
  if (preset.mode === 'daily') return `${head} * * *`;
  if (preset.mode === 'weekly') {
    const weekdays = normalizeWeekdays(preset.weekdays);
    if (weekdays.length === 0) return '';
    return `${head} * * ${weekdays.join(',')}`;
  }
  if (!Number.isInteger(preset.day) || preset.day < 1 || preset.day > MONTH_DAY_MAX) return '';
  return `${head} ${preset.day} * *`;
}

/** A comma list of ascending, non-repeating cron weekday digits; else null. */
function parseWeekdayField(field: string): number[] | null {
  const parts = field.split(',');
  const weekdays: number[] = [];
  for (const part of parts) {
    if (!/^[0-6]$/.test(part)) return null;
    const value = Number(part);
    const previous = weekdays[weekdays.length - 1];
    if (previous !== undefined && value <= previous) return null;
    weekdays.push(value);
  }
  return weekdays;
}

/**
 * A cron expression → the preset that renders it, or null when it matches
 * none of the three preset shapes exactly (which selects Advanced).
 */
export function cronToPreset(expr: string): CadencePreset | null {
  const fields = expr.trim().split(/\s+/);
  if (fields.length !== 5) return null;
  const [minuteField, hourField, domField, monthField, dowField] = fields as [
    string,
    string,
    string,
    string,
    string,
  ];
  const minute = plainNumber(minuteField, 0, 59);
  const hour = plainNumber(hourField, 0, 23);
  if (minute === null || hour === null) return null;
  // Every preset fires in every month; a month restriction is Advanced.
  if (monthField !== '*') return null;
  const time = `${pad2(hour)}:${pad2(minute)}`;
  if (domField === '*' && dowField === '*') return { mode: 'daily', time };
  if (domField === '*') {
    const weekdays = parseWeekdayField(dowField);
    return weekdays === null ? null : { mode: 'weekly', time, weekdays };
  }
  if (dowField === '*') {
    const day = plainNumber(domField, 1, MONTH_DAY_MAX);
    return day === null ? null : { mode: 'monthly', time, day };
  }
  // Both a day of month AND weekdays: cron ORs them, which no preset means.
  return null;
}

/**
 * One-line human cadence for a list row: the preset read back in words when
 * the expression decomposes, else the raw cron verbatim — an Advanced cadence
 * has no shorter honest description than itself.
 */
export function cadenceSummary(expr: string): string {
  const preset = cronToPreset(expr);
  if (preset === null) return expr.trim();
  if (preset.mode === 'daily') return `Daily at ${preset.time}`;
  if (preset.mode === 'weekly') {
    // Listed in display order (Mon first), not the expression's cron order.
    const labels = WEEKDAYS.filter((day) => preset.weekdays.includes(day.value)).map(
      (day) => day.label,
    );
    return `Weekly on ${labels.join(', ')} at ${preset.time}`;
  }
  return `Monthly on day ${preset.day} at ${preset.time}`;
}
