// New run page contract (issue #66; the composer since issue #41; at /new
// with a phone-only "New run" header since issue #76):
// - with repos present the page is the repository pills, the composer (the
//   field with Model / Effort / ⋯ chips and the send) and the selected repo's
//   Issues card — no Repository chip, no Agent chip in the bar, no AFK strip,
//   no readiness line; zero repos shows the exact "No repositories yet" empty
//   state instead (the Playwright smoke and login/setup round-trip assert it);
// - Send spawns the selected repo with label/model/effort and the typed text
//   as first_message (issue #96 — one POST), then navigates to /runs/:id; an
//   empty box spawns a plain run; a failure keeps the text and shows the
//   server message verbatim;
// - a run option is two taps (chip → option); a pick that differs from the
//   inherited default outlines its chip in the accent; agent, remote control
//   and label live in More options, and only explicit agent/remote picks ride
//   the request (ADR-0030, issue #163); per-model efforts (issue #156);
// - the pills: recent repos from `lab.last-repo` (a JSON array now; the old
//   bare id still preselects), "All N" opens the repository picker in place
//   and never navigates; picking a repo records it and clears the attachment;
// - an issue's action (Triage / Implement / Discuss) attaches to the composer
//   and rides as first_message with the `<action>-<n>` label default;
// - blockers at the composer: logged out, cloning and clone failed disable
//   the field; a failing tracker check warns and leaves it enabled; the host
//   Runner warning shows whenever the effective Runner is host.
//
// The pickers render in a Portal on document.body, so they are queried on
// `document`, not on the mount container.

import { MemoryRouter, Route, createMemoryHistory, useParams } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { IssueSummary, Provider, ProviderAuthStatus, Repo } from '../api';
import App from '../App';
import NewRun from './NewRun';

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((event: { data: string }) => void)[]>();

  constructor() {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: { data: string }) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  close(): void {}

  /** Delivers a server event to this connection's subscribers. */
  emit(type: string, payload: Record<string, unknown>): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data: JSON.stringify(payload) });
    }
  }
}

/** The app's one SSE connection (events.tsx). */
function eventSource(): FakeEventSource {
  const source = FakeEventSource.instances.at(-1);
  if (source === undefined) throw new Error('no EventSource opened');
  return source;
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
    created_at: '2026-07-06T00:00:00.000Z',
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
    runner: 'container',
    container_memory: null,
    container_pids: null,
    container_nofile: null,
    image_ref: null,
    ...overrides,
  };
}

// Both claude-shaped models share one effort catalog and report NO
// default_effort — the first-entry rule keeps resolving "low", as before
// issue #156 enriched the model entries.
const CLAUDE_EFFORTS = [
  { value: 'low', label: 'Low' },
  { value: 'high', label: 'High' },
];

const PROVIDERS: Provider[] = [
  {
    id: 'claude-code',
    display_name: 'Claude Code',
    supports_remote: true,
    auth: { kind: 'oauth-code' },
    models: [
      { value: 'sonnet', label: 'Sonnet', efforts: CLAUDE_EFFORTS },
      { value: 'opus', label: 'Opus', efforts: CLAUDE_EFFORTS },
    ],
    efforts: CLAUDE_EFFORTS,
    options: [],
  },
];

/** A second provider WITHOUT an effort knob (empty efforts catalogs). */
const CODEX: Provider = {
  id: 'codex',
  display_name: 'Codex',
  // The remote-control knob is claude-only (issue #163): codex ignores it, so
  // the composer's toggle must render disabled with a note naming THIS provider.
  supports_remote: false,
  auth: { kind: 'api-key' },
  models: [
    { value: 'gpt-5-codex', label: 'GPT-5 Codex', efforts: [] },
    { value: 'gpt-5', label: 'GPT-5', efforts: [] },
  ],
  efforts: [],
  options: [],
};

// A provider whose models carry DIFFERENT effort catalogs + reported defaults
// (issue #156): terra offers the full ladder up to ultra, luna stops at high.
// The provider-level `efforts` stays the union — the settings pickers' list,
// which the composer must NOT use.
const TERRA_EFFORTS = [
  { value: 'low', label: 'Low' },
  { value: 'medium', label: 'Medium' },
  { value: 'high', label: 'High' },
  { value: 'xhigh', label: 'X-High' },
  { value: 'max', label: 'Max' },
  { value: 'ultra', label: 'Ultra' },
];
const LUNA_EFFORTS = [
  { value: 'low', label: 'Low' },
  { value: 'medium', label: 'Medium' },
  { value: 'high', label: 'High' },
];
const GPT: Provider = {
  id: 'gpt',
  display_name: 'GPT',
  supports_remote: true,
  auth: { kind: 'api-key' },
  models: [
    {
      value: 'gpt-5.6-terra',
      label: 'GPT-5.6-Terra',
      efforts: TERRA_EFFORTS,
      default_effort: 'medium',
    },
    {
      value: 'gpt-5.6-luna',
      label: 'GPT-5.6-Luna',
      efforts: LUNA_EFFORTS,
      default_effort: 'medium',
    },
  ],
  efforts: TERRA_EFFORTS,
  options: [],
};

function issueFixture(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    number: 47,
    title: 'Rename the module path',
    body: '',
    state: 'open',
    labels: ['needs-triage'],
    comments_count: 0,
    created_at: '2026-10-01T00:00:00.000Z',
    updated_at: '2026-10-01T00:00:00.000Z',
    ...overrides,
  };
}

/** A readiness report whose tracker check fails, naming the field that fixes it. */
function failingTracker(): Repo['summary'] {
  return {
    claimable: null,
    open_issues: null,
    readiness: {
      state: 'failing',
      checks: [
        {
          id: 'tracker',
          state: 'failing',
          detail: 'The tracker token was refused.',
          fix: { scope: 'repo', section: 'integrations', field: 'forge_credential_id' },
        },
      ],
    },
  };
}

