// Shared test harness for the repo home suites (issue #61): mounts the real
// repositories route table (./routes.tsx) under the App root in a
// MemoryRouter, with a stub API that answers every read the frame and its
// tabs make. Tests shape responses through the mutable `h` object (the
// repo-settings/harness.tsx convention: importers mutate fields, never rebind
// exports) and add endpoints through `h.handle`.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import type { MemoryHistory } from '@solidjs/router';
import type { JSX } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, vi } from 'vitest';
import type { Instance, ParkedEntry, Readiness, Repo } from '../../api';
import App from '../../App';
import RepoRoutes from './routes';

export const REPO_ID = 'repo_1';

/** A complete, ready, forge-bound repo; override per test. */
export function baseRepo(over: Partial<Repo> = {}): Repo {
  return {
    id: REPO_ID,
    name: 'coding-lab',
    remote_url: 'git@github.com:Cloonar/coding-lab.git',
    credential_id: null,
    forge_credential_id: null,
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
    created_at: '2026-07-06T00:00:00Z',
    last_opened_at: null,
    summary: {
      claimable: 3,
      open_issues: 12,
      readiness: { state: 'passing', checks: [] },
    },
    autoland_enabled: false,
    max_fix_attempts: 2,
    auto_merge: true,
    lander_provider: null,
    lander_model: null,
    lander_effort: null,
    runner: 'container',
    container_memory: null,
    container_pids: null,
    container_nofile: null,
    image_ref: null,
    ...over,
  };
}

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

/** Pushes one SSE event to every open stream. */
export function emit(type: string, payload: Record<string, unknown>): void {
  for (const source of FakeEventSource.instances) source.emit(type, payload);
}

export function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

export type StubResponse = ReturnType<typeof jsonResponse>;

export interface RepoHomeHarnessState {
  /** GET /repos/repo_1; `null` answers 500 with `repoError`. */
  repo: Repo | null;
  repoError: string;
  /** "<METHOD> <url>" of every API call, in order. */
  requests: string[];
  /** Consulted first: return a response to answer a call, undefined to fall through. */
  handle?: (method: string, url: string, init?: RequestInit) => StubResponse | undefined;
  /** GET /instances (the side rail, the list's run counts, Overview's Live runs). */
  instances: Instance[];
  /** GET /repos (the list); undefined answers `[h.repo]`. */
  repos?: Repo[];
  /** GET /repos/repo_1/readiness; undefined mirrors `h.repo.summary.readiness`, null answers 500. */
  readiness?: Readiness | null;
  /** GET /repos/repo_1/parked; null answers 404 (the endpoint is not mounted). */
  parked: ParkedEntry[] | null;
}
export const h = {} as RepoHomeHarnessState;

