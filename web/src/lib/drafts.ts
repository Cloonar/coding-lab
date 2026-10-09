// Unsent composer drafts in localStorage (issue #92): the chat reply box per
// run and the whole New run composition survive leaving the page, a reload and
// a backgrounded phone browser, until they are sent or started. One helper for
// both pages, after the recent-repos helper's pattern (lib/newRun.ts): every
// storage access in try/catch — a failing localStorage (private mode, storage
// disabled, quota) degrades to no persistence, never to a broken page — and
// each entry a JSON object `{ v, at }` stamped on write. Reads drop entries
// older than DRAFT_MAX_AGE_MS, so drafts for runs that no longer exist do not
// pile up. Drafts clear when the thing is sent or started; they are never
// defaults for the next one (ADR-0030).

/** Every draft key starts with this; the read-time sweep only touches these. */
export const DRAFT_PREFIX = 'lab.draft.';

/** The one New run composition. */
export const NEW_RUN_DRAFT_KEY = `${DRAFT_PREFIX}new-run`;

/** A run's chat reply box. */
export function chatDraftKey(runID: string): string {
  return `${DRAFT_PREFIX}chat.${runID}`;
}

/** An entry older than this (about seven days) is dropped on read. */
export const DRAFT_MAX_AGE_MS = 7 * 24 * 60 * 60 * 1000;

/** The stored shape. */
interface StoredDraft {
  v: unknown;
  at: number;
}

/**
 * The stored entry's value, or null for a missing, garbage, non-JSON or stale
 * entry (`at` older than maxAge, or in the future beyond it). Pure; never
 * throws. The value itself is unchecked — readDraft's `accept` shapes it.
 */
export function parseDraft(
  raw: string | null,
  now: number,
  maxAge: number = DRAFT_MAX_AGE_MS,
): { value: unknown } | null {
  if (raw === null) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return null;
  const entry = parsed as Partial<StoredDraft>;
  if (typeof entry.at !== 'number' || !Number.isFinite(entry.at)) return null;
  if (!('v' in entry) || entry.v === undefined) return null;
  if (Math.abs(now - entry.at) > maxAge) return null;
  return { value: entry.v };
}

/**
 * The draft stored under `key`, shaped by `accept` (null rejects it), else
 * null. Reading also sweeps every stale or unreadable draft entry — this one
 * included — out of storage. An unavailable localStorage reads as no draft.
 */
export function readDraft<T>(key: string, accept: (value: unknown) => T | null): T | null {
  const now = Date.now();
  sweepDrafts(now);
  let raw: string | null;
  try {
    raw = localStorage.getItem(key);
  } catch {
    return null;
  }
  const entry = parseDraft(raw, now);
  if (entry === null) return null;
  try {
    return accept(entry.value);
  } catch {
    return null;
  }
}

/** Stores `value` under `key`, stamped now. A failing localStorage is ignored. */
export function writeDraft(key: string, value: unknown): void {
  try {
    const entry: StoredDraft = { v: value, at: Date.now() };
    localStorage.setItem(key, JSON.stringify(entry));
  } catch {
    // Private mode / storage disabled / quota: the in-memory draft still works.
  }
}

/** Removes the draft under `key`. A failing localStorage is ignored. */
export function clearDraft(key: string): void {
  try {
    localStorage.removeItem(key);
  } catch {
    // Nothing stored that could be removed.
  }
}

/** Removes every draft entry that would not parse as fresh at `now`. */
function sweepDrafts(now: number): void {
  try {
    const stale: string[] = [];
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i);
      if (key === null || !key.startsWith(DRAFT_PREFIX)) continue;
      if (parseDraft(localStorage.getItem(key), now) === null) stale.push(key);
    }
    for (const key of stale) localStorage.removeItem(key);
  } catch {
    // Storage unavailable: nothing to sweep.
  }
}
