import { request } from './core';
import type { ConversationState } from './runs';

// --- Embedded chat (issue #7 / ADR-0016) ---

export type MessageKind = 'text' | 'tool' | 'dialog' | 'lifecycle';

/** Rich view union (issue #146): produced server-side by each provider's chat
 *  mapper; the client renders purely by kind and stays provider-blind. Absent
 *  view → raw input/output fallback. */
export type ToolView =
  | { kind: 'diff'; path: string; text: string }
  | { kind: 'command'; command: string }
  | { kind: 'write'; path: string; text: string }
  | { kind: 'read'; path: string; text: string };

export interface ToolInfo {
  name: string;
  title: string;
  input?: string;
  output?: string;
  status: 'running' | 'ok' | 'error';
  /** Rich render hint (issue #146). The server truncates `view.text` (and
   *  `output`) at ~20KB with an in-text marker line, so the client renders it
   *  as-is — no client-side truncation. */
  view?: ToolView;
}

export interface DialogOption {
  label: string;
  description?: string;
  is_other?: boolean;
}

/** Dialog kinds (issue #51 decision 3). "approval" = generic yes/no approval. */
export type DialogKind = 'question' | 'plan' | 'approval';

/**
 * One question of a multi-question dialog. The synthesized free-text "Other"
 * row (is_other) IS the "free text allowed" signal, same convention as the
 * flat single-question shape.
 */
export interface Question {
  /** Short chip label rendered above the question text. */
  header?: string;
  text: string;
  options: DialogOption[];
  multi_select?: boolean;
}

/**
 * The recorded resolution of an answered dialog (provider.go DialogOutcome).
 * Its PRESENCE on a Dialog is the answered signal: every field is omitempty,
 * so a plan rejected without typed feedback serializes as `"outcome": {}` —
 * never key "answered" on the inner fields.
 */
export interface DialogOutcome {
  /** Resolved without an answer: denial, interrupt, unattended timeout, or unreadable result. */
  dismissed?: boolean;
  /** Question kind: one per question, dialog order. */
  results?: QuestionResult[];
  /** Plan kind: the plan was approved. */
  approved?: boolean;
  /** Plan kind: rejection feedback text, if typed. */
  feedback?: string;
}

/**
 * One question's resolved answer for display (provider.go QuestionResult).
 * Neither `chosen` nor `other_text` means that question got no recorded
 * answer; a multi-select result can carry BOTH.
 */
export interface QuestionResult {
  /** The question text. */
  question: string;
  /** Chosen listed option label(s), recorded toggle order. */
  chosen?: string[];
  /** Free-text answer (the synthesized Other row). */
  other_text?: string;
}

export interface Dialog {
  tool_id: string;
  dialog_kind: DialogKind;
  /** For the plan kind, the FULL plan markdown (issue #56 — no length cap). */
  prompt: string;
  options?: DialogOption[];
  multi?: boolean;
  /** Present ONLY for multi-question dialogs (issue #51 decision 3). */
  questions?: Question[];
  answerable: boolean;
  /**
   * Resolution of an answered dialog (issue #56 decision 3): non-null MEANS
   * answered; absent means still pending. Answered dialogs keep their full
   * options/questions; the live pending_dialog field never carries one.
   */
  outcome?: DialogOutcome;
}

/** One entry of the universal chat schema. */
export interface ChatMessage {
  seq: number;
  kind: MessageKind;
  role?: 'user' | 'assistant';
  time?: string;
  text?: string;
  thinking?: boolean;
  error?: boolean;
  tool?: ToolInfo;
  dialog?: Dialog;
  /**
   * Server-computed FNV-64a hash over the rendered message (issue #175) — the
   * identity-stable-merge key. Identical rendered content yields an identical
   * hash; any change (a tool status flip, output growth, a dialog outcome)
   * yields a different one, so the chat's merge keeps the previous message
   * OBJECT — and its settled DOM — when a refetch redelivers unchanged
   * content. Optional because older servers omit it; an absent hash falls
   * back to later-window-wins.
   */
  content_hash?: string;
}

/** transcript: whether the file is readable yet. */
export type TranscriptStatus = 'available' | 'locating' | 'gone';

/**
 * Raw context-occupancy token counts (issue #243 / ADR-0061): the adapter's
 * used tokens over the model's raw context limit. The provider ships raw counts
 * only — the UI does the division and formats the `62%` / `127k of 200k tokens`
 * strings — and sends null when it can't say (no assistant turn yet, unknown
 * model).
 */
export interface ContextUsage {
  used: number;
  limit: number;
}

