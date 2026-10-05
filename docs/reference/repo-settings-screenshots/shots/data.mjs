// The sample lab the screenshots show: the fifteen repositories, runs, Schedules, secrets and
// imports of the reference mockup (docs/reference/repo-settings-mockup.html). Agents and models
// carry placeholder names, never a real vendor's. Times are relative to now, so "4 min ago" stays
// true whenever the script runs.
const NOW = Date.now();
const MIN = 60e3;
const DAY = 24 * 60 * MIN;
const ago = (ms) => new Date(NOW - ms).toISOString();

export const providers = [
  {
    id: 'agent-a',
    display_name: 'Agent A',
    supports_remote: true,
    auth: { kind: 'oauth-code' },
    models: [
      { value: 'model-large', label: 'Large', efforts: [] },
      { value: 'model-medium', label: 'Medium', efforts: [] },
      { value: 'model-small', label: 'Small', efforts: [] },
    ],
    efforts: ['low', 'medium', 'high'].map((value) => ({ value, label: value })),
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

/** What each overridable field resolves to when the repo leaves it unset (POST …/inherited). */
export const inherited = {
  provider: 'agent-a',
  afk_provider_default: 'agent-a',
  lander_provider: 'agent-a',
  model_default: 'model-large',
  effort_default: 'high',
  afk_model_default: 'model-large',
  afk_effort_default: 'high',
  lander_model: 'model-large',
  lander_effort: 'high',
  remote_default: true,
  afk_remote_default: false,
  afk_options: {},
  budget_minutes: 120,
  max_instances_override: 2,
  git_author_name: 'lab-bot',
  git_author_email: 'lab-bot@example.com',
  runner: 'container',
  image_ref: 'ghcr.io/example/lab-dev:latest',
  container_memory: '8g',
  container_pids: 4096,
  container_nofile: 16384,
};

export const credentials = [
  { id: 'c1', name: 'gh-cloonar', kind: 'https_token', referenced: true },
  { id: 'c2', name: 'deploy-key', kind: 'ssh_key', referenced: false },
  { id: 'c3', name: 'gh-cloonar', kind: 'forge_token', referenced: true },
  { id: 'c4', name: 'forgejo-bot', kind: 'forge_token', referenced: true },
].map((c) => ({ ...c, created_at: ago(90 * DAY), updated_at: ago(90 * DAY) }));

export const flows = [
  {
    key: 'autolander',
    label: 'Autolander',
    description: 'Files each finding as a fully specified issue labeled ready-for-agent.',
  },
  {
    key: 'human-triage',
    label: 'Human triage',
    description: 'Files each finding as an issue labeled needs-triage, for you to review.',
  },
];

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
    fix: { scope: 'repo', section: 'integrations', field: 'forge_credential_id' },
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

// In the mockup's order: latest run first, then the repos that never had a run.
export const repos = [
  repo('coding-lab', 'git@github.com:Cloonar/coding-lab.git', {
    last_opened_at: ago(4 * MIN),
    afk_auto_enabled: true,
    autoland_enabled: true,
    afk_model_default: 'model-medium', // the one value set here, as in the mockup
    summary: summary(3, 12),
  }),
  repo('mobile-app', 'git@github.com:example/mobile-app.git', {
    created_at: ago(12 * MIN),
    clone_status: 'cloning',
    summary: summary(
      null,
      null,
      [{ id: 'clone', state: 'pending', detail: 'Cloning.' }],
      'pending',
    ),
  }),
  repo('cloonar-nixos', 'https://git.example.com/cloonar/nixos.git', {
    forge_kind: 'forgejo',
    forge_credential_id: 'c4',
    runner: 'host', // the mockup's "Runner on host" repo
    last_opened_at: ago(60 * MIN),
    summary: summary(0, 4),
  }),
  repo('data-pipeline', 'git@github.com:example/data-pipeline.git', {
    last_opened_at: ago(180 * MIN),
    afk_auto_enabled: true,
    summary: summary(4, 9),
  }),
  repo('billing-api', 'https://git.example.com/acme/billing-api.git', {
    forge_kind: 'forgejo',
    forge_credential_id: 'c4',
    incogni: true,
    last_opened_at: ago(300 * MIN),
    summary: summary(1, 3),
  }),
  repo('website', 'git@github.com:example/website.git', {
    last_opened_at: ago(26 * 60 * MIN),
    afk_auto_enabled: true,
    consecutive_failures: 3, // three-strikes pause
    autoland_enabled: true,
    summary: summary(5, 7),
  }),
  repo('auth-service', 'https://git.example.com/acme/auth-service.git', {
    forge_kind: 'forgejo',
    forge_credential_id: 'c4',
    last_opened_at: ago(30 * 60 * MIN),
    afk_auto_enabled: true,
    summary: summary(2, 8, TRACKER_REJECTED),
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
      { id: 'clone', state: 'failing', detail: 'The last clone failed.', action: 'retry_clone' },
    ]),
  }),
  repo('design-tokens', 'git@github.com:example/design-tokens.git', {
    last_opened_at: ago(6 * DAY),
  }),
  repo('cli-tools', 'git@github.com:example/cli-tools.git', {
    last_opened_at: ago(9 * DAY),
    summary: summary(2, 5),
  }),
  repo('status-page', 'git@github.com:example/status-page.git', {
    last_opened_at: ago(14 * DAY),
    autoland_enabled: true,
  }),
  repo('terraform-modules', 'https://git.example.com/acme/terraform-modules.git', {
    forge_kind: 'forgejo',
    last_opened_at: ago(20 * DAY),
  }),
  repo('mail-templates', 'git@github.com:example/mail-templates.git', {
    last_opened_at: ago(31 * DAY),
  }),
  repo('analytics-dash', 'https://git.example.com/acme/analytics-dash.git', {
    forge_kind: 'forgejo',
    incogni: true,
    created_at: ago(45 * DAY), // never had a run: no "Last run", sorted last
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
    session_name: `${repoName}~${id}`, // an AFK run's is "<repo>~afk-<N>": the rail shows its budget
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
  instance('run_1', 'coding-lab', 'Fix chat dock overlap', { state: 'needs_input' }),
  instance('run_2', 'coding-lab', 'afk/61 Autoland re-arm', {
    kind: 'afk_auto',
    issue_number: 61,
    branch: 'afk/61',
    session_name: 'coding-lab~afk-61',
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

export const parked = {
  'coding-lab': [
    {
      branch: 'lab/try-sse-backoff',
      worktree_path: '/srv/lab/worktrees/coding-lab-try-sse-backoff',
      dirty: true,
      commits_ahead: 2,
      unpushed: 1,
    },
  ],
};

// --- Schedules, secrets, imports ----------------------------------------------------------------

const schedule = (over) => ({
  repo_id: 'coding-lab',
  enabled: true,
  budget_minutes: null,
  model: null,
  effort: null,
  provider: null,
  consecutive_failures: 0,
  paused: false,
  last_fired_at: ago(DAY),
  next_run_at: left(20 * 60),
  created_at: ago(30 * DAY),
  updated_at: ago(DAY),
  ...over,
});

export const schedules = {
  'coding-lab': [
    schedule({
      id: 'sched_1',
      name: 'Nightly dependency check',
      cadence: '0 3 * * *',
      prompt: 'Investigate available dependency updates and file what is worth doing.',
      flows: ['autolander'],
      next_run_display: 'Tue 2026-10-06 03:00',
      last_run: {
        id: 'run_11',
        started_at: ago(27 * 60 * MIN),
        ended_at: ago(26 * 60 * MIN),
        outcome: 'success',
      },
    }),
    schedule({
      id: 'sched_2',
      name: 'Weekly triage sweep',
      cadence: '0 8 * * 1',
      prompt:
        'Read the open needs-triage issues and look for duplicates and missing reproduction steps.',
      flows: ['human-triage'],
      next_run_display: 'Mon 2026-10-12 08:00',
      last_run: {
        id: 'run_12',
        started_at: ago(7 * DAY),
        ended_at: ago(7 * DAY - 30 * MIN),
        outcome: 'success',
      },
    }),
    schedule({
      id: 'sched_3',
      name: 'Docs drift check',
      cadence: '0 17 * * 5',
      prompt: 'Compare the docs with the code and file what drifted.',
      flows: ['autolander'],
      paused: true,
      consecutive_failures: 3,
      next_run_at: null,
      next_run_display: null,
      last_run: {
        id: 'run_13',
        started_at: ago(3 * DAY),
        ended_at: ago(3 * DAY - 10 * MIN),
        outcome: 'death',
      },
    }),
  ],
};

const secret = (id, name, description, days) => ({
  id,
  name,
  description,
  created_at: ago(120 * DAY),
  updated_at: ago(days * DAY),
  exposed_run_id: null,
  exposed_at: null,
});
export const secrets = {
  'coding-lab': [
    secret('sec_1', 'NPM_TOKEN', 'Registry publish token', 23),
    secret('sec_2', 'SENTRY_AUTH_TOKEN', 'Release uploads', 63),
    secret('sec_3', 'STAGING_DB_URL', 'Read-only staging database', 76),
  ],
};

/** repo → the repos it imports. The importers of a repo are derived from this. */
export const imports = { 'coding-lab': ['cloonar-nixos'] };

export const sshTargets = [
  { id: 'tgt_1', name: 'staging-diag', description: 'through the bastion', assigned: true },
];

export const issues = [
  { number: 61, title: 'Repositories and repo settings redesign', labels: ['enhancement'] },
  { number: 58, title: 'Chat about this selects the picker row', labels: ['bug'] },
  { number: 55, title: 'Runner section reads the global default', labels: [] },
].map((issue, i) => ({
  body: '',
  state: 'open',
  comments_count: 0,
  created_at: ago((i + 2) * DAY),
  updated_at: ago((i + 1) * 60 * MIN),
  ...issue,
}));
