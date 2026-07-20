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
import type { Provider, Repo, RepoSecret } from '../api';
import App from '../App';
import RepoSettings from './RepoSettings';

const REPO_ID = 'repo_1';

/** claude-code catalog with the ultracode bool option (issue #19). */
function baseProviders(): Provider[] {
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
const CODEX: Provider = {
  id: 'codex',
  display_name: 'Codex',
  supports_remote: false,
  auth: { kind: 'api-key' },
  models: [{ value: 'gpt-5-codex', label: 'GPT-5 Codex', efforts: [] }],
  efforts: [{ value: 'medium', label: 'medium' }],
  options: [],
};

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
let settingsOnServer: Record<string, unknown>;
let patchBodies: Record<string, unknown>[];
let secretsOnServer: RepoSecret[];
let secretRequestBodies: Record<string, unknown>[];
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
      // Global settings feed the effective-provider chains the catalogs
      // resolve against (provider_default / spawn_provider_default_afk).
      if (url === '/api/v1/settings' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...settingsOnServer }));
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
      // Repo secrets (issue #104): metadata-only list + write-only
      // create/rotate/delete. The fake server never stores or echoes a
      // value — same write-only discipline the real API enforces.
      if (url === `/api/v1/repos/${REPO_ID}/secrets` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { secrets: secretsOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/secrets` && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        secretRequestBodies.push(body);
        const created: RepoSecret = {
          id: `sec_${secretsOnServer.length + 1}`,
          name: String(body.name),
          description: String(body.description ?? ''),
          created_at: '2026-07-10T00:00:00.000Z',
          updated_at: '2026-07-10T00:00:00.000Z',
          exposed_run_id: null,
          exposed_at: null,
        };
        secretsOnServer = [...secretsOnServer, created];
        return Promise.resolve(jsonResponse(201, created));
      }
      const secretMatch = /^\/api\/v1\/repos\/repo_1\/secrets\/([^/]+)$/.exec(url);
      if (secretMatch && method === 'PATCH') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        secretRequestBodies.push(body);
        const id = secretMatch[1];
        // Rotate clears the exposure flag (RotateRepoSecret's job, issue #108) —
        // the mock mirrors the real server so the refetch-clears-the-badge
        // behavior is exercisable here.
        const updated: RepoSecret = {
          ...(secretsOnServer.find((s) => s.id === id) as RepoSecret),
          updated_at: '2026-07-10T01:00:00.000Z',
          exposed_run_id: null,
          exposed_at: null,
        };
        secretsOnServer = secretsOnServer.map((s) => (s.id === id ? updated : s));
        return Promise.resolve(jsonResponse(200, updated));
      }
      if (secretMatch && method === 'DELETE') {
        const id = secretMatch[1];
        secretsOnServer = secretsOnServer.filter((s) => s.id !== id);
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

function textarea(name: string): HTMLTextAreaElement {
  const el = container.querySelector<HTMLTextAreaElement>(`textarea[name="${name}"]`);
  if (!el) throw new Error(`missing textarea[name="${name}"]`);
  return el;
}

function button(text: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'));
  const el = buttons.find((b) => b.textContent?.trim() === text);
  if (!el) throw new Error(`missing button ${JSON.stringify(text)}`);
  return el;
}

function typeInto(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/** The unified Select trigger button (field skin) for a form field name. */
function selectTrigger(name: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(`button[name="${name}"]`);
  if (!el) throw new Error(`missing select trigger button[name="${name}"]`);
  return el;
}

/** The label the named Select currently shows on its trigger. */
function selectedLabel(name: string): string {
  return selectTrigger(name).querySelector('.select-field-label')?.textContent ?? '';
}

function optionRows(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('[role="option"]'));
}

/** Opens the named Select and clicks the option with the given label. */
async function chooseFromSelect(name: string, optionLabel: string): Promise<void> {
  selectTrigger(name).click();
  await settle();
  const row = optionRows().find(
    (r) => r.querySelector('.select-option-label')?.textContent === optionLabel,
  );
  if (!row) throw new Error(`missing option ${JSON.stringify(optionLabel)} in ${name}`);
  row.click();
  await settle();
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

/** The Secrets section's <section> element, scoped for row/form queries. */
function secretsSection(): HTMLElement {
  const header = Array.from(container.querySelectorAll('h2')).find(
    (h) => h.textContent === 'Secrets',
  );
  if (!header) throw new Error('missing Secrets section heading');
  const section = header.closest('section');
  if (!section) throw new Error('Secrets heading has no enclosing <section>');
  return section as HTMLElement;
}

/** Submits the (single) form inside root — scoped so it never hits the main settings form. */
function submitFormWithin(root: ParentNode): void {
  const form = root.querySelector('form');
  if (!form) throw new Error('missing form within scope');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
}

beforeEach(() => {
  repoOnServer = baseRepo();
  providersOnServer = baseProviders();
  settingsOnServer = { provider_default: 'claude-code' };
  patchBodies = [];
  secretsOnServer = [];
  secretRequestBodies = [];
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
      () => container.querySelector<HTMLButtonElement>('button[name="afk_model_default"]'),
      'AFK defaults section',
    );

    // Unset seeds to the inherit entry (value ''), which titles the trigger…
    expect(model.textContent).toContain('Inherit global AFK default');
    // …and sits first (and selected) in the open panel.
    model.click();
    await settle();
    const rows = optionRows();
    expect(rows[0]?.textContent).toBe('Inherit global AFK default');
    expect(rows[0]?.getAttribute('aria-selected')).toBe('true');
    model.click(); // toggle shut again
    await settle();

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
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="afk_model_default"]'),
      'AFK model select',
    );

    await chooseFromSelect('afk_model_default', 'Sonnet');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ afk_model_default: 'sonnet' }]);
    expect(repoOnServer.afk_model_default).toBe('sonnet');
  });
});

describe('RepoSettings AFK seed prompt (issue #52)', () => {
  it("renders empty with the repo's afk_prompt_effective as the placeholder", async () => {
    await mountSettings();
    const field = await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    expect(field.value).toBe('');
    expect(field.placeholder).toBe(repoOnServer.afk_prompt_effective);
  });

  it('Customize copies afk_prompt_effective into the textarea for editing', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    button('Customize').click();

    expect(textarea('afk_prompt').value).toBe(repoOnServer.afk_prompt_effective);
  });

  it('editing the prompt and saving PATCHes afk_prompt as a string', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    typeInto(textarea('afk_prompt'), 'Always branch from main and open a PR when finished.');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([
      { afk_prompt: 'Always branch from main and open a PR when finished.' },
    ]);
    expect(repoOnServer.afk_prompt).toBe('Always branch from main and open a PR when finished.');
  });

  it('clearing a stored override PATCHes afk_prompt as null', async () => {
    repoOnServer = { ...repoOnServer, afk_prompt: 'A previously customized prompt.' };
    await mountSettings();
    const field = await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );
    expect(field.value).toBe('A previously customized prompt.');

    typeInto(field, '');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ afk_prompt: null }]);
  });
});

// Agent selection (issue #66 / ADR-0030): base + AFK provider selects with an
// explicit inherit entry, PATCHing via the '' → null convention; the
// model/effort catalogs re-resolve live against the DRAFTED providers; stored
// foreign values persist ("(not in catalog)") — nothing auto-clears.
describe('RepoSettings agent selection (issue #66)', () => {
  beforeEach(() => {
    providersOnServer = [...baseProviders(), CODEX];
  });

  it('choosing an agent PATCHes {provider: id}', async () => {
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agent select');
    expect(selectedLabel('provider')).toBe('Inherit global default');

    await chooseFromSelect('provider', 'Codex');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ provider: 'codex' }]);
    expect(repoOnServer.provider).toBe('codex');
  });

  it('choosing inherit PATCHes {provider: null}', async () => {
    repoOnServer = { ...repoOnServer, provider: 'claude-code' };
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agent select');
    expect(selectedLabel('provider')).toBe('Claude Code');

    await chooseFromSelect('provider', 'Inherit global default');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ provider: null }]);
  });

  it('choosing an AFK agent PATCHes {afk_provider_default: id}', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector('button[name="afk_provider_default"]'),
      'AFK agent select',
    );
    expect(selectedLabel('afk_provider_default')).toBe('Inherit global AFK default');

    await chooseFromSelect('afk_provider_default', 'Codex');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ afk_provider_default: 'codex' }]);
    expect(repoOnServer.afk_provider_default).toBe('codex');
  });

  it('flipping the AFK agent draft re-catalogs the AFK model select before save', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector('button[name="afk_provider_default"]'),
      'AFK agent select',
    );

    // Before the flip: the AFK model catalog is the claude-code one.
    selectTrigger('afk_model_default').click();
    await settle();
    let labels = optionRows().map((r) => r.textContent);
    expect(labels).toContain('Sonnet');
    expect(labels).not.toContain('GPT-5 Codex');
    selectTrigger('afk_model_default').click(); // toggle shut
    await settle();

    await chooseFromSelect('afk_provider_default', 'Codex');

    // After the flip (still unsaved): the catalog is the codex one, with the
    // inherit entry intact at the top.
    selectTrigger('afk_model_default').click();
    await settle();
    labels = optionRows().map((r) => r.textContent);
    expect(labels[0]).toBe('Inherit global AFK default');
    expect(labels).toContain('GPT-5 Codex');
    expect(labels).not.toContain('Sonnet');
  });

  it('keeps a stored foreign model_default marked "(not in catalog)" across a provider flip', async () => {
    repoOnServer = { ...repoOnServer, model_default: 'weird-model' };
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agent select');

    // Foreign to the effective catalog: offered as-is, marked, never dropped.
    expect(selectedLabel('model_default')).toBe('weird-model (not in catalog)');

    await chooseFromSelect('provider', 'Codex');
    expect(selectedLabel('model_default')).toBe('weird-model (not in catalog)');

    submitForm();
    await settle();

    // The flip PATCHes ONLY the provider — the stored model_default persists
    // (skip-layer makes it harmless at spawn; flipping back restores it).
    expect(patchBodies).toEqual([{ provider: 'codex' }]);
    expect(repoOnServer.model_default).toBe('weird-model');
  });
});

// Remote control (issue #163): a 3-way Inherit / On / Off select at BOTH scopes
// over a tri-state column — the reference implementation for a nullable bool
// override (it sidesteps issue #21's 2-state-checkbox-over-a-3-state-model bug
// by construction). The inherit row names what it currently resolves to.
describe('RepoSettings remote control', () => {
  it('offers Inherit / On / Off at both scopes, naming the effective inherited value', async () => {
    settingsOnServer = { provider_default: 'claude-code', spawn_remote_default: true };
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="remote_default"]'), 'remote select');

    expect(selectedLabel('remote_default')).toBe('Inherit global default — currently on');
    expect(selectedLabel('afk_remote_default')).toBe('Inherit global AFK default — currently on');

    selectTrigger('remote_default').click();
    await settle();
    expect(optionRows().map((r) => r.querySelector('.select-option-label')?.textContent)).toEqual([
      'Inherit global default — currently on',
      'On',
      'Off',
    ]);
  });

  it('an explicit repo off PATCHes false, and the AFK inherit row follows the draft live', async () => {
    settingsOnServer = { provider_default: 'claude-code', spawn_remote_default: true };
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="remote_default"]'), 'remote select');

    await chooseFromSelect('remote_default', 'Off');
    // The AFK chain walks through the repo's manual default, so the (unsaved)
    // draft already changes what AFK inherit means.
    expect(selectedLabel('afk_remote_default')).toBe('Inherit global AFK default — currently off');

    submitForm();
    await settle();

    // `false` is an explicit off, not an omission — it must reach the server.
    expect(patchBodies).toEqual([{ remote_default: false }]);
    expect(repoOnServer.remote_default).toBe(false);
  });

  it('seeds a stored AFK override and clears it back to inherit as null', async () => {
    repoOnServer = { ...baseRepo(), afk_remote_default: true };
    await mountSettings();
    await waitFor(
      () => container.querySelector('button[name="afk_remote_default"]'),
      'AFK remote select',
    );
    expect(selectedLabel('afk_remote_default')).toBe('On');

    await chooseFromSelect('afk_remote_default', 'Inherit global AFK default — currently off');
    submitForm();
    await settle();

    // Only the AFK key — the untouched base select stays out of the patch.
    expect(patchBodies).toEqual([{ afk_remote_default: null }]);
  });

  it('disables both selects with a note when the resolved provider has no remote knob', async () => {
    providersOnServer = [...baseProviders(), CODEX];
    repoOnServer = { ...baseRepo(), provider: 'codex' };
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="remote_default"]'), 'remote select');

    expect(selectTrigger('remote_default').disabled).toBe(true);
    expect(selectTrigger('afk_remote_default').disabled).toBe(true);
    expect(container.textContent).toContain('Codex ignores this.');
  });
});

// Autoland (issue #181 / ADR-0048): the four per-repo settings, default off /
// 2 / on / inherit; autoland_enabled disables on a non-forge binding (the
// poller has no PR-comment listing to read there).
describe('RepoSettings Autoland', () => {
  it('renders the defaults: off, 2 attempts, merge on, inherit agent', async () => {
    await mountSettings();
    const autoland = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="autoland_enabled"]'),
      'autoland checkbox',
    );

    expect(autoland.checked).toBe(false);
    expect(autoland.disabled).toBe(false); // baseRepo() is forge-bound
    expect(input('auto_merge').checked).toBe(true);
    expect(input('max_fix_attempts').value).toBe('2');
    expect(selectedLabel('lander_provider')).toBe('Inherit repo agent');
    // Lander model/effort (issue #189) default to the inherit row too.
    expect(selectedLabel('lander_model')).toBe('Inherit repo default');
    expect(selectedLabel('lander_effort')).toBe('Inherit repo default');
  });

  it('disables autoland_enabled with a hint on a non-forge (builtin) binding', async () => {
    repoOnServer = { ...baseRepo(), tracker_binding: 'builtin', forge_kind: 'none' };
    await mountSettings();
    const autoland = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="autoland_enabled"]'),
      'autoland checkbox',
    );

    expect(autoland.disabled).toBe(true);
    expect(container.textContent).toContain('Autoland needs a forge tracker binding.');
  });

  it('toggling autoland_enabled and auto_merge, editing attempts, and picking a lander agent PATCHes all four', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="autoland_enabled"]'),
      'autoland checkbox',
    );

    toggleCheckbox(input('autoland_enabled'), true);
    toggleCheckbox(input('auto_merge'), false);
    typeInto(input('max_fix_attempts'), '5');
    await chooseFromSelect('lander_provider', 'Claude Code');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([
      {
        autoland_enabled: true,
        auto_merge: false,
        max_fix_attempts: 5,
        lander_provider: 'claude-code',
      },
    ]);
    expect(repoOnServer.autoland_enabled).toBe(true);
    expect(repoOnServer.auto_merge).toBe(false);
    expect(repoOnServer.max_fix_attempts).toBe(5);
    expect(repoOnServer.lander_provider).toBe('claude-code');
  });

  it('picking inherit for the lander agent PATCHes null', async () => {
    repoOnServer = { ...baseRepo(), lander_provider: 'claude-code' };
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="lander_provider"]'), 'lander select');
    expect(selectedLabel('lander_provider')).toBe('Claude Code');

    await chooseFromSelect('lander_provider', 'Inherit repo agent');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ lander_provider: null }]);
  });

  it('picking a lander model and effort PATCHes lander_model / lander_effort', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector('button[name="lander_model"]'),
      'lander model select',
    );
    // The catalog resolves against the lander's effective provider — here the
    // repo's chain falls back to provider_default (claude-code).
    expect(selectedLabel('lander_model')).toBe('Inherit repo default');

    await chooseFromSelect('lander_model', 'Sonnet');
    await chooseFromSelect('lander_effort', 'high');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ lander_model: 'sonnet', lander_effort: 'high' }]);
    expect(repoOnServer.lander_model).toBe('sonnet');
    expect(repoOnServer.lander_effort).toBe('high');
  });

  it('clearing a stored lander model back to inherit PATCHes null', async () => {
    repoOnServer = { ...baseRepo(), lander_model: 'sonnet' };
    await mountSettings();
    await waitFor(
      () => container.querySelector('button[name="lander_model"]'),
      'lander model select',
    );
    expect(selectedLabel('lander_model')).toBe('Sonnet');

    await chooseFromSelect('lander_model', 'Inherit repo default');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ lander_model: null }]);
  });

  it('rejects a blank max_fix_attempts client-side without PATCHing', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="max_fix_attempts"]'),
      'max fix attempts field',
    );

    typeInto(input('max_fix_attempts'), '');
    submitForm();
    await settle();

    expect(patchBodies).toHaveLength(0);
    expect(container.textContent).toContain(
      'Max fix attempts must be a whole number of 0 or more.',
    );
  });

  it('rejects a negative max_fix_attempts client-side without PATCHing', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="max_fix_attempts"]'),
      'max fix attempts field',
    );

    typeInto(input('max_fix_attempts'), '-1');
    submitForm();
    await settle();

    expect(patchBodies).toHaveLength(0);
    expect(container.textContent).toContain(
      'Max fix attempts must be a whole number of 0 or more.',
    );
  });
});

// Repo secrets section (issue #104): write-only per-repo secrets — the API
// mock here only ever hands back metadata, so "no value ever rendered" is
// pinned by construction; these tests additionally assert the value INPUT is
// password-typed and that requests carry only what the contract calls for.
describe('RepoSettings secrets section', () => {
  it('renders listed secrets with name, description and updated date', async () => {
    secretsOnServer = [
      {
        id: 'sec_1',
        name: 'API_KEY',
        description: 'third-party api',
        created_at: '2026-07-01T00:00:00.000Z',
        updated_at: '2026-07-02T00:00:00.000Z',
        exposed_run_id: null,
        exposed_at: null,
      },
    ];
    await mountSettings();
    await waitFor(() => container.querySelector('h2') && secretsSection(), 'secrets section');

    const section = secretsSection();
    expect(section.textContent).toContain('API_KEY');
    expect(section.textContent).toContain('third-party api');
    // The secret name renders in a monospace element, per the design.
    const nameEl = section.querySelector('.card-title.mono');
    expect(nameEl?.textContent).toBe('API_KEY');

    // No value ever appears anywhere on the page — the mock only ever hands
    // back metadata, same as the real write-only API.
    expect(container.textContent).not.toContain('sekrit');
  });

  it('renders the empty state when the repo has no secrets', async () => {
    await mountSettings();
    await waitFor(
      () => (secretsSection().textContent?.includes('No secrets yet') ? true : null),
      'empty state',
    );
    expect(secretsSection().textContent).toContain('No secrets yet');
  });

  it('add-secret form submits name, description and value, then refreshes the list', async () => {
    await mountSettings();
    await waitFor(() => secretsSection(), 'secrets section');

    button('+ Add secret').click();
    await settle();

    const nameField = input('secret-name');
    const valueField = input('secret-value');
    expect(valueField.type).toBe('password');

    typeInto(nameField, 'DEPLOY_TOKEN');
    typeInto(input('secret-description'), 'deploy pipeline token');
    typeInto(valueField, 'a-fresh-secret-value');

    submitFormWithin(secretsSection());
    await settle();

    expect(secretRequestBodies).toEqual([
      { name: 'DEPLOY_TOKEN', description: 'deploy pipeline token', value: 'a-fresh-secret-value' },
    ]);
    // The create form closes and the list reflects the new secret's metadata
    // only — never the value that was just submitted.
    expect(secretsSection().textContent).toContain('DEPLOY_TOKEN');
    expect(secretsSection().textContent).not.toContain('a-fresh-secret-value');
  });

  it('rotate submits only the new value, never the name or id, and clears an exposure badge', async () => {
    secretsOnServer = [
      {
        id: 'sec_1',
        name: 'API_KEY',
        description: '',
        created_at: '2026-07-01T00:00:00.000Z',
        updated_at: '2026-07-01T00:00:00.000Z',
        // Exposed (issue #108): rotating is the remediation, so the refetch
        // after a successful rotate should clear the badge below.
        exposed_run_id: 'run_leaker',
        exposed_at: '2026-07-05T00:00:00.000Z',
      },
    ];
    await mountSettings();
    await waitFor(() => secretsSection().querySelector('.card-title.mono'), 'secret row');

    expect(secretsSection().querySelector('.chip.exposed')).not.toBeNull();
    expect(secretsSection().textContent).toContain('Exposed in run');
    expect(
      secretsSection().querySelector<HTMLAnchorElement>('a[href="/runs/run_leaker"]'),
    ).not.toBeNull();

    button('Rotate').click();
    await settle();

    const rotateField = input('secret-rotate-value');
    expect(rotateField.type).toBe('password');
    typeInto(rotateField, 'rotated-secret-value');

    submitFormWithin(secretsSection());
    await settle();

    expect(secretRequestBodies).toEqual([{ value: 'rotated-secret-value' }]);
    // The row collapses back out of rotate mode and the list refreshes.
    expect(secretsSection().querySelector('input[name="secret-rotate-value"]')).toBeNull();
    expect(secretsSection().textContent).not.toContain('rotated-secret-value');
    // The refetched row has null exposure fields (the mock's rotate handler
    // mirrors RotateRepoSecret's clear-on-rotate) — the badge is gone.
    expect(secretsSection().querySelector('.chip.exposed')).toBeNull();
    expect(secretsSection().textContent).not.toContain('Exposed in run');
  });

  it('delete asks for confirmation before removing the secret', async () => {
    secretsOnServer = [
      {
        id: 'sec_1',
        name: 'API_KEY',
        description: '',
        created_at: '2026-07-01T00:00:00.000Z',
        updated_at: '2026-07-01T00:00:00.000Z',
        exposed_run_id: null,
        exposed_at: null,
      },
    ];
    await mountSettings();
    await waitFor(() => secretsSection().querySelector('.card-title.mono'), 'secret row');

    // Declining the confirm leaves the secret in place.
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValueOnce(false);
    button('Delete').click();
    await settle();
    expect(confirmSpy).toHaveBeenCalledWith('Delete secret "API_KEY"?');
    expect(secretsSection().textContent).toContain('API_KEY');

    // Confirming removes it.
    confirmSpy.mockReturnValueOnce(true);
    button('Delete').click();
    await settle();
    expect(secretsOnServer).toHaveLength(0);
    expect(secretsSection().textContent).toContain('No secrets yet');
  });
});
