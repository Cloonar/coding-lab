// The sample lab the New run screenshots show: the nine repositories of the reference mockup
// (docs/reference/new-run-mockup.html) with their open issues, and two agents. Agents and models
// carry placeholder names, never a real vendor's. Times are relative to now, so "3 d" stays true
// whenever the script runs.
const NOW = Date.now();
const MIN = 60e3;
const DAY = 24 * 60 * MIN;
const ago = (ms) => new Date(NOW - ms).toISOString();

// --- agents -------------------------------------------------------------------------------------

const EFFORTS = ['low', 'medium', 'high', 'max'].map((value) => ({
  value,
  label: value[0].toUpperCase() + value.slice(1),
}));
export const providers = [
  {
    id: 'agent-a',
    display_name: 'Agent A',
    supports_remote: true,
    auth: { kind: 'oauth-code' },
    models: [
      {
        value: 'model-large',
        label: 'Large',
        efforts: EFFORTS,
        default_effort: 'high',
      },
      { value: 'model-medium', label: 'Medium', efforts: EFFORTS.slice(0, 3) },
      { value: 'model-small', label: 'Small', efforts: EFFORTS.slice(0, 3) },
    ],
    efforts: EFFORTS,
    options: [],
  },
  {
    // No remote control (ADR-0045), one model without efforts.
    id: 'agent-b',
    display_name: 'Agent B',
    supports_remote: false,
    auth: { kind: 'api-key' },
    models: [{ value: 'b-one', label: 'B One', efforts: [] }],
    efforts: [],
    options: [],
  },
];

export const globalSettings = {
  provider_default: 'agent-a',
  spawn_model_default: 'model-large',
  spawn_effort_default: 'high',
  spawn_remote_default: true,
  runner_default: 'container',
  container_memory: '8g',
  container_pids: 4096,
  container_nofile: 16384,
  dev_image_default: 'ghcr.io/example/lab-dev:latest',
  dev_image_fallback: '',
};

// --- repositories -------------------------------------------------------------------------------

const passing = (id, detail) => ({ id, state: 'passing', detail });
const ALL_PASS = [
  passing('clone', 'The reference repo is up to date.'),
  passing('git_credential', 'gh-cloonar can fetch and push.'),
  passing('tracker', 'forge · github, issues are readable.'),
  passing('agent_login', 'Agent A is logged in.'),
  passing('dev_image', 'Inherited from the global setting.'),
  passing('imports', 'None declared.'),
];
const TRACKER_REJECTED = [
  {
    id: 'tracker',
    state: 'failing',
    detail: 'The forge token was rejected, so issues cannot be read.',
    fix: {
      scope: 'repo',
      section: 'integrations',
      field: 'forge_credential_id',
    },
  },
  ...ALL_PASS.filter((check) => check.id !== 'tracker'),
];
const summary = (claimable, openIssues, checks = ALL_PASS, state) => ({
  claimable,
  open_issues: openIssues,
  readiness: {
    state: state ?? (checks.some((c) => c.state === 'failing') ? 'failing' : 'passing'),
    checks,
  },
});

function repo(name, remote, over = {}) {
  return {
    id: name,
    name,
    remote_url: remote,
    credential_id: 'c1',
    forge_credential_id: 'c3',
    tracker_binding: 'forge',
    forge_kind: 'github',
    default_branch: 'main',
    provider: null,
    incogni: false,
    model_default: null,
    effort_default: null,
    remote_default: null,
    afk_provider_default: null,
    afk_model_default: null,
    afk_effort_default: null,
    afk_remote_default: null,
    afk_options: null,
    afk_prompt: null,
    afk_prompt_effective: 'Resolve issue #<N> on branch <BRANCH>, then open a PR.',
    git_author_name: null,
    git_author_email: null,
    afk_branch_pattern: 'afk/<N>',
    manual_branch_prefix: 'lab/',
    afk_auto_enabled: false,
    consecutive_failures: 0,
    budget_minutes: null,
    max_instances_override: null,
    clone_status: 'ready',
    clone_error: null,
    created_at: ago(120 * DAY),
    last_opened_at: null,
    autoland_enabled: false,
    max_fix_attempts: 2,
    auto_merge: true,
    lander_provider: null,
    lander_model: null,
    lander_effort: null,
    runner: null,
    container_memory: null,
    container_pids: null,
    container_nofile: null,
    image_ref: null,
    summary: summary(0, 4),
    ...over,
  };
}

