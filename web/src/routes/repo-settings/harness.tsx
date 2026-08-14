// Shared test harness for the repo-settings area suites (issue #198), the
// runchat/harness.tsx precedent applied to the RepoSettings.test.tsx stub
// server and DOM helpers. Section suites mount the real route at
// /repos/:id/settings/:section? (App root, MemoryRouter) and poke the mutable
// `h` state object to shape server responses.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, vi } from 'vitest';
import type { MemoryHistory } from '@solidjs/router';
import type {
  CredentialListItem,
  OneCLIDashboardExposure,
  OneCLIGrant,
  OneCLIGrantKind,
  OneCLIPool,
  Provider,
  Repo,
  RepoImport,
  RepoSecret,
  Schedule,
} from '../../api';
import App from '../../App';
import RepoSettingsRoute from './index';

export const REPO_ID = 'repo_1';

/** claude-code catalog with the ultracode bool option (issue #19). */
export function baseProviders(): Provider[] {
  return [
    {
      id: 'claude-code',
      display_name: 'Claude Code',
      supports_remote: true,
      auth: { kind: 'oauth-code' },
      models: [
        { value: 'opus[1m]', label: 'Opus (1M)', efforts: [] },
        { value: 'sonnet', label: 'Sonnet', efforts: [] },
      ],
      // The settings pickers consume the provider-level UNION (issue #156).
      efforts: [{ value: 'high', label: 'high' }],
      options: [
        {
          key: 'ultracode',
          label: 'Ultracode (multi-agent workflows)',
          type: 'bool',
          default: 'false',
        },
      ],
    },
  ];
}

/**
 * The lab-wide OneCLI project pool (issue #25) the credential-gateway grant
 * picker offers: one stored secret and one app connection, so both halves of
 * the pool render. Configured and non-empty by default — that is the normal
 * path, and every other Secrets-section suite mounts the picker alongside the
 * legacy card, so the default must not be one of the exceptional states.
 */
export function baseOneCLIPool(): OneCLIPool {
  return {
    configured: true,
    secrets: [{ id: 'sec_pool_1', name: 'ANTHROPIC_API_KEY', provider: 'anthropic' }],
    connections: [{ id: 'conn_pool_1', name: 'GitHub app', provider: 'github' }],
  };
}

/** A second provider with its own catalogs (agent-selection tests). */
export const CODEX: Provider = {
  id: 'codex',
  display_name: 'Codex',
  supports_remote: false,
  auth: { kind: 'api-key' },
  models: [{ value: 'gpt-5-codex', label: 'GPT-5 Codex', efforts: [] }],
  efforts: [{ value: 'medium', label: 'medium' }],
  options: [],
};

/** Stand-in for EventSource: lets tests push SSE events into the app. */
export class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((event: { data: string }) => void)[]>();

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: { data: string }) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  close(): void {}

  emit(type: string, payload: Record<string, unknown>): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data: JSON.stringify(payload) });
    }
  }
}

export function baseRepo(): Repo {
  return {
    id: REPO_ID,
    name: 'coding-lab',
    remote_url: 'git@git.cloonar.com:Cloonar/coding-lab.git',
    credential_id: null,
    forge_credential_id: null,
    tracker_binding: 'forge',
    forge_kind: 'forgejo',
    // The verified scenario: settings opened while the clone still runs, the
    // provisional default branch not yet replaced by detection.
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
    clone_status: 'cloning',
    clone_error: null,
    created_at: '2026-07-06T00:00:00Z',
    last_opened_at: null,
    autoland_enabled: false,
    max_fix_attempts: 2,
    auto_merge: true,
    lander_provider: null,
    lander_model: null,
    lander_effort: null,
    runner: 'host',
    container_memory: null,
    container_pids: null,
    container_nofile: null,
    image_ref: null,
  };
}

/**
 * Two more registered repos (issue #261's Imports picker candidates), beyond
 * baseRepo() itself: repo_2 is ready, repo_3 is still cloning — the picker
 * must offer both (it is not the spawn-time clone-status guard), so a
 * still-cloning candidate stays selectable by construction here.
 */
export function otherRepos(): Repo[] {
  return [
    { ...baseRepo(), id: 'repo_2', name: 'other-repo', clone_status: 'ready' },
    { ...baseRepo(), id: 'repo_3', name: 'third-repo', clone_status: 'cloning' },
  ];
}

