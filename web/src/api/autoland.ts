import { request } from './core';

// --- Autoland: human re-arm of an escalated PR (issue #188) ---

/**
 * POST .../rearm's response: just enough for the caller to confirm the
 * gesture landed (repo, pull, and the moment it took effect) without a
 * follow-up fetch.
 */
export interface AutolandRearmResponse {
  repo_id: string;
  pull_number: number;
  rearmed_at: string;
}

/**
 * POST /repos/{id}/autoland/pulls/{pull}/rearm: supersedes escalation
 * terminality for this PR and zeroes its fix/escalate attempt budgets in one
 * atomic move — the poller sees the PR again next pass exactly as it would a
 * rejected-at-zero-attempts PR. Idempotent: re-arming a PR that was never
 * escalated, or one already re-armed, is a legal no-op that just moves the
 * re-arm moment forward — never a 409. 404 unknown repo, 400 bad pull number.
 */
export function rearmPull(repoID: string, pull: number): Promise<AutolandRearmResponse> {
  return request<AutolandRearmResponse>(
    'POST',
    `/repos/${encodeURIComponent(repoID)}/autoland/pulls/${pull}/rearm`,
  );
}
