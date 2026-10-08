// The New run page's rules (issue #66), as pure functions: the recent
// repositories list behind the pills (and its localStorage key), the repo
// picker's rows and filter, the Issues card (triage state, filters, counts,
// age), the issue action that rides as the run's first_message (issue #96),
// the AFK line, and the blockers shown at the composer. routes/NewRun.tsx and
// its components render what these return; nothing here touches the DOM
// (the two storage helpers guard themselves), so every rule is unit-tested
// in newRun.test.ts.
//
// The reference for copy and behavior is docs/reference/new-run-mockup.html.

import type { Repo } from '../api';
import type { CloneProgress } from '../stores/cloneProgress';
import { AFK_PAUSE_THRESHOLD } from './afk';
import { READY_LABEL } from './issues';
import { checksOf, checkState, fixHref, readinessState } from './readiness';
import { parseRemote } from './remoteUrl';
import { remoteLabel } from './repoName';

// --- Recent repositories ---

/**
 * The localStorage key behind the pills. It used to hold the one last-used
 * repo id as a bare string; it now holds a JSON array of ids, most recent
 * first (parseRecentRepos reads both).
 */
export const RECENT_REPOS_KEY = 'lab.last-repo';

/** How many ids are kept in storage; the pills show fewer (recentRepos). */
export const RECENT_REPOS_STORED_MAX = 10;

/** How many pills the row shows. */
export const RECENT_REPOS_PILLS = 4;

/**
 * Reads the stored value. The new format is a JSON array of ids; the old
 * format (what the page stored before the pills) is one bare id, which reads
 * as a list of one. Null, garbage and non-string entries are dropped; the
 * result has no duplicates and keeps the stored order. Never throws.
 */
export function parseRecentRepos(raw: string | null): string[] {
  if (raw === null) return [];
  const text = raw.trim();
  if (text === '') return [];
  let ids: unknown[];
  try {
    const parsed: unknown = JSON.parse(text);
    if (Array.isArray(parsed)) ids = parsed;
    else if (typeof parsed === 'string') ids = [parsed];
    // A bare id that happens to be valid JSON, such as "12345".
    else if (typeof parsed === 'number') ids = [text];
    else ids = [];
  } catch {
    // Not JSON: the old format, one bare id — unless it is a broken
    // array/object, which is garbage rather than an id.
    ids = text.startsWith('[') || text.startsWith('{') ? [] : [text];
  }
  const out: string[] = [];
  for (const id of ids) {
    if (typeof id === 'string' && id !== '' && !out.includes(id)) out.push(id);
  }
  return out;
}

/** The stored recent ids; an unavailable localStorage (private mode) reads as none. */
export function readRecentRepos(): string[] {
  try {
    return parseRecentRepos(localStorage.getItem(RECENT_REPOS_KEY));
  } catch {
    return [];
  }
}

/** Stores the recent ids. A failing localStorage is ignored: the in-memory list still works. */
export function writeRecentRepos(ids: string[]): void {
  try {
    localStorage.setItem(RECENT_REPOS_KEY, JSON.stringify(ids));
  } catch {
    // Private mode / storage disabled.
  }
}

/**
 * `id` moved (or added) to the front, any earlier copy removed, the list cut
 * to RECENT_REPOS_STORED_MAX. Pure: returns a new array.
 */
export function pushRecentRepo(ids: readonly string[], id: string): string[] {
  return [id, ...ids.filter((x) => x !== id)].slice(0, RECENT_REPOS_STORED_MAX);
}

/** Whether a run can start in the repo's clone: only a finished clone can. */
export function isStartable(repo: Repo): boolean {
  return repo.clone_status === 'ready';
}

/**
 * The pills: the repos named by `ids`, in that order, that still exist and are
 * startable, at most `max` (a stored id that is gone or un-ready is skipped).
 * A first visit — nothing stored, or nothing stored is usable — falls back to
 * the first startable repos in list order, so the row is never empty while a
 * startable repo exists. A partly usable stored list is NOT padded: it is
 * the operator's own history.
 */
export function recentRepos(
  repos: readonly Repo[],
  ids: readonly string[],
  max = RECENT_REPOS_PILLS,
): Repo[] {
  const byId = new Map(repos.map((repo) => [repo.id, repo]));
  const stored: Repo[] = [];
  for (const id of ids) {
    const repo = byId.get(id);
    if (repo !== undefined && isStartable(repo) && !stored.includes(repo)) stored.push(repo);
  }
  const list = stored.length > 0 ? stored : repos.filter(isStartable);
  return list.slice(0, Math.max(0, max));
}

