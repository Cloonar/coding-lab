// Shared test harness for the repo-settings area suites (issues #198, #61),
// the runchat/harness.tsx precedent applied to the RepoSettings.test.tsx stub
// server and DOM helpers. Section suites mount the real repositories route
// table (routes/repo-home/routes.tsx) at /repos/:id/settings/:section? (App
// root, MemoryRouter) — so the one-page settings render inside the real repo
// home frame, with its form store, save bar and leave guard — and poke the
// mutable `h` state object to shape server responses.
//
// POST /repos/:id/inherited is answered by fakeInherited() below: a stand-in
// for the server's resolvers that walks the same chains over the fixture
// repo, the drafts sent along, `h.settingsOnServer` and `h.providersOnServer`
// — so a suite shapes what a field inherits by shaping those, the way it
// shapes any other answer. (The app itself never walks a chain; this is the
// fake SERVER.)
//
// PATCH /repos/:id applies the server's own PAIR checks (pairRefusal() below,
// after reposvc.UpdateSettings): a forge binding needs a forge credential,
// the AFK branch pattern and the manual branch prefix must not overlap, and
// Autoland needs the forge binding — each refused with the server's message
// at the field the server pins it to. So a fixture that sends a pair the real
// server would refuse is refused here too.
//
// The page's layout seam (scrolling.ts `viewport`) is replaced by a fake for
// every test: jsdom has no layout, so `h.tops` says where each section sits
// and `h.scrolls` records where the page scrolled to.

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
  RepoInherited,
  RepoSecret,
  RepoSSHTarget,
  Runner,
  Schedule,
} from '../../api';
import App from '../../App';
import { providerFor, resolveRemote, resolveSpawnOption } from '../../lib/spawn';
import RepoRoutes from '../repo-home/routes';
import { INHERITED_DEBOUNCE_MS } from './form';
import { viewport, type Viewport } from './scrolling';

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

/**
 * The repo's view of Warpgate's SSH targets (issue #39), as GET
 * /repos/{id}/warpgate/targets answers it in one body — unlike the OneCLI
 * pool, there is no separate lab-wide list to join against a grant set.
 * Configured and non-empty by default, one assigned and one not, so both
 * toggle states render on the normal path — every other Secrets-section suite
 * mounts this picker alongside SecretGrants and the legacy Secrets card.
 */
