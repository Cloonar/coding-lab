// New-run composer contract (issue #41, Phase 2b):
// - with repos present, the composer renders (field, textarea, repo/model/
//   effort chips) and NOT the empty state;
// - Send spawns the selected repo with label/model/effort and the typed text as
//   first_message (issue #96 — one POST, no post-spawn queue hop), and navigates
//   to /runs/:id;
// - an empty box spawns a plain run with no first_message;
// - a spawn failure keeps the text in the box, shows the server message
//   verbatim, and never navigates;
// - zero repos hides the composer and shows the exact "No repositories yet"
//   empty state (the Playwright smoke + login/setup round-trip assert this on `/`);
// - a logged-out provider surfaces the slim reconnect banner, named by the
//   provider's display_name (issue #51 decision 9).

import { MemoryRouter, Route, createMemoryHistory, useParams } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Provider, ProviderAuthStatus, Repo } from '../api';
import App from '../App';
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
    afk_provider_default: null,
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

let reposOnServer: Repo[];
let providersOnServer: Provider[];
let settingsOnServer: Record<string, unknown>;
let authOnServer: ProviderAuthStatus;
let authRequests: string[];
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
      if (/^\/api\/v1\/repos\/[^/]+\/ready/.test(url) && method === 'GET') {
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

/** Opens a composer Select chip and clicks the option with the given label. */
async function chooseFromChip(chipLabel: string, optionLabel: string): Promise<void> {
  container.querySelector<HTMLButtonElement>(`button[aria-label="${chipLabel}"]`)!.click();
  await settle();
  const row = Array.from(container.querySelectorAll<HTMLButtonElement>('[role="option"]')).find(
    (r) => r.querySelector('.select-option-label')?.textContent === optionLabel,
  );
  if (!row) throw new Error(`missing option ${JSON.stringify(optionLabel)} in ${chipLabel}`);
  row.click();
  await settle();
}

function startButton(): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>('button[aria-label="Start run"]')!;
}

/** The text a composer Select chip currently shows on its trigger. */
function chipLabel(label: string): string | null {
  const chip = container.querySelector(`button[aria-label="${label}"] .composer-chip-label`);
  return chip === null ? null : chip.textContent;
}

