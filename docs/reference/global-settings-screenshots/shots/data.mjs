// The sample lab the global Settings screenshots show (issue #85): the settings a lab is seeded
// with plus a few overrides, a provider catalog with placeholder names (never a real vendor's), the
// credential gateway and SSH bastion status cards of General, and fifteen repositories — the
// Runner section counts how many inherit the runner default. Times are relative to now.
const NOW = Date.now();
const DAY = 24 * 60 * 60e3;
const ago = (ms) => new Date(NOW - ms).toISOString();

export const providers = [
  {
    id: "agent-a",
    display_name: "Agent A",
    supports_remote: true,
    auth: { kind: "oauth-code" },
    models: [
      { value: "model-large", label: "Large", efforts: [] },
      { value: "model-medium", label: "Medium", efforts: [] },
      { value: "model-small", label: "Small", efforts: [] },
    ],
    efforts: ["low", "medium", "high"].map((value) => ({
      value,
      label: value,
    })),
    // The bool spawn option bag AFK runs offer on this provider.
    options: [
      { key: "fast_mode", label: "Fast mode", type: "bool", default: "false" },
    ],
  },
  {
    id: "agent-b",
    display_name: "Agent B",
    supports_remote: false,
    auth: { kind: "api-key" },
    models: [{ value: "model-b1", label: "B one", efforts: [] }],
    efforts: ["low", "high"].map((value) => ({ value, label: value })),
    options: [],
  },
];

/** GET /settings: seeded values, one AFK override set (the AFK model), the rest inherited. */
export const globalSettings = {
  provider_default: "agent-a",
  spawn_model_default: "model-large",
  spawn_effort_default: "high",
  spawn_remote_default: true,
  spawn_provider_default_afk: "",
  spawn_model_default_afk: "model-medium",
  spawn_effort_default_afk: "",
  spawn_remote_default_afk: null,
  spawn_options_afk: JSON.stringify({ fast_mode: "false" }),
  spawn_model_default_lander: "",
  spawn_effort_default_lander: "",
  afk_prompt: "",
  afk_prompt_default:
    "Resolve issue #<N> on branch <BRANCH>, then open a pull request. End with the done signal.",
  dialog_timeout_minutes: 30,
  max_instances: 4,
  afk_budget_minutes: 120,
  afk_tick_seconds: 30,
  afk_schedule_seconds: 60,
  sweep_interval_minutes: 15,
  git_author_name: "lab-bot",
  git_author_email: "lab-bot@example.com",
  transcript_retention_days: 30,
  runner_default: "container",
  dev_image_default: "",
  dev_image_fallback: "ghcr.io/example/lab-dev:latest",
  container_memory: "8g",
  container_pids: 4096,
  container_nofile: 16384,
};

/** GET /onecli/health — the credential gateway card of General. */
export const gatewayHealth = {
  state: "ok",
  api: {
    configured: true,
    reachable: true,
    url: "http://onecli:10254",
    status: "ok",
  },
  gateway: { configured: true, reachable: true, url: "http://onecli:10255" },
};

/** GET /warpgate/health — the SSH bastion card of General. */
export const bastionHealth = {
  state: "ok",
  api: { configured: true, reachable: true, authenticated: true },
  ssh: { configured: true, reachable: true },
  hostKey: {
    state: "pinned",
    pinned: ["SHA256:q1w2e3r4t5y6u7i8o9p0"],
    observed: [],
  },
};

/** GET /providers/{id}/auth/status — the app shell's login check. */
export const authStatus = {
  logged_in: true,
  email: "operator@example.com",
  method: "oauth",
  checked_at: ago(60e3),
};

/** GET /push/subscriptions — the Web Push devices the Notifications section lists. */
export const pushDevices = [
  {
    id: "push_1",
    endpoint: "https://push.example.com/a1",
    label: "Phone",
    created_at: ago(40 * DAY),
  },
  {
    id: "push_2",
    endpoint: "https://push.example.com/b2",
    label: "Laptop",
    created_at: ago(12 * DAY),
  },
];

// --- repositories (only the Runner section's inheriting-repo count reads them) -------------------

const NAMES = [
  "coding-lab",
  "mobile-app",
  "cloonar-nixos",
  "data-pipeline",
  "billing-api",
  "website",
  "auth-service",
  "docs-site",
  "infra-docs",
  "design-tokens",
  "cli-tools",
  "status-page",
  "terraform-modules",
  "mail-templates",
  "analytics-dash",
];
/** Pinned runners; every other repo leaves it unset and inherits the global default. */
const PINNED = {
  "cloonar-nixos": "host",
  "data-pipeline": "container",
  website: "container",
};

export const repos = NAMES.map((name, i) => ({
  id: name,
  name,
  remote_url: `git@github.com:example/${name}.git`,
  credential_id: "c1",
  forge_credential_id: "c3",
  tracker_binding: "forge",
  forge_kind: "github",
  default_branch: "main",
  runner: PINNED[name] ?? null,
  clone_status: "ready",
  clone_error: null,
  created_at: ago((120 - i) * DAY),
  last_opened_at: null,
}));