export function baseSSHTargets(): RepoSSHTarget[] {
  return [
    { id: 'tgt_1', name: 'staging', description: 'Staging box', assigned: true },
    { id: 'tgt_2', name: 'prod-db', description: 'Prod DB jump host', assigned: false },
  ];
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
    summary: {
      claimable: null,
      open_issues: null,
      readiness: { state: 'passing', checks: [] },
    },
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
    next_run_at: null,
    next_run_display: null,
    last_run: null,
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
  /** Forces the next repo PATCH to be refused with this answer (400 unless
   *  `status` says otherwise); `field` names the offending PATCH key the way
   *  the server does (issue #61). The body is still recorded in patchBodies. */
  patchRefusal: { error: string; field?: string; status?: number } | null;
  /** Makes every repo PATCH fail like a dropped connection (fetch rejects). */
  patchOffline: boolean;
  /** When set, a repo PATCH answers only once this promise resolves — a test
   *  holds a save in flight with it. */
  patchHold: Promise<void> | null;
  /** Every POST /repos/:id/inherited body (the drafts sent along), in order. */
  inheritedBodies: Record<string, unknown>[];
  /** Entries laid over fakeInherited()'s answer — e.g. `{ model_default: null }`
   *  for a chain the server could not resolve. */
  inheritedPatch: Partial<RepoInherited>;
  /** Makes POST /repos/:id/inherited answer 500 with this message. */
  inheritedError: string | null;
  /** When set, called for every inherited request with its number (1-based):
   *  a returned promise holds THAT request's answer until it resolves — how a
   *  test lets an older request finish after a newer one. */
  inheritedGate: ((request: number) => Promise<void> | undefined) | null;
  /** The fake layout (see installRepoSettingsHooks): the top edge of each
   *  element, in px from the viewport top, by element id. A section without
   *  an entry sits far below the fold. */
  tops: Record<string, number>;
  /** The fake layout's "the page is scrolled to its end". */
  pageAtEnd: boolean;
  /** How often the page read an element's position from the fake layout. */
  layoutReads: number;
  /** Makes GET /providers answer 500 with this message. */
  providersError: string | null;
  /** Makes GET /credentials answer 500 with this message. */
  credentialsError: string | null;
  /** Number of GET /providers and GET /credentials requests so far. */
  providersGets: number;
  credentialsGets: number;
  /** Every scroll the page asked for, in order: the target element's id (a
   *  section) or its data-field (a field), the offset, and whether animated. */
  scrolls: { target: string; offset: number; smooth: boolean }[];
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
  /** Makes the next schedule POST/PATCH answer 400 with this refusal — with
   *  `field`, the key the editor shows it under. */
  scheduleRefusal: { error: string; field?: string } | null;
  /** Schedule ids of every POST …/schedules/:sid/run, in order. */
  runNowRequests: string[];
  /** Makes Run now answer 409 with this reason (verbatim) instead of 202. */
  runNowRefusal: string | null;
  /** Makes Run now fail with another status (a 500, say) and this message. */
  runNowError: { status: number; error: string } | null;
  /** While set, every schedule write (POST, PATCH, DELETE, re-enable, Run
   *  now) is answered only once this resolves — how a test acts while a
   *  request is in flight. The request is recorded at once. */
  scheduleHold: Promise<void> | null;
  /** While set, GET …/schedules is answered only once this resolves. */
  schedulesHold: Promise<void> | null;
  /** Makes GET …/schedules answer 500 with this message. */
  schedulesGetError: string | null;
  /** Makes every secret write (POST, PATCH, DELETE) answer 400 with this
   *  refusal — with `field`, the key the row shows it under. */
  secretRefusal: { error: string; field?: string } | null;
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
  /** GET /repos/{id}/warpgate/targets's `configured` flag (issue #39): false
   *  is the "SSH bastion integration is off in this lab" state. */
  sshTargetsConfigured: boolean;
  /** This repo's Warpgate SSH targets, each already carrying its own
   *  `assigned` flag — mutated by the stub's own assign/unassign handlers. */
  sshTargetsOnServer: RepoSSHTarget[];
  /** `"<METHOD> <url>"` for every target assign/unassign, in order. */
  sshTargetRequests: string[];
  /** Forces GET .../warpgate/targets to answer 502 with this message — the
   *  one read this picker allows to degrade to an error banner. */
  sshTargetsReadError: string | null;
  /** Forces every target PUT/DELETE to answer 400 with this message — the
   *  server-side refusal a toggle must surface without flipping the row. */
  sshTargetWriteError: string | null;
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
        h.credentialsGets += 1;
        if (h.credentialsError !== null) {
          return Promise.resolve(jsonResponse(500, { error: h.credentialsError }));
        }
        return Promise.resolve(jsonResponse(200, { credentials: h.credentialsOnServer }));
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        h.providersGets += 1;
        if (h.providersError !== null) {
          return Promise.resolve(jsonResponse(500, { error: h.providersError }));
        }
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
        if (h.patchOffline) return Promise.reject(new TypeError('Failed to fetch'));
        const answer = () => {
          const refusal = h.patchRefusal;
          if (refusal !== null) {
            const { status, ...body } = refusal;
            return jsonResponse(status ?? 400, body);
          }
          // The pairs the server checks as a whole, refused as it refuses them.
          const pair = pairRefusal(h.repoOnServer, patch);
          if (pair !== null) return jsonResponse(400, pair);
          h.repoOnServer = { ...h.repoOnServer, ...patch };
          return jsonResponse(200, { ...h.repoOnServer });
        };
        return h.patchHold !== null ? h.patchHold.then(answer) : Promise.resolve(answer());
      }
      // Inherited values (issue #61): what each overridable field resolves to
      // while the repo's own value is null, for the drafts sent along.
      if (url === `/api/v1/repos/${REPO_ID}/inherited` && method === 'POST') {
        const drafts = JSON.parse(String(init?.body ?? '{}')) as Record<string, unknown>;
        h.inheritedBodies.push(drafts);
        const answer = () =>
          h.inheritedError !== null
            ? jsonResponse(500, { error: h.inheritedError })
            : jsonResponse(200, { ...fakeInherited(drafts), ...h.inheritedPatch });
        const held = h.inheritedGate?.(h.inheritedBodies.length);
        return held !== undefined ? held.then(answer) : Promise.resolve(answer());
      }
      // The repo home's other tabs (issue #61), for the suites that carry
      // pending changes across them: Overview reads the readiness report, the
      // instance list (below) and parked work; Issues its list, the ready
      // queue and the labels.
      if (url === `/api/v1/repos/${REPO_ID}/readiness` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { state: 'passing', checks: [] }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/parked` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { parked: [] }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/issues?state=open` && method === 'GET') {
        return Promise.resolve(
          jsonResponse(200, { binding: h.repoOnServer.tracker_binding, issues: [] }),
        );
      }
      if (url === `/api/v1/repos/${REPO_ID}/ready` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { issues: [] }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/labels` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { labels: [] }));
      }
      // The delete dialog's importer lookup (Danger zone).
      if (url === `/api/v1/repos/${REPO_ID}/importers` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { importers: [] }));
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
      if (
        h.secretRefusal !== null &&
        method !== 'GET' &&
        url.startsWith(`/api/v1/repos/${REPO_ID}/secrets`)
      ) {
        if (init?.body !== undefined && init.body !== null) {
          h.secretRequestBodies.push(JSON.parse(String(init.body)) as Record<string, unknown>);
        }
        return Promise.resolve(jsonResponse(400, { ...h.secretRefusal }));
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
      // SSH-bastion targets (issue #39): the repo's view of Warpgate's SSH
      // targets, each already flagged with the repo's assignment — one read,
      // not a pool+grants join. h.sshTargetsReadError turns the read into the
      // 502 the real server answers when Warpgate is configured but erroring;
      // "unconfigured" is a 200 with configured:false instead.
      if (url === `/api/v1/repos/${REPO_ID}/warpgate/targets` && method === 'GET') {
        if (h.sshTargetsReadError !== null) {
          return Promise.resolve(jsonResponse(502, { error: h.sshTargetsReadError }));
        }
        return Promise.resolve(
          jsonResponse(200, { configured: h.sshTargetsConfigured, targets: h.sshTargetsOnServer }),
        );
      }
      const sshTargetMatch = new RegExp(`^/api/v1/repos/${REPO_ID}/warpgate/targets/([^/]+)$`).exec(
        url,
      );
      if (sshTargetMatch && (method === 'PUT' || method === 'DELETE')) {
        h.sshTargetRequests.push(`${method} ${url}`);
        if (h.sshTargetWriteError !== null) {
          return Promise.resolve(jsonResponse(400, { error: h.sshTargetWriteError }));
        }
        const id = sshTargetMatch[1] ?? '';
        h.sshTargetsOnServer = h.sshTargetsOnServer.map((t) =>
          t.id === id ? { ...t, assigned: method === 'PUT' } : t,
        );
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
        const list = () =>
          h.schedulesGetError !== null
            ? jsonResponse(500, { error: h.schedulesGetError })
            : jsonResponse(200, { schedules: h.schedules });
        return h.schedulesHold !== null ? h.schedulesHold.then(list) : Promise.resolve(list());
      }
      // A schedule write is recorded at once and answered — applied, too —
      // only after h.scheduleHold, when a test holds one.
      const held = (answer: () => ReturnType<typeof jsonResponse>) =>
        h.scheduleHold !== null ? h.scheduleHold.then(answer) : Promise.resolve(answer());
      if (url === `/api/v1/repos/${REPO_ID}/schedules` && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.scheduleBodies.push(body);
        return held(() => {
          if (h.scheduleRefusal !== null) return jsonResponse(400, { ...h.scheduleRefusal });
          // A name is unique per repo — the real 409 the section must surface.
          if (h.schedules.some((s) => s.name === body.name)) {
            return jsonResponse(409, { error: 'name already taken', field: 'name' });
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
          return jsonResponse(201, created);
        });
      }
      // Run now (issue #61): 202 with the run, or the server's refusal as a
      // 409 whose message is the reason. Never moves the cadence.
      const runNowMatch = /^\/api\/v1\/repos\/repo_1\/schedules\/([^/]+)\/run$/.exec(url);
      if (runNowMatch && method === 'POST') {
        const id = runNowMatch[1] ?? '';
        h.runNowRequests.push(id);
        return held(() => {
          if (h.runNowError !== null) {
            return jsonResponse(h.runNowError.status, { error: h.runNowError.error });
          }
          if (h.runNowRefusal !== null) return jsonResponse(409, { error: h.runNowRefusal });
          return jsonResponse(202, {
            run: {
              id: `run_now_${h.runNowRequests.length}`,
              repo_id: REPO_ID,
              kind: 'scheduled',
              provider: 'claude-code',
              issue_number: null,
              pr_number: null,
              outcome: 'active',
              failure_reason: null,
              schedule_id: id,
            },
          });
        });
      }
      const reenableMatch = /^\/api\/v1\/repos\/repo_1\/schedules\/([^/]+)\/reenable$/.exec(url);
      if (reenableMatch && method === 'POST') {
        const id = reenableMatch[1];
        return held(() => {
          const fresh = {
            ...(h.schedules.find((s) => s.id === id) as Schedule),
            paused: false,
            consecutive_failures: 0,
          };
          h.schedules = h.schedules.map((s) => (s.id === id ? fresh : s));
          return jsonResponse(200, fresh);
        });
      }
      const scheduleMatch = /^\/api\/v1\/repos\/repo_1\/schedules\/([^/]+)$/.exec(url);
      if (scheduleMatch && method === 'PATCH') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.scheduleBodies.push(body);
        const id = scheduleMatch[1];
        return held(() => {
          if (h.scheduleRefusal !== null) return jsonResponse(400, { ...h.scheduleRefusal });
          const updated = { ...(h.schedules.find((s) => s.id === id) as Schedule), ...body };
          h.schedules = h.schedules.map((s) => (s.id === id ? updated : s));
          return jsonResponse(200, updated);
        });
      }
      if (scheduleMatch && method === 'DELETE') {
        const id = scheduleMatch[1];
        return held(() => {
          if (h.scheduleRefusal !== null) return jsonResponse(400, { ...h.scheduleRefusal });
          h.schedules = h.schedules.filter((s) => s.id !== id);
          return jsonResponse(204, undefined);
        });
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

/** gitx.patternsOverlap: some rendered AFK branch starts with the manual prefix. */
function patternsOverlap(pattern: string, manualPrefix: string): boolean {
  const at = pattern.indexOf('<N>');
  const prefix = pattern.slice(0, at);
  const suffix = pattern.slice(at + '<N>'.length);
  if (prefix.startsWith(manualPrefix)) return true;
  if (!manualPrefix.startsWith(prefix)) return false;
  const rest = manualPrefix.slice(prefix.length);
  const digits = /^[0-9]*/.exec(rest)?.[0] ?? '';
  const tail = rest.slice(digits.length);
  if (digits === '' || digits.startsWith('0')) return false;
  if (tail === '') return true;
  for (let j = 1; j <= digits.length; j += 1) {
    if (suffix.startsWith(rest.slice(j))) return true;
  }
  return false;
}

/**
 * The fake server's pair checks on PATCH /repos/:id, in reposvc's order and
 * with its messages and field pins: each pair is checked on the values that
 * WOULD result, only when the request touches one of its halves, and the
 * refusal names the key the request sent (see reposvc.UpdateSettings).
 */
export function pairRefusal(
  current: Repo,
  patch: Record<string, unknown>,
): { error: string; field: string } | null {
  const sent = (key: string): boolean => Object.hasOwn(patch, key);
  const next = { ...current, ...patch } as Repo;
  if (
    (sent('tracker_binding') || sent('forge_credential_id')) &&
    next.tracker_binding === 'forge' &&
    next.forge_credential_id === null
  ) {
    return {
      field: sent('tracker_binding') ? 'tracker_binding' : 'forge_credential_id',
      error:
        'tracker_binding: "forge" requires a forge_token credential (set forge_credential_id or use "builtin")',
    };
  }
  if (
    (sent('afk_branch_pattern') || sent('manual_branch_prefix')) &&
    next.afk_branch_pattern.split('<N>').length === 2 &&
    patternsOverlap(next.afk_branch_pattern, next.manual_branch_prefix)
  ) {
    return {
      field: sent('afk_branch_pattern') ? 'afk_branch_pattern' : 'manual_branch_prefix',
      error: `afk branch pattern "${next.afk_branch_pattern}" and manual branch prefix "${next.manual_branch_prefix}" overlap: a branch could match both`,
    };
  }
  if (
    (sent('autoland_enabled') || sent('tracker_binding')) &&
    next.autoland_enabled &&
    next.tracker_binding !== 'forge'
  ) {
    return {
      field: sent('autoland_enabled') ? 'autoland_enabled' : 'tracker_binding',
      error: 'autoland_enabled: requires a forge tracker binding',
    };
  }
  return null;
}

/**
 * The fake server's answer to POST /repos/:id/inherited: for every
 * overridable field, what it resolves to with the repo's OWN value nulled —
 * the chains of internal/httpapi/inherited.go, walked over the fixture repo
 * (with the request's drafts laid over it), h.settingsOnServer and
 * h.providersOnServer. Defaults the real server seeds stand in where the
 * settings fixture says nothing (budget 120, cap 6, limits 8g/4096/16384).
 */
export function fakeInherited(drafts: Record<string, unknown> = {}): RepoInherited {
  const repo = { ...h.repoOnServer, ...drafts } as Repo;
  const settings = h.settingsOnServer;
  const text = (key: string): string | undefined => {
    const value = settings[key];
    return typeof value === 'string' && value !== '' ? value : undefined;
  };
  const flag = (key: string): boolean | null | undefined =>
    settings[key] as boolean | null | undefined;
  const num = (key: string, fallback: number): number =>
    typeof settings[key] === 'number' ? settings[key] : fallback;
  const providers = h.providersOnServer;

  // The agent per run class, with and without the repo's own pick.
  const manualBelow = providerFor(providers, text('provider_default'));
  const manual = providerFor(providers, repo.provider, text('provider_default'));
  const afkBelow = providerFor(
    providers,
    text('spawn_provider_default_afk'),
    repo.provider,
    text('provider_default'),
  );
  const afk = providerFor(
    providers,
    repo.afk_provider_default,
    text('spawn_provider_default_afk'),
    repo.provider,
    text('provider_default'),
  );
  const lander = providerFor(
    providers,
    repo.lander_provider,
    repo.provider,
    text('provider_default'),
  );
  const runner = settings['runner_default'];
  const bag = (settings['spawn_options_afk'] ?? {}) as Record<string, string>;
  const landerModel =
    lander === null
      ? null
      : resolveSpawnOption(
          lander.models,
          text('spawn_model_default_lander'),
          repo.model_default,
          text('spawn_model_default'),
        );
  const landerEfforts = (provider: Provider, model: string | null): Provider['efforts'] => {
    const own = provider.models.find((candidate) => candidate.value === model)?.efforts ?? [];
    return own.length > 0 ? own : provider.efforts;
  };

  return {
    provider: manualBelow?.id ?? null,
    afk_provider_default: afkBelow?.id ?? null,
    lander_provider: manual?.id ?? null,
    model_default:
      manual === null ? null : resolveSpawnOption(manual.models, text('spawn_model_default')),
    effort_default:
      manual === null ? null : resolveSpawnOption(manual.efforts, text('spawn_effort_default')),
    afk_model_default:
      afk === null
        ? null
        : resolveSpawnOption(
            afk.models,
            text('spawn_model_default_afk'),
            repo.model_default,
            text('spawn_model_default'),
          ),
    afk_effort_default:
      afk === null
        ? null
        : resolveSpawnOption(
            afk.efforts,
            text('spawn_effort_default_afk'),
            repo.effort_default,
            text('spawn_effort_default'),
          ),
    lander_model: landerModel,
    // Resolved against the model the launch requests (afk.LanderModelEffort):
    // a model with an effort list of its own answers from that list.
    lander_effort:
      lander === null
        ? null
        : resolveSpawnOption(
            landerEfforts(lander, repo.lander_model ?? landerModel),
            text('spawn_effort_default_lander'),
            repo.effort_default,
            text('spawn_effort_default'),
          ),
    // Clamped by the provider's capability, as the spawn clamps it.
    remote_default:
      manual === null
        ? null
        : manual.supports_remote && resolveRemote(flag('spawn_remote_default')),
    afk_remote_default:
      afk === null
        ? null
        : afk.supports_remote &&
          resolveRemote(
            flag('spawn_remote_default_afk'),
            repo.remote_default,
            flag('spawn_remote_default'),
          ),
    afk_options:
      afk === null
        ? null
        : Object.fromEntries(
            afk.options.filter((o) => o.key in bag).map((o) => [o.key, String(bag[o.key])]),
          ),
    budget_minutes: num('afk_budget_minutes', 120),
    max_instances_override: num('max_instances', 6),
    git_author_name: text('git_author_name') ?? '',
    git_author_email: text('git_author_email') ?? '',
    runner: runner === 'host' || runner === 'container' ? (runner as Runner) : null,
    image_ref: text('dev_image_default') ?? text('dev_image_fallback') ?? '',
    container_memory: text('container_memory') ?? '8g',
    container_pids: num('container_pids', 4096),
    container_nofile: num('container_nofile', 16384),
  };
}

export const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

/**
 * Waits out the debounce between an edit of a field other fields' chains
 * read and the request for the inherited values it changes, then lets that
 * request land. Real timers, like settlePreview(): the debounce is behavior.
 */
export async function settleInherited(): Promise<void> {
  await new Promise<void>((resolve) => setTimeout(resolve, INHERITED_DEBOUNCE_MS + 40));
  await settle();
}

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
        <RepoRoutes />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
  // The Settings tab asks for the inherited values as soon as the repo has
  // loaded; let that first answer land, so a test starts from a settled page
  // (a held or failing request is simply not waited for).
  if (path.includes('/settings')) {
    for (let i = 0; i < 20 && h.inheritedBodies.length === 0; i += 1) await flush();
    await settle();
  }
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

/** A button by its visible text, or — an icon-only control — by its accessible name. */
export function button(text: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'));
  const el =
    buttons.find((b) => b.textContent?.trim() === text) ??
    buttons.find((b) => b.getAttribute('aria-label') === text);
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

/** A switch (role="switch") by its form name. */
export function switchButton(name: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(`button[role="switch"][name="${name}"]`);
  if (!el) throw new Error(`missing switch button[name="${name}"]`);
  return el;
}

/** Whether the named switch is on. */
export function switchOn(name: string): boolean {
  return switchButton(name).getAttribute('aria-checked') === 'true';
}

/** Flips the named switch to `on` (a click, when it is not there already). */
export function setSwitch(name: string, on: boolean): void {
  if (switchOn(name) !== on) switchButton(name).click();
}

/** One segment (role="radio") of a segmented control, by form name and value. */
export function segment(name: string, value: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(
    `button[role="radio"][name="${name}"][value="${value}"]`,
  );
  if (!el) throw new Error(`missing segment button[name="${name}"][value="${value}"]`);
  return el;
}

/** The checked value of a segmented control. */
export function segmentValue(name: string): string | null {
  return (
    container
      .querySelector<HTMLButtonElement>(`button[role="radio"][name="${name}"][aria-checked="true"]`)
      ?.getAttribute('value') ?? null
  );
}

// --- the one-page settings (issue #61) -------------------------------------------

/** The page-level <section> of a settings section, by slug. */
export function pageSection(slug: string): HTMLElement {
  const el = container.querySelector<HTMLElement>(`section#settings-${slug}`);
  if (!el) throw new Error(`missing settings section "${slug}"`);
  return el;
}

