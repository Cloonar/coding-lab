// New-run composer contract (issue #41, Phase 2b):
// - with repos present, the composer renders (field, textarea, repo/model/
//   effort chips) and NOT the empty state;
// - Send spawns the selected repo with label/model/effort, queues the typed
//   text as the run's first message, and navigates to /runs/:id;
// - an empty box spawns a plain run and queues nothing;
// - a spawn failure keeps the text in the box, shows the server message
//   verbatim, and never navigates or queues;
// - zero repos hides the composer and shows the exact "No repositories yet"
//   empty state (the Playwright smoke + login/setup round-trip assert this on `/`);
// - a logged-out provider surfaces the slim reconnect banner, named by the
//   provider's display_name (issue #51 decision 9).

import { MemoryRouter, Route, createMemoryHistory, useParams } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Provider, ProviderAuthStatus, Repo } from '../api';
import App from '../App';
import { clearQueued, peekQueued } from '../lib/queuedMessage';
import NewRun from './NewRun';

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

function repoFixture(overrides: Partial<Repo> = {}): Repo {
  return {
    id: 'repo_1',
    name: 'coding-lab',
    remote_url: 'git@h:o/r.git',
    credential_id: null,
    forge_credential_id: null,
    tracker_binding: 'builtin',
    forge_kind: 'none',
    default_branch: 'main',
    provider: 'claude-code',
    incogni: false,
    model_default: null,
    effort_default: null,
    afk_model_default: null,
    afk_effort_default: null,
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
    created_at: '2026-07-06T00:00:00.000Z',
    last_opened_at: null,
    ...overrides,
  };
}

const PROVIDERS: Provider[] = [
  {
    id: 'claude-code',
    display_name: 'Claude Code',
    auth: { kind: 'oauth-code' },
    models: [
      { value: 'sonnet', label: 'Sonnet' },
      { value: 'opus', label: 'Opus' },
    ],
    efforts: [
      { value: 'low', label: 'Low' },
      { value: 'high', label: 'High' },
    ],
    options: [],
  },
];

