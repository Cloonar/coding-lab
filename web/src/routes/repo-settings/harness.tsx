// Shared test harness for the repo-settings area suites (issue #198), the
// runchat/harness.tsx precedent applied to the RepoSettings.test.tsx stub
// server and DOM helpers. Section suites mount the real route at
// /repos/:id/settings/:section? (App root, MemoryRouter) and poke the mutable
// `h` state object to shape server responses.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, vi } from 'vitest';
import type { MemoryHistory } from '@solidjs/router';
import type { CredentialListItem, Provider, Repo, RepoSecret } from '../../api';
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
  };
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
        // Rotate clears the exposure flag (RotateRepoSecret's job, issue #108) —
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
    h.providersOnServer = baseProviders();
    h.settingsOnServer = { provider_default: 'claude-code' };
    h.credentialsOnServer = [];
    h.patchBodies = [];
    h.secretsOnServer = [];
    h.secretRequestBodies = [];
    h.deleteRequests = [];
    h.deleteStatus = 204;
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
