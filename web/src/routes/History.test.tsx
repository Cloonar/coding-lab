// History's escalated-run re-arm affordance (issue #188):
// - the Re-arm button and the "Autoland is ignoring PR #N" note appear only
//   on a run whose outcome is 'escalated' AND whose pull_number is set — not
//   on any other outcome, and not on an escalated run with pull_number: null
//   (a run row that predates this feature, or an escalation with no PR);
// - clicking it POSTs /repos/{id}/autoland/pulls/{n}/rearm;
// - the button goes busy ('Re-arming…', disabled) while the request is in
//   flight, same shape as AFKStrip's Reset;
// - a failing request surfaces its message rather than failing silently.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App from '../App';
import History from './History';

const REPO_ID = 'repo_1';

/** Minimal EventSource stand-in so EventsProvider can mount under jsdom. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;

  constructor() {
    FakeEventSource.instances.push(this);
  }

  addEventListener(): void {}
  close(): void {}
}

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

/** One GET /runs row, shaped like api/runs.ts's Run — see that file's fields. */
function runFixture(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'run_1',
    repo_id: REPO_ID,
    kind: 'escalate',
    provider: 'claude-code',
    issue_number: null,
    pull_number: null,
    branch: 'afk/7',
    worktree_path: '/wt/x',
    session_name: 'proj~dom-20260706-1500',
    title: null,
    model: 'opus[1m]',
    effort: 'max',
    remote: false,
    deep_link_url: null,
    started_at: '2026-07-06T15:00:00.000Z',
    budget_deadline: null,
    ended_at: '2026-07-06T15:10:00.000Z',
    outcome: 'escalated',
    failure_reason: null,
    ...overrides,
  };
}

let runsOnServer: Record<string, unknown>[];
let runFetchCount: number;
let rearmRequests: { url: string; body: unknown }[];
let rearmResponse: { status: number; body: unknown };
let rearmHold: boolean;
let releaseRearm: (() => void) | null;
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
      if (url === '/api/v1/repos' && method === 'GET') {
        return Promise.resolve(
          jsonResponse(200, { repos: [{ id: REPO_ID, name: 'coding-lab' }] }),
        );
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      if (url === '/api/v1/runs?limit=50' && method === 'GET') {
        runFetchCount += 1;
        return Promise.resolve(jsonResponse(200, { runs: runsOnServer }));
      }
      if (
        url.startsWith(`/api/v1/repos/${REPO_ID}/autoland/pulls/`) &&
        url.endsWith('/rearm') &&
        method === 'POST'
      ) {
        rearmRequests.push({
          url,
          body: init?.body === undefined ? undefined : (JSON.parse(String(init.body)) as unknown),
        });
        if (rearmHold) {
          return new Promise((resolve) => {
            releaseRearm = () => resolve(jsonResponse(rearmResponse.status, rearmResponse.body));
          });
        }
        return Promise.resolve(jsonResponse(rearmResponse.status, rearmResponse.body));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountHistory(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/history' });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/history" component={History} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

/** The one run card whose "Open chat" link points at this run id. */
function cardFor(runID: string): HTMLElement {
  const cards = Array.from(container.querySelectorAll<HTMLElement>('.run-card'));
  const el = cards.find((c) => c.querySelector(`a[href="/runs/${runID}"]`) !== null);
  if (!el) throw new Error(`missing run card for ${runID}`);
  return el;
}

function rearmButton(card: HTMLElement): HTMLButtonElement | null {
  return card.querySelector('button.run-rearm');
}

beforeEach(() => {
  runFetchCount = 0;
  rearmRequests = [];
  rearmResponse = {
    status: 200,
    body: { repo_id: REPO_ID, pull_number: 42, rearmed_at: '2026-08-03T00:00:00.000Z' },
  };
  rearmHold = false;
  releaseRearm = null;
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('History escalated-run re-arm', () => {
  it('shows the Re-arm button and the PR suppression note on an escalated run', async () => {
    runsOnServer = [runFixture({ id: 'run_1', pull_number: 42 })];
    await mountHistory();

    const card = cardFor('run_1');
    expect(card.textContent).toContain('Autoland is ignoring PR #42 until it is re-armed.');
    expect(rearmButton(card)).not.toBeNull();
  });

  it('hides the Re-arm button on a non-escalated run', async () => {
    runsOnServer = [
      runFixture({ id: 'run_1', kind: 'manual', outcome: 'success', pull_number: null }),
    ];
    await mountHistory();

    const card = cardFor('run_1');
    expect(rearmButton(card)).toBeNull();
    expect(card.textContent).not.toContain('re-armed');
  });

  it('hides the Re-arm button on an escalated run with no pull_number', async () => {
    runsOnServer = [runFixture({ id: 'run_1', pull_number: null })];
    await mountHistory();

    const card = cardFor('run_1');
    expect(rearmButton(card)).toBeNull();
    expect(card.textContent).not.toContain('re-armed');
  });

  it('POSTs .../autoland/pulls/{n}/rearm and refetches the runs list on success', async () => {
    runsOnServer = [runFixture({ id: 'run_1', pull_number: 42 })];
    await mountHistory();
    expect(runFetchCount).toBe(1);

    rearmButton(cardFor('run_1'))?.click();
    await settle();

    expect(rearmRequests).toEqual([
      { url: `/api/v1/repos/${REPO_ID}/autoland/pulls/42/rearm`, body: undefined },
    ]);
    expect(runFetchCount).toBe(2); // onRearmed() refetches the page
  });

  it('goes busy (disabled, "Re-arming…") while the request is in flight', async () => {
    runsOnServer = [runFixture({ id: 'run_1', pull_number: 42 })];
    rearmHold = true;
    await mountHistory();

    const button = rearmButton(cardFor('run_1'));
    button?.click();
    await settle();

    expect(button?.disabled).toBe(true);
    expect(button?.textContent).toBe('Re-arming…');
    expect(runFetchCount).toBe(1); // no refetch yet — the request hasn't resolved

    releaseRearm?.();
    await settle();

    expect(rearmButton(cardFor('run_1'))?.disabled).toBe(false);
    expect(runFetchCount).toBe(2);
  });

  it('surfaces a failing request instead of failing silently', async () => {
    runsOnServer = [runFixture({ id: 'run_1', pull_number: 42 })];
    rearmResponse = { status: 404, body: { error: 'not found' } };
    await mountHistory();

    rearmButton(cardFor('run_1'))?.click();
    await settle();

    const card = cardFor('run_1');
    expect(card.textContent).toContain('not found');
    expect(rearmButton(card)?.disabled).toBe(false); // not stuck busy after the error
    expect(runFetchCount).toBe(1); // a failed rearm must not have refetched
  });
});