export interface MessagesResponse {
  messages: ChatMessage[];
  state: ConversationState;
  cursor: number;
  has_more: boolean;
  /**
   * The run's live interactive dialog from the provider's live-signals spool
   * (ADR-0020), null when none is pending. Top-level, NOT a message in the
   * stream — the agent CLI never flushes a pending tool_use to the transcript,
   * so this is the only live source. Present alongside state:"question".
   * Prefer it over the messages-scan, which stays a dormant fallback for a
   * future flushed tool_use. Optional on the client (older responses omit
   * it) — treat absent as null.
   */
  pending_dialog?: Dialog | null;
  transcript: TranscriptStatus;
  /**
   * Opaque identity of the located transcript (issue #34). It changes when the
   * run's transcript rotates — a /clear or /rewind starts a new sessionId →
   * transcript file and the backend re-points this same run at it. The chat
   * view keys its stream reset on a change here (the fresh transcript restarts
   * seq at 1, which would otherwise collide with the accumulated messages).
   * Empty/absent while locating or gone; older responses omit it → treat as
   * unchanged.
   */
  transcript_id?: string;
  /**
   * Adapter-composed context-occupancy meter (issue #243 / ADR-0061): raw
   * used/limit token counts the header renders as the model chip's `· 62%`
   * suffix with a `127k of 200k tokens` title, null when the provider can't
   * compute it (no assistant turn yet, unknown model). Optional on the client
   * (older responses omit it) — treat absent as null, exactly like
   * pending_dialog above.
   */
  context_usage?: ContextUsage | null;
}

/** A window of an instance's messages. after=<seq> tails; before=<seq> pages up. */
export function getRunMessages(
  id: string,
  opts: { after?: number; before?: number; limit?: number } = {},
): Promise<MessagesResponse> {
  const params = new URLSearchParams();
  if (opts.after !== undefined) params.set('after', String(opts.after));
  if (opts.before !== undefined) params.set('before', String(opts.before));
  if (opts.limit !== undefined) params.set('limit', String(opts.limit));
  const query = params.toString();
  return request<MessagesResponse>(
    'GET',
    `/runs/${encodeURIComponent(id)}/messages` + (query === '' ? '' : `?${query}`),
  );
}

/**
 * The reply endpoint's 200 response body (issue #149): an operator-facing
 * informational `notice` — e.g. "already up to date with origin/main", or
 * "base not pulled (agent is busy); the conversation was cleared on the old
 * base" — never an error (errors keep the existing `{"error"}` envelope and
 * a non-2xx status). The common 204 (no notice) still parses to undefined.
 */
export interface ReplyResult {
  notice?: string;
}

/** Free-text reply. 409 for an ended run or a pending dialog; 400 for empty.
 *  200 with a `notice` body is informational (issue #149), not an error. */
export function replyRun(id: string, text: string): Promise<ReplyResult | undefined> {
  return request<ReplyResult | undefined>('POST', `/runs/${encodeURIComponent(id)}/reply`, {
    text,
  });
}

/**
 * One question's answer inside a multi-question dialog. Association is
 * POSITIONAL — answers[i] answers questions[i]; there is no question-index
 * field. Each entry mirrors the flat single-question shape (provider.go
 * QuestionAnswer is authoritative): a single-select question sends `index` =
 * the chosen OPTION index (`other_text` rides along only when that row is the
 * synthesized is_other row); a multi_select question sends `selected` = the
 * toggled REAL option indices ascending (`index` absent) plus `other_text`
 * when the Other row is toggled — the is_other row's index never appears in
 * `selected` (its free text IS its toggle; the adapter pastes it onto the
 * TUI's "Type something" row). `other_text` alone with an empty `selected`
 * is a valid multi-select answer.
 */
export interface QuestionAnswer {
  index?: number;
  selected?: number[];
  other_text?: string;
}

export interface AnswerRequest {
  tool_id: string;
  index?: number;
  selected?: number[];
  other_text?: string;
  /**
   * Per-question answers, positionally aligned with dialog.questions; when
   * non-empty the flat fields are ignored.
   */
  answers?: QuestionAnswer[];
}

export function answerRun(id: string, req: AnswerRequest): Promise<void> {
  return request<void>('POST', `/runs/${encodeURIComponent(id)}/answer`, req);
}

export function interruptRun(id: string): Promise<void> {
  return request<void>('POST', `/runs/${encodeURIComponent(id)}/interrupt`);
}

// --- Slash-command catalog (issue #51 decision 5) ---

/**
 * One chat-safe slash command of the run's provider catalog. `name` comes
 * WITHOUT the leading slash; `source` is builtin|project|user; `role` is the
 * optional semantic tag (only "clear" defined for now) the UI may bind an
 * affordance to. The server curates out non-chat-safe entries, but the flag
 * is still serialized.
 */
export interface RunCommand {
  name: string;
  description?: string;
  arg_hint?: string;
  source: string;
  role?: string;
  chat_safe: boolean;
}

/** The run's slash-command catalog (worktree-dependent, server-cached ~30s). */
export async function fetchRunCommands(id: string): Promise<RunCommand[]> {
  const res = await request<{ commands: RunCommand[] }>(
    'GET',
    `/runs/${encodeURIComponent(id)}/commands`,
  );
  return res.commands;
}