/** A field's wrapper (label, control, problem, hint), by its PATCH key. */
export function fieldWrapper(key: string): HTMLElement {
  const el = container.querySelector<HTMLElement>(`[data-field="${key}"]`);
  if (!el) throw new Error(`missing field "${key}"`);
  return el;
}

/** Whether a field carries the changed mark: the dot's class AND its words. */
export function fieldChanged(key: string): boolean {
  const wrapper = fieldWrapper(key);
  const marked = wrapper.classList.contains('changed');
  const worded = wrapper.textContent?.includes('(unsaved change)') === true;
  if (marked !== worded) throw new Error(`field "${key}": changed dot and words disagree`);
  return marked;
}

/** The problem shown under a field, or null. */
export function fieldError(key: string): string | null {
  return fieldWrapper(key).querySelector('.sfield-error')?.textContent ?? null;
}

/** The hint under a field ('' when it has none). */
export function fieldHint(key: string): string {
  return fieldWrapper(key).querySelector('.sfield-hint')?.textContent ?? '';
}

/** An overridable field's state at its label: 'inherited', 'set here', or
 *  null for a field that cannot inherit. */
export function fieldState(key: string): string | null {
  return fieldWrapper(key).querySelector('.sfield-state')?.textContent ?? null;
}

/** The "Default: …" text under a field set here (null when it shows none). */
export function fieldDefault(key: string): string | null {
  return fieldWrapper(key).querySelector('.sfield-default small')?.textContent ?? null;
}

