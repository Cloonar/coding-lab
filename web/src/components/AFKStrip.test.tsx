// AFK strip contract (issue #41) — the compact port of the old repo-card
// AFKSection, so its behavior is verified here:
// - 'Run one (N ready)' carries the claimable-count hint and stays a real,
//   enabled button at every count (0 → only visually greyed) — the hint never
//   HTML-blocks the authoritative click; the server 409s a stale click;
// - an unknown count (ready endpoint failed) → a plain button, no hint;
// - a successful start reports the spawned run (the parent toasts it);
// - the paused banner ('Paused after 3 failures') appears at the >= 3 boundary
//   with its Reset POSTing afk/reset;
// - the auto toggle is a real button PUTting {enabled: !current}.
//
// AFKCard (the repo home's Overview, issue #61) shares that behavior but reads
// its count from the repo summary — it never requests the ready queue — and
// lays it out as a card: the sentence, Run one, Auto as a switch.

import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Repo, Run } from '../api';
import { EventsProvider } from '../events';
import AFKStrip, { AFKCard } from './AFKStrip';

const REPO_ID = 'repo_1';

/** Minimal EventSource stand-in so EventsProvider can mount under jsdom. */
class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

function repoFixture(overrides: Partial<Repo> = {}): Repo {
  return {
    id: REPO_ID,
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
    runner: 'host',
    container_memory: null,
    container_pids: null,
    container_nofile: null,
    image_ref: null,
    ...overrides,
  };
}