export function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      h.requests.push(`${method} ${url}`);
      const custom = h.handle?.(method, url, init);
      if (custom !== undefined) return Promise.resolve(custom);
      if (method !== 'GET') {
        return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
      }
      const repoPath = `/api/v1/repos/${REPO_ID}`;
      switch (url) {
        case '/api/v1/auth/state':
          return Promise.resolve(
            jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
          );
        // AppShell's side rail (and the list's / Overview's live runs).
        case '/api/v1/instances':
          return Promise.resolve(jsonResponse(200, { instances: h.instances }));
        // The repositories list.
        case '/api/v1/repos':
          return Promise.resolve(
            jsonResponse(200, { repos: h.repos ?? (h.repo === null ? [] : [h.repo]) }),
          );
        // The Overview tab.
        case `${repoPath}/readiness`:
          return Promise.resolve(
            h.readiness === null
              ? jsonResponse(500, { error: 'readiness unavailable' })
              : jsonResponse(200, h.readiness ?? h.repo?.summary.readiness ?? null),
          );
        case `${repoPath}/parked`:
          return Promise.resolve(
            h.parked === null
              ? jsonResponse(404, { error: 'not found' })
              : jsonResponse(200, { parked: h.parked }),
          );
        case repoPath:
          return Promise.resolve(
            h.repo === null
              ? jsonResponse(500, { error: h.repoError })
              : jsonResponse(200, { ...h.repo }),
          );
        // The Issues tab.
        case `${repoPath}/issues?state=open`:
          return Promise.resolve(
            jsonResponse(200, { binding: h.repo?.tracker_binding ?? 'forge', issues: [] }),
          );
        case `${repoPath}/ready`:
          return Promise.resolve(jsonResponse(200, { issues: [] }));
        case `${repoPath}/labels`:
          return Promise.resolve(jsonResponse(200, { labels: [] }));
        case `${repoPath}/issues/12`:
          return Promise.resolve(
            jsonResponse(200, {
              number: 12,
              title: 'Fix login',
              body: '',
              state: 'open',
              labels: [],
              comments: [],
              created_at: '2026-07-06T00:00:00Z',
              updated_at: '2026-07-06T00:00:00Z',
            }),
          );
        // The CRs tab.
        case `${repoPath}/crs?state=open`:
          return Promise.resolve(jsonResponse(200, { crs: [] }));
        case `${repoPath}/crs/3`:
          return Promise.resolve(
            jsonResponse(200, {
              number: 3,
              title: 'Add retry loop',
              state: 'open',
              head_branch: 'afk/7',
              base_branch: 'main',
              closes: [],
              created_at: '2026-07-06T00:00:00Z',
              merged_at: null,
              merge_commit: null,
              body: '',
              diff: '',
            }),
          );
        // The Settings tab and Add repository.
        case '/api/v1/credentials':
          return Promise.resolve(jsonResponse(200, { credentials: [] }));
        case '/api/v1/providers':
          return Promise.resolve(jsonResponse(200, { providers: [] }));
        case '/api/v1/settings':
          return Promise.resolve(jsonResponse(200, { provider_default: '' }));
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

export async function waitFor<T>(get: () => T | null | undefined, what: string): Promise<T> {
  for (let i = 0; i < 50; i += 1) {
    const value = get();
    if (value !== null && value !== undefined) return value;
    await flush();
  }
  throw new Error(`timed out waiting for ${what}`);
}

let dispose: (() => void) | undefined;
export let container: HTMLDivElement = document.createElement('div');
export let routerHistory: MemoryHistory;

/**
 * Mounts the route table at `path` (default: the repo home). `extraRoutes`
 * adds test-only <Route>s beside the real table (e.g. a page that navigates
 * into the repo home with router state).
 */
export async function mountRepoHome(
  path: string = `/repos/${REPO_ID}`,
  extraRoutes?: () => JSX.Element,
): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  routerHistory = createMemoryHistory();
  routerHistory.set({ value: path });
  dispose = render(
    () => (
      <MemoryRouter history={routerHistory} root={App}>
        <RepoRoutes />
        {extraRoutes?.()}
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

export function unmount(): void {
  dispose?.();
  dispose = undefined;
  container.remove();
}

/**
 * jsdom has no window.matchMedia (createMediaQuery then reads "no match": the
 * phone layout). setDesktop installs a fake whose desktop breakpoint query
 * matches or not, and flips it live — listeners hear the change, so a mounted
 * page crosses the breakpoint. Removed with the other globals after each test.
 */
let media: { matches: boolean; listeners: Set<() => void> } | undefined;

export function setDesktop(matches: boolean): void {
  if (media === undefined) {
    const state = { matches: false, listeners: new Set<() => void>() };
    media = state;
    vi.stubGlobal(
      'matchMedia',
      vi.fn((query: string) => ({
        get matches() {
          return query === '(min-width: 1024px)' && state.matches;
        },
        media: query,
        onchange: null,
        addEventListener: (_type: string, listener: () => void) => state.listeners.add(listener),
        removeEventListener: (_type: string, listener: () => void) =>
          state.listeners.delete(listener),
        addListener: () => {},
        removeListener: () => {},
        dispatchEvent: () => false,
      })),
    );
  }
  media.matches = matches;
  for (const listener of media.listeners) listener();
}

/** A live instance of the repo; override per test. */
export function baseInstance(over: Partial<Instance> = {}): Instance {
  return {
    id: 'run_1',
    repo_id: REPO_ID,
    repo_name: 'coding-lab',
    kind: 'manual',
    provider: 'agent-a',
    issue_number: null,
    pull_number: null,
    branch: 'lab/fix-chat-dock',
    worktree_path: '/wt/fix-chat-dock',
    session_name: 'coding-lab~dominik-20260706-1530',
    title: 'Fix chat dock overlap',
    model: 'model-a',
    effort: 'high',
    remote: false,
    deep_link_url: null,
    started_at: '2026-07-06T15:30:00Z',
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

export function installRepoHomeHooks(): void {
  beforeEach(() => {
    h.repo = baseRepo();
    h.repoError = 'repo lookup failed';
    h.requests = [];
    h.handle = undefined;
    h.instances = [];
    h.repos = undefined;
    h.readiness = undefined;
    h.parked = [];
    stubApi();
  });

  afterEach(() => {
    unmount();
    FakeEventSource.instances = [];
    media = undefined; // the stub it held is gone with unstubAllGlobals
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });
}
