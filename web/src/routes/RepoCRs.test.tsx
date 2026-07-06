// RepoCRs behavioral contract:
// - CR cards render number, title, state chip, head → base and closes chips
//   linking to the issues;
// - the state filter refetches server-side with ?state=…;
// - a scoped cr.changed refetches, foreign repoIDs do not.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { CRSummary } from '../api';
import App from '../App';
import RepoCRs from './RepoCRs';

const REPO_ID = 'repo_1';

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

  emit(type: string, payload: Record<string, unknown>): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data: JSON.stringify(payload) });
    }
  }
}

const OPEN_CRS: CRSummary[] = [
  {
    number: 4,
    title: 'Add retry loop',
    state: 'open',
    head_branch: 'afk/7',
    base_branch: 'main',
    closes: [7],
    created_at: '2026-07-06T00:00:00.000Z',
    merged_at: null,
    merge_commit: null,
  },
  {
    number: 3,
    title: 'Polish CSS',
    state: 'open',
    head_branch: 'lab/polish',
    base_branch: 'main',
    closes: [],
    created_at: '2026-07-05T00:00:00.000Z',
    merged_at: null,
    merge_commit: null,
  },
];

let crFetches: string[];
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
          jsonResponse(200, { id: REPO_ID, name: 'coding-lab', tracker_binding: 'builtin' }),
        );
      }
      if (url.startsWith(`/api/v1/repos/${REPO_ID}/crs?`) && method === 'GET') {
        crFetches.push(url);
        const state = new URLSearchParams(url.split('?')[1]).get('state');
        const crs = state === 'open' ? OPEN_CRS : [];
        return Promise.resolve(jsonResponse(200, { crs }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountCRs(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/repos/${REPO_ID}/crs` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/repos/:id/crs" component={RepoCRs} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function segButton(name: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll<HTMLButtonElement>('button.seg'));
  const el = buttons.find((b) => b.textContent?.trim() === name);
  if (!el) throw new Error(`missing state filter button ${name}`);
  return el;
}

function emitCRChanged(repoID: string): void {
  for (const source of FakeEventSource.instances) {
    source.emit('cr.changed', { repoID });
  }
}

beforeEach(() => {
  crFetches = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('RepoCRs', () => {
  it('renders CR cards with state chips, branches and closes chips', async () => {
    await mountCRs();

    expect(container.textContent).toContain('coding-lab · Change requests');
    const cards = container.querySelectorAll('.cr-card');
    expect(cards).toHaveLength(2);
    expect(container.textContent).toContain('CR #4');
    expect(container.textContent).toContain('Add retry loop');
    expect(container.textContent).toContain('afk/7 → main');
    expect(container.querySelectorAll('.chip.state-open')).toHaveLength(2);

    // The card links to the detail; the closes chip links to the issue.
    expect(container.querySelector(`a[href="/repos/${REPO_ID}/crs/4"]`)).not.toBeNull();
    const closesChip = container.querySelector(`a[href="/repos/${REPO_ID}/issues/7"]`);
    expect(closesChip?.textContent).toContain('closes #7');
  });

  it('refetches server-side when the state filter changes', async () => {
    await mountCRs();

    segButton('merged').click();
    await settle();

    expect(crFetches).toEqual([
      `/api/v1/repos/${REPO_ID}/crs?state=open`,
      `/api/v1/repos/${REPO_ID}/crs?state=merged`,
    ]);
    expect(container.textContent).toContain('No merged change requests.');
  });

  it('refetches on cr.changed for this repo only', async () => {
    await mountCRs();
    expect(crFetches).toHaveLength(1);

    emitCRChanged('repo_other');
    await settle();
    expect(crFetches).toHaveLength(1);

    emitCRChanged(REPO_ID);
    await settle();
    expect(crFetches).toHaveLength(2);
  });
});