/**
 * The repo the page opens on: the most recent usable one (the first pill).
 * Covers a stored id that was deleted or went un-ready: the next usable one
 * wins. When no repo is startable at all (a lone repo still cloning, or one
 * whose clone failed), the most recent existing repo, else the first listed,
 * is selected anyway, so the composer can show what blocks it (its cloning or
 * clone-failed banner) instead of an unexplained disabled field. null only
 * for an empty list.
 */
export function preselectedRepo(repos: readonly Repo[], ids: readonly string[]): Repo | null {
  const usable = recentRepos(repos, ids, 1)[0];
  if (usable !== undefined) return usable;
  for (const id of ids) {
    const repo = repos.find((r) => r.id === id);
    if (repo !== undefined) return repo;
  }
  return repos[0] ?? null;
}

// --- Repository picker ---

/**
 * The remote as one short display line, host plus path ("github.com/Cloonar/
 * coding-lab"), shown under/after a repo's name in the picker and matched by
 * its filter. '' for a remote that parses as none of the shapes git accepts.
 * (Named after the host because that is what the operator recognises; the path
 * rides along as it does on the repositories list.)
 */
export function repoHost(repo: Repo): string {
  return parseRemote(repo.remote_url) === null ? '' : remoteLabel(repo.remote_url);
}

/**
 * The dot on a pill or picker row: green when the summary's readiness roll-up
 * passes, red when it fails, hollow otherwise (pending, no report, or a repo
 * whose clone has not finished — the mockup shows those hollow too).
 */
export function readinessDot(repo: Repo): 'ok' | 'err' | 'pending' {
  if (!isStartable(repo)) return 'pending';
  const readiness = repo.summary?.readiness;
  if (readiness === undefined || readiness === null) return 'pending';
  const state = readinessState(readiness);
  return state === 'passing' ? 'ok' : state === 'failing' ? 'err' : 'pending';
}

/** The failing tracker check of a repo, if its readiness report has one. */
function failingTracker(repo: Repo) {
  return checksOf(repo.summary?.readiness).find(
    (check) => check.id === 'tracker' && checkState(check.state) === 'failing',
  );
}

/**
 * Status text of a picker row that is not plain ready: "clone failed",
 * "cloning 62%" (live percent from the clone-progress store) or "cloning…",
 * and "tracker failing" on a finished clone whose tracker check fails.
 * undefined for a row with nothing to say.
 */
export function repoRowStatus(repo: Repo, progress: CloneProgress | null): string | undefined {
  if (repo.clone_status === 'error') return 'clone failed';
  if (repo.clone_status === 'cloning') {
    const percent = progress?.percent;
    return percent !== null && percent !== undefined ? `cloning ${percent}%` : 'cloning…';
  }
  if (failingTracker(repo) !== undefined) return 'tracker failing';
  return undefined;
}

/**
 * Whether the picker shows the row disabled: exactly when the repo is not
 * startable (cloning, clone failed). A repo whose tracker check fails shows
 * its "tracker failing" status text but stays pickable: the issue says a
 * failing tracker "leaves the field enabled, because a manual run can still
 * start", and the composer's banner (composerBlockers) with its Fix link is
 * the only place to see and repair that — which needs the repo selected.
 * The mockup agrees (its startable() is the clone status alone).
 */
export function repoRowDisabled(repo: Repo): boolean {
  return !isStartable(repo);
}

/**
 * Narrows the picker as the operator types: a case-insensitive substring match
 * over the name and the remote's host-and-path line (repoHost). An empty or
 * whitespace-only query keeps every repo. Pure: returns a new array.
 */
export function filterRepos(repos: readonly Repo[], query: string): Repo[] {
  const q = query.trim().toLowerCase();
  if (q === '') return [...repos];
  return repos.filter(
    (repo) => repo.name.toLowerCase().includes(q) || repoHost(repo).toLowerCase().includes(q),
  );
}

// --- Issues ---

/** The one triage state an issue shows as a chip; `unlabeled` shows none. */
export type TriageState = 'needs-triage' | 'ready-for-agent' | 'needs-info' | 'unlabeled';

/**
 * The issue's triage state from its labels, in the mockup's precedence:
 * ready-for-agent, then needs-triage, then needs-info, else unlabeled. Other
 * labels (bug, enhancement…) do not count.
 */
export function triageState(labels: readonly string[]): TriageState {
  if (labels.includes(READY_LABEL)) return 'ready-for-agent';
  if (labels.includes('needs-triage')) return 'needs-triage';
  if (labels.includes('needs-info')) return 'needs-info';
  return 'unlabeled';
}

