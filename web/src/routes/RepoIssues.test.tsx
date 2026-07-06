// RepoIssues behavioral contract:
// - the binding gates mutations: a builtin repo shows the New issue / Labels
//   entry points, a forge-bound repo hides them behind the managed-on-the-
//   forge note while the read view keeps working;
// - the label chips narrow the ALREADY-FETCHED page client-side (no refetch),
//   while the state filter refetches server-side with ?state=…;
// - a scoped issue.changed refetches, foreign repoIDs do not.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { IssueSummary, Label, TrackerBinding } from '../api';
import App from '../App';
import RepoIssues from './RepoIssues';

const REPO_ID = 'repo_1';

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

const OPEN_ISSUES: IssueSummary[] = [
  {
    number: 1,
    title: 'Fix login',
    body: '',
    state: 'open',
    labels: ['bug', 'ready-for-agent'],
    comments_count: 2,
    created_at: '2026-07-06T00:00:00.000Z',
    updated_at: '2026-07-06T01:00:00.000Z',
  },
  {
    number: 2,
    title: 'Polish UI',
    body: '',
    state: 'open',
    labels: ['ui'],
    comments_count: 0,
    created_at: '2026-07-06T00:00:00.000Z',
    updated_at: '2026-07-06T00:30:00.000Z',
  },
];

const LABELS: Label[] = [
  { id: 'lbl_1', name: 'bug', color: '#d73a4a', description: '' },
  { id: 'lbl_2', name: 'ui', color: '#1d76db', description: '' },
  { id: 'lbl_3', name: 'ready-for-agent', color: '#0e8a16', description: '' },
];

let binding: TrackerBinding;
let issuesFail: boolean;
let readyFail: boolean;
let issuesFetches: string[]; // full issue-list URLs, in order
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
      if (url === `/api/v1/repos/${REPO_ID}` && method === 'GET') {
        return Promise.resolve(
          jsonResponse(200, { id: REPO_ID, name: 'coding-lab', tracker_binding: binding }),
        );
      }
      if (url.startsWith(`/api/v1/repos/${REPO_ID}/issues?`) && method === 'GET') {
        issuesFetches.push(url);
        if (issuesFail) {
          return Promise.resolve(jsonResponse(502, { error: 'tracker unavailable' }));
        }
        const state = new URLSearchParams(url.split('?')[1]).get('state');
        const issues = state === 'open' ? OPEN_ISSUES : [];
        return Promise.resolve(jsonResponse(200, { binding, issues }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/ready` && method === 'GET') {
        if (readyFail) {
          return Promise.resolve(jsonResponse(502, { error: 'ready queue unavailable' }));
        }
        return Promise.resolve(jsonResponse(200, { issues: [OPEN_ISSUES[0]] }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/labels` && method === 'GET') {
        if (binding !== 'builtin') {
          return Promise.reject(new Error('labels endpoint hit for a forge repo'));
        }
        return Promise.resolve(jsonResponse(200, { labels: LABELS }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountIssues(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/repos/${REPO_ID}/issues` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/repos/:id/issues" component={RepoIssues} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function chipButton(name: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll<HTMLButtonElement>('button.chip-toggle'));
  const el = buttons.find((b) => b.textContent?.trim() === name);
  if (!el) throw new Error(`missing label chip button ${name}`);
  return el;
}

function segButton(name: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll<HTMLButtonElement>('button.seg'));
  const el = buttons.find((b) => b.textContent?.trim() === name);
  if (!el) throw new Error(`missing state filter button ${name}`);
  return el;
}

function emitIssueChanged(repoID: string): void {
  for (const source of FakeEventSource.instances) {
    source.emit('issue.changed', { repoID });
  }
}

beforeEach(() => {
  binding = 'builtin';
  issuesFail = false;
  readyFail = false;
  issuesFetches = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('RepoIssues (builtin repo)', () => {
  it('shows the ready queue, the issue cards and the mutation entry points', async () => {
    await mountIssues();

    expect(container.textContent).toContain('Ready for agent (1)');
    expect(container.querySelectorAll('.issue-card')).toHaveLength(2);
    expect(container.textContent).toContain('#1');
    expect(container.textContent).toContain('Fix login');
    expect(container.textContent).toContain('2 comments');
    expect(container.querySelector(`a[href="/repos/${REPO_ID}/issues/new"]`)).not.toBeNull();
    expect(container.querySelector(`a[href="/repos/${REPO_ID}/labels"]`)).not.toBeNull();
    expect(container.textContent).not.toContain('Managed on the forge');
  });

  it('narrows by label client-side without refetching the page', async () => {
    await mountIssues();
    expect(issuesFetches).toHaveLength(1);

    chipButton('ui').click();
    await settle();

    const cards = container.querySelectorAll('.issue-card');
    expect(cards).toHaveLength(1);
    expect(cards[0]?.textContent).toContain('Polish UI');
    expect(issuesFetches).toHaveLength(1); // same state page — no refetch

    // Un-toggling the active chip restores the full page, still no refetch.
    chipButton('ui').click();
    await settle();
    expect(container.querySelectorAll('.issue-card')).toHaveLength(2);
    expect(issuesFetches).toHaveLength(1);
  });

  it('refetches server-side when the state filter changes', async () => {
    await mountIssues();

    segButton('closed').click();
    await settle();

    expect(issuesFetches).toEqual([
      `/api/v1/repos/${REPO_ID}/issues?state=open`,
      `/api/v1/repos/${REPO_ID}/issues?state=closed`,
    ]);
    expect(container.textContent).toContain('No closed issues.');
  });

  it('refetches on issue.changed for this repo only', async () => {
    await mountIssues();
    expect(issuesFetches).toHaveLength(1);

    emitIssueChanged('repo_other');
    await settle();
    expect(issuesFetches).toHaveLength(1);

    emitIssueChanged(REPO_ID);
    await settle();
    expect(issuesFetches).toHaveLength(2);
  });
});

describe('RepoIssues (fetch failures)', () => {
  it('shows the error banner when the issue list fails, page skeleton intact', async () => {
    issuesFail = true;
    await mountIssues();

    const banner = container.querySelector('.banner.error');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain('tracker unavailable');
    // The rest of the page stays up instead of going silently blank.
    expect(container.textContent).toContain('coding-lab · Issues');
    expect(container.querySelectorAll('button.seg')).toHaveLength(3);
    expect(container.querySelectorAll('.issue-card')).toHaveLength(0);
    expect(container.querySelector('.empty')).toBeNull();
  });

  it('keeps the issue cards when only the ready queue fails', async () => {
    readyFail = true;
    await mountIssues();

    expect(container.querySelectorAll('.issue-card')).toHaveLength(2);
    expect(container.textContent).not.toContain('Ready for agent');
    expect(container.querySelector('.banner.error')).toBeNull();
  });
});

describe('RepoIssues (forge repo)', () => {
  beforeEach(() => {
    binding = 'forge';
  });

  it('hides every mutation entry point behind the forge note, read view intact', async () => {
    await mountIssues();

    expect(container.textContent).toContain('Managed on the forge');
    expect(container.querySelector(`a[href="/repos/${REPO_ID}/issues/new"]`)).toBeNull();
    expect(container.querySelector(`a[href="/repos/${REPO_ID}/labels"]`)).toBeNull();
    // Read views still work: cards and the ready queue render.
    expect(container.querySelectorAll('.issue-card')).toHaveLength(2);
    expect(container.textContent).toContain('Ready for agent (1)');
  });
});