/** A field's Reset action, or null while the field is inherited. */
export function resetButton(key: string): HTMLButtonElement | null {
  return fieldWrapper(key).querySelector<HTMLButtonElement>('.sfield-default button');
}

/** The labels of a segmented control's segments, in order. */
export function segmentLabels(name: string): string[] {
  return Array.from(
    container.querySelectorAll<HTMLButtonElement>(`button[role="radio"][name="${name}"]`),
  ).map((b) => b.textContent ?? '');
}

/** The frame's save bar, or null while nothing is pending. */
export function saveBar(): HTMLElement | null {
  return container.querySelector<HTMLElement>('.settings-savebar');
}

function saveBarButton(text: string): HTMLButtonElement {
  const bar = saveBar();
  if (!bar) throw new Error(`no save bar: nothing is pending (wanted its "${text}" button)`);
  const el = Array.from(bar.querySelectorAll('button')).find((b) => b.textContent?.trim() === text);
  if (!el) throw new Error(`missing save bar button ${JSON.stringify(text)}`);
  return el;
}

/** The save bar's headline ("3 unsaved changes" / "1 problem to fix"). */
export function saveBarTitle(): string {
  return saveBar()?.querySelector('strong')?.textContent ?? '';
}

/** The section names the save bar links to, in order. */
export function saveBarSections(): string[] {
  return Array.from(saveBar()?.querySelectorAll('a.settings-savebar-link') ?? []).map(
    (a) => a.textContent ?? '',
  );
}

