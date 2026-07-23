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

import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Repo, Run } from '../api';
import { EventsProvider } from '../events';
import AFKStrip from './AFKStrip';

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
