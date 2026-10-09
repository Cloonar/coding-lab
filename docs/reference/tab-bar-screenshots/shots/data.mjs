// The sample lab the tab bar screenshots show: the six live runs and the ended runs of the reference
// mockup (docs/reference/tab-bar-mockup.html), on the fifteen repositories of the repo-settings
// screenshots (reused from their data.mjs, so the Repos tab shows the same lab). Agents and models
// carry placeholder names, never a real vendor's. Times are relative to now, so "18m" stays true
// whenever the script runs.
export {
  credentials,
  flows,
  globalSettings,
  imports,
  inherited,
  issues,
  parked,
  providers,
  repos,
  schedules,
  secrets,
  sshTargets,
} from '../../repo-settings-screenshots/shots/data.mjs';

const NOW = Date.now();
const MIN = 60e3;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const ago = (ms) => new Date(NOW - ms).toISOString();
const left = (ms) => new Date(NOW + ms).toISOString();

/** Milliseconds since local midnight: "today" rows stay on today whenever the script runs. */
const SINCE_MIDNIGHT = NOW - new Date(new Date(NOW).setHours(0, 0, 0, 0)).getTime();
const today = (ms) => ago(Math.min(ms, Math.max(SINCE_MIDNIGHT - 5 * MIN, 0)));
/** A time on the local day `days` back, at `hour`:`minute`. */
function dayAt(days, hour, minute = 0) {
  const d = new Date(NOW);
  d.setDate(d.getDate() - days);
  d.setHours(hour, minute, 0, 0);
  return d.toISOString();
}

// --- live runs (GET /instances) -----------------------------------------------------------------

function instance(id, repoName, title, over = {}) {
  return {
    id,
    repo_id: repoName,
    repo_name: repoName,
    kind: 'manual',
    provider: 'agent-a',
    issue_number: null,
    pull_number: null,
    branch: 'lab/20261009-0912',
    worktree_path: `/srv/lab/worktrees/${repoName}-${id}`,
    session_name: `${repoName}~operator-20261009-0912`, // an AFK run's is "<repo>~afk-<N>"
    title,
    model: 'model-large',
    effort: 'high',
    remote: false,
    deep_link_url: null,
    started_at: ago(20 * MIN),
    budget_deadline: null,
    ended_at: null,
    outcome: 'active',
    failure_reason: null,
    live: true,
    connecting: false,
    state: 'working',
    ...over,
  };
}

// The mockup's six: two that need you, two working (one AFK with its budget), two idle (one 3 behind).
export const instances = [
  instance('run_1', 'coding-lab', 'Record the Runner on the run', {
    state: 'question',
    started_at: ago(2 * MIN),
  }),
  instance('run_2', 'coding-lab', 'Warpgate dashboard exposure', {
    state: 'needs_input',
    branch: 'lab/20261009-0831',
    session_name: 'coding-lab~operator-20261009-0831',
    started_at: ago(18 * MIN),
  }),
  instance('run_3', 'cloonar-nixos', 'OneCLI refusals at eval time', {
    branch: 'lab/20261009-0905',
    session_name: 'cloonar-nixos~operator-20261009-0905',
    started_at: ago(1 * MIN),
  }),
  instance('run_4', 'coding-lab', 'afk/56 Record the Runner', {
    kind: 'afk_auto',
    issue_number: 56,
    branch: 'afk/56',
    session_name: 'coding-lab~afk-56',
    started_at: ago(8 * MIN),
    budget_deadline: left(72.5 * MIN), // "~1h 12m left"
  }),
  instance('run_5', 'coding-lab', 'Server container quickstart', {
    state: 'idle',
    branch: 'lab/20261008-1740',
    session_name: 'coding-lab~operator-20261008-1740',
    started_at: ago(HOUR + 5 * MIN),
    commits_behind: 3,
  }),
  instance('run_6', 'cloonar-nixos', 'Bump flake inputs', {
    state: 'idle',
    effort: 'medium',
    branch: 'lab/20261008-1512',
    session_name: 'cloonar-nixos~operator-20261008-1512',
    started_at: ago(3 * HOUR + 10 * MIN),
  }),
];

// --- ended runs (GET /runs) ---------------------------------------------------------------------

/** A Run row: an instance without the liveness fields GET /instances adds. */
const asRun = ({ repo_name: _name, live: _live, connecting: _conn, state: _state, ...run }) => run;
const run = (id, repoID, over) => ({ ...asRun(instance(id, repoID, null)), ...over });