/** Clicks the save bar's Save and lets the request land. */
export async function save(): Promise<void> {
  saveBarButton('Save').click();
  await settle();
}

/** Clicks the save bar's Discard. */
export async function discard(): Promise<void> {
  saveBarButton('Discard').click();
  await settle();
}

/** The frame's toast text ('' when none shows). */
export function toastText(): string {
  return container.querySelector('.toast')?.textContent ?? '';
}

/** The in-page dialog that is open, or null. */
export function openDialog(): HTMLElement | null {
  // The Dialog primitive's panel (the leave guard, the delete confirmation);
  // the schedule editor is a dialog of its own (scheduleEditor()).
  return container.querySelector<HTMLElement>(
    '.dialog[role="dialog"], .dialog[role="alertdialog"]',
  );
}

/** Follows an in-app link the way a click does (the router intercepts it). */
export async function followLink(link: Element | null | undefined): Promise<void> {
  if (!(link instanceof HTMLElement)) throw new Error('missing link to follow');
  link.click();
  await settle();
}

/** A tab of the repo home frame, by its visible name. */
export function repoTab(name: string): HTMLAnchorElement {
  const el = Array.from(container.querySelectorAll<HTMLAnchorElement>('nav.repo-tabs a')).find(
    (a) => a.textContent?.trim().startsWith(name),
  );
  if (!el) throw new Error(`missing repo tab ${JSON.stringify(name)}`);
  return el;
}

