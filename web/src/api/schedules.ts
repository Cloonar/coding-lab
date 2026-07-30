import { request } from './core';

// --- Schedules (issue #247 / ADR-0062) ---

/**
 * A per-repo Schedule: a cadence (cron), the freeform prompt, the selected
 * flow keys, and the per-Schedule overrides — plus its own three-strikes
 * state. Every key is always present on the wire; nullable columns arrive as
 * JSON null, so a cleared override is never confused with an absent key.
 */
export interface Schedule {
  id: string;
  repo_id: string;
  name: string;
  /** Five-field cron, server-local time. lib/cronPreset decomposes it. */
  cadence: string;
  prompt: string;
  /**
   * Selected flow keys, always in CATALOG order (the server normalizes on
   * write) — the same order a firing appends their instruction blocks in.
   */
  flows: string[];
  enabled: boolean;
  /** null = inherit the layer below (30 minutes for the budget). */
  budget_minutes: number | null;
  model: string | null;
  effort: string | null;
  provider: string | null;
  /**
   * Read-only here: only the engine strikes and only the re-enable endpoint
   * clears. A PATCH carrying either key is a 400 by design.
   */
  consecutive_failures: number;
  paused: boolean;
  /** null until the first firing. */
  last_fired_at: string | null;
  created_at: string;
  updated_at: string;
}

/** POST body: name + cadence are required, everything else optional. */
export interface ScheduleCreate {
  name: string;
  cadence: string;
  prompt?: string;
  flows?: string[];
  /** Omitted defaults to true server-side. */
  enabled?: boolean;
  budget_minutes?: number | null;
  model?: string | null;
  effort?: string | null;
  provider?: string | null;
}

/**
 * PATCH body: partial, and only the changed fields ride it. An empty prompt is
 * a legal clear; null clears a nullable override ('' clears it too). `paused`
 * and `consecutive_failures` are rejected as unknown fields — re-enabling goes
 * through its own endpoint.
 */
export interface SchedulePatch {
  name?: string;
  cadence?: string;
  prompt?: string;
  flows?: string[] | null;
  enabled?: boolean;
  budget_minutes?: number | null;
  model?: string | null;
  effort?: string | null;
  provider?: string | null;
}

/** One built-in flow: a routing instruction block, versioned with the binary. */
export interface ScheduleFlow {
  key: string;
  label: string;
  description: string;
}

/**
 * The server-rendered cron preview. Always a 200, valid or not: an invalid
 * expression is normal while the operator is still typing one, so the parse
 * verdict rides the body instead of the status.
 */
export interface CronPreview {
  expr: string;
  valid: boolean;
  /** The parser's own message when valid is false; null otherwise. */
  error: string | null;
  /** RFC3339 firings, null when invalid. */
  next: string[] | null;
  /** The same firings rendered server-local, e.g. "Mon 2026-08-03 06:00". */
  next_display: string[] | null;
}

/** GET /repos/{id}/schedules — ordered by name. */
export async function listRepoSchedules(repoID: string): Promise<Schedule[]> {
  const res = await request<{ schedules: Schedule[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/schedules`,
  );
  return res.schedules;
}

/** POST /repos/{id}/schedules -> 201; 400 on validation, 409 on a name clash. */
export function createRepoSchedule(repoID: string, body: ScheduleCreate): Promise<Schedule> {
  return request<Schedule>('POST', `/repos/${encodeURIComponent(repoID)}/schedules`, body);
}

/** PATCH /repos/{id}/schedules/{sid} — send only the fields that changed. */
export function patchRepoSchedule(
  repoID: string,
  scheduleID: string,
  patch: SchedulePatch,
): Promise<Schedule> {
  return request<Schedule>(
    'PATCH',
    `/repos/${encodeURIComponent(repoID)}/schedules/${encodeURIComponent(scheduleID)}`,
    patch,
  );
}

/** DELETE /repos/{id}/schedules/{sid} -> 204. A live run outlives its Schedule. */
export function deleteRepoSchedule(repoID: string, scheduleID: string): Promise<void> {
  return request<void>(
    'DELETE',
    `/repos/${encodeURIComponent(repoID)}/schedules/${encodeURIComponent(scheduleID)}`,
  );
}

/**
 * POST /repos/{id}/schedules/{sid}/reenable -> 200 with the fresh row. The
 * only path out of a three-strikes pause; a no-op on a Schedule that is not
 * paused.
 */
export function reenableRepoSchedule(repoID: string, scheduleID: string): Promise<Schedule> {
  return request<Schedule>(
    'POST',
    `/repos/${encodeURIComponent(repoID)}/schedules/${encodeURIComponent(scheduleID)}/reenable`,
  );
}

/** GET /schedule-flows — the built-in catalog, in catalog order. */
export async function listScheduleFlows(): Promise<ScheduleFlow[]> {
  const res = await request<{ flows: ScheduleFlow[] }>('GET', '/schedule-flows');
  return res.flows;
}

/** GET /cron/preview?expr=… — the upcoming firings, or the parser's refusal. */
export function previewCron(expr: string): Promise<CronPreview> {
  return request<CronPreview>('GET', `/cron/preview?expr=${encodeURIComponent(expr)}`);
}
