// Add repository (issue #61; the Agent pick of issue #66 / ADR-0030 left the
// form): the name follows the URL until edited, the forge credential hides
// for the Built-in binding, the POST never carries `provider`, errors land
// under their field (an empty or unparsable URL before any request, a server
// refusal at the field it names, a field-less one in the banner), and success
// opens the new repo's home.

import { MemoryRouter, Route, createMemoryHistory, useLocation } from '@solidjs/router';
import type { MemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { CredentialListItem, Provider } from '../api';
import App from '../App';
import AddRepo from './AddRepo';

const PROVIDERS: Provider[] = [
  {
    id: 'agent-a',
    display_name: 'Agent A',
    supports_remote: true,
    auth: { kind: 'oauth-code' },
    models: [{ value: 'model-a', label: 'Model A', efforts: [] }],
    efforts: [{ value: 'high', label: 'high' }],
    options: [],
  },
  {
    id: 'agent-b',
    display_name: 'Agent B',
    supports_remote: false,
    auth: { kind: 'api-key' },
    models: [{ value: 'model-b', label: 'Model B', efforts: [] }],
    efforts: [],
    options: [],
  },
];

const CREDENTIALS: CredentialListItem[] = [
  {
    id: 'cred_git',
    name: 'deploy-key',
    kind: 'ssh_key',
    created_at: '2026-07-06T00:00:00Z',
    updated_at: '2026-07-06T00:00:00Z',
    referenced: false,
  },
  {
    id: 'cred_forge',
    name: 'gh-bot',
    kind: 'forge_token',
    created_at: '2026-07-06T00:00:00Z',
    updated_at: '2026-07-06T00:00:00Z',
    referenced: false,
  },
];

/** Stand-in for EventSource so the authenticated App shell can mount. */
class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
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

/** Answers GET /credentials with 500 while true. */
let credentialsFail: boolean;
let createBodies: Record<string, unknown>[];
/** The answer to POST /repos; default: 201 with the new repo. */
let createResponse: ReturnType<typeof jsonResponse>;
let requests: string[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;
let history: MemoryHistory;
/** Router state the new repo's home was opened with. */
let arrivedState: unknown;

function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal('scrollTo', vi.fn()); // the router scrolls on navigate; jsdom has no layout
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      requests.push(`${method} ${url}`);
      if (url === '/api/v1/auth/state' && method === 'GET') {
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
        );
      }
      if (url === '/api/v1/credentials' && method === 'GET') {
        return Promise.resolve(
          credentialsFail
            ? jsonResponse(500, { error: 'vault is sealed' })
            : jsonResponse(200, { credentials: CREDENTIALS }),
        );
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: PROVIDERS }));
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      if (url === '/api/v1/repos' && method === 'POST') {
        createBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return Promise.resolve(createResponse);
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

/** Stands in for the repo home: records the router state it was opened with. */
function RepoHomeProbe() {
  const location = useLocation();
  arrivedState = location.state;
  return <p class="repo-home-probe">repo home</p>;
}

async function mountAddRepo(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  history = createMemoryHistory();
  history.set({ value: '/repos/new' });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/repos/new" component={AddRepo} />
        <Route path="/repos/:id" component={RepoHomeProbe} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function typeInto(el: HTMLInputElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

function input(name: string): HTMLInputElement {
  const el = container.querySelector<HTMLInputElement>(`input[name="${name}"]`);
  if (!el) throw new Error(`missing input[name="${name}"]`);
  return el;
}

function select(name: string): HTMLSelectElement | null {
  return container.querySelector<HTMLSelectElement>(`select[name="${name}"]`);
}

function choose(el: HTMLSelectElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

/** The tracker binding segment with the given label. */
function binding(label: string): HTMLButtonElement {
  const el = Array.from(
    container.querySelectorAll<HTMLButtonElement>('button[role="radio"][name="tracker_binding"]'),
  ).find((b) => b.textContent === label);
  if (!el) throw new Error(`missing tracker binding ${label}`);
  return el;
}

/** The error line under a control, found through its aria-describedby. */
function errorUnder(control: HTMLElement): string | null {
  const ids = (control.getAttribute('aria-describedby') ?? '').split(' ').filter(Boolean);
  for (const id of ids) {
    const el = document.getElementById(id);
    if (el?.classList.contains('field-error')) return el.textContent;
  }
  return null;
}

async function submitForm(): Promise<void> {
  const form = container.querySelector('form');
  if (!form) throw new Error('missing add-repo form');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
  await settle();
}

beforeEach(() => {
  credentialsFail = false;
  createBodies = [];
  requests = [];
  arrivedState = undefined;
  createResponse = jsonResponse(201, {
    id: 'repo_9',
    name: 'search-indexer',
    clone_status: 'cloning',
  });
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('AddRepo form', () => {
  it('lays out the back link, heading and the one primary action', async () => {
    await mountAddRepo();

    const back = container.querySelector<HTMLAnchorElement>('a.back-link');
    expect(back?.getAttribute('href')).toBe('/repos');
    expect(container.querySelector('h1')?.textContent).toBe('Add repository');
    const submit = container.querySelector<HTMLButtonElement>('form button[type="submit"]');
    expect(submit?.textContent).toBe('Add and start cloning');
    expect(container.querySelector('.add-repo-note')?.textContent).toContain(
      'start from your global settings',
    );
    // Incogni is a switch, off by default.
    const incogni = container.querySelector('button[role="switch"][name="incogni"]');
    expect(incogni?.getAttribute('aria-checked')).toBe('false');
  });

  it('fills the name from the URL until the operator edits it', async () => {
    await mountAddRepo();
    const url = input('remote_url');
    const name = input('name');

    typeInto(url, 'git@github.com:example/search-indexer.git');
    await settle();
    expect(name.value).toBe('search-indexer');
    expect(container.textContent).toContain('Remote on github.com.');

    typeInto(url, 'https://git.example.com/o/other-repo');
    await settle();
    expect(name.value).toBe('other-repo');

    typeInto(name, 'my-name');
    await settle();
    typeInto(url, 'git@github.com:example/third.git');
    await settle();
    expect(name.value).toBe('my-name'); // edited: no longer overwritten

    await submitForm();
    expect(createBodies).toEqual([
      { remote_url: 'git@github.com:example/third.git', name: 'my-name' },
    ]);
  });

  it('sends no name while it only follows the URL (the server derives it)', async () => {
    await mountAddRepo();
    typeInto(input('remote_url'), 'git@h:o/r.git');
    await submitForm();

    expect(createBodies).toEqual([{ remote_url: 'git@h:o/r.git' }]);
  });

  it('hides the forge credential for the Built-in binding and never sends it then', async () => {
    await mountAddRepo();
    typeInto(input('remote_url'), 'git@h:o/r.git');
    choose(select('credential_id')!, 'cred_git');
    expect(binding('Auto').getAttribute('aria-checked')).toBe('true');
    expect(select('forge_credential_id')).not.toBeNull();
    choose(select('forge_credential_id')!, 'cred_forge');

    binding('Built-in').click();
    await settle();
    expect(binding('Built-in').getAttribute('aria-checked')).toBe('true');
    expect(select('forge_credential_id')).toBeNull();

    await submitForm();
    expect(createBodies).toEqual([
      { remote_url: 'git@h:o/r.git', credential_id: 'cred_git', tracker_binding: 'builtin' },
    ]);
  });

  it('sends the forge binding with its forge credential', async () => {
    await mountAddRepo();
    typeInto(input('remote_url'), 'git@h:o/r.git');
    binding('Forge').click();
    await settle();
    choose(select('forge_credential_id')!, 'cred_forge');
    container.querySelector<HTMLButtonElement>('button[role="switch"][name="incogni"]')!.click();
    await submitForm();

    expect(createBodies).toEqual([
      {
        remote_url: 'git@h:o/r.git',
        tracker_binding: 'forge',
        forge_credential_id: 'cred_forge',
        incogni: true,
      },
    ]);
  });
});

describe('AddRepo credentials', () => {
  it('says the list failed to load, instead of claiming there are none, and retries', async () => {
    credentialsFail = true;
    await mountAddRepo();

    const hint = container.querySelector('.add-repo-credentials-failed');
    expect(hint?.textContent).toContain('Your credentials could not be loaded (vault is sealed)');
    expect(container.textContent).not.toContain('A private remote needs one');
    // The git credential select is described by the note.
    const gitSelect = select('credential_id');
    expect(gitSelect?.getAttribute('aria-describedby')).toContain(hint?.id ?? '-');

    credentialsFail = false;
    const retry = Array.from(container.querySelectorAll('button')).find(
      (b) => b.textContent === 'Load credentials again',
    );
    retry?.click();
    await settle();

    expect(container.querySelector('.add-repo-credentials-failed')).toBeNull();
    expect(Array.from(select('credential_id')?.options ?? []).length).toBeGreaterThan(1);
    expect(requests.filter((r) => r === 'GET /api/v1/credentials')).toHaveLength(2);
  });
});

describe('AddRepo has no Agent pick', () => {
  // Adapted from the issue #66 cases: the pick left the form, so a new repo
  // always inherits the global default and `provider` is never sent.
  it('renders no Agent control', async () => {
    await mountAddRepo();

    expect(container.querySelector('[name="provider"]')).toBeNull();
    expect(container.textContent).not.toContain('Agent A');
    // The page asks for no catalog. The one GET /providers here is the app
    // shell's own (its More-tab logged-out dot, issue #76); a request from the
    // form would make it two.
    expect(requests.filter((r) => r === 'GET /api/v1/providers')).toHaveLength(1);
  });

  it('omits provider entirely from the POST body', async () => {
    await mountAddRepo();
    typeInto(input('remote_url'), 'git@h:o/r.git');

    await submitForm();

    expect(createBodies).toEqual([{ remote_url: 'git@h:o/r.git' }]);
    expect(createBodies[0]).not.toHaveProperty('provider');
  });
});

describe('AddRepo errors', () => {
  it('an empty URL shows the error under the field, focuses it and sends nothing', async () => {
    await mountAddRepo();
    await submitForm();

    const url = input('remote_url');
    expect(url.getAttribute('aria-invalid')).toBe('true');
    expect(errorUnder(url)).toBe('Paste the remote URL, for example git@github.com:owner/repo.git');
    expect(document.activeElement).toBe(url);
    expect(createBodies).toEqual([]);
    expect(requests.filter((r) => r.startsWith('POST'))).toEqual([]);
  });

  it('an unparsable URL shows the error under the field and sends nothing', async () => {
    await mountAddRepo();
    typeInto(input('remote_url'), 'coding-lab');
    await submitForm();

    const url = input('remote_url');
    expect(errorUnder(url)).toMatch(/^This is not a remote lab can clone\./);
    expect(createBodies).toEqual([]);

    // Editing the URL clears the error.
    typeInto(url, 'git@h:o/r.git');
    await settle();
    expect(url.getAttribute('aria-invalid')).toBeNull();
    expect(errorUnder(url)).toBeNull();
  });

  it("a server refusal naming 'name' lands under Name and focuses it", async () => {
    createResponse = jsonResponse(409, {
      error: 'a repository named "r" already exists',
      field: 'name',
    });
    await mountAddRepo();
    typeInto(input('remote_url'), 'git@h:o/r.git');
    await submitForm();

    const name = input('name');
    expect(name.getAttribute('aria-invalid')).toBe('true');
    expect(errorUnder(name)).toBe('a repository named "r" already exists');
    expect(document.activeElement).toBe(name);
    expect(container.querySelector('.banner.error')).toBeNull();
    expect(history.get()).toBe('/repos/new');
  });

  it("a refusal naming 'tracker_binding' lands under the binding and focuses its pick", async () => {
    createResponse = jsonResponse(400, {
      error: 'tracker_binding: "forge" requires a forge_token credential',
      field: 'tracker_binding',
    });
    await mountAddRepo();
    typeInto(input('remote_url'), 'git@h:o/r.git');
    binding('Forge').click();
    await settle();
    await submitForm();

    const group = container.querySelector<HTMLElement>('[role="radiogroup"]')!;
    expect(group.getAttribute('aria-invalid')).toBe('true');
    expect(errorUnder(group)).toContain('requires a forge_token credential');
    expect(document.activeElement).toBe(binding('Forge'));
  });

  it('a refusal that names no field shows in the form banner', async () => {
    createResponse = jsonResponse(500, { error: 'clone dir not writable' });
    await mountAddRepo();
    typeInto(input('remote_url'), 'git@h:o/r.git');
    await submitForm();

    expect(container.querySelector('.banner.error')?.textContent).toContain(
      'clone dir not writable',
    );
    expect(container.querySelectorAll('.field-error')).toHaveLength(0);
    expect(history.get()).toBe('/repos/new');
  });
});

describe('AddRepo success', () => {
  it("opens the new repo's home with a confirmation notice", async () => {
    await mountAddRepo();
    typeInto(input('remote_url'), 'git@github.com:example/search-indexer.git');
    await submitForm();

    expect(history.get()).toBe('/repos/repo_9');
    expect(container.querySelector('.repo-home-probe')).not.toBeNull();
    expect(arrivedState).toEqual({ notice: 'Added search-indexer. Cloning has started.' });
  });
});