/** Scrolls the fake page: sets where the sections sit, then fires `scroll`. */
export async function scrollPage(tops: Record<string, number>, atEnd = false): Promise<void> {
  h.tops = tops;
  h.pageAtEnd = atEnd;
  window.dispatchEvent(new Event('scroll'));
  await settle();
}

/** The server-side push: repo.changed makes RepoSettingsView refetch. */
export function emitRepoChanged(): void {
  for (const source of FakeEventSource.instances) {
    source.emit('repo.changed', { repoID: REPO_ID });
  }
}

/** The server-side push for a run of this repo starting or ending. */
export function emitRunChanged(): void {
  for (const source of FakeEventSource.instances) {
    source.emit('run.changed', { repoID: REPO_ID, runID: 'run_x' });
  }
}

/**
 * A card (`section.card`) inside the page, by its own heading. The page-level
 * section headings (issue #61) are `section.settings-section > header h2` and
 * never match: "Secrets" names both the page section — which also holds the
 * credential-gateway and SSH-target pickers — and the legacy secrets card in
 * it, and these helpers mean the card.
 */
function sectionCard(title: string): HTMLElement {
  const header = Array.from(container.querySelectorAll('section.card h2')).find(
    (h2) => h2.textContent === title,
  );
  if (!header) throw new Error(`missing ${title} card heading`);
  const section = header.closest('section.card');
  if (!section) throw new Error(`${title} heading has no enclosing card`);
  return section as HTMLElement;
}