// In the mockup's order. The page's pills are the first four (coding-lab, data-pipeline,
// cloonar-nixos, billing-api), which the script also stores as lab.last-repo.
export const repos = [
  repo('coding-lab', 'git@github.com:Cloonar/coding-lab.git', {
    last_opened_at: ago(4 * MIN),
    afk_auto_enabled: true,
    autoland_enabled: true,
    summary: summary(3, 12),
  }),
  repo('data-pipeline', 'git@github.com:example/data-pipeline.git', {
    last_opened_at: ago(180 * MIN),
    afk_auto_enabled: false, // Auto off: the Issues card offers Run one
    summary: summary(4, 4),
  }),
  repo('cloonar-nixos', 'https://git.example.com/cloonar/nixos.git', {
    forge_kind: 'forgejo',
    forge_credential_id: 'c4',
    runner: 'host', // the host-Runner warning
    last_opened_at: ago(60 * MIN),
    summary: summary(0, 1),
  }),
  repo('billing-api', 'https://git.example.com/acme/billing-api.git', {
    forge_kind: 'forgejo',
    forge_credential_id: 'c4',
    provider: 'agent-b',
    incogni: true,
    last_opened_at: ago(300 * MIN),
    summary: summary(1, 2),
  }),
  repo('website', 'git@github.com:example/website.git', {
    last_opened_at: ago(26 * 60 * MIN),
    afk_auto_enabled: true,
    consecutive_failures: 3, // AFK paused after three failed runs
    autoland_enabled: true,
    summary: summary(5, 5),
  }),
  repo('auth-service', 'https://git.example.com/acme/auth-service.git', {
    forge_kind: 'forgejo',
    forge_credential_id: 'c4',
    last_opened_at: ago(30 * 60 * MIN),
    afk_auto_enabled: true,
    summary: summary(2, 8, TRACKER_REJECTED), // tracker check failing
  }),
  repo('mobile-app', 'git@github.com:example/mobile-app.git', {
    created_at: ago(12 * MIN),
    clone_status: 'cloning', // 62%, delivered over the event stream
    summary: summary(
      null,
      null,
      [{ id: 'clone', state: 'pending', detail: 'Cloning.' }],
      'pending',
    ),
  }),
  repo('docs-site', 'git@github.com:example/docs-site.git', {
    last_opened_at: ago(2 * DAY),
    afk_auto_enabled: true,
    autoland_enabled: true,
  }),
  repo('infra-docs', 'https://git.example.com/acme/infra-docs.git', {
    forge_kind: 'forgejo',
    last_opened_at: ago(3 * DAY),
    clone_status: 'error',
    clone_error: 'the remote rejected the git credential.',
    summary: summary(null, null, [
      {
        id: 'clone',
        state: 'failing',
        detail: 'The last clone failed.',
        action: 'retry_clone',
      },
    ]),
  }),
];

// --- runs ---------------------------------------------------------------------------------------

function instance(id, repoName, title, over = {}) {
  return {
    id,
    repo_id: repoName,
    repo_name: repoName,
    kind: 'manual',
    provider: 'agent-a',
    issue_number: null,
    pull_number: null,
    branch: 'lab/fix-chat-dock',
    worktree_path: `/srv/lab/worktrees/${repoName}-${id}`,
    session_name: `${repoName}~${id}`,
    title,
    model: 'model-large',
    effort: 'high',
    remote: false,
    deep_link_url: null,
    started_at: ago(20 * MIN),
    budget_deadline: null,
    ended_at: null,
    outcome: 'running',
    failure_reason: null,
    live: true,
    connecting: false,
    state: 'working',
    ...over,
  };
}
const left = (minutes) => new Date(NOW + minutes * MIN).toISOString();
export const instances = [
  instance('run_1', 'coding-lab', 'Fix chat dock overlap', {
    state: 'needs_input',
  }),
  instance('run_2', 'coding-lab', 'afk/56 Record the Runner on the run', {
    kind: 'afk_auto',
    issue_number: 56,
    branch: 'afk/56',
    session_name: 'coding-lab~afk-56',
    budget_deadline: left(72.5),
  }),
  instance('run_3', 'data-pipeline', 'afk/118 Backfill events', {
    kind: 'afk_auto',
    issue_number: 118,
    branch: 'afk/118',
    session_name: 'data-pipeline~afk-118',
    budget_deadline: left(48.5),
  }),
  instance('run_4', 'cloonar-nixos', 'Bump flake inputs', { state: 'idle' }),
];