let reposOnServer: Repo[];
let providersOnServer: Provider[];
let settingsOnServer: Record<string, unknown>;
let authOnServer: ProviderAuthStatus;
let issuesOnServer: IssueSummary[];
let authRequests: string[];
let repoListRequests: number;
let retryRequests: string[];
let instancePost: { status: number; runID: string };
let instancePosts: { repo: string; body: Record<string, unknown> }[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function jsonResponse(status: number, body?: unknown) {
  const text = body === undefined ? '' : JSON.stringify(body);
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
        repoListRequests += 1;
        return Promise.resolve(jsonResponse(200, { repos: reposOnServer }));
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: providersOnServer }));
      }
      if (url === '/api/v1/settings' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...settingsOnServer }));
      }
      // Per-provider-id auth route (issue #51 decision 7), keyed on the
      // EFFECTIVE provider; the requested id is recorded for assertions.
      const authMatch = /^\/api\/v1\/providers\/([^/]+)\/auth\/status$/.exec(url);
      if (authMatch !== null && method === 'GET') {
        authRequests.push(authMatch[1]!);
        return Promise.resolve(jsonResponse(200, authOnServer));
      }
      // The Issues card's bounded read (ADR-0050).
      if (/^\/api\/v1\/repos\/[^/]+\/issues\?state=open$/.test(url) && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { issues: issuesOnServer }));
      }
      const retryMatch = /^\/api\/v1\/repos\/([^/]+)\/clone\/retry$/.exec(url);
      if (retryMatch !== null && method === 'POST') {
        retryRequests.push(retryMatch[1]!);
        return Promise.resolve(jsonResponse(202));
      }
      const spawnMatch = /^\/api\/v1\/repos\/([^/]+)\/instances$/.exec(url);
      if (spawnMatch !== null && method === 'POST') {
        instancePosts.push({
          repo: spawnMatch[1]!,
          body: JSON.parse(String(init?.body)) as Record<string, unknown>,
        });
        if (instancePost.status >= 400) {
          return Promise.resolve(jsonResponse(instancePost.status, { error: 'cap reached (2/2)' }));
        }
        return Promise.resolve(
          jsonResponse(201, { id: instancePost.runID, repo_id: spawnMatch[1]! }),
        );
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

/** The request bodies of the spawn POSTs, in order. */
const posts = () => instancePosts.map((p) => p.body);

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flush();
}

function RunStub() {
  const params = useParams<{ id: string }>();
  return <div class="run-stub">run:{params.id}</div>;
}

async function mountNewRun(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/new' });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/new" component={NewRun} />
        <Route path="/runs/:id" component={RunStub} />
        <Route path="*" component={() => <p class="elsewhere">navigated</p>} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

/** A run-option chip (Model / Effort) by its name; its aria-label is "Model: Sonnet". */
function chip(name: string): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>(`button.run-chip[aria-label^="${name}: "]`);
}

/** The text a run-option chip shows. */
function chipLabel(name: string): string | null {
  return chip(name)?.querySelector('.composer-chip-label')?.textContent ?? null;
}

function moreChip(): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>('button[aria-label="More options"]')!;
}

const picker = () => document.querySelector<HTMLElement>('.picker');

/** The open picker's option rows, by title. */
function optionTitles(): string[] {
  return Array.from(document.querySelectorAll('.picker [role="option"] .picker-option-title')).map(
    (el) => el.textContent ?? '',
  );
}

/** Opens a chip's picker and clicks the option with the given title: two taps. */
async function chooseFromChip(name: string, optionTitle: string): Promise<void> {
  chip(name)!.click();
  await settle();
  const row = Array.from(
    document.querySelectorAll<HTMLButtonElement>('.picker [role="option"]'),
  ).find((r) => r.querySelector('.picker-option-title')?.textContent === optionTitle);
  if (!row) throw new Error(`missing option ${JSON.stringify(optionTitle)} in ${name}`);
  row.click();
  await settle();
}

/** Opens a chip's picker, reads its option titles, and closes it. */
async function chipOptionTitles(name: string): Promise<string[]> {
  chip(name)!.click();
  await settle();
  const titles = optionTitles();
  chip(name)!.click(); // the chip toggles: a second click closes the picker
  await settle();
  return titles;
}

/** Opens More options (when closed) and hands back its panel. */
async function openMore(): Promise<HTMLElement> {
  if (document.querySelector('.picker.run-more') === null) {
    moreChip().click();
    await settle();
  }
  const panel = document.querySelector<HTMLElement>('.picker.run-more');
  if (panel === null) throw new Error('More options did not open');
  return panel;
}

/** Picks an agent in More options' segmented control. */
async function chooseAgent(name: string): Promise<void> {
  const panel = await openMore();
  const radio = Array.from(panel.querySelectorAll<HTMLButtonElement>('[role="radio"]')).find(
    (r) => r.textContent === name,
  );
  if (!radio) throw new Error(`missing agent ${JSON.stringify(name)}`);
  radio.click();
  await settle();
}

/** The agent More options shows as checked. */
async function checkedAgent(): Promise<string | null> {
  const panel = await openMore();
  return panel.querySelector('[role="radio"][aria-checked="true"]')?.textContent ?? null;
}

function closePicker(): Promise<void> {
  document.querySelector<HTMLButtonElement>('.picker .picker-close')?.click();
  return settle();
}

function sendButton(): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>('button.composer-send')!;
}

function pill(name: string): HTMLButtonElement | undefined {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('.repo-pill')).find(
    (b) => b.textContent === name,
  );
}

function storedRecent(): string | null {
  return localStorage.getItem('lab.last-repo');
}

// jsdom has no window.matchMedia — install a fake that resolves ONLY the
// fine-pointer query isComposerSend checks (ADR-0031, issue #70). Must match
// query strings exactly and read `false` for anything else. vi.stubGlobal
// ties the mock's lifetime to the existing vi.unstubAllGlobals() in
// afterEach, so no separate cleanup is needed here.
function finePointer(matches: boolean): void {
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches: matches && query === '(hover: hover) and (pointer: fine)',
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      onchange: null,
      dispatchEvent: () => false,
    })),
  );
}

function composerInput(): HTMLTextAreaElement {
  return container.querySelector('.composer-input') as HTMLTextAreaElement;
}

function typeText(value: string): void {
  const input = composerInput();
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
}