// The mockup's Today / Yesterday rows plus an older day. A Run carries no PR state, so the mockup's
// "merged" / "PR open" chips are the run's own outcome here (see lib/runGroups).
export const runs = [
  // The live runs come back from GET /runs too (outcome active); the Ended side leaves them out.
  ...instances.map(asRun),
  run('run_21', 'coding-lab', {
    kind: 'lander',
    title: 'Fix chat dock overlap',
    pull_number: 77,
    branch: 'lab/20261008-2029',
    started_at: today(2 * HOUR + 40 * MIN),
    ended_at: today(2 * HOUR),
    outcome: 'success',
  }),
  run('run_22', 'coding-lab', {
    kind: 'escalate',
    title: 'afk/61 Autoland re-arm',
    issue_number: 61,
    pull_number: 76,
    branch: 'afk/61',
    session_name: 'coding-lab~afk-61',
    started_at: today(6 * HOUR + 10 * MIN),
    ended_at: today(5 * HOUR),
    outcome: 'escalated',
  }),
  run('run_23', 'cloonar-nixos', {
    kind: 'scheduled',
    title: 'sched-7f3a · nightly deps',
    branch: 'sched/7f3a-20261008',
    session_name: 'cloonar-nixos~sched-7f3a-20261008-0300',
    started_at: dayAt(1, 3, 0),
    ended_at: dayAt(1, 5, 0),
    outcome: 'death',
    failure_reason: 'The run exceeded its 120 min budget.',
  }),
  run('run_24', 'data-pipeline', {
    kind: 'afk_auto',
    title: 'Backfill events',
    issue_number: 118,
    branch: 'afk/118',
    session_name: 'data-pipeline~afk-118',
    started_at: dayAt(1, 1, 10),
    ended_at: dayAt(1, 1, 52),
    outcome: 'stopped',
  }),
  run('run_25', 'website', {
    kind: 'afk_auto',
    title: 'Bump the static site generator',
    issue_number: 212,
    branch: 'afk/212',
    session_name: 'website~afk-212',
    started_at: dayAt(3, 14, 0),
    ended_at: dayAt(3, 16, 0),
    outcome: 'timeout',
  }),
  run('run_26', 'coding-lab', {
    title: 'Chat header context meter',
    branch: 'lab/20261006-1010',
    session_name: 'coding-lab~operator-20261006-1010',
    started_at: dayAt(3, 10, 10),
    ended_at: dayAt(3, 11, 25),
    outcome: 'success',
  }),
];

// --- the chat of run_1 (GET /runs/run_1/messages) -----------------------------------------------

const QUESTION = {
  tool_id: 'toolu_runner',
  dialog_kind: 'question',
  prompt: 'Which name should the field carry in the run row?',
  options: [
    {
      label: 'runner',
      description: 'Matches the Runner term in CONTEXT.md and the settings UI. (Recommended)',
    },
    { label: 'executor', description: 'Shorter in the API, but a new word for the same thing.' },
    { label: 'Other', is_other: true },
  ],
  answerable: true,
};

export const messages = {
  messages: [
    {
      seq: 1,
      kind: 'text',
      role: 'user',
      time: ago(2 * MIN),
      text: 'Record which Runner a run used on the run row, so History can show it.',
    },
    {
      seq: 2,
      kind: 'text',
      role: 'assistant',
      time: ago(110e3),
      text: 'I will add a column to the runs table and stamp it at spawn. First a look at how the Runner is resolved today.',
    },
    {
      seq: 3,
      kind: 'tool',
      role: 'assistant',
      time: ago(100e3),
      tool: {
        name: 'Bash',
        title: 'grep -rn "Runner" internal/',
        status: 'ok',
        view: { kind: 'command', command: 'grep -rn "Runner" internal/' },
        output: 'internal/spawn/runner.go:14: // Runner resolves container or host.',
      },
    },
    {
      seq: 4,
      kind: 'text',
      role: 'assistant',
      time: ago(70e3),
      text: 'The resolved value lives in spawn.Resolve. Before I add the migration, one naming question.',
    },
  ],
  state: 'question',
  cursor: 4,
  has_more: false,
  pending_dialog: QUESTION,
  transcript: 'available',
  transcript_id: 't-run_1',
  context_usage: { used: 41_000, limit: 200_000 },
};

export const tokens = [
  { id: 'tok_1', name: 'ci-deploy', created_at: ago(40 * DAY), last_used_at: ago(2 * HOUR) },
  { id: 'tok_2', name: 'laptop-cli', created_at: ago(12 * DAY), last_used_at: ago(3 * DAY) },
];
