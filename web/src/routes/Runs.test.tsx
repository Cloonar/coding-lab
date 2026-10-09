// The Runs page at `/` (issue #76): it reads live runs from the shell's
// instances context (never its own fetch); below 1024px it is RunList's
// grouped rows under the brand + live dot head; at >=1024px it is the Runs
// table (run, repository, state, model, branch, base, last) with one tbody
// per group; Loading while the shell hasn't loaded; an error banner on a
// failed load; an empty state (title, hint, a New run button to /new, a link
// to the ended runs) when nothing is live; the table's Model column reads the
// provider catalog's labels and Last the spaced age;
// the Live / Ended switch marks Live current.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Instance } from '../api';
import { AuthProvider } from '../auth';
import { EventsProvider } from '../events';
import { ShellInstancesContext, type ShellInstances } from '../lib/shellInstances';
import Runs from './Runs';

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

function instance(overrides: Partial<Instance>): Instance {
  return {
    id: 'run_1',
    repo_id: 'repo_1',
    repo_name: 'proj',
    kind: 'manual',
    provider: 'claude-code',
    issue_number: null,
    pull_number: null,
    branch: 'lab/x',
    worktree_path: '/wt/x',
    session_name: 'proj~dom-20260706-1500',
    title: null,
    model: 'opus[1m]',
    effort: 'max',
    remote: true,
    deep_link_url: null,
    started_at: new Date(Date.now() - 2.5 * 60_000).toISOString(),
    budget_deadline: null,
    ended_at: null,
    outcome: 'active',
    failure_reason: null,
    live: true,
    connecting: false,
    state: '',
    ...overrides,
  };
}

/** matchMedia fake: every query matches iff `desktop` (the only one used is 1024px). */
function stubMatchMedia(desktop: boolean): void {
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches: desktop,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      onchange: null,
      dispatchEvent: () => false,
    })),
  );
}

/** The provider catalog the desktop Model column labels from. */
const PROVIDERS = [
  {
    id: 'claude-code',
    display_name: 'Claude Code',
    models: [{ value: 'opus[1m]', label: 'Opus (1M)', efforts: [] }],
    efforts: [{ value: 'max', label: 'Max' }],
    options: [],
  },
];

let dispose: (() => void) | undefined;
let container: HTMLDivElement;
let history: ReturnType<typeof createMemoryHistory>;
let fetchMock: ReturnType<typeof vi.fn>;

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 4; i += 1) await flush();
}

async function mount(shell: Partial<ShellInstances> & { all: () => Instance[] }): Promise<void> {
  const value: ShellInstances = { loaded: () => true, error: () => undefined, ...shell };
  container = document.createElement('div');
  document.body.appendChild(container);
  history = createMemoryHistory();
  history.set({ value: '/' });
  dispose = render(
    () => (
      <AuthProvider>
        <EventsProvider>
          <ShellInstancesContext.Provider value={value}>
            <MemoryRouter history={history}>
              <Route path="/" component={Runs} />
              <Route path="/runs/:id" component={() => <p class="chat-stub">chat</p>} />
              <Route path="*" component={() => null} />
            </MemoryRouter>
          </ShellInstancesContext.Provider>
        </EventsProvider>
      </AuthProvider>
    ),
    container,
  );
  await settle();
}