/**
 * The real built-in flow catalog (issue #247 / ADR-0062), in catalog order —
 * the same two keys, labels and descriptions internal/afk/flows.go ships, so
 * a section test asserting canonical flow order is asserting the real thing.
 */
export const SCHEDULE_FLOWS = [
  {
    key: 'autolander',
    label: 'Autolander',
    description:
      'Files each finding as a fully specified issue labeled ready-for-agent, for the AFK pipeline to claim and land.',
  },
  {
    key: 'human-triage',
    label: 'Human triage',
    description:
      'Files each finding as an issue labeled needs-triage, for a human to triage before any agent claims it.',
  },
];

/** A Schedule row with every key present, overridable per test. */
export function baseSchedule(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 'sched_1',
    repo_id: REPO_ID,
    name: 'Weekly dependency check',
    cadence: '30 6 * * 1,4',
    prompt: 'Investigate available dependency updates.',
    flows: ['autolander'],
    enabled: true,
    budget_minutes: null,
    model: null,
    effort: null,
    provider: null,
    consecutive_failures: 0,
    paused: false,
    last_fired_at: null,
    created_at: '2026-07-20T00:00:00.000Z',
    updated_at: '2026-07-20T00:00:00.000Z',
    ...over,
  };
}

/** The cron expression the preview stub answers as unparseable. */
export const BAD_CRON = 'bad';

export function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

/**
 * Mutable server/response state the tests poke directly (e.g.
 * `h.repoOnServer = {...}`). Plain `export let` bindings can't work here —
 * ESM importers get a read-only view of an imported binding, so a test file
 * can never reassign one. Routing every whole-variable reassignment through
 * fields on a single exported object sidesteps that: importers mutate `h.x`,
 * never rebind `x` itself.
 */
export interface RepoSettingsHarnessState {
  repoOnServer: Repo;
  /** GET /repos (issue #261): the registered-repo catalog the Imports
   *  picker draws candidates from — baseRepo() plus otherRepos() by default. */
  reposOnServer: Repo[];
  providersOnServer: Provider[];
  settingsOnServer: Record<string, unknown>;
  credentialsOnServer: CredentialListItem[];
  patchBodies: Record<string, unknown>[];
  secretsOnServer: RepoSecret[];
  secretRequestBodies: Record<string, unknown>[];
  /** Full URLs of DELETE /repos/:id calls — `?force=true` rides the URL. */
  deleteRequests: string[];
  /** Repo DELETE status — 204 by default; a Danger test flips it to 409. */
  deleteStatus: number;
  /** The repo's Schedules (issue #247), mutated by the stub's own handlers. */
  schedules: Schedule[];
  /** Every schedule POST/PATCH payload, for exact-body assertions. */
  scheduleBodies: Record<string, unknown>[];
  /** Cron expressions the preview endpoint was asked about, in order. */
  cronPreviewExprs: string[];
  /** This repo's declared imports (issue #261), sorted by name like the real API. */
  importsOnServer: RepoImport[];
  /** Every imports POST body, for exact target_repo_id assertions. */
  importPostBodies: Record<string, unknown>[];
  /** Forces the next imports POST to answer 400 with this message — the
   *  self-import/unknown-target 400s the client-side picker already
   *  excludes by construction, so a test reaches them this way instead. */
  importPostError: string | null;
  /** Full URLs of DELETE .../imports/:targetId calls. */
  importDeleteRequests: string[];
  /** GET /onecli/pool (issue #25): the lab-wide OneCLI project pool the grant
   *  picker toggles — baseOneCLIPool() by default. Set `configured: false` for
   *  the "integration not set up in this lab" state, or both arrays empty for
   *  the reachable-but-empty pool. */
  poolOnServer: OneCLIPool;
  /** This repo's credential-gateway grants, mutated by the stub's own
   *  attach/detach handlers exactly as the real server would. */
  grantsOnServer: OneCLIGrant[];
  /** `"<METHOD> <url>"` for every grant attach/detach, in order — the picker
   *  applies each toggle immediately, so this is what an exact-call assertion
   *  reads. */
  grantRequests: string[];
  /** GET /onecli/dashboard: the resolved dashboard exposure the picker's
   *  link-out is composed from, and the ONLY source it may compose one from
   *  (ADR-0067's 2026-08-14 amendment). `null` makes that read answer 502
   *  instead — the exposure-unknown path, where no link may render and the
   *  picker itself must still work. */
  dashboardExposure: OneCLIDashboardExposure | null;
  /** Forces BOTH gateway reads (pool and grants) to answer 502 with this
   *  message — what the real server answers when OneCLI is configured but
   *  erroring, i.e. the unreachable state the picker must render as a
   *  retryable banner rather than an endless loading line. */
  gatewayReadError: string | null;
  /** Forces every grant attach/detach to answer 400 with this message — the
   *  server-side refusal a toggle must surface without flipping the row. */
  grantWriteError: string | null;
}
export const h = {} as RepoSettingsHarnessState;