/** Opens a composer Select chip, reads its option labels, and closes it. */
async function chipOptionLabels(label: string): Promise<string[]> {
  const trigger = container.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`)!;
  trigger.click();
  await settle();
  const labels = Array.from(container.querySelectorAll('[role="option"] .select-option-label')).map(
    (el) => el.textContent ?? '',
  );
  trigger.click(); // the trigger toggles: a second click closes the panel
  await settle();
  return labels;
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

beforeEach(() => {
  reposOnServer = [repoFixture()];
  providersOnServer = [...PROVIDERS];
  settingsOnServer = {};
  authOnServer = { logged_in: true, email: 'me@x', method: 'oauth', checked_at: '' };
  authRequests = [];
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
});

describe('NewRun composer', () => {
  it('renders the composer (not the empty state) when repos exist', async () => {
    await mountHome();

    expect(container.querySelector('.composer-field')).not.toBeNull();
    expect(container.querySelector('.composer-input')).not.toBeNull();
    expect(container.querySelector('button[aria-label="Model"]')).not.toBeNull();
    expect(container.querySelector('button[aria-label="Effort"]')).not.toBeNull();
    // With a single registered provider there is no choice to offer: the
    // agent chip must not render at all (ADR-0030 — pixel-identical composer).
    expect(container.querySelector('button[aria-label="Agent"]')).toBeNull();
    // The repo chip names the selected (first ready) repo.
    expect(container.querySelector('button[aria-label="Repository"]')?.textContent).toContain(
      'coding-lab',
    );
    // Not the zero-repos slate.
    expect(container.textContent).not.toContain('No repositories yet');
  });

  it('spawns with label/model/effort and the typed text as first_message, and navigates', async () => {
    await mountHome();

    const input = container.querySelector('.composer-input') as HTMLTextAreaElement;
    input.value = 'do the thing';
    input.dispatchEvent(new Event('input', { bubbles: true }));

    await chooseFromChip('Model', 'Opus');
    await chooseFromChip('Effort', 'High');

    // Open the `…` popover and set the optional label.
    container.querySelector<HTMLButtonElement>('button[aria-label="More options"]')!.click();
    await settle();
    const labelInput = container.querySelector('input[name="label"]') as HTMLInputElement;
    labelInput.value = 'debug';
    labelInput.dispatchEvent(new Event('input', { bubbles: true }));

    startButton().click();
    await settle();

    // The typed text rides the spawn as first_message (issue #96) — exactly one
    // POST, no post-spawn queue hop.
    expect(instancePosts).toHaveLength(1);
    expect(instancePosts[0]).toEqual({
      label: 'debug',
      model: 'opus',
      effort: 'high',
      first_message: 'do the thing',
    });
    // …and the composer navigated to the chat.
    expect(container.textContent).toContain('run:run_new');
  });

  it('spawns a plain run with no first_message when the box is empty', async () => {
    await mountHome();

    startButton().click();
    await settle();

    expect(instancePosts).toHaveLength(1);
    // No label (untouched); model/effort default to the first catalog option;
    // an empty box sends NO first_message key (issue #96).
    expect(instancePosts[0]).toEqual({ model: 'sonnet', effort: 'low' });
    expect(instancePosts[0]).not.toHaveProperty('first_message');
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
    // The banner shows the raw 409, the text stays, and nothing navigated.
    expect(container.querySelector('.banner.error')?.textContent).toContain('cap reached (2/2)');
    expect((container.querySelector('.composer-input') as HTMLTextAreaElement).value).toBe(
      'try me',
    );
    expect(container.textContent).not.toContain('run:run_new');
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

// Composer keyboard send (ADR-0031, issue #70): shares isComposerSend with
// the chat composer. Bare Enter spawns only on a fine-pointer setup;
// Shift/Alt+Enter never spawn; Cmd/Ctrl+Enter spawns everywhere, including on
// an empty box — unlike the chat composer, an empty box is a valid "plain
// spawn" here (today's Start-button behavior, pinned), so bare Enter needs
// its own empty guard while Cmd/Ctrl+Enter keeps sending through.
describe('NewRun composer keyboard send (issue #70)', () => {
  it('fine-pointer: Shift+Enter never spawns; bare Enter spawns and sends the typed text as first_message', async () => {
    finePointer(true);
    await mountHome();
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
    expect(instancePosts[0]).toMatchObject({ first_message: 'do the thing' });
    expect(container.textContent).toContain('run:run_new');
  });

  it('fine-pointer: Cmd/Ctrl+Enter spawns', async () => {
    finePointer(true);
    await mountHome();
    typeText('ctrl spawn');
    await settle();

    composerInput().dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(instancePosts).toHaveLength(1);
    expect(instancePosts[0]).toMatchObject({ first_message: 'ctrl spawn' });
  });

  it('bare Enter never spawns without a fine pointer (no matchMedia, or a touch profile)', async () => {
    // Default jsdom: no window.matchMedia at all — reads as "not fine-pointer".
    await mountHome();
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
    expect(instancePosts[0]).toMatchObject({ first_message: 'no matchMedia here' });
  });

  it('touch profile: bare Enter does not spawn; Cmd/Ctrl+Enter still does', async () => {
    finePointer(false);
    await mountHome();
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
    expect(instancePosts[0]).toMatchObject({ first_message: 'tap city' });
  });

  it('fine-pointer: Enter fired mid-IME-composition does not spawn', async () => {
    finePointer(true);
    await mountHome();
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
    await mountHome();

    const evt = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    composerInput().dispatchEvent(evt);
    await settle();
    expect(instancePosts).toHaveLength(0);
    expect(evt.defaultPrevented).toBe(true); // no stray newline in an already-empty box

    composerInput().dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    // Empty text is a valid "plain spawn" (today's Start-button behavior) —
    // Cmd/Ctrl+Enter keeps spawning it even under the new bare-Enter gate.
    expect(instancePosts).toHaveLength(1);
    expect(instancePosts[0]).not.toHaveProperty('first_message');
    expect(container.textContent).toContain('run:run_new');
  });

  it('carries the "Start run (Enter)" tooltip on the Start button', async () => {
    await mountHome();
    expect(startButton().title).toBe('Start run (Enter)');
  });
});

// The agent chip (issue #66 / ADR-0030): rendered only with ≥2 registered
// providers; the effective provider = ephemeral pick → repo override → global
// provider_default → first provider, and everything downstream (model/effort
// catalogs, the auth banner) follows it.
describe('NewRun composer agent chip (multi-provider)', () => {
  beforeEach(() => {
    providersOnServer = [...PROVIDERS, CODEX];
  });

  it('shows the chip resolving the global provider_default when the repo inherits', async () => {
    reposOnServer = [repoFixture({ provider: null })];
    settingsOnServer = { provider_default: 'codex' };
    await mountHome();

    expect(chipLabel('Agent')).toBe('Codex');
    // The model catalog follows the effective provider…
    expect(chipLabel('Model')).toBe('GPT-5 Codex');
    // …and a provider without an effort knob renders NO effort chip at all.
    expect(container.querySelector('button[aria-label="Effort"]')).toBeNull();
  });

  it('lets the repo provider override the global default', async () => {
    reposOnServer = [repoFixture({ provider: 'claude-code' })];
    settingsOnServer = { provider_default: 'codex' };
    await mountHome();

    expect(chipLabel('Agent')).toBe('Claude Code');
    expect(chipLabel('Model')).toBe('Sonnet');
    expect(container.querySelector('button[aria-label="Effort"]')).not.toBeNull();
  });

  it('picking an agent re-catalogs model/effort, resets prior picks, and rides the POST', async () => {
    reposOnServer = [repoFixture({ provider: 'claude-code' })];
    await mountHome();

    // Foreign picks made under the previous provider…
    await chooseFromChip('Model', 'Opus');
    await chooseFromChip('Effort', 'High');

    await chooseFromChip('Agent', 'Codex');

    // …are reset: the model chip re-catalogs to the new provider's default
    // and the effort chip disappears (empty efforts catalog).
    expect(chipLabel('Model')).toBe('GPT-5 Codex');
    expect(container.querySelector('button[aria-label="Effort"]')).toBeNull();

    startButton().click();
    await settle();

    // The explicit pick rides along; the stale Opus/High picks must NOT.
    expect(instancePosts).toEqual([{ provider: 'codex', model: 'gpt-5-codex' }]);
  });

  it('omits provider from the POST when the operator never touched the chip', async () => {
    reposOnServer = [repoFixture({ provider: 'codex' })];
    await mountHome();

    startButton().click();
    await settle();

    // The repo/global layers are the SERVER's to resolve — only an explicit
    // per-spawn pick is sent.
    expect(instancePosts).toEqual([{ model: 'gpt-5-codex' }]);
  });

  it('resets the ephemeral pick on repo switch', async () => {
    reposOnServer = [
      repoFixture({ provider: 'claude-code' }),
      repoFixture({ id: 'repo_2', name: 'other-repo', provider: 'claude-code' }),
    ];
    await mountHome();

    await chooseFromChip('Agent', 'Codex');
    expect(chipLabel('Agent')).toBe('Codex');
    expect(container.querySelector('button[aria-label="Effort"]')).toBeNull();

    await chooseFromChip('Repository', 'other-repo');

    // The pick is ephemeral: the new repo resolves its own effective provider
    // and the model/effort surfaces follow it again.
    expect(chipLabel('Agent')).toBe('Claude Code');
    expect(chipLabel('Model')).toBe('Sonnet');
    expect(container.querySelector('button[aria-label="Effort"]')).not.toBeNull();
  });

  it('keys the auth banner on the effective provider and names its display_name', async () => {
    reposOnServer = [repoFixture({ provider: 'codex' })];
    authOnServer = { logged_in: false, email: '', method: '', checked_at: '' };
    await mountHome();

    // The status route was asked about the EFFECTIVE provider…
    expect(authRequests).toContain('codex');
    // …and the banner copy flows from its display_name.
    expect(container.querySelector('.newrun-warn')?.textContent).toContain('Codex is logged out');
  });
});

// Per-model efforts (issue #156): the composer's effort chip catalogs the
// SELECTED MODEL, not the provider union — effort support varies per model
// and codex does not clamp, so an unsupported model+effort combo would 400
// at spawn. A stale pick snaps to the new model's reported default; a
// still-valid pick is kept.
describe('NewRun composer per-model efforts (issue #156)', () => {
  beforeEach(() => {
    providersOnServer = [GPT];
    reposOnServer = [repoFixture({ provider: 'gpt' })];
  });

  it('catalogs the effort chip from the selected model and re-catalogs on a model switch', async () => {
    await mountHome();

    // Terra (the resolved default model) offers the full ladder…
    expect(await chipOptionLabels('Effort')).toEqual([
      'Low',
      'Medium',
      'High',
      'X-High',
      'Max',
      'Ultra',
    ]);

    await chooseFromChip('Model', 'GPT-5.6-Luna');

    // …luna only its own list: Ultra (and the rest of the union) is gone.
    expect(await chipOptionLabels('Effort')).toEqual(['Low', 'Medium', 'High']);
  });

  it("snaps a stale effort pick to the new model's default_effort on the POST", async () => {
    await mountHome();

    await chooseFromChip('Effort', 'Ultra');
    await chooseFromChip('Model', 'GPT-5.6-Luna');

    // Luna has no "ultra": the chip already shows the snapped default…
    expect(chipLabel('Effort')).toBe('Medium');

    startButton().click();
    await settle();

    // …and the POST carries luna's reported default, never the stale pick.
    expect(instancePosts).toEqual([{ model: 'gpt-5.6-luna', effort: 'medium' }]);
  });

  it('keeps a still-valid effort pick across a model switch', async () => {
    await mountHome();

    await chooseFromChip('Effort', 'High');
    await chooseFromChip('Model', 'GPT-5.6-Luna');

    // Luna supports "high" too: the explicit pick survives the switch…
    expect(chipLabel('Effort')).toBe('High');

    startButton().click();
    await settle();

    // …and rides the POST.
    expect(instancePosts).toEqual([{ model: 'gpt-5.6-luna', effort: 'high' }]);
  });

  it("an untouched composer sends the model's reported default_effort, not the first entry", async () => {
    await mountHome();

    startButton().click();
    await settle();

    // Terra's efforts START at "low" but report "medium" as the default —
    // the reported default beats the first-entry rule.
    expect(instancePosts).toEqual([{ model: 'gpt-5.6-terra', effort: 'medium' }]);
  });

  it('a global default effort valid for the model beats the model default', async () => {
    settingsOnServer = { spawn_effort_default: 'xhigh' };
    await mountHome();

    startButton().click();
    await settle();

    expect(instancePosts).toEqual([{ model: 'gpt-5.6-terra', effort: 'xhigh' }]);
  });

  it('skips a global default effort the model does not support; the model default rides', async () => {
    settingsOnServer = { spawn_effort_default: 'turbo' };
    await mountHome();

    startButton().click();
    await settle();

    expect(instancePosts).toEqual([{ model: 'gpt-5.6-terra', effort: 'medium' }]);
  });
});
