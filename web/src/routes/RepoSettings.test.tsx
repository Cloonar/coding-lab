// Regression tests for the stale-draft revert bug (RepoSettings):
//
// SettingsForm seeds its drafts from a snapshot of the repo. When an SSE
// repo.changed refetch lands while the form stays mounted (e.g. clone
// completes and default-branch detection rewrites default_branch), a save of
// an UNRELATED field must not diff stale drafts against the refreshed repo
// and silently PATCH old values back. Untouched drafts follow the server;
// dirty drafts keep the operator's edit.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Provider, Repo } from '../api';
import App from '../App';
import RepoSettings from './RepoSettings';

const REPO_ID = 'repo_1';

/** claude-code catalog with the ultracode bool option (issue #19). */
function baseProviders(): Provider[] {
  return [
    {
      id: 'claude-code',
      models: [
        { value: 'opus[1m]', label: 'Opus (1M)' },
        { value: 'sonnet', label: 'Sonnet' },
      ],
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

/** Stand-in for EventSource: lets tests push SSE events into the app. */
class FakeEventSource {
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

function baseRepo(): Repo {
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
    provider: 'claude',
    incogni: false,
    model_default: null,
    effort_default: null,
    afk_model_default: null,
    afk_effort_default: null,
    afk_options: null,
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
  };
}

function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

let repoOnServer: Repo;
let providersOnServer: Provider[];
let patchBodies: Record<string, unknown>[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function stubApi(): void {
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
        return Promise.resolve(jsonResponse(200, { credentials: [] }));
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: providersOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...repoOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}` && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as Record<string, unknown>;
        patchBodies.push(patch);
        repoOnServer = { ...repoOnServer, ...patch };
        return Promise.resolve(jsonResponse(200, { ...repoOnServer }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

/** Lets queued fetches resolve and Solid propagate the results. */
async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function waitFor<T>(get: () => T | null, what: string): Promise<T> {
  for (let i = 0; i < 50; i += 1) {
    const value = get();
    if (value !== null) return value;
    await flush();
  }
  throw new Error(`timed out waiting for ${what}`);
}

async function mountSettings(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/repos/${REPO_ID}/settings` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/repos/:id/settings" component={RepoSettings} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function input(name: string): HTMLInputElement {
  const el = container.querySelector<HTMLInputElement>(`input[name="${name}"]`);
  if (!el) throw new Error(`missing input[name="${name}"]`);
  return el;
}

function typeInto(el: HTMLInputElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

function chooseOption(el: HTMLSelectElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

function toggleCheckbox(el: HTMLInputElement, checked: boolean): void {
  el.checked = checked;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

function submitForm(): void {
  const form = container.querySelector('form');
  if (!form) throw new Error('missing settings form');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
}

/** The server-side push: repo.changed makes RepoSettingsView refetch. */
function emitRepoChanged(): void {
  for (const source of FakeEventSource.instances) {
    source.emit('repo.changed', { repoID: REPO_ID });
  }
}

beforeEach(() => {
  repoOnServer = baseRepo();
  providersOnServer = baseProviders();
  patchBodies = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('RepoSettings stale-draft handling', () => {
  it('saving an unrelated edit after a server-side default_branch change PATCHes only that edit', async () => {
    await mountSettings();
    const branch = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="default_branch"]'),
      'settings form',
    );
    expect(branch.value).toBe('main');

    // Clone completes: detection rewrites default_branch server-side and the
    // page refetches on repo.changed while the form stays mounted.
    repoOnServer = { ...repoOnServer, default_branch: 'master', clone_status: 'ready' };
    emitRepoChanged();
    await settle();

    // The untouched draft follows the server...
    expect(branch.value).toBe('master');

    // ...and saving an edit to ONLY the budget must not revert it.
    typeInto(input('budget_minutes'), '45');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ budget_minutes: 45 }]);
    expect(repoOnServer.default_branch).toBe('master');

    // The seed advanced with the save: a second submit has nothing to send.
    submitForm();
    await settle();
    expect(patchBodies).toHaveLength(1);
  });

  it('keeps a dirty draft across a refetch and PATCHes only that field', async () => {
    await mountSettings();
    const branch = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="default_branch"]'),
      'settings form',
    );
    typeInto(branch, 'trunk'); // operator edits default_branch first

    // Server-side changes land while the operator is mid-edit.
    repoOnServer = {
      ...repoOnServer,
      name: 'renamed-on-server',
      default_branch: 'master',
      clone_status: 'ready',
    };
    emitRepoChanged();
    await settle();

    expect(branch.value).toBe('trunk'); // dirty draft survives the refetch
    expect(input('name').value).toBe('renamed-on-server'); // untouched field follows

    submitForm();
    await settle();

    // Only the operator's edit is PATCHed — the server-side rename is not
    // clobbered back to the stale draft value.
    expect(patchBodies).toEqual([{ default_branch: 'trunk' }]);
    expect(repoOnServer.name).toBe('renamed-on-server');
  });
});

describe('RepoSettings AFK defaults', () => {
  it('renders the inherit option and the schema-driven ultracode checkbox', async () => {
    await mountSettings();
    const model = await waitFor(
      () => container.querySelector<HTMLSelectElement>('select[name="afk_model_default"]'),
      'AFK defaults section',
    );

    // The inherit entry sits first with an empty value, and unset seeds to it.
    expect(model.options[0]?.value).toBe('');
    expect(model.options[0]?.textContent).toBe('Inherit global AFK default');
    expect(model.value).toBe('');

    // The ultracode bool option renders unchecked (repo afk_options is null).
    const ultracode = input('afk_options.ultracode');
    expect(ultracode.type).toBe('checkbox');
    expect(ultracode.checked).toBe(false);
  });

  it('toggling ultracode PATCHes the full declared bag', async () => {
    await mountSettings();
    const ultracode = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_options.ultracode"]'),
      'ultracode checkbox',
    );

    toggleCheckbox(ultracode, true);
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ afk_options: { ultracode: 'true' } }]);
    expect(repoOnServer.afk_options).toEqual({ ultracode: 'true' });

    // The seed advanced with the save: a second submit has nothing to send.
    submitForm();
    await settle();
    expect(patchBodies).toHaveLength(1);
  });

  it('selecting an AFK model PATCHes afk_model_default only', async () => {
    await mountSettings();
    const model = await waitFor(
      () => container.querySelector<HTMLSelectElement>('select[name="afk_model_default"]'),
      'AFK model select',
    );

    chooseOption(model, 'sonnet');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ afk_model_default: 'sonnet' }]);
    expect(repoOnServer.afk_model_default).toBe('sonnet');
  });
});
