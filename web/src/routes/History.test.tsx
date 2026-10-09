// History — the Runs page's Ended side (issue #76): ended runs only (outcome
// 'active' rows are the Live side's, and 'active' is no filter option), grouped
// by the local day they ended (Today / Yesterday / short date, newest first),
// each row one <A> to /runs/:id carrying the outcome chip word, the repo name
// mapped from repo_id, and `PR #n` or the branch; the outcome filter narrows.
//
// The escalated-run re-arm affordance (issue #188):
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
import { AuthProvider } from '../auth';
import { EventsProvider } from '../events';
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
        return Promise.resolve(jsonResponse(200, { repos: [{ id: REPO_ID, name: 'coding-lab' }] }));
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

async function mountHistory(path = '/history'): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: path });
  dispose = render(
    () => (
      <AuthProvider>
        <EventsProvider>
          <MemoryRouter history={history}>
            <Route path="/history" component={History} />
            <Route path="*" component={() => null} />
          </MemoryRouter>
        </EventsProvider>
      </AuthProvider>
    ),
    container,
  );
  await settle();
}

/** The one ended-run row (link + anything under it) for this run id. */
function cardFor(runID: string): HTMLElement {
  const cards = Array.from(container.querySelectorAll<HTMLElement>('.ended-run'));
  const el = cards.find((c) => c.querySelector(`a[href="/runs/${runID}"]`) !== null);
  if (!el) throw new Error(`missing run row for ${runID}`);
  return el;
}

/** ISO string for a LOCAL wall-clock time `daysAgo` days before today. */
function localDay(daysAgo: number, h: number, m = 0, s = 0): string {
  const now = new Date();
  return new Date(
    now.getFullYear(),
    now.getMonth(),
    now.getDate() - daysAgo,
    h,
    m,
    s,
  ).toISOString();
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

describe('History as the Ended view (issue #76)', () => {
  it('excludes live (active) runs and offers no active filter', async () => {
    runsOnServer = [
      runFixture({ id: 'run_live', outcome: 'active', ended_at: null }),
      runFixture({ id: 'run_done', kind: 'manual', outcome: 'success' }),
    ];
    await mountHistory();

    expect(container.querySelector('a[href="/runs/run_live"]')).toBeNull();
    expect(container.querySelector('a[href="/runs/run_done"]')).not.toBeNull();
    const options = Array.from(
      container.querySelectorAll<HTMLOptionElement>('select[name="outcome-filter"] option'),
    ).map((o) => o.value);
    expect(options).toEqual(['', 'success', 'death', 'timeout', 'stopped', 'escalated']);
  });

  it('reads "No ended runs yet." when only live runs exist', async () => {
    runsOnServer = [runFixture({ id: 'run_live', outcome: 'active', ended_at: null })];
    await mountHistory();
    expect(container.textContent).toContain('No ended runs yet.');
  });

  it('groups rows by the day they ended, newest first', async () => {
    runsOnServer = [
      runFixture({ id: 'run_old', outcome: 'stopped', ended_at: localDay(9, 12) }),
      runFixture({ id: 'run_y', outcome: 'death', ended_at: localDay(1, 12) }),
      runFixture({ id: 'run_t', outcome: 'success', ended_at: localDay(0, 0, 0, 1) }),
    ];
    await mountHistory();

    const groups = Array.from(container.querySelectorAll('.runlist-group'));
    expect(groups).toHaveLength(3);
    expect(groups[0]?.querySelector('.runlist-label')?.textContent).toBe('Today');
    expect(groups[1]?.querySelector('.runlist-label')?.textContent).toBe('Yesterday');
    const hrefs = Array.from(container.querySelectorAll('a.runlist-row')).map((a) =>
      a.getAttribute('href'),
    );
    expect(hrefs).toEqual(['/runs/run_t', '/runs/run_y', '/runs/run_old']);
  });

  it('shows the outcome word, the repo name and PR #n or the branch', async () => {
    runsOnServer = [
      runFixture({ id: 'run_pr', outcome: 'escalated', pull_number: 42 }),
      runFixture({ id: 'run_br', kind: 'manual', outcome: 'death', branch: 'lab/x' }),
    ];
    await mountHistory();

    const pr = cardFor('run_pr');
    expect(pr.querySelector('.outcome-chip')?.textContent).toBe('escalated');
    expect(pr.querySelector('.outcome-chip')?.classList.contains('outcome-escalated')).toBe(true);
    expect(pr.querySelector('.runlist-sub')?.textContent).toBe('coding-lab · PR #42 · 10m');
    // The Re-arm button sits under the row, never inside its link.
    expect(pr.querySelector('a.runlist-row button')).toBeNull();
    expect(rearmButton(pr)).not.toBeNull();

    const br = cardFor('run_br');
    expect(br.querySelector('.outcome-chip')?.textContent).toBe('died');
    expect(br.querySelector('.runlist-sub')?.textContent).toBe('coding-lab · lab/x · 10m');
  });

  it('narrows to the ?outcome= filter', async () => {
    runsOnServer = [
      runFixture({ id: 'run_done', kind: 'manual', outcome: 'success' }),
      runFixture({ id: 'run_died', kind: 'manual', outcome: 'death' }),
    ];
    await mountHistory('/history?outcome=death');

    expect(container.querySelector('a[href="/runs/run_done"]')).toBeNull();
    expect(container.querySelector('a[href="/runs/run_died"]')).not.toBeNull();
  });

  it('selects the Ended side of the Live / Ended switch', async () => {
    runsOnServer = [];
    await mountHistory();
    const ended = container.querySelector('.runs-switch a[href="/history"]');
    const live = container.querySelector('.runs-switch a[href="/"]');
    expect(ended?.getAttribute('aria-current')).toBe('page');
    expect(live?.getAttribute('aria-current')).toBeNull();
  });
});