/** A page section's own card (issue #61): the list under the heading. */
function listCard(slug: string, className: string): HTMLElement {
  const card = pageSection(slug).querySelector<HTMLElement>(`section.card.${className}`);
  if (!card) throw new Error(`missing .${className} card in ${slug}`);
  return card;
}

/** The secrets list (issue #104), scoped for row/form queries. */
export function secretsSection(): HTMLElement {
  return listCard('secrets', 'secrets-list');
}

/** The credential-gateway grant picker's card (issue #25), scoped for row/form queries. */
export function grantsSection(): HTMLElement {
  return sectionCard('Credential gateway');
}

/** The SSH-targets picker's card (issue #39), scoped for row/form queries. */
export function sshTargetsSection(): HTMLElement {
  return sectionCard('SSH targets');
}

/** The schedules list, scoped for row queries. */
export function schedulesSection(): HTMLElement {
  return listCard('schedules', 'schedules-list');
}

/** The schedule editor (sections/ScheduleEditor.tsx), or null while closed. */
export function scheduleEditor(): HTMLElement | null {
  return container.querySelector<HTMLElement>('.schedule-editor');
}

/** The imports list, scoped for row/form queries. */
export function importsSection(): HTMLElement {
  return listCard('imports', 'imports-list');
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

// The desktop breakpoint — must byte-match the query the page and
// SettingsLayout build (the AppShell DESKTOP_MIN_PX shell breakpoint).
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

/** The real layout seam, put back after every test. */
const realViewport: Viewport = { ...viewport };

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
      // The global runner default (issue #55), seeded host server-side, and the
      // dev image chain's two settings-level rungs: no global default image, and
      // no --container-image flag. The Runner section's inherit row and dev
      // image hint read these.
      runner_default: 'host',
      dev_image_default: '',
      dev_image_fallback: '',
    };
    h.credentialsOnServer = [];
    h.patchBodies = [];
    h.patchRefusal = null;
    h.patchOffline = false;
    h.patchHold = null;
    h.inheritedBodies = [];
    h.inheritedPatch = {};
    h.inheritedError = null;
    h.inheritedGate = null;
    h.tops = {};
    h.pageAtEnd = false;
    h.layoutReads = 0;
    h.providersError = null;
    h.credentialsError = null;
    h.providersGets = 0;
    h.credentialsGets = 0;
    h.scrolls = [];
    h.secretsOnServer = [];
    h.secretRequestBodies = [];
    h.deleteRequests = [];
    h.deleteStatus = 204;
    h.schedules = [];
    h.scheduleBodies = [];
    h.cronPreviewExprs = [];
    h.scheduleRefusal = null;
    h.runNowRequests = [];
    h.runNowRefusal = null;
    h.runNowError = null;
    h.scheduleHold = null;
    h.schedulesHold = null;
    h.schedulesGetError = null;
    h.secretRefusal = null;
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
    h.sshTargetsConfigured = true;
    h.sshTargetsOnServer = baseSSHTargets();
    h.sshTargetRequests = [];
    h.sshTargetsReadError = null;
    h.sshTargetWriteError = null;
    stubApi();
    // jsdom has no layout: the page's seam answers from `h` instead. A
    // section without an entry in h.tops is far below the fold.
    Object.assign(viewport, {
      topOf: (element) => {
        h.layoutReads += 1;
        return h.tops[element.id] ?? 100_000;
      },
      heightOf: () => 0,
      scrollTo: (element, offset, smooth) => {
        const target = element.getAttribute('data-field') ?? element.id;
        h.scrolls.push({ target, offset, smooth });
      },
      atEnd: () => h.pageAtEnd,
    } satisfies Viewport);
    // The router scrolls to the top after a navigation; jsdom only logs
    // "not implemented" for it.
    vi.stubGlobal('scrollTo', vi.fn());
    // A frame is the next macrotask here, so settle() is enough to let the
    // page's once-per-frame work run (jsdom's own frames tick every 16ms).
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) =>
      setTimeout(() => callback(performance.now()), 0),
    );
    vi.stubGlobal('cancelAnimationFrame', (handle: number) => clearTimeout(handle));
  });

  afterEach(() => {
    dispose?.();
    dispose = undefined;
    container.remove();
    FakeEventSource.instances = [];
    vi.unstubAllGlobals();
    mediaStub = undefined; // the stub it memoized is gone with unstubAllGlobals
    vi.restoreAllMocks();
    Object.assign(viewport, realViewport);
  });
}