// --- open issues, newest first ------------------------------------------------------------------

/** [number, title, labels, age in days, open PR (optional)] */
const issue = ([number, title, labels, days, pull = null]) => ({
  number,
  title,
  labels,
  body: '',
  state: 'open',
  comments_count: 0,
  created_at: ago(days * DAY),
  updated_at: ago(Math.max(1, days - 1) * DAY),
  pull,
});

/** An open PR resolving an issue (issue #88): its `PR #n` chip and the sheet's Land row. */
const pull = (number, headBranch, repo) => ({
  number,
  head_branch: headBranch,
  url: `https://github.com/${repo}/pull/${number}`,
  escalated: false,
});

export const issues = {
  // The mockup's six, and six more so "All 12 open issues" has something to filter.
  'coding-lab': [
    [
      56,
      'Record the Runner a run was spawned with on the run',
      ['ready-for-agent', 'enhancement'],
      3,
    ],
    [
      47,
      'Warpgate: dashboard exposure and per-run recordings deep link',
      ['needs-triage', 'enhancement'],
      8,
    ],
    [
      45,
      'Chat header context meter: the Large denominator is 200K but first-party logins run 1M',
      ['needs-triage'],
      8,
    ],
    // An AFK run opened PR #61 for #44: the card shows its PR chip, the sheet offers Land.
    [
      44,
      'Schedule editor loses the raw cron on blur',
      ['ready-for-agent', 'enhancement'],
      12,
      pull(61, 'afk/44', 'Cloonar/coding-lab'),
    ],
    [41, 'History: filter by outcome', ['needs-info'], 19],
    [38, 'Docs: the quickstart skips the OneCLI grant step', [], 26],
    [37, 'Parked view: Discard asks twice on Safari', ['needs-triage'], 31],
    [35, 'Run details: show the budget deadline', ['bug'], 38],
    [
      33,
      'fix(nix): assert the two dead-setting OneCLI dashboard refusals at eval time',
      ['needs-triage', 'bug'],
      49,
    ],
    [30, 'Container runner: preflight misses a missing crun', ['needs-info', 'bug'], 55],
    [
      22,
      'labctl pr create: allow opening a PR from a specific branch (--head)',
      ['ready-for-agent', 'enhancement'],
      62,
    ],
    [
      11,
      'Publish a lab server container image for a one-command quickstart',
      ['ready-for-agent', 'enhancement'],
      70,
    ],
  ].map(issue),
  'data-pipeline': [
    [130, 'Alert when a partition is missing', ['needs-triage', 'enhancement'], 6],
    [124, 'Export the rollup as parquet', ['needs-info', 'enhancement'], 5],
    [121, 'Dedupe late-arriving events before the nightly rollup', ['ready-for-agent', 'bug'], 2],
    [118, 'Backfill the events table from the archive', ['ready-for-agent', 'bug'], 1],
  ]
    .map(issue)
    .sort((a, b) => b.created_at.localeCompare(a.created_at)),
  'cloonar-nixos': [[9, 'Pin the kernel to the LTS channel on dev-new', ['needs-triage'], 5]].map(
    issue,
  ),
  'billing-api': [
    [15, 'Invoice PDF: the VAT line is missing for reverse charge', ['needs-triage', 'bug'], 6],
    [14, 'Round invoice totals once, at the end', ['ready-for-agent', 'bug'], 4],
  ]
    .map(issue)
    .sort((a, b) => b.created_at.localeCompare(a.created_at)),
  website: [
    [214, 'Dark mode: the code blocks keep a light background', ['bug'], 12],
    [209, 'Fix the OG image for docs pages', ['bug'], 10],
    [208, 'Add the changelog feed', ['enhancement'], 9],
    [203, 'Blog index pagination', ['enhancement'], 4],
    [201, 'Pricing page: the yearly toggle resets on reload', ['bug'], 2],
  ]
    .map(issue)
    .sort((a, b) => b.created_at.localeCompare(a.created_at)),
};