function typeLabel(panel: HTMLElement, value: string): void {
  const input = panel.querySelector('input[name="label"]') as HTMLInputElement;
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
}

/** Taps issue #n on the Issues card and chooses an action in the sheet. */
async function attachAction(n: number, action: 'Triage' | 'Implement' | 'Discuss'): Promise<void> {
  const row = Array.from(
    container.querySelectorAll<HTMLButtonElement>('.issues-card .issue-row'),
  ).find((r) => r.querySelector('.issue-row-number')?.textContent === `#${n}`);
  if (!row) throw new Error(`missing issue row #${n}`);
  row.click();
  await settle();
  const button = Array.from(
    document.querySelectorAll<HTMLButtonElement>('.issue-action-sheet .issue-action'),
  ).find((b) => b.querySelector('.issue-action-name')?.textContent?.startsWith(action));
  if (!button) throw new Error(`missing action ${action}`);
  button.click();
  await settle();
}

beforeEach(() => {
  reposOnServer = [repoFixture()];
  providersOnServer = [...PROVIDERS];
  settingsOnServer = {};
  authOnServer = { logged_in: true, email: 'me@x', method: 'oauth', checked_at: '' };
  issuesOnServer = [];
  authRequests = [];
  repoListRequests = 0;
  retryRequests = [];
  instancePost = { status: 201, runID: 'run_new' };
  instancePosts = [];
  // A stale recent list must not strand the composer; default to the empty slate.
  localStorage.removeItem('lab.last-repo');
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  document.body.style.overflow = '';
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('NewRun page', () => {
  it('renders the pills, the composer and the Issues card (not the empty state) when repos exist', async () => {
    await mountNewRun();

    expect(container.querySelector('.repo-pills')).not.toBeNull();
    expect(pill('coding-lab')?.getAttribute('aria-pressed')).toBe('true');
    expect(container.querySelector('.repo-pill-all')?.textContent).toContain('All 1');
    expect(container.querySelector('.composer-field')).not.toBeNull();
    expect(composerInput().placeholder).toBe('Describe a task for coding-lab…');
    expect(chipLabel('Model')).toBe('Sonnet');
    expect(chipLabel('Effort')).toBe('Low');
    expect(moreChip()).not.toBeNull();
    expect(container.querySelector('.issues-card h2')?.textContent).toBe('Issues');
    // The old bar's Select chips, the AFK strip and the logged-out line are gone.
    expect(container.querySelector('button[aria-label="Repository"]')).toBeNull();
    expect(container.querySelector('button[aria-label="Agent"]')).toBeNull();
    expect(container.querySelector('.afk-strip')).toBeNull();
    expect(container.querySelector('.newrun-warn')).toBeNull();
    // A repo that can run shows nothing about it: no blocker, no readiness line.
    expect(container.querySelector('.composer-blockers')).toBeNull();
    expect(container.textContent).not.toContain('No repositories yet');
    // DOM order: the phone header, pills, composer, Issues card (CSS moves the
    // dock last on a phone).
    const parts = Array.from(container.querySelectorAll('main.newrun > div[class^="newrun-"]')).map(
      (el) => el.className,
    );
    expect(parts).toEqual(['newrun-head', 'newrun-pills', 'newrun-dock', 'newrun-issues']);
  });

  it('opens with a "New run" page header below 1024px (issue #76)', async () => {
    await mountNewRun();
    const head = container.querySelector('main.newrun > .newrun-head');
    expect(head?.querySelector('.section-head h2')?.textContent).toBe('New run');
  });

  it('keeps the header in the zero-repos state too', async () => {
    reposOnServer = [];
    await mountNewRun();
    expect(container.querySelector('.newrun-head h2')?.textContent).toBe('New run');
    expect(container.textContent).toContain('No repositories yet');
  });

  it('has no page header from 1024px: the centered column stays bare', async () => {
    vi.stubGlobal(
      'matchMedia',
      vi.fn((query: string) => ({
        matches: query === '(min-width: 1024px)',
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
        onchange: null,
        dispatchEvent: () => false,
      })),
    );
    await mountNewRun();
    expect(container.querySelector('.newrun-head')).toBeNull();
    expect(container.querySelector('.composer-field')).not.toBeNull();
  });

  it('spawns with label/model/effort and the typed text as first_message, and navigates', async () => {
    await mountNewRun();

    typeText('do the thing');
    await chooseFromChip('Model', 'Opus');
    await chooseFromChip('Effort', 'High');
    typeLabel(await openMore(), 'debug');
    await closePicker();

    sendButton().click();
    await settle();

    // The typed text rides the spawn as first_message (issue #96) — exactly one
    // POST, no post-spawn queue hop.
    expect(instancePosts).toEqual([
      {
        repo: 'repo_1',
        body: { label: 'debug', model: 'opus', effort: 'high', first_message: 'do the thing' },
      },
    ]);
    // …and the composer navigated to the chat.
    expect(container.textContent).toContain('run:run_new');
  });

  it('spawns a plain run with no first_message when the box is empty', async () => {
    await mountNewRun();

    sendButton().click();
    await settle();

    // No label (untouched); model/effort default to the first catalog option;
    // an empty box sends NO first_message key (issue #96).
    expect(posts()).toEqual([{ model: 'sonnet', effort: 'low' }]);
    expect(container.textContent).toContain('run:run_new');
  });

  it('keeps the text and shows the server message verbatim on a spawn failure', async () => {
    instancePost = { status: 409, runID: 'run_new' };
    await mountNewRun();

    typeText('try me');
    sendButton().click();
    await settle();

    expect(instancePosts).toHaveLength(1);
    // The banner shows the raw 409, the text stays, and nothing navigated.
    expect(container.querySelector('.banner.error')?.textContent).toContain('cap reached (2/2)');
    expect(composerInput().value).toBe('try me');
    expect(container.textContent).not.toContain('run:run_new');
  });

  it('shows the exact zero-repos empty state and hides the composer', async () => {
    reposOnServer = [];
    await mountNewRun();

    expect(container.textContent).toContain('No repositories yet');
    expect(container.querySelector('.composer-field')).toBeNull();
    expect(container.querySelector('.repo-pills')).toBeNull();
    expect(container.querySelector('.issues-card')).toBeNull();
    // The plain page: none of the docked layout.
    expect(container.querySelector('main.newrun')?.classList.contains('newrun-docked')).toBe(false);
    const addLink = Array.from(container.querySelectorAll('a')).find(
      (a) => a.getAttribute('href') === '/repos/new',
    );
    expect(addLink?.textContent).toContain('add one');
  });
});

// Model and Effort (issue #66): a chip opens its picker at once and an option
// closes it — two taps; the inherited default is marked, the hint names where
// it comes from, and a pick that differs from it outlines the chip.
describe('NewRun run-option chips', () => {
  it('changes the model in two taps; the chip takes the accent and the request carries it', async () => {
    await mountNewRun();
    expect(chip('Model')!.classList.contains('changed')).toBe(false);

    chip('Model')!.click(); // tap 1
    await settle();
    expect(picker()).not.toBeNull();
    expect(optionTitles()).toEqual(['Sonnet', 'Opus']);
    // The inherited default is marked, the current value checked.
    const sonnet = document.querySelector('.picker [role="option"]')!;
    expect(sonnet.querySelector('.picker-option-default')).not.toBeNull();
    expect(sonnet.getAttribute('aria-selected')).toBe('true');
    expect(picker()!.querySelector('.picker-hint')?.textContent).toBe(
      'The default comes from global Settings. A pick here applies to this run only.',
    );

    Array.from(document.querySelectorAll<HTMLButtonElement>('.picker [role="option"]'))[1]!.click(); // tap 2
    await settle();

    expect(picker()).toBeNull();
    expect(chipLabel('Model')).toBe('Opus');
    expect(chip('Model')!.classList.contains('changed')).toBe(true);

    sendButton().click();
    await settle();
    expect(posts()[0]).toMatchObject({ model: 'opus' });
  });

  it('keeps the model, effort, agent and attachment picks across a repo.changed refetch', async () => {
    providersOnServer = [...PROVIDERS, CODEX];
    issuesOnServer = [issueFixture()];
    await mountNewRun();
    await chooseFromChip('Model', 'Opus');
    await chooseFromChip('Effort', 'High');
    await attachAction(47, 'Triage');
    typeText('fix the thing');
    expect(chipLabel('Model')).toBe('Opus');
    const listsBefore = repoListRequests;

    // The server announces the same repo (a readiness verdict, an AFK sweep,
    // a clone landing elsewhere): the list refetches, the selected repo and
    // its provider are unchanged, so nothing the operator picked may move.
    eventSource().emit('repo.changed', { repoID: 'repo_1' });
    await settle();

    expect(repoListRequests).toBe(listsBefore + 1);
    expect(chipLabel('Model')).toBe('Opus');
    expect(chipLabel('Effort')).toBe('High');
    expect(container.querySelector('.composer-attach')).not.toBeNull();
    expect(composerInput().value).toBe('fix the thing');

    sendButton().click();
    await settle();
    expect(posts()[0]).toMatchObject({ model: 'opus', effort: 'high', label: 'triage-47' });
  });

  it("names the repo's settings as the source when the repo's own default applies", async () => {
    reposOnServer = [repoFixture({ model_default: 'opus', effort_default: 'high' })];
    settingsOnServer = { spawn_model_default: 'sonnet' };
    await mountNewRun();

    expect(chipLabel('Model')).toBe('Opus');
    chip('Model')!.click();
    await settle();
    expect(picker()!.querySelector('.picker-hint')?.textContent).toContain(
      "The default comes from coding-lab's settings.",
    );
    await closePicker();

    chip('Effort')!.click();
    await settle();
    expect(picker()!.querySelector('.picker-hint')?.textContent).toContain("coding-lab's settings");
  });

  it('picking the inherited default again clears the pick and the accent', async () => {
    await mountNewRun();

    await chooseFromChip('Model', 'Opus');
    expect(chip('Model')!.classList.contains('changed')).toBe(true);
    await chooseFromChip('Model', 'Sonnet');
    expect(chip('Model')!.classList.contains('changed')).toBe(false);

    await chooseFromChip('Effort', 'High');
    expect(chip('Effort')!.classList.contains('changed')).toBe(true);
  });
});

// Composer keyboard send (ADR-0031, issue #70): shares isComposerSend with
// the chat composer. Bare Enter spawns only on a fine-pointer setup;
// Shift/Alt+Enter never spawn; Cmd/Ctrl+Enter spawns everywhere, including on
// an empty box — unlike the chat composer, an empty box is a valid "plain
// spawn" here, so bare Enter needs its own empty guard (which an attached
// issue action lifts) while Cmd/Ctrl+Enter keeps sending through.
describe('NewRun composer keyboard send (issue #70)', () => {
  it('fine-pointer: Shift+Enter never spawns; bare Enter spawns and sends the typed text as first_message', async () => {
    finePointer(true);
    await mountNewRun();
    typeText('do the thing');
    await settle();

    const shiftEnter = new KeyboardEvent('keydown', {
      key: 'Enter',
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    composerInput().dispatchEvent(shiftEnter);
    await settle();
    expect(instancePosts).toHaveLength(0);
    expect(shiftEnter.defaultPrevented).toBe(false); // browser-default newline left alone

    composerInput().dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(instancePosts).toHaveLength(1);
    expect(posts()[0]).toMatchObject({ first_message: 'do the thing' });
    expect(container.textContent).toContain('run:run_new');
  });

  it('fine-pointer: Cmd/Ctrl+Enter spawns', async () => {
    finePointer(true);
    await mountNewRun();
    typeText('ctrl spawn');
    await settle();

    composerInput().dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(instancePosts).toHaveLength(1);
    expect(posts()[0]).toMatchObject({ first_message: 'ctrl spawn' });
  });

  it('bare Enter never spawns without a fine pointer (no matchMedia, or a touch profile)', async () => {
    // Default jsdom: no window.matchMedia at all — reads as "not fine-pointer".
    await mountNewRun();
    typeText('no matchMedia here');
    await settle();

    composerInput().dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(instancePosts).toHaveLength(0);

    composerInput().dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(instancePosts).toHaveLength(1);
    expect(posts()[0]).toMatchObject({ first_message: 'no matchMedia here' });
  });

  it('touch profile: bare Enter does not spawn; Cmd/Ctrl+Enter still does', async () => {
    finePointer(false);
    await mountNewRun();
    typeText('tap city');
    await settle();

    composerInput().dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(instancePosts).toHaveLength(0);

    composerInput().dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(instancePosts).toHaveLength(1);
    expect(posts()[0]).toMatchObject({ first_message: 'tap city' });
  });

  it('fine-pointer: Enter fired mid-IME-composition does not spawn', async () => {
    finePointer(true);
    await mountNewRun();
    typeText('still composing');
    await settle();

    composerInput().dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'Enter',
        isComposing: true,
        bubbles: true,
        cancelable: true,
      }),
    );
    await settle();
    expect(instancePosts).toHaveLength(0);
  });

  it('fine-pointer + empty box: bare Enter does not spawn (and preventDefaults it); Cmd/Ctrl+Enter still spawns the plain run', async () => {
    finePointer(true);
    await mountNewRun();

    const evt = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    composerInput().dispatchEvent(evt);
    await settle();
    expect(instancePosts).toHaveLength(0);
    expect(evt.defaultPrevented).toBe(true); // no stray newline in an already-empty box

    composerInput().dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    // Empty text is a valid "plain spawn" — Cmd/Ctrl+Enter keeps spawning it
    // even under the bare-Enter gate.
    expect(instancePosts).toHaveLength(1);
    expect(posts()[0]).not.toHaveProperty('first_message');
    expect(container.textContent).toContain('run:run_new');
  });

  it('fine-pointer + empty box with an issue action attached: bare Enter sends it', async () => {
    finePointer(true);
    issuesOnServer = [issueFixture()];
    await mountNewRun();
    await attachAction(47, 'Triage');

    composerInput().dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(posts()).toEqual([
      { label: 'triage-47', model: 'sonnet', effort: 'low', first_message: '/triage #47' },
    ]);
  });

  it('carries the "Start run (Enter)" tooltip on the send button', async () => {
    await mountNewRun();
    expect(sendButton().title).toBe('Start run (Enter)');
    expect(sendButton().getAttribute('aria-label')).toBe('Start run');
  });
});

// The agent (ADR-0030): in More options, a segmented control only with ≥2
// registered providers; the effective provider = ephemeral pick → repo
// override → global provider_default → first provider, and everything
// downstream (model/effort catalogs, the logged-out blocker) follows it.
describe('NewRun agent in More options (multi-provider)', () => {
  beforeEach(() => {
    providersOnServer = [...PROVIDERS, CODEX];
  });

  it('offers no agent control with a single provider', async () => {
    providersOnServer = [...PROVIDERS];
    await mountNewRun();

    const panel = await openMore();
    expect(panel.querySelector('[role="radiogroup"]')).toBeNull();
  });

  it('resolves the global provider_default when the repo inherits', async () => {
    reposOnServer = [repoFixture({ provider: null })];
    settingsOnServer = { provider_default: 'codex' };
    await mountNewRun();

    // The model catalog follows the effective provider…
    expect(chipLabel('Model')).toBe('GPT-5 Codex');
    // …and a provider without an effort knob renders NO effort chip at all.
    expect(chip('Effort')).toBeNull();
    expect(await checkedAgent()).toBe('Codex');
  });

  it('lets the repo provider override the global default', async () => {
    reposOnServer = [repoFixture({ provider: 'claude-code' })];
    settingsOnServer = { provider_default: 'codex' };
    await mountNewRun();

    expect(chipLabel('Model')).toBe('Sonnet');
    expect(chip('Effort')).not.toBeNull();
    expect(await checkedAgent()).toBe('Claude Code');
  });

  it('picking an agent re-catalogs model/effort, resets prior picks, outlines ⋯ and rides the POST', async () => {
    reposOnServer = [repoFixture({ provider: 'claude-code' })];
    await mountNewRun();

    // Foreign picks made under the previous provider…
    await chooseFromChip('Model', 'Opus');
    await chooseFromChip('Effort', 'High');
    expect(moreChip().classList.contains('changed')).toBe(false);

    await chooseAgent('Codex');
    await closePicker();

    // …are reset: the model chip re-catalogs to the new provider's default
    // and the effort chip disappears (empty efforts catalog).
    expect(chipLabel('Model')).toBe('GPT-5 Codex');
    expect(chip('Effort')).toBeNull();
    expect(moreChip().classList.contains('changed')).toBe(true);

    sendButton().click();
    await settle();

    // The explicit pick rides along; the stale Opus/High picks must NOT.
    expect(posts()).toEqual([{ provider: 'codex', model: 'gpt-5-codex' }]);
  });

  it('omits provider from the POST when the operator never touched the agent', async () => {
    reposOnServer = [repoFixture({ provider: 'codex' })];
    await mountNewRun();

    sendButton().click();
    await settle();

    // The repo/global layers are the SERVER's to resolve — only an explicit
    // per-spawn pick is sent.
    expect(posts()).toEqual([{ model: 'gpt-5-codex' }]);
  });

  it('resets the ephemeral pick on repo switch', async () => {
    reposOnServer = [
      repoFixture({ provider: 'claude-code' }),
      repoFixture({ id: 'repo_2', name: 'other-repo', provider: 'claude-code' }),
    ];
    await mountNewRun();

    await chooseAgent('Codex');
    await closePicker();
    expect(chip('Effort')).toBeNull();

    pill('other-repo')!.click();
    await settle();

    // The pick is ephemeral: the new repo resolves its own effective provider
    // and the model/effort surfaces follow it again.
    expect(chipLabel('Model')).toBe('Sonnet');
    expect(chip('Effort')).not.toBeNull();
    expect(await checkedAgent()).toBe('Claude Code');
  });

  it('keys the logged-out blocker on the effective provider and names its display_name', async () => {
    reposOnServer = [repoFixture({ provider: 'codex' })];
    authOnServer = { logged_in: false, email: '', method: '', checked_at: '' };
    await mountNewRun();

    // The status route was asked about the EFFECTIVE provider…
    expect(authRequests).toContain('codex');
    // …and the banner copy flows from its display_name.
    expect(container.querySelector('.composer-blocker-logged-out')?.textContent).toContain(
      'Codex is logged out.',
    );
  });
});

// Per-model efforts (issue #156): the effort chip catalogs the SELECTED
// MODEL, not the provider union — effort support varies per model and codex
// does not clamp, so an unsupported model+effort combo would 400 at spawn. A
// stale pick snaps to the new model's reported default; a still-valid pick is
// kept.
describe('NewRun composer per-model efforts (issue #156)', () => {
  beforeEach(() => {
    providersOnServer = [GPT];
    reposOnServer = [repoFixture({ provider: 'gpt' })];
  });

  it('catalogs the effort chip from the selected model and re-catalogs on a model switch', async () => {
    await mountNewRun();

    // Terra (the resolved default model) offers the full ladder…
    expect(await chipOptionTitles('Effort')).toEqual([
      'Low',
      'Medium',
      'High',
      'X-High',
      'Max',
      'Ultra',
    ]);

    await chooseFromChip('Model', 'GPT-5.6-Luna');

    // …luna only its own list: Ultra (and the rest of the union) is gone.
    expect(await chipOptionTitles('Effort')).toEqual(['Low', 'Medium', 'High']);
  });

  it("snaps a stale effort pick to the new model's default_effort on the POST", async () => {
    await mountNewRun();

    await chooseFromChip('Effort', 'Ultra');
    await chooseFromChip('Model', 'GPT-5.6-Luna');

    // Luna has no "ultra": the chip already shows the snapped default…
    expect(chipLabel('Effort')).toBe('Medium');

    sendButton().click();
    await settle();

    // …and the POST carries luna's reported default, never the stale pick.
    expect(posts()).toEqual([{ model: 'gpt-5.6-luna', effort: 'medium' }]);
  });

  it('keeps a still-valid effort pick across a model switch', async () => {
    await mountNewRun();

    await chooseFromChip('Effort', 'High');
    await chooseFromChip('Model', 'GPT-5.6-Luna');

    // Luna supports "high" too: the explicit pick survives the switch…
    expect(chipLabel('Effort')).toBe('High');

    sendButton().click();
    await settle();

    // …and rides the POST.
    expect(posts()).toEqual([{ model: 'gpt-5.6-luna', effort: 'high' }]);
  });

  it("an untouched composer sends the model's reported default_effort, not the first entry", async () => {
    await mountNewRun();

    sendButton().click();
    await settle();

    // Terra's efforts START at "low" but report "medium" as the default —
    // the reported default beats the first-entry rule.
    expect(posts()).toEqual([{ model: 'gpt-5.6-terra', effort: 'medium' }]);
  });

  it('a global default effort valid for the model beats the model default', async () => {
    settingsOnServer = { spawn_effort_default: 'xhigh' };
    await mountNewRun();

    sendButton().click();
    await settle();

    expect(posts()).toEqual([{ model: 'gpt-5.6-terra', effort: 'xhigh' }]);
  });

  it('skips a global default effort the model does not support; the model default rides', async () => {
    settingsOnServer = { spawn_effort_default: 'turbo' };
    await mountNewRun();

    sendButton().click();
    await settle();

    expect(posts()).toEqual([{ model: 'gpt-5.6-terra', effort: 'medium' }]);
  });
});

// Remote control in More options (issue #163): pre-filled from the RESOLVED
// default (repo override → global default → off) and sent ONLY when the
// operator's value differs from it — `false` included, since an explicit off
// over an inherited on is a real pick, not an omission.
describe('NewRun composer remote control', () => {
  /** Opens More options and hands back its remote switch. */
  async function remoteSwitch(): Promise<HTMLButtonElement> {
    const panel = await openMore();
    const el = panel.querySelector<HTMLButtonElement>('button[role="switch"][name="remote"]');
    if (el === null) throw new Error('missing remote switch in More options');
    return el;
  }
  const isOn = (el: HTMLButtonElement) => el.getAttribute('aria-checked') === 'true';

  it('defaults off and sends no remote key when untouched', async () => {
    await mountNewRun();

    expect(isOn(await remoteSwitch())).toBe(false);
    expect(document.querySelector('.picker.run-more')?.textContent).toContain('inherited · off');
    await closePicker();
    sendButton().click();
    await settle();

    expect(posts()).toEqual([{ model: 'sonnet', effort: 'low' }]);
  });

  it('turning it on sends remote:true, reads "set here" and outlines ⋯', async () => {
    await mountNewRun();

    (await remoteSwitch()).click();
    await settle();
    expect(document.querySelector('.picker.run-more')?.textContent).toContain('set here');
    await closePicker();
    expect(moreChip().classList.contains('changed')).toBe(true);

    sendButton().click();
    await settle();
    expect(posts()).toEqual([{ model: 'sonnet', effort: 'low', remote: true }]);
  });

  it('pre-fills from the global default; an untouched inherited-on sends nothing', async () => {
    settingsOnServer = { spawn_remote_default: true };
    await mountNewRun();

    expect(isOn(await remoteSwitch())).toBe(true);
    await closePicker();
    sendButton().click();
    await settle();

    // The resolved default is the server's to walk — nothing to say here.
    expect(posts()[0]).not.toHaveProperty('remote');
  });

  it('pre-fills from the repo override and sends an explicit false when turned off', async () => {
    // The tri-state's whole point: `false` must ride the request, or the server
    // would re-resolve the inherited ON and ignore the operator.
    reposOnServer = [repoFixture({ remote_default: true })];
    settingsOnServer = { spawn_remote_default: false };
    await mountNewRun();

    const el = await remoteSwitch();
    expect(isOn(el)).toBe(true);
    el.click();
    await settle();
    await closePicker();
    sendButton().click();
    await settle();

    expect(posts()).toEqual([{ model: 'sonnet', effort: 'low', remote: false }]);
  });

  it('a repo override beats the global default in the pre-fill', async () => {
    reposOnServer = [repoFixture({ remote_default: false })];
    settingsOnServer = { spawn_remote_default: true };
    await mountNewRun();

    expect(isOn(await remoteSwitch())).toBe(false);
  });

  it('disables the switch with a note for a provider with no remote knob', async () => {
    providersOnServer = [...PROVIDERS, CODEX];
    reposOnServer = [repoFixture({ provider: 'codex' })];
    await mountNewRun();

    const el = await remoteSwitch();
    expect(el.disabled).toBe(true);
    // Named by display_name (issue #51 decision 9) — never a hardcoded brand.
    expect(document.querySelector('.picker.run-more')?.textContent).toContain('Codex ignores this');
    await closePicker();

    sendButton().click();
    await settle();
    expect(posts()[0]).not.toHaveProperty('remote');
  });

  it('a typed label outlines ⋯ and rides the request', async () => {
    await mountNewRun();

    typeLabel(await openMore(), 'mine');
    await closePicker();
    expect(moreChip().classList.contains('changed')).toBe(true);
    sendButton().click();
    await settle();
    expect(posts()[0]).toMatchObject({ label: 'mine' });
  });
});

// The Runner (issue #55): the repo's own, else the global runner_default. More
// options names it and links to the repo's Runner settings; a host Runner
// shows its warning above the field.
describe('NewRun Runner', () => {
  it('names an inherited container Runner and links to the Runner settings', async () => {
    reposOnServer = [repoFixture({ runner: null })];
    settingsOnServer = { runner_default: 'container' };
    await mountNewRun();

    expect(container.querySelector('.composer-blocker-host')).toBeNull();
    const panel = await openMore();
    const sentence = panel.querySelector('.run-more-runner');
    expect(sentence?.textContent).toContain('Runs in a container (inherited)');
    expect(sentence?.querySelector('a')?.getAttribute('href')).toBe(
      '/repos/repo_1/settings/runner',
    );
  });

  it("warns above the field when the repo's own Runner is host", async () => {
    reposOnServer = [repoFixture({ runner: 'host' })];
    settingsOnServer = { runner_default: 'container' };
    await mountNewRun();

    expect(container.querySelector('.composer-blocker-host')?.textContent).toContain(
      'Runs on the host, unsandboxed, with full host access.',
    );
    // A warning, not a blocker: the field stays usable.
    expect(composerInput().disabled).toBe(false);
  });

  it('warns when the inherited global Runner is host', async () => {
    reposOnServer = [repoFixture({ runner: null })];
    settingsOnServer = { runner_default: 'host' };
    await mountNewRun();

    expect(container.querySelector('.composer-blocker-host')).not.toBeNull();
  });
});

// The pills and the repository picker (issue #66): recent repos from
// `lab.last-repo`, "All N" opens the picker in place, a pick records the
// repo (startable ones only) and clears the attachment.
describe('NewRun repository pills', () => {
  beforeEach(() => {
    reposOnServer = [
      repoFixture(),
      repoFixture({ id: 'repo_2', name: 'other-repo', remote_url: 'git@github.com:o/other.git' }),
      repoFixture({ id: 'repo_3', name: 'third-repo' }),
    ];
  });

  it('"All N" opens the repository picker with a working filter and never navigates', async () => {
    await mountNewRun();

    const all = container.querySelector<HTMLButtonElement>('.repo-pill-all')!;
    expect(all.textContent).toContain('All 3');
    all.click();
    await settle();

    expect(picker()).not.toBeNull();
    expect(container.querySelector('.elsewhere')).toBeNull();
    const search = document.querySelector<HTMLInputElement>('.picker input.select-search')!;
    search.value = 'other';
    search.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(optionTitles()).toEqual(['other-repo']);

    document.querySelector<HTMLButtonElement>('.picker [role="option"]')!.click();
    await settle();

    expect(picker()).toBeNull();
    expect(pill('other-repo')?.getAttribute('aria-pressed')).toBe('true');
    expect(container.querySelector('.elsewhere')).toBeNull();
    expect(storedRecent()).toBe('["repo_2"]');

    sendButton().click();
    await settle();
    expect(instancePosts[0]?.repo).toBe('repo_2');
  });

  it('writes the recent list to lab.last-repo as a JSON array, most recent first', async () => {
    await mountNewRun();

    pill('third-repo')!.click();
    await settle();
    expect(storedRecent()).toBe('["repo_3"]');
    pill('coding-lab')!.click();
    await settle();
    expect(storedRecent()).toBe('["repo_1","repo_3"]');
    // A tap does not reorder the row under the operator's thumb.
    expect(
      Array.from(container.querySelectorAll('.repo-pill:not(.repo-pill-all)')).map(
        (b) => b.textContent,
      ),
    ).toEqual(['coding-lab', 'other-repo', 'third-repo']);
  });

  it('preselects the most recent repo from a stored JSON list', async () => {
    localStorage.setItem('lab.last-repo', JSON.stringify(['repo_3', 'repo_2']));
    await mountNewRun();

    expect(pill('third-repo')?.getAttribute('aria-pressed')).toBe('true');
    // The pills are the stored list, in order (not padded).
    expect(
      Array.from(container.querySelectorAll('.repo-pill:not(.repo-pill-all)')).map(
        (b) => b.textContent,
      ),
    ).toEqual(['third-repo', 'other-repo']);
  });

  it('still preselects from the old bare-id format', async () => {
    localStorage.setItem('lab.last-repo', 'repo_2');
    await mountNewRun();

    expect(pill('other-repo')?.getAttribute('aria-pressed')).toBe('true');
    sendButton().click();
    await settle();
    expect(instancePosts[0]?.repo).toBe('repo_2');
  });

  it('changing the repository clears the attachment', async () => {
    issuesOnServer = [issueFixture()];
    await mountNewRun();
    await attachAction(47, 'Triage');
    expect(container.querySelector('.composer-attach')).not.toBeNull();

    pill('other-repo')!.click();
    await settle();

    expect(container.querySelector('.composer-attach')).toBeNull();
    expect(composerInput().placeholder).toBe('Describe a task for other-repo…');
    expect(sendButton().getAttribute('aria-label')).toBe('Start run');
  });

  it("switching to a repo whose tracker fails shows none of the previous repo's issues", async () => {
    issuesOnServer = [issueFixture(), issueFixture({ number: 48 }), issueFixture({ number: 49 })];
    reposOnServer[1] = {
      ...reposOnServer[1]!,
      summary: { ...failingTracker(), open_issues: 8 },
    };
    await mountNewRun();
    expect(container.querySelector('.issues-card-count')?.textContent).toBe('3');

    pill('other-repo')!.click();
    await settle();

    // The summary's count, not the stale 3 read for coding-lab; no rows, no
    // AFK line, no Auto switch — the check's detail instead.
    expect(container.querySelector('.issues-card-count')?.textContent).toBe('8');
    expect(container.querySelector('.issue-row')).toBeNull();
    expect(container.querySelector('.issues-card-afk')).toBeNull();
    expect(container.querySelector('.issues-card-auto')).toBeNull();
    expect(container.querySelector('.issues-card')?.textContent).toContain(
      'The tracker token was refused.',
    );
  });
});

// An issue's action (issue #66): tapping a row asks what the agent should do;
// the choice attaches to the composer and rides as the run's first_message
// with the `<action>-<n>` label default (a typed label wins).
describe('NewRun issue actions', () => {
  beforeEach(() => {
    issuesOnServer = [
      issueFixture(),
      issueFixture({ number: 56, title: 'Flaky e2e', labels: ['ready-for-agent'] }),
    ];
  });

  it('Triage: attaches, relabels Send, and starts the run with /triage #n and label triage-n', async () => {
    await mountNewRun();

    await attachAction(47, 'Triage');

    expect(container.querySelector('.composer-attach-text')?.textContent).toBe(
      'Triage #47 · Rename the module path',
    );
    expect(composerInput().placeholder).toBe('Anything the agent should know? (optional)');
    expect(sendButton().getAttribute('aria-label')).toBe('Start: Triage #47');
    expect(document.activeElement).toBe(composerInput());

    typeText('Check the labels too');
    sendButton().click();
    await settle();

    expect(posts()).toHaveLength(1);
    const body = posts()[0]!;
    expect(String(body.first_message).startsWith('/triage #47')).toBe(true);
    expect(body).toEqual({
      label: 'triage-47',
      model: 'sonnet',
      effort: 'low',
      first_message: '/triage #47\nCheck the labels too',
    });
    expect(container.textContent).toContain('run:run_new');
  });

  it('a typed label wins over the action default', async () => {
    await mountNewRun();

    await attachAction(56, 'Implement');
    typeLabel(await openMore(), 'flaky');
    await closePicker();
    sendButton().click();
    await settle();

    expect(posts()[0]).toMatchObject({
      label: 'flaky',
      first_message:
        'Implement issue #56 "Flaky e2e". Read it with `labctl issue view 56` first; it is your brief.',
    });
  });

  it('Discuss asks what to discuss; removing the attachment restores the plain composer', async () => {
    await mountNewRun();

    await attachAction(47, 'Discuss');
    expect(composerInput().placeholder).toBe('Say what you want to discuss about #47…');

    container.querySelector<HTMLButtonElement>('.composer-attach-remove')!.click();
    await settle();

    expect(container.querySelector('.composer-attach')).toBeNull();
    expect(composerInput().placeholder).toBe('Describe a task for coding-lab…');
    sendButton().click();
    await settle();
    expect(posts()).toEqual([{ model: 'sonnet', effort: 'low' }]);
  });
});

// Blockers at the composer (issue #66): only what blocks a run, right above
// the field.
describe('NewRun blockers', () => {
  it('a logged-out agent: Reconnect banner, the field and Send disabled', async () => {
    authOnServer = { logged_in: false, email: '', method: '', checked_at: '' };
    await mountNewRun();

    const banner = container.querySelector('.composer-blocker-logged-out');
    // Copy flows from the provider's display_name, not a hardcoded brand.
    expect(banner?.textContent).toContain('Claude Code is logged out.');
    expect(banner?.querySelector('a')?.getAttribute('href')).toBe('/credentials');
    expect(composerInput().disabled).toBe(true);
    expect(sendButton().disabled).toBe(true);
    // More options stays usable: another agent may be logged in.
    expect(moreChip().disabled).toBe(false);
  });

  it('a cloning repo: the live-percent notice, the field disabled', async () => {
    reposOnServer = [repoFixture({ clone_status: 'cloning' })];
    await mountNewRun();

    expect(container.querySelector('.composer-blocker-cloning')?.textContent).toContain(
      'Runs can start when the clone finishes.',
    );
    expect(composerInput().disabled).toBe(true);
    expect(sendButton().disabled).toBe(true);
    // A pill tap on a repo that cannot start is not remembered.
    pill('coding-lab')!.click();
    await settle();
    expect(storedRecent()).toBeNull();
  });

  it('a failed clone: the clone error with a Retry that retries and refetches; the field disabled', async () => {
    reposOnServer = [repoFixture({ clone_status: 'error', clone_error: 'auth failed' })];
    await mountNewRun();

    const banner = container.querySelector('.composer-blocker-clone-failed');
    expect(banner?.textContent).toContain('auth failed');
    expect(composerInput().disabled).toBe(true);

    const before = repoListRequests;
    banner!.querySelector<HTMLButtonElement>('button.composer-blocker-action')!.click();
    await settle();
    expect(retryRequests).toEqual(['repo_1']);
    expect(repoListRequests).toBeGreaterThan(before);
  });

  it('a failing tracker check: the warning with Fix, and the field stays enabled', async () => {
    reposOnServer = [repoFixture({ summary: failingTracker() })];
    await mountNewRun();

    const banner = container.querySelector('.composer-blocker-tracker');
    expect(banner?.textContent).toContain('The tracker token was refused. A run can still start.');
    expect(banner?.querySelector('a')?.getAttribute('href')).toBe(
      '/repos/repo_1/settings/integrations?field=forge_credential_id',
    );
    expect(composerInput().disabled).toBe(false);
    expect(sendButton().disabled).toBe(false);

    sendButton().click();
    await settle();
    expect(instancePosts).toHaveLength(1);
  });
});