let claimableCount: number;
let claimableFail: boolean;
let startResponses: { status: number; body: unknown }[];
let requests: { method: string; url: string; body?: unknown }[];
let started: Run[];
let repoChanged: number;
let errors: string[];
let notices: string[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function jsonResponse(status: number, body?: unknown) {
  const text = body === undefined ? '' : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () =>
      text === ''
        ? Promise.reject(new SyntaxError('empty body'))
        : Promise.resolve(JSON.parse(text) as unknown),
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
      requests.push({
        method,
        url,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      if (url === `/api/v1/repos/${REPO_ID}/ready?claimable=1` && method === 'GET') {
        if (claimableFail) {
          return Promise.resolve(jsonResponse(502, { error: 'tracker unavailable' }));
        }
        const issues = Array.from({ length: claimableCount }, (_, i) => ({
          number: i + 1,
          title: `issue ${i + 1}`,
        }));
        return Promise.resolve(jsonResponse(200, { issues }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/afk/start` && method === 'POST') {
        const next = startResponses.shift() ?? {
          status: 202,
          body: { run: { id: 'run_1', issue_number: 7 } },
        };
        return Promise.resolve(jsonResponse(next.status, next.body));
      }
      if (url === `/api/v1/repos/${REPO_ID}/afk/auto` && method === 'PUT') {
        return Promise.resolve(jsonResponse(200, { id: REPO_ID, afk_auto_enabled: true }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/afk/reset` && method === 'POST') {
        return Promise.resolve(jsonResponse(200, { id: REPO_ID, consecutive_failures: 0 }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountAFK(repo: Repo): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(
    () => (
      <EventsProvider>
        <AFKStrip
          repo={repo}
          onRepoChanged={() => {
            repoChanged += 1;
          }}
          onStarted={(run) => started.push(run)}
          onError={(message) => errors.push(message)}
        />
      </EventsProvider>
    ),
    container,
  );
  await settle();
}

function button(text: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'));
  const el = buttons.find((b) => b.textContent?.includes(text));
  if (!el) throw new Error(`missing button containing ${JSON.stringify(text)}`);
  return el;
}

function requestsTo(url: string): { method: string; url: string; body?: unknown }[] {
  return requests.filter((r) => r.url === url);
}

beforeEach(() => {
  claimableCount = 2;
  claimableFail = false;
  startResponses = [];
  requests = [];
  started = [];
  repoChanged = 0;
  errors = [];
  notices = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('AFKStrip start button (claimable-count hint)', () => {
  it('shows the count and stays enabled while claimable issues exist', async () => {
    await mountAFK(repoFixture());

    const start = button('Run one (2 ready)');
    expect(start.disabled).toBe(false);
  });

  it('renders an enabled but greyed button at a known count of 0 (never blocks the click)', async () => {
    claimableCount = 0;
    // The stale count is 0, but the server is authoritative — it 409s the click.
    startResponses = [{ status: 409, body: { error: 'no ready-for-agent issues to start' } }];
    await mountAFK(repoFixture());

    const start = button('Run one (0 ready)');
    expect(start.disabled).toBe(false); // NOT HTML-disabled — only greyed
    expect(start.classList.contains('greyed')).toBe(true);

    // The authoritative click reaches the server and surfaces its 409.
    start.click();
    await settle();
    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/start`)).toHaveLength(1);
    expect(errors).toEqual(['no ready-for-agent issues to start']);
  });

  it('falls back to a plain enabled button when the count is unknown', async () => {
    claimableFail = true;
    await mountAFK(repoFixture());

    const start = button('Run one');
    expect(start.textContent).not.toContain('ready)');
    expect(start.disabled).toBe(false);
  });

  it('POSTs afk/start and reports the spawned run (parent toasts, no navigation)', async () => {
    await mountAFK(repoFixture());

    button('Run one (2 ready)').click();
    claimableCount = 1; // the claim consumed one issue; the refetch sees it
    await settle();

    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/start`)).toHaveLength(1);
    expect(started).toHaveLength(1);
    expect(started[0]?.issue_number).toBe(7);
    expect(errors).toEqual([]);
    expect(button('Run one (1 ready)').disabled).toBe(false);
  });

  it('surfaces the server 409 verbatim (the click is authoritative, not the hint)', async () => {
    startResponses = [{ status: 409, body: { error: 'no ready-for-agent issues to start' } }];
    await mountAFK(repoFixture());

    button('Run one (2 ready)').click();
    await settle();

    expect(errors).toEqual(['no ready-for-agent issues to start']);
    expect(started).toEqual([]);
  });
});

describe('AFKStrip paused banner (three strikes, boundary >= 3)', () => {
  it('stays hidden below the threshold', async () => {
    await mountAFK(repoFixture({ consecutive_failures: 2 }));

    expect(container.textContent).not.toContain('Paused after 3 failures');
  });

  it('appears at exactly 3 failures with a working Reset', async () => {
    await mountAFK(repoFixture({ consecutive_failures: 3 }));

    expect(container.textContent).toContain('Paused after 3 failures');

    button('Reset').click();
    await settle();

    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/reset`)).toHaveLength(1);
    expect(repoChanged).toBe(1);
  });

  it('keeps the start button rendered while paused (server 409s it)', async () => {
    await mountAFK(repoFixture({ consecutive_failures: 4 }));

    expect(button('Run one (2 ready)').disabled).toBe(false);
  });
});

describe('AFKStrip auto toggle', () => {
  it('PUTs the flipped flag and refreshes the repo row', async () => {
    await mountAFK(repoFixture({ afk_auto_enabled: false }));

    button('Auto: Off').click();
    await settle();

    const puts = requestsTo(`/api/v1/repos/${REPO_ID}/afk/auto`);
    expect(puts).toHaveLength(1);
    expect(puts[0]?.method).toBe('PUT');
    expect(puts[0]?.body).toEqual({ enabled: true });
    expect(repoChanged).toBe(1);
  });

  it('renders On and PUTs {enabled: false} when currently enabled', async () => {
    await mountAFK(repoFixture({ afk_auto_enabled: true }));

    button('Auto: On').click();
    await settle();

    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/auto`)[0]?.body).toEqual({ enabled: false });
  });
});

async function mountCard(repo: Repo): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(
    () => (
      <EventsProvider>
        <AFKCard
          repo={repo}
          onRepoChanged={() => {
            repoChanged += 1;
          }}
          onStarted={(run) => started.push(run)}
          onError={(message) => errors.push(message)}
          onAutoChanged={(enabled) => notices.push(`auto ${String(enabled)}`)}
          onReset={() => notices.push('reset')}
        />
      </EventsProvider>
    ),
    container,
  );
  await settle();
}

const withClaimable = (claimable: number | null, over: Partial<Repo> = {}): Repo =>
  repoFixture({
    summary: { claimable, open_issues: null, readiness: { state: 'passing', checks: [] } },
    ...over,
  });

const readyRequests = () => requests.filter((r) => r.url.includes('/ready'));

describe('AFKCard (Overview)', () => {
  it('reads the count from the repo summary and never requests the ready queue', async () => {
    await mountCard(withClaimable(3));

    expect(container.querySelector('section.afk-card h2')?.textContent).toBe('AFK');
    expect(container.querySelector('.afk-card-count')?.textContent).toBe(
      '3 issues ready for an agent.',
    );
    expect(button('Run one').disabled).toBe(false);
    expect(readyRequests()).toEqual([]);
  });

  it('greys Run one at a known 0 and shows no number while the count is unknown', async () => {
    await mountCard(withClaimable(0));
    const start = button('Run one');
    expect(start.classList.contains('greyed')).toBe(true);
    expect(start.disabled).toBe(false); // a hint only

    dispose?.();
    container.remove();
    await mountCard(withClaimable(null));
    expect(container.querySelector('.afk-card-count')?.textContent).not.toMatch(/\d/);
    expect(button('Run one').classList.contains('greyed')).toBe(false);
    expect(button('Run one').disabled).toBe(false);
    expect(readyRequests()).toEqual([]);
  });

  it('Run one POSTs afk/start, reports the run and refreshes the repo', async () => {
    await mountCard(withClaimable(2));

    button('Run one').click();
    await settle();

    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/start`)).toHaveLength(1);
    expect(started[0]?.issue_number).toBe(7);
    expect(repoChanged).toBe(1);
    expect(readyRequests()).toEqual([]);
  });

  it('surfaces a refused start verbatim', async () => {
    startResponses = [{ status: 409, body: { error: 'repo is paused' } }];
    await mountCard(withClaimable(2));

    button('Run one').click();
    await settle();

    expect(errors).toEqual(['repo is paused']);
    expect(started).toEqual([]);
  });

  it('Auto is a switch that applies at once', async () => {
    await mountCard(withClaimable(2, { afk_auto_enabled: false }));

    const auto = container.querySelector<HTMLButtonElement>('button[role="switch"]');
    expect(auto?.getAttribute('aria-checked')).toBe('false');
    // Named by its visible label.
    const label = container.querySelector(`label[for="${auto?.id ?? ''}"]`);
    expect(label?.textContent).toBe('Auto');

    auto?.click();
    // The requested state shows while the request is in flight.
    expect(auto?.getAttribute('aria-checked')).toBe('true');
    await settle();

    const puts = requestsTo(`/api/v1/repos/${REPO_ID}/afk/auto`);
    expect(puts).toHaveLength(1);
    expect(puts[0]?.body).toEqual({ enabled: true });
    expect(repoChanged).toBe(1);
    expect(notices).toEqual(['auto true']);
  });

  it('shows the three-strikes banner with a working Reset', async () => {
    await mountCard(withClaimable(2, { consecutive_failures: 3 }));

    expect(container.querySelector('.afk-card-paused')?.textContent).toContain(
      'Paused after 3 failed runs.',
    );
    button('Reset').click();
    await settle();

    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/reset`)).toHaveLength(1);
    expect(repoChanged).toBe(1);
    expect(notices).toEqual(['reset']);
  });

  it('has no paused banner below the threshold', async () => {
    await mountCard(withClaimable(2, { consecutive_failures: 2 }));
    expect(container.querySelector('.afk-card-paused')).toBeNull();
  });
});