let reposOnServer: Repo[];
let authOnServer: ProviderAuthStatus;
let instancePost: { status: number; runID: string };
let instancePosts: Record<string, unknown>[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

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
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      if (url === '/api/v1/repos' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { repos: reposOnServer }));
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: PROVIDERS }));
      }
      if (url === '/api/v1/settings' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, {}));
      }
      // Per-provider-id auth route (issue #51 decision 7), keyed on the
      // selected repo's provider.
      if (url === '/api/v1/providers/claude-code/auth/status' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, authOnServer));
      }
      if (url.startsWith('/api/v1/repos/repo_1/ready') && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { issues: [{ number: 1, title: 'x' }] }));
      }
      if (url === '/api/v1/repos/repo_1/instances' && method === 'POST') {
        instancePosts.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        if (instancePost.status >= 400) {
          return Promise.resolve(jsonResponse(instancePost.status, { error: 'cap reached (2/2)' }));
        }
        return Promise.resolve(jsonResponse(201, { id: instancePost.runID, repo_id: 'repo_1' }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flush();
}

function RunStub() {
  const params = useParams<{ id: string }>();
  return <div class="run-stub">run:{params.id}</div>;
}

async function mountHome(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/' });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/" component={NewRun} />
        <Route path="/runs/:id" component={RunStub} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function setSelectValue(label: string, value: string): void {
  const select = container.querySelector<HTMLSelectElement>(`select[aria-label="${label}"]`)!;
  select.value = value;
  select.dispatchEvent(new Event('input', { bubbles: true }));
}

function startButton(): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>('button[aria-label="Start run"]')!;
}

beforeEach(() => {
  reposOnServer = [repoFixture()];
  authOnServer = { logged_in: true, email: 'me@x', method: 'oauth', checked_at: '' };
  instancePost = { status: 201, runID: 'run_new' };
  instancePosts = [];
  // A stale last-repo must not strand the composer; default to the empty slate.
  try {
    localStorage.removeItem('lab.last-repo');
  } catch {
    /* jsdom always has storage; guard mirrors the component */
  }
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
  clearQueued('run_new');
  clearQueued('run_empty');
});

describe('NewRun composer', () => {
  it('renders the composer (not the empty state) when repos exist', async () => {
    await mountHome();

    expect(container.querySelector('.composer-field')).not.toBeNull();
    expect(container.querySelector('.composer-input')).not.toBeNull();
    expect(container.querySelector('select[aria-label="Model"]')).not.toBeNull();
    expect(container.querySelector('select[aria-label="Effort"]')).not.toBeNull();
    // The repo chip names the selected (first ready) repo.
    expect(container.querySelector('.repo-chip')?.textContent).toContain('coding-lab');
    // Not the zero-repos slate.
    expect(container.textContent).not.toContain('No repositories yet');
  });

  it('spawns with label/model/effort, queues the typed text, and navigates', async () => {
    await mountHome();

    const input = container.querySelector('.composer-input') as HTMLTextAreaElement;
    input.value = 'do the thing';
    input.dispatchEvent(new Event('input', { bubbles: true }));

    setSelectValue('Model', 'opus');
    setSelectValue('Effort', 'high');

    // Open the `…` popover and set the optional label.
    container.querySelector<HTMLButtonElement>('button[aria-label="More options"]')!.click();
    await settle();
    const labelInput = container.querySelector('input[name="label"]') as HTMLInputElement;
    labelInput.value = 'debug';
    labelInput.dispatchEvent(new Event('input', { bubbles: true }));

    startButton().click();
    await settle();

    expect(instancePosts).toHaveLength(1);
    expect(instancePosts[0]).toEqual({ label: 'debug', model: 'opus', effort: 'high' });
    // The typed text is parked as the new run's first message…
    expect(peekQueued('run_new')).toBe('do the thing');
    // …and the composer navigated to the chat.
    expect(container.textContent).toContain('run:run_new');
  });

  it('spawns a plain run and queues nothing when the box is empty', async () => {
    await mountHome();

    startButton().click();
    await settle();

    expect(instancePosts).toHaveLength(1);
    // No label (untouched); model/effort default to the first catalog option.
    expect(instancePosts[0]).toEqual({ model: 'sonnet', effort: 'low' });
    // Nothing was queued for the spawned run (empty box).
    expect(peekQueued('run_new')).toBeUndefined();
    expect(container.textContent).toContain('run:run_new');
  });

  it('keeps the text and shows the server message verbatim on a spawn failure', async () => {
    instancePost = { status: 409, runID: 'run_new' };
    await mountHome();

    const input = container.querySelector('.composer-input') as HTMLTextAreaElement;
    input.value = 'try me';
    input.dispatchEvent(new Event('input', { bubbles: true }));

    startButton().click();
    await settle();

    expect(instancePosts).toHaveLength(1);
    // The banner shows the raw 409, the text stays, and nothing navigated/queued.
    expect(container.querySelector('.banner.error')?.textContent).toContain('cap reached (2/2)');
    expect((container.querySelector('.composer-input') as HTMLTextAreaElement).value).toBe(
      'try me',
    );
    expect(container.textContent).not.toContain('run:run_new');
    expect(peekQueued('run_new')).toBeUndefined();
  });

  it('shows the exact zero-repos empty state and hides the composer', async () => {
    reposOnServer = [];
    await mountHome();

    expect(container.textContent).toContain('No repositories yet');
    expect(container.querySelector('.composer-field')).toBeNull();
    const addLink = Array.from(container.querySelectorAll('a')).find(
      (a) => a.getAttribute('href') === '/repos/new',
    );
    expect(addLink?.textContent).toContain('add one');
  });

  it('surfaces the reconnect banner named by display_name when the provider is logged out', async () => {
    authOnServer = { logged_in: false, email: '', method: '', checked_at: '' };
    await mountHome();

    const warn = container.querySelector('.newrun-warn');
    // Copy flows from the provider's display_name, not a hardcoded brand.
    expect(warn?.textContent).toContain('Claude Code is logged out');
    const link = warn?.querySelector('a');
    expect(link?.getAttribute('href')).toBe('/credentials');
  });
});
