import { request } from './core';
import type { TrackerBinding } from './repos';

// --- M4: issues, comments, labels, ready queue ---

/** Persisted issue state. The list filter adds the synthetic "all". */
export type IssueState = 'open' | 'closed';
export type IssueStateFilter = 'open' | 'closed' | 'all';

/** One comment as the issue-detail endpoint serializes it (author + time). */
export interface IssueComment {
  author: string;
  body: string;
  created_at: string;
}

/** List-row shape: label names and a comment count, but no comment bodies. */
export interface IssueSummary {
  number: number;
  title: string;
  body: string;
  state: IssueState;
  labels: string[];
  comments_count: number;
  created_at: string;
  updated_at: string;
}

/** Full issue: label names plus the loaded comments (detail view). */
export interface IssueDetail {
  number: number;
  title: string;
  body: string;
  state: IssueState;
  labels: string[];
  comments: IssueComment[];
  created_at: string;
  updated_at: string;
}

/**
 * GET /repos/{id}/issues. `binding` is authoritative for the UI's
 * mutation gate: only a builtin tracker lets lab create/edit issues.
 */
export interface IssuesResponse {
  binding: TrackerBinding;
  issues: IssueSummary[];
}

export interface CreateIssueRequest {
  title: string;
  body?: string;
  labels?: string[];
}

export interface IssuePatch {
  title?: string;
  body?: string;
  state?: IssueState;
}

/** A repo label: the five seeded triage labels plus any custom ones. */
export interface Label {
  id: string;
  name: string;
  color: string;
  description: string;
}

export interface CreateLabelRequest {
  name: string;
  color?: string;
  description?: string;
}

export interface LabelPatch {
  name?: string;
  color?: string;
  description?: string;
}

/** Issue list, filtered by state server-side (open|closed|all). */
export function listIssues(
  repoID: string,
  state: IssueStateFilter = 'open',
): Promise<IssuesResponse> {
  return request<IssuesResponse>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/issues?state=${encodeURIComponent(state)}`,
  );
}

/** The ready-for-agent queue (Tracker.ReadyIssues, either backend). */
export async function listReadyIssues(repoID: string): Promise<IssueSummary[]> {
  const res = await request<{ issues: IssueSummary[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/ready`,
  );
  return res.issues;
}

/** Full issue with its comments loaded. */
export function getIssue(repoID: string, number: number): Promise<IssueDetail> {
  return request<IssueDetail>('GET', `/repos/${encodeURIComponent(repoID)}/issues/${number}`);
}

/** Builtin only — 409 {"error"} on a forge-bound repo. */
export function createIssue(repoID: string, req: CreateIssueRequest): Promise<IssueDetail> {
  return request<IssueDetail>('POST', `/repos/${encodeURIComponent(repoID)}/issues`, req);
}

/**
 * Sends only the given fields. `title`/`body` patch on either binding — the
 * server routes them through the tracker seam. `state` is builtin-only; a
 * forge-bound repo answers 400.
 */
export function updateIssue(
  repoID: string,
  number: number,
  patch: IssuePatch,
): Promise<IssueDetail> {
  return request<IssueDetail>(
    'PATCH',
    `/repos/${encodeURIComponent(repoID)}/issues/${number}`,
    patch,
  );
}

/** Builtin only. The comment is attributed to "operator" server-side. */
export function createIssueComment(
  repoID: string,
  number: number,
  body: string,
): Promise<IssueComment> {
  return request<IssueComment>(
    'POST',
    `/repos/${encodeURIComponent(repoID)}/issues/${number}/comments`,
    { body },
  );
}

/** Builtin only. Replaces the issue's label set; unknown name → 400. */
export function setIssueLabels(
  repoID: string,
  number: number,
  labels: string[],
): Promise<IssueDetail> {
  return request<IssueDetail>(
    'PUT',
    `/repos/${encodeURIComponent(repoID)}/issues/${number}/labels`,
    { labels },
  );
}

/** Builtin repos: the repo's label set (id, name, color, description). */
export async function listLabels(repoID: string): Promise<Label[]> {
  const res = await request<{ labels: Label[] }>(
    'GET',
    `/repos/${encodeURIComponent(repoID)}/labels`,
  );
  return res.labels;
}

export function createLabel(repoID: string, req: CreateLabelRequest): Promise<Label> {
  return request<Label>('POST', `/repos/${encodeURIComponent(repoID)}/labels`, req);
}

export function updateLabel(repoID: string, labelID: string, patch: LabelPatch): Promise<Label> {
  return request<Label>(
    'PATCH',
    `/repos/${encodeURIComponent(repoID)}/labels/${encodeURIComponent(labelID)}`,
    patch,
  );
}

/** 409s with the server's message when the name collides within the repo. */
export function deleteLabel(repoID: string, labelID: string): Promise<void> {
  return request<void>(
    'DELETE',
    `/repos/${encodeURIComponent(repoID)}/labels/${encodeURIComponent(labelID)}`,
  );
}