const sample = () => [
  instance({ id: 'run_idle', session_name: 'proj~i-20260706-1500', state: 'idle' }),
  instance({ id: 'run_work', session_name: 'proj~w-20260706-1501', state: 'working' }),
  instance({
    id: 'run_q',
    session_name: 'proj~q-20260706-1502',
    state: 'question',
    commits_behind: 3,
  }),
  instance({ id: 'run_dead', session_name: 'proj~d-20260706-1503', live: false }),
];

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
  fetchMock = vi.fn((input: unknown) => {
    const url = String(input);
    if (url === '/api/v1/auth/state') {
      return Promise.resolve(
        jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
      );
    }
    if (url === '/api/v1/providers') {
      return Promise.resolve(jsonResponse(200, { providers: PROVIDERS }));
    }
    return Promise.reject(new Error(`unexpected fetch: ${url}`));
  });
  vi.stubGlobal('fetch', fetchMock);
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('Runs page — phone', () => {
  it('renders grouped rows from the shell context, never fetching instances itself', async () => {
    stubMatchMedia(false);
    await mount({ all: sample });

    const labels = Array.from(container.querySelectorAll('.runlist-label')).map(
      (l) => l.textContent,
    );
    expect(labels).toEqual(['Needs you1', 'Working1', 'Idle1']);
    expect(
      Array.from(container.querySelectorAll('a.runlist-row')).map((a) => a.getAttribute('href')),
    ).toEqual(['/runs/run_q', '/runs/run_work', '/runs/run_idle']);
    expect(container.querySelector('.runlist-page')).not.toBeNull();
    expect(container.querySelector('table')).toBeNull();
    const urls = fetchMock.mock.calls.map((call) => String(call[0]));
    expect(urls.some((url) => url.includes('/instances'))).toBe(false);
  });

  it('shows the brand and the live dot in the head, with Live current', async () => {
    stubMatchMedia(false);
    await mount({ all: sample });

    const head = container.querySelector('.runs-head')!;
    expect(head.querySelector('.brand')?.textContent).toBe('lab.');
    expect(head.querySelector('.live-dot')).not.toBeNull();
    const live = container.querySelector('.runs-switch a[href="/"]');
    expect(live?.getAttribute('aria-current')).toBe('page');
    expect(live?.textContent).toBe('Live · 3');
    expect(
      container.querySelector('.runs-switch a[href="/history"]')?.getAttribute('aria-current'),
    ).toBeNull();
  });

  it('shows the empty state — title, hint, New run, Ended runs — when nothing is live', async () => {
    stubMatchMedia(false);
    await mount({ all: () => [instance({ id: 'run_dead', live: false })] });

    const empty = container.querySelector('.empty')!;
    expect(empty.querySelector('.runs-empty-title')?.textContent).toBe('No live runs');
    expect(empty.querySelector('.runs-empty-hint')?.textContent).toContain('New tab');
    const button = empty.querySelector('a.runs-empty-new');
    expect(button?.getAttribute('href')).toBe('/new');
    expect(button?.textContent).toBe('New run');
    expect(empty.querySelector('.runs-empty-ended a')?.getAttribute('href')).toBe('/history');
  });

  it('shows Loading until the shell has loaded', async () => {
    stubMatchMedia(false);
    await mount({ all: () => [], loaded: () => false });
    expect(container.textContent).toContain('Loading…');
    expect(container.querySelector('.empty')).toBeNull();
  });

  it('shows the error banner (and no empty claim) on a failed load', async () => {
    stubMatchMedia(false);
    await mount({ all: () => [], error: () => new TypeError('Failed to fetch') });
    expect(container.querySelector('.banner.error')?.textContent).toContain('Network error');
    expect(container.querySelector('.empty')).toBeNull();
  });
});

describe('Runs page — desktop table', () => {
  it('renders the columns, a Runs heading, and one tbody per group', async () => {
    stubMatchMedia(true);
    await mount({ all: sample });

    expect(container.querySelector('.runs-head h1')?.textContent).toBe('Runs');
    expect(container.querySelector('.runs-head .brand')).toBeNull();
    const heads = Array.from(container.querySelectorAll('thead th')).map((th) => th.textContent);
    expect(heads).toEqual(['Run', 'Repository', 'State', 'Model', 'Branch', 'Base', 'Last']);

    const groups = Array.from(container.querySelectorAll('tbody .runs-table-group th')).map(
      (th) => th.textContent,
    );
    expect(groups).toEqual(['Needs you1', 'Working1', 'Idle1']);

    const row = container.querySelector<HTMLTableRowElement>('tbody tr.runs-table-row')!;
    const cells = Array.from(row.querySelectorAll('td')).map((td) => td.textContent);
    expect(cells).toEqual([
      'q · 15:02',
      'proj',
      'Asking a question',
      'Opus (1M) · Max',
      'lab/x',
      '3 behind',
      '2 min',
    ]);
    expect(row.querySelector('a')?.getAttribute('href')).toBe('/runs/run_q');
    expect(row.querySelector('button')).toBeNull();
  });

  it('falls back to the raw model id for a provider the catalog lacks', async () => {
    stubMatchMedia(true);
    await mount({
      all: () => [instance({ id: 'run_x', provider: 'other', model: 'x-1', effort: '' })],
    });
    const cells = container.querySelectorAll('tbody tr.runs-table-row td');
    expect(cells[3]?.textContent).toBe('x-1');
  });

  it('opens the chat on a click anywhere on the row', async () => {
    stubMatchMedia(true);
    await mount({ all: sample });

    const row = container.querySelectorAll<HTMLTableRowElement>('tbody tr.runs-table-row')[1]!;
    row.querySelectorAll('td')[1]!.click();
    await settle();
    expect(container.querySelector('.chat-stub')).not.toBeNull();
  });
});