/**
 * The chip tint of a state: ready-for-agent is the run (green) tint,
 * needs-triage the notice (amber) tint, needs-info the idle tint, and an
 * unlabeled issue gets no chip (null).
 */
export function triageTint(state: TriageState): 'run' | 'notice' | 'idle' | null {
  switch (state) {
    case 'ready-for-agent':
      return 'run';
    case 'needs-triage':
      return 'notice';
    case 'needs-info':
      return 'idle';
    case 'unlabeled':
      return null;
  }
}

/** The issue picker's state chips: All plus the four triage states. */
export type IssueFilter = 'all' | TriageState;

/** The chips in the order the picker lists them. */
export const ISSUE_FILTERS: readonly IssueFilter[] = [
  'all',
  'needs-triage',
  'ready-for-agent',
  'needs-info',
  'unlabeled',
];

interface FilterableIssue {
  number: number;
  title: string;
  labels: readonly string[];
}

function matchesQuery(issue: FilterableIssue, query: string): boolean {
  const q = query.trim().toLowerCase().replace(/^#/, '').trim();
  if (q === '') return true;
  // Digits only: a number, matched as a prefix ("4" finds #4, #47, #402). It
  // does not also scan titles, or typing "1" would match nearly everything.
  if (/^\d+$/.test(q)) return String(issue.number).startsWith(q);
  // Otherwise words from the title: every word must occur, in any order.
  const title = issue.title.toLowerCase();
  return q.split(/\s+/).every((word) => title.includes(word));
}

/**
 * The issue picker's rows: those matching the text query (an issue number,
 * with or without a leading `#`, matched as a prefix; anything else as words
 * from the title — every word must occur, case-insensitive, in any order) and
 * the state chip. An empty query keeps every issue; 'all' keeps every state.
 * Pure: returns a new array in the input order.
 */
export function filterIssues<T extends FilterableIssue>(
  issues: readonly T[],
  query: string,
  state: IssueFilter,
): T[] {
  return issues.filter(
    (issue) =>
      (state === 'all' || triageState(issue.labels) === state) && matchesQuery(issue, query),
  );
}

/**
 * The count on each state chip. Like the mockup, the counts cover the whole
 * list and ignore the text query (the chips say what exists, the rows what
 * matches); pass `query` to count only the issues it matches. The picker
 * hides a zero-count chip except All.
 */
export function issueFilterCounts(
  issues: readonly FilterableIssue[],
  query = '',
): Record<IssueFilter, number> {
  const counts: Record<IssueFilter, number> = {
    all: 0,
    'needs-triage': 0,
    'ready-for-agent': 0,
    'needs-info': 0,
    unlabeled: 0,
  };
  for (const issue of issues) {
    if (!matchesQuery(issue, query)) continue;
    counts.all += 1;
    counts[triageState(issue.labels)] += 1;
  }
  return counts;
}

function createdMs(iso: string): number {
  const t = Date.parse(iso);
  return Number.isNaN(t) ? 0 : t;
}

/**
 * The newest open issues for the card: created_at descending, the higher
 * number first on a tie (an unparseable time sorts oldest), at most `max`.
 * Pure: returns a new array.
 */
export function newestIssues<T extends { number: number; created_at: string }>(
  issues: readonly T[],
  max = 4,
): T[] {
  return [...issues]
    .sort((a, b) => createdMs(b.created_at) - createdMs(a.created_at) || b.number - a.number)
    .slice(0, Math.max(0, max));
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * An issue's age in the compact form of the mockup, without "ago" (the action
 * sheet writes "opened 3 d ago"): "now" under a minute, "5 min", "3 h",
 * "3 d" up to 13 days, "7 wk" up to eight weeks, "2 mo", "1 y". A time ahead
 * of the clock (skew) reads "now"; an unparseable one reads ''. It does not
 * reuse relativeTime(), which words the same spans as "3 days ago".
 */
export function issueAge(createdAt: string, nowMs: number): string {
  const t = Date.parse(createdAt);
  if (Number.isNaN(t)) return '';
  const ago = nowMs - t;
  if (ago < MINUTE) return 'now';
  if (ago < HOUR) return `${Math.floor(ago / MINUTE)} min`;
  if (ago < DAY) return `${Math.floor(ago / HOUR)} h`;
  const days = Math.floor(ago / DAY);
  if (days < 14) return `${days} d`;
  if (days < 60) return `${Math.floor(days / 7)} wk`;
  if (days < 365) return `${Math.floor(days / 30)} mo`;
  return `${Math.floor(days / 365)} y`;
}

// --- Issue actions (the run's first_message, issue #96) ---

/** What the agent is asked to do with the tapped issue. */
export type IssueAction = 'triage' | 'implement' | 'discuss';

/** The action sheet's three rows, in order, with their one-line descriptions. */
export const ISSUE_ACTIONS: readonly {
  id: IssueAction;
  label: string;
  describe: (n: number) => string;
}[] = [
  {
    id: 'triage',
    label: 'Triage',
    describe: (n) =>
      `Runs /triage #${n}: the agent reads it, asks you what is missing and sets the label.`,
  },
  {
    id: 'implement',
    label: 'Implement',
    describe: (n) => `A run with #${n} as its brief, on a branch of its own.`,
  },
  {
    id: 'discuss',
    label: 'Discuss',
    describe: (n) => `Opens a chat with #${n} as context. Nothing happens until you type.`,
  },
];

function actionLabel(action: IssueAction): string {
  return ISSUE_ACTIONS.find((a) => a.id === action)?.label ?? action;
}

/** The action marked "Suggested": triage for needs-triage, implement for ready-for-agent, else discuss. */
export function suggestedAction(labels: readonly string[]): IssueAction {
  switch (triageState(labels)) {
    case 'needs-triage':
      return 'triage';
    case 'ready-for-agent':
      return 'implement';
    default:
      return 'discuss';
  }
}

/**
 * The run's first_message for an issue action. The action's fixed line, then
 * the typed text on its own line when there is any (trimmed; empty text adds
 * nothing, not even a trailing newline):
 * - triage: `/triage #47`
 * - implement: `Implement issue #47 "<title>". Read it with `labctl issue view 47` first; it is your brief.`
 * - discuss: `Let's discuss issue #47 "<title>". Read it with `labctl issue view 47`, then wait for my questions.`
 */
export function composeFirstMessage(
  action: IssueAction,
  issue: { number: number; title: string },
  text: string,
): string {
  const n = issue.number;
  let head: string;
  switch (action) {
    case 'triage':
      head = `/triage #${n}`;
      break;
    case 'implement':
      head = `Implement issue #${n} "${issue.title}". Read it with \`labctl issue view ${n}\` first; it is your brief.`;
      break;
    case 'discuss':
      head = `Let's discuss issue #${n} "${issue.title}". Read it with \`labctl issue view ${n}\`, then wait for my questions.`;
      break;
  }
  const typed = text.trim();
  return typed === '' ? head : `${head}\n${typed}`;
}

/** The run label an attached action defaults to: `triage-47`. */
export function defaultRunLabel(action: IssueAction, issueNumber: number): string {
  return `${action}-${issueNumber}`;
}

/** The part of an attachment the label, the Send button and the placeholder need. */
export interface AttachmentRef {
  action: IssueAction;
  number: number;
}

/** The run's label: what was typed (trimmed) wins, else the default of the attached action, else ''. */
export function runLabelFor(typed: string, attachment: AttachmentRef | null): string {
  const label = typed.trim();
  if (label !== '') return label;
  return attachment === null ? '' : defaultRunLabel(attachment.action, attachment.number);
}

/** The attachment chip's text: `Triage #47 · <title>`. */
export function attachmentText(
  action: IssueAction,
  issue: { number: number; title: string },
): string {
  return `${actionLabel(action)} #${issue.number} · ${issue.title}`;
}

/** The Send button's label: `Start: Triage #47` with an attachment, else `Start run`. */
export function sendLabel(attachment: AttachmentRef | null): string {
  return attachment === null
    ? 'Start run'
    : `Start: ${actionLabel(attachment.action)} #${attachment.number}`;
}

/**
 * The textarea's placeholder: `Describe a task for <repo>…` with nothing
 * attached; Discuss asks what to discuss; Triage and Implement take optional
 * extra guidance.
 */
export function composerPlaceholder(attachment: AttachmentRef | null, repoName: string): string {
  if (attachment === null) return `Describe a task for ${repoName}…`;
  if (attachment.action === 'discuss') {
    return `Say what you want to discuss about #${attachment.number}…`;
  }
  return 'Anything the agent should know? (optional)';
}

// --- AFK line ---

export interface AFKLineInput {
  /** The repo's Auto switch. */
  auto: boolean;
  /** Paused after AFK_PAUSE_THRESHOLD consecutive failures (wins over `auto`). */
  paused: boolean;
  /** The claimable count; null = not known yet (the count segment is left out). */
  ready: number | null;
  /** Live AFK runs in the repo. */
  liveAFK: number;
}

/**
 * The Issues card's one AFK line, and the button that goes with it:
 * - Auto on: `Auto on · 3 ready · 1 AFK run live · next claim when a slot frees`
 * - Auto off: `Auto off · 3 ready` and a Run one button ('run-one')
 * - paused: `AFK paused after 3 failed runs · 3 ready` and a Reset button ('reset')
 * The live segment is left out at 0 ("2 AFK runs live" otherwise); an unknown
 * ready count leaves out its segment.
 */
export function afkLine(input: AFKLineInput): { text: string; action: 'run-one' | 'reset' | null } {
  const ready = input.ready === null ? [] : [`${input.ready} ready`];
  if (input.paused) {
    return {
      text: [`AFK paused after ${AFK_PAUSE_THRESHOLD} failed runs`, ...ready].join(' · '),
      action: 'reset',
    };
  }
  const live =
    input.liveAFK > 0 ? [`${input.liveAFK} AFK run${input.liveAFK === 1 ? '' : 's'} live`] : [];
  const parts = [`Auto ${input.auto ? 'on' : 'off'}`, ...ready, ...live];
  if (input.auto) parts.push('next claim when a slot frees');
  return { text: parts.join(' · '), action: input.auto ? null : 'run-one' };
}

// --- Blockers at the composer ---

/** The host-Runner warning, shown above the field whenever the effective Runner is `host`. */
export const HOST_RUNNER_WARNING = 'Runs on the host, unsandboxed, with full host access.';

/**
 * One banner directly above the field. `disablesField` says whether the
 * textarea and Send are disabled while it shows; `fixHref`/`fixLabel` is the
 * remedy link, `retryClone` asks for the existing clone-retry button instead.
 */
export type Blocker = {
  kind: 'cloning' | 'clone-failed' | 'logged-out' | 'tracker';
  variant: 'notice' | 'error' | 'warning';
  message: string;
  disablesField: boolean;
  fixHref?: string;
  fixLabel?: string;
  retryClone?: boolean;
};

const VARIANT_RANK: Record<Blocker['variant'], number> = { error: 0, warning: 1, notice: 2 };

/**
 * What blocks (or warns about) a run at the composer, most severe first
 * (errors, then the warning, then notices):
 * - clone failed: error with the clone error and a retry; disables the field
 * - agent logged out: error `<Agent> is logged out.` with Reconnect →
 *   /credentials; disables the field
 * - tracker check failing: warning with the check's detail and, when the check
 *   names a fix, Fix → the settings field it names; the field STAYS enabled,
 *   because a manual run can still start without the tracker
 * - cloning: notice with the live percent; disables the field
 * A repo that can run shows nothing. The tracker check is only looked at on a
 * finished clone (before that the clone is the one thing to wait for). With no
 * repo selected only the logged-out blocker can apply.
 */
export function composerBlockers(input: {
  repo: Repo | null;
  progress: CloneProgress | null;
  loggedOut: boolean;
  providerName: string;
}): Blocker[] {
  const { repo, progress, loggedOut, providerName } = input;
  const blockers: Blocker[] = [];

  if (repo?.clone_status === 'error') {
    const error = repo.clone_error?.trim();
    blockers.push({
      kind: 'clone-failed',
      variant: 'error',
      message: error !== undefined && error !== '' ? error : 'The clone failed.',
      disablesField: true,
      retryClone: true,
    });
  }

  if (loggedOut) {
    blockers.push({
      kind: 'logged-out',
      variant: 'error',
      message: `${providerName} is logged out.`,
      disablesField: true,
      fixHref: '/credentials',
      fixLabel: 'Reconnect',
    });
  }

  if (repo !== null && isStartable(repo)) {
    const check = failingTracker(repo);
    if (check !== undefined) {
      const detail = check.detail.trim();
      blockers.push({
        kind: 'tracker',
        variant: 'warning',
        message: `${detail !== '' ? detail : 'The tracker cannot be read.'} A run can still start.`,
        disablesField: false,
        ...(check.fix !== undefined
          ? { fixHref: fixHref(repo.id, check.fix), fixLabel: 'Fix' }
          : {}),
      });
    }
  }

  if (repo?.clone_status === 'cloning') {
    const percent = progress?.percent;
    blockers.push({
      kind: 'cloning',
      variant: 'notice',
      message:
        percent !== null && percent !== undefined
          ? `Cloning ${percent}%. Runs can start when the clone finishes.`
          : 'Cloning… Runs can start when the clone finishes.',
      disablesField: true,
    });
  }

  // Array.prototype.sort is stable: equal ranks keep the order above.
  return blockers.sort((a, b) => VARIANT_RANK[a.variant] - VARIANT_RANK[b.variant]);
}

/** Whether any blocker disables the field (the textarea and Send). */
export function fieldDisabled(blockers: Blocker[]): boolean {
  return blockers.some((blocker) => blocker.disablesField);
}