let dispose: (() => void) | undefined;
// Eagerly initialized so a test that never mounts (a pure data check) still
// has a valid — empty, detached — container for queries and afterEach.
export let container: HTMLDivElement = document.createElement('div');
export let routerHistory: MemoryHistory;

export function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === '/api/v1/auth/state' && method === 'GET') {
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
        );
      }
      if (url === '/api/v1/credentials' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { credentials: h.credentialsOnServer }));
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: h.providersOnServer }));
      }
      // Global settings feed the effective-provider chains the catalogs
      // resolve against (provider_default / spawn_provider_default_afk).
      if (url === '/api/v1/settings' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...h.settingsOnServer }));
      }
      // The registered-repo catalog (issue #261): the Imports picker's
      // candidate list. Kept separate from h.repoOnServer, which only ever
      // answers the single-repo GET below.
      if (url === '/api/v1/repos' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { repos: h.reposOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...h.repoOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}` && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.patchBodies.push(patch);
        h.repoOnServer = { ...h.repoOnServer, ...patch };
        return Promise.resolve(jsonResponse(200, { ...h.repoOnServer }));
      }
      // Danger zone: DELETE /repos/:id (force rides the query string). A 409
      // mimics the running-clone conflict that reveals the force checkbox.
      if (
        (url === `/api/v1/repos/${REPO_ID}` || url === `/api/v1/repos/${REPO_ID}?force=true`) &&
        method === 'DELETE'
      ) {
        h.deleteRequests.push(url);
        if (h.deleteStatus >= 400) {
          return Promise.resolve(
            jsonResponse(h.deleteStatus, { error: 'clone in progress — retry with force' }),
          );
        }
        return Promise.resolve(jsonResponse(h.deleteStatus, undefined));
      }
      // Repo secrets (issue #104): metadata-only list + write-only
      // create/rotate/delete. The fake server never stores or echoes a
      // value — same write-only discipline the real API enforces.
      if (url === `/api/v1/repos/${REPO_ID}/secrets` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { secrets: h.secretsOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/secrets` && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.secretRequestBodies.push(body);
        const created: RepoSecret = {
          id: `sec_${h.secretsOnServer.length + 1}`,
          name: String(body.name),
          description: String(body.description ?? ''),
          created_at: '2026-07-10T00:00:00.000Z',
          updated_at: '2026-07-10T00:00:00.000Z',
          exposed_run_id: null,
          exposed_at: null,
        };
        h.secretsOnServer = [...h.secretsOnServer, created];
        return Promise.resolve(jsonResponse(201, created));
      }
      const secretMatch = /^\/api\/v1\/repos\/repo_1\/secrets\/([^/]+)$/.exec(url);
      if (secretMatch && method === 'PATCH') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.secretRequestBodies.push(body);
        const id = secretMatch[1];
        // Rotate clears the exposure flag (RotateRepoSecret's doing, issue #108) —
        // the mock mirrors the real server so the refetch-clears-the-badge
        // behavior is exercisable here.
        const updated: RepoSecret = {
          ...(h.secretsOnServer.find((s) => s.id === id) as RepoSecret),
          updated_at: '2026-07-10T01:00:00.000Z',
          exposed_run_id: null,
          exposed_at: null,
        };
        h.secretsOnServer = h.secretsOnServer.map((s) => (s.id === id ? updated : s));
        return Promise.resolve(jsonResponse(200, updated));
      }
      if (secretMatch && method === 'DELETE') {
        const id = secretMatch[1];
        h.secretsOnServer = h.secretsOnServer.filter((s) => s.id !== id);
        return Promise.resolve(jsonResponse(204, undefined));
      }
      // Repo imports (issue #261): directional, consumer-declared read-only
      // snapshots. GET lists {id, name} sorted by name; POST is idempotent
      // (adding an existing import also 201s); self-import and an unknown
      // target both 400 — h.importPostError lets a test force that 400 past
      // the picker's own client-side filtering (see its doc comment).
      if (url === `/api/v1/repos/${REPO_ID}/imports` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { imports: h.importsOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/imports` && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.importPostBodies.push(body);
        if (h.importPostError !== null) {
          return Promise.resolve(jsonResponse(400, { error: h.importPostError }));
        }
        const targetID = String(body.target_repo_id);
        const target = h.reposOnServer.find((r) => r.id === targetID);
        if (targetID === REPO_ID) {
          return Promise.resolve(
            jsonResponse(400, { error: 'imports: a repository cannot import itself' }),
          );
        }
        if (target === undefined) {
          return Promise.resolve(
            jsonResponse(400, { error: `imports: unknown target repository "${targetID}"` }),
          );
        }
        const created: RepoImport = { id: target.id, name: target.name };
        if (!h.importsOnServer.some((imp) => imp.id === created.id)) {
          h.importsOnServer = [...h.importsOnServer, created].sort((a, b) =>
            a.name.localeCompare(b.name),
          );
        }
        return Promise.resolve(jsonResponse(201, created));
      }
      const importMatch = /^\/api\/v1\/repos\/repo_1\/imports\/([^/]+)$/.exec(url);
      if (importMatch && method === 'DELETE') {
        h.importDeleteRequests.push(url);
        const id = importMatch[1];
        h.importsOnServer = h.importsOnServer.filter((imp) => imp.id !== id);
        return Promise.resolve(jsonResponse(204, undefined));
      }
      // Credential gateway (issue #25): the lab-wide OneCLI project pool, this
      // repo's grant set over it, and the dashboard exposure the picker's
      // link-out is composed from. h.gatewayReadError turns the two READS into
      // the 502 the real server answers when OneCLI is configured but
      // erroring — "unconfigured" is a 200 with configured:false instead, and
      // conflating the two is exactly what the section must not do.
      if (url === '/api/v1/onecli/pool' && method === 'GET') {
        if (h.gatewayReadError !== null) {
          return Promise.resolve(jsonResponse(502, { error: h.gatewayReadError }));
        }
        return Promise.resolve(jsonResponse(200, { ...h.poolOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/onecli/grants` && method === 'GET') {
        if (h.gatewayReadError !== null) {
          return Promise.resolve(jsonResponse(502, { error: h.gatewayReadError }));
        }
        // One integration, one configured flag: the grants read reports the
        // same "is the gateway set up" answer the pool read does.
        return Promise.resolve(
          jsonResponse(200, { configured: h.poolOnServer.configured, grants: h.grantsOnServer }),
        );
      }
      // Built from REPO_ID rather than spelled out, so renaming the fixture
      // repo can never silently stop matching and turn every toggle into an
      // "unexpected fetch" the way a hardcoded id would.
      const grantMatch = new RegExp(
        `^/api/v1/repos/${REPO_ID}/onecli/grants/([^/]+)/([^/]+)$`,
      ).exec(url);
      if (grantMatch && (method === 'PUT' || method === 'DELETE')) {
        h.grantRequests.push(`${method} ${url}`);
        if (h.grantWriteError !== null) {
          return Promise.resolve(jsonResponse(400, { error: h.grantWriteError }));
        }
        const kind = (grantMatch[1] ?? '') as OneCLIGrantKind;
        const id = grantMatch[2] ?? '';
        if (method === 'PUT') {
          // Attach is idempotent (see attachRepoOneCLIGrant): a repeat PUT
          // still 204s and still leaves exactly one row.
          if (!h.grantsOnServer.some((g) => g.kind === kind && g.id === id)) {
            const half = kind === 'secrets' ? h.poolOnServer.secrets : h.poolOnServer.connections;
            const entry = half.find((e) => e.id === id);
            h.grantsOnServer = [...h.grantsOnServer, { kind, id, name: entry?.name ?? id }];
          }
        } else {
          h.grantsOnServer = h.grantsOnServer.filter((g) => !(g.kind === kind && g.id === id));
        }
        return Promise.resolve(jsonResponse(204, undefined));
      }
      if (url === '/api/v1/onecli/dashboard' && method === 'GET') {
        if (h.dashboardExposure === null) {
          return Promise.resolve(
            jsonResponse(502, { error: 'onecli: dashboard exposure unknown' }),
          );
        }
        return Promise.resolve(jsonResponse(200, { ...h.dashboardExposure }));
      }
      // Schedules (issue #247 / ADR-0062): per-repo cadence CRUD over the
      // mutable h.schedules array, plus the two read-only surfaces the editor
      // needs — the built-in flow catalog and the server-rendered cron
      // preview, which is the ONLY thing that ever says when a cadence fires.
      if (url === `/api/v1/repos/${REPO_ID}/schedules` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { schedules: h.schedules }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/schedules` && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.scheduleBodies.push(body);
        // A name is unique per repo — the real 409 the section must surface.
        if (h.schedules.some((s) => s.name === body.name)) {
          return Promise.resolve(jsonResponse(409, { error: 'name already taken' }));
        }
        const created = baseSchedule({
          id: `sched_${h.schedules.length + 1}`,
          name: String(body.name ?? ''),
          cadence: String(body.cadence ?? ''),
          prompt: String(body.prompt ?? ''),
          flows: (body.flows as string[] | undefined) ?? [],
          enabled: (body.enabled as boolean | undefined) ?? true,
        });
        h.schedules = [...h.schedules, created];
        return Promise.resolve(jsonResponse(201, created));
      }
      const reenableMatch = /^\/api\/v1\/repos\/repo_1\/schedules\/([^/]+)\/reenable$/.exec(url);
      if (reenableMatch && method === 'POST') {
        const id = reenableMatch[1];
        const fresh = {
          ...(h.schedules.find((s) => s.id === id) as Schedule),
          paused: false,
          consecutive_failures: 0,
        };
        h.schedules = h.schedules.map((s) => (s.id === id ? fresh : s));
        return Promise.resolve(jsonResponse(200, fresh));
      }
      const scheduleMatch = /^\/api\/v1\/repos\/repo_1\/schedules\/([^/]+)$/.exec(url);
      if (scheduleMatch && method === 'PATCH') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.scheduleBodies.push(body);
        const id = scheduleMatch[1];
        const updated = { ...(h.schedules.find((s) => s.id === id) as Schedule), ...body };
        h.schedules = h.schedules.map((s) => (s.id === id ? updated : s));
        return Promise.resolve(jsonResponse(200, updated));
      }
      if (scheduleMatch && method === 'DELETE') {
        const id = scheduleMatch[1];
        h.schedules = h.schedules.filter((s) => s.id !== id);
        return Promise.resolve(jsonResponse(204, undefined));
      }
      if (url === '/api/v1/schedule-flows' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { flows: SCHEDULE_FLOWS }));
      }
      if (url.startsWith('/api/v1/cron/preview?expr=') && method === 'GET') {
        const expr = decodeURIComponent(url.slice('/api/v1/cron/preview?expr='.length));
        h.cronPreviewExprs.push(expr);
        // Always a 200, valid or not — an unparseable expression is normal
        // while the operator is still typing one.
        if (expr === BAD_CRON) {
          return Promise.resolve(
            jsonResponse(200, {
              expr,
              valid: false,
              error: 'cron: expected 5 fields, got 1',
              next: null,
              next_display: null,
            }),
          );
        }
        return Promise.resolve(
          jsonResponse(200, {
            expr,
            valid: true,
            error: null,
            next: ['2026-08-03T06:00:00+02:00', '2026-08-06T06:00:00+02:00'],
            next_display: ['Mon 2026-08-03 06:00', 'Thu 2026-08-06 06:00'],
          }),
        );
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

export const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

/** Lets queued fetches resolve and Solid propagate the results. */
export async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flush();
}

export async function waitFor<T>(get: () => T | null, what: string): Promise<T> {
  for (let i = 0; i < 50; i += 1) {
    const value = get();
    if (value !== null) return value;
    await flush();
  }
  throw new Error(`timed out waiting for ${what}`);
}

/** Mounts the area route at `path` (default: the bare settings index). */
export async function mountSettings(path: string = `/repos/${REPO_ID}/settings`): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  routerHistory = createMemoryHistory();
  routerHistory.set({ value: path });
  dispose = render(
    () => (
      <MemoryRouter history={routerHistory} root={App}>
        <Route path="/repos/:id/settings/:section?" component={RepoSettingsRoute} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

/** Tear down the current mount — for tests that remount within one `it`
 *  (routes/settings/harness.tsx's precedent, same three lines). */
export function unmount(): void {
  dispose?.();
  dispose = undefined;
  container.remove();
}

export function input(name: string): HTMLInputElement {
  const el = container.querySelector<HTMLInputElement>(`input[name="${name}"]`);
  if (!el) throw new Error(`missing input[name="${name}"]`);
  return el;
}

export function textarea(name: string): HTMLTextAreaElement {
  const el = container.querySelector<HTMLTextAreaElement>(`textarea[name="${name}"]`);
  if (!el) throw new Error(`missing textarea[name="${name}"]`);
  return el;
}

export function button(text: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'));
  const el = buttons.find((b) => b.textContent?.trim() === text);
  if (!el) throw new Error(`missing button ${JSON.stringify(text)}`);
  return el;
}

export function typeInto(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/** A native <select> (the Integrations card) by its form name. */
export function nativeSelect(name: string): HTMLSelectElement {
  const el = container.querySelector<HTMLSelectElement>(`select[name="${name}"]`);
  if (!el) throw new Error(`missing select[name="${name}"]`);
  return el;
}

/** Picks a value on a native <select> and fires its change event. */
export function chooseNative(name: string, value: string): void {
  const el = nativeSelect(name);
  el.value = value;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

/** The unified Select trigger button (field skin) for a form field name. */
export function selectTrigger(name: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(`button[name="${name}"]`);
  if (!el) throw new Error(`missing select trigger button[name="${name}"]`);
  return el;
}

/** The label the named Select currently shows on its trigger. */
export function selectedLabel(name: string): string {
  return selectTrigger(name).querySelector('.select-field-label')?.textContent ?? '';
}

export function optionRows(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('[role="option"]'));
}

/** Opens the named Select and clicks the option with the given label. */
export async function chooseFromSelect(name: string, optionLabel: string): Promise<void> {
  selectTrigger(name).click();
  await settle();
  const row = optionRows().find(
    (r) => r.querySelector('.select-option-label')?.textContent === optionLabel,
  );
  if (!row) throw new Error(`missing option ${JSON.stringify(optionLabel)} in ${name}`);
  row.click();
  await settle();
}

export function toggleCheckbox(el: HTMLInputElement, checked: boolean): void {
  el.checked = checked;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

export function submitForm(): void {
  const form = container.querySelector('form');
  if (!form) throw new Error('missing settings form');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
}

/** The server-side push: repo.changed makes RepoSettingsView refetch. */
export function emitRepoChanged(): void {
  for (const source of FakeEventSource.instances) {
    source.emit('repo.changed', { repoID: REPO_ID });
  }
}

/** The Secrets section's <section> element, scoped for row/form queries.
 *  Scoped to `section h2`: the mobile back header renders its own 'Secrets'
 *  h2 OUTSIDE any section, which must never match here. */
export function secretsSection(): HTMLElement {
  const header = Array.from(container.querySelectorAll('section h2')).find(
    (h2) => h2.textContent === 'Secrets',
  );
  if (!header) throw new Error('missing Secrets section heading');
  const section = header.closest('section');
  if (!section) throw new Error('Secrets heading has no enclosing <section>');
  return section as HTMLElement;
}

/** The credential-gateway grant picker's <section> (issue #25), scoped the
 *  same way secretsSection() is — it renders ABOVE the legacy Secrets card on
 *  the same subpage, so the two must never be queried as one. */
export function grantsSection(): HTMLElement {
  const header = Array.from(container.querySelectorAll('section h2')).find(
    (h2) => h2.textContent === 'Credential gateway',
  );
  if (!header) throw new Error('missing Credential gateway section heading');
  const section = header.closest('section');
  if (!section) throw new Error('Credential gateway heading has no enclosing <section>');
  return section as HTMLElement;
}

/** The Schedules section's <section>, scoped the same way secretsSection() is. */
export function schedulesSection(): HTMLElement {
  const header = Array.from(container.querySelectorAll('section h2')).find(
    (h2) => h2.textContent === 'Schedules',
  );
  if (!header) throw new Error('missing Schedules section heading');
  const section = header.closest('section');
  if (!section) throw new Error('Schedules heading has no enclosing <section>');
  return section as HTMLElement;
}

/** The Imports section's <section>, scoped the same way secretsSection() is. */
export function importsSection(): HTMLElement {
  const header = Array.from(container.querySelectorAll('section h2')).find(
    (h2) => h2.textContent === 'Imports',
  );
  if (!header) throw new Error('missing Imports section heading');
  const section = header.closest('section');
  if (!section) throw new Error('Imports heading has no enclosing <section>');
  return section as HTMLElement;
}

/**
 * Waits out the cadence editor's preview debounce and lets the resulting
 * request land. Real timers on purpose: the debounce is the behavior under
 * test, and faking the clock here would also fake the harness's own flush.
 */
export async function settlePreview(): Promise<void> {
  await new Promise<void>((resolve) => setTimeout(resolve, 450));
  await settle();
}

/** Submits the (single) form inside root — scoped so it never hits a sibling form. */
export function submitFormWithin(root: ParentNode): void {
  const form = root.querySelector('form');
  if (!form) throw new Error('missing form within scope');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
}

// SettingsLayout's desktop breakpoint — must byte-match the query it builds
// (the AppShell DESKTOP_MIN_PX shell breakpoint).
export const DESKTOP_QUERY = '(min-width: 1024px)';

// jsdom has no window.matchMedia — install a fake resolving the queries the
// app probes (SettingsLayout's desktop breakpoint here). Each query gets ONE
// persistent MediaQueryList whose `matches` flips via set() — dispatching
// 'change' to whatever listeners the component registered — so a test can
// cross the breakpoint live. Everything starts false (mobile), keeping the
// untouched tests valid. vi.stubGlobal ties the mock's lifetime to
// vi.unstubAllGlobals() in afterEach; the memo below is cleared there
// alongside it.
let mediaStub: { set: (query: string, matches: boolean) => void } | undefined;

export function stubMatchMedia(): { set: (query: string, matches: boolean) => void } {
  if (mediaStub !== undefined) return mediaStub;
  type Entry = { mql: { matches: boolean } & Record<string, unknown>; listeners: Set<() => void> };
  const entries = new Map<string, Entry>();
  const entry = (query: string): Entry => {
    let e = entries.get(query);
    if (e === undefined) {
      const listeners = new Set<() => void>();
      e = {
        listeners,
        mql: {
          matches: false,
          media: query,
          addEventListener: (type: string, listener: () => void) => {
            if (type === 'change') listeners.add(listener);
          },
          removeEventListener: (_type: string, listener: () => void) => {
            listeners.delete(listener);
          },
          addListener: () => {},
          removeListener: () => {},
          onchange: null,
          dispatchEvent: () => false,
        },
      };
      entries.set(query, e);
    }
    return e;
  };
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => entry(query).mql),
  );
  mediaStub = {
    set: (query, matches) => {
      const e = entry(query);
      e.mql.matches = matches;
      for (const listener of e.listeners) listener();
    },
  };
  return mediaStub;
}

/** Flips the desktop breakpoint (installs the matchMedia stub on first use). */
export function setDesktop(matches: boolean): void {
  stubMatchMedia().set(DESKTOP_QUERY, matches);
}

export function installRepoSettingsHooks(): void {
  beforeEach(() => {
    h.repoOnServer = baseRepo();
    h.reposOnServer = [baseRepo(), ...otherRepos()];
    h.providersOnServer = baseProviders();
    h.settingsOnServer = {
      provider_default: 'claude-code',
      // The container resource-limit defaults (issue #205), seeded server-side —
      // the Runner section's inherit hints read these.
      container_memory: '8g',
      container_pids: 4096,
      container_nofile: 16384,
    };
    h.credentialsOnServer = [];
    h.patchBodies = [];
    h.secretsOnServer = [];
    h.secretRequestBodies = [];
    h.deleteRequests = [];
    h.deleteStatus = 204;
    h.schedules = [];
    h.scheduleBodies = [];
    h.cronPreviewExprs = [];
    h.importsOnServer = [];
    h.importPostBodies = [];
    h.importPostError = null;
    h.importDeleteRequests = [];
    h.poolOnServer = baseOneCLIPool();
    h.grantsOnServer = [];
    h.grantRequests = [];
    // An exposed dashboard by default, so the picker's link-out renders on the
    // normal path; `mode: 'off'` and `null` are the two opt-in alternatives.
    h.dashboardExposure = { mode: 'port', url: 'https://lab.example.com:8443' };
    h.gatewayReadError = null;
    h.grantWriteError = null;
    stubApi();
  });

  afterEach(() => {
    dispose?.();
    dispose = undefined;
    container.remove();
    FakeEventSource.instances = [];
    vi.unstubAllGlobals();
    mediaStub = undefined; // the stub it memoized is gone with unstubAllGlobals
    vi.restoreAllMocks();
  });
}
