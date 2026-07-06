// RepoLabels behavioral contract:
// - a failed getRepo renders the error banner (the h2 falls back to
//   'Repository') instead of blanking the page below the heading;
// - an SSE-triggered refetch (issue.changed for this repo) reconciles the
//   label rows by id, so an in-progress edit — the open LabelForm and its
//   typed draft — survives the refetch instead of being remounted and wiped.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Label, TrackerBinding } from '../api';
import App from '../App';
import RepoLabels from './RepoLabels';

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

let binding: TrackerBinding;
let repoFails: boolean;
let labelsOnServer: Label[];
let labelsFetches: number;
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
        if (repoFails) {
          return Promise.resolve(jsonResponse(500, { error: 'repo lookup failed' }));
        }
        return Promise.resolve(
          jsonResponse(200, { id: REPO_ID, name: 'coding-lab', tracker_binding: binding }),
        );
      }
      if (url === `/api/v1/repos/${REPO_ID}/labels` && method === 'GET') {
        labelsFetches += 1;
        // JSON round-trip: every fetch returns fresh object identities, like
        // the real API — exactly what used to tear the <For> rows down.
        return Promise.resolve(jsonResponse(200, { labels: labelsOnServer }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountLabels(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/repos/${REPO_ID}/labels` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/repos/:id/labels" component={RepoLabels} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function rowFor(name: string): HTMLLIElement {
  const rows = Array.from(container.querySelectorAll<HTMLLIElement>('ul.label-list > li'));
  const row = rows.find((li) => li.textContent?.includes(name));
  if (!row) throw new Error(`missing label row ${name}`);
  return row;
}

function rowButton(row: HTMLElement, text: string): HTMLButtonElement {
  const buttons = Array.from(row.querySelectorAll<HTMLButtonElement>('button'));
  const el = buttons.find((b) => b.textContent?.trim() === text);
  if (!el) throw new Error(`missing button ${text}`);
  return el;
}

function emitIssueChanged(repoID: string): void {
  for (const source of FakeEventSource.instances) {
    source.emit('issue.changed', { repoID });
  }
}

beforeEach(() => {
  binding = 'builtin';
  repoFails = false;
  labelsFetches = 0;
  labelsOnServer = [
    { id: 'lbl_1', name: 'bug', color: '#d73a4a', description: 'defects' },
    { id: 'lbl_2', name: 'ui', color: '#1d76db', description: '' },
  ];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('RepoLabels (repo fetch fails)', () => {
  it('shows the repo error banner instead of a blank page', async () => {
    repoFails = true;
    await mountLabels();

    // Header falls back and the failure is visible — not a silent blank.
    expect(container.textContent).toContain('Repository · Labels');
    const banner = container.querySelector('.banner.error');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain('repo lookup failed');
  });
});

describe('RepoLabels (builtin repo)', () => {
  it('lists the labels with edit and delete controls', async () => {
    await mountLabels();

    expect(labelsFetches).toBe(1);
    expect(container.textContent).toContain('bug');
    expect(container.textContent).toContain('ui');
    expect(rowButton(rowFor('bug'), 'Edit')).not.toBeNull();
    expect(rowButton(rowFor('bug'), 'Delete')).not.toBeNull();
  });

  it('keeps an in-progress edit across an issue.changed refetch', async () => {
    await mountLabels();

    // Open the edit form on "bug" and type a draft rename.
    rowButton(rowFor('bug'), 'Edit').click();
    await settle();
    const nameInput = container.querySelector<HTMLInputElement>('input[name="label-name"]');
    if (!nameInput) throw new Error('missing label name input');
    expect(nameInput.value).toBe('bug');
    nameInput.value = 'bug-draft';
    nameInput.dispatchEvent(new Event('input', { bubbles: true }));

    // A label appears server-side and issue.changed triggers the refetch.
    labelsOnServer = [
      ...labelsOnServer,
      { id: 'lbl_3', name: 'brand-new', color: '#0e8a16', description: '' },
    ];
    emitIssueChanged(REPO_ID);
    await settle();

    // The refetch landed (new label rendered, list re-fetched)…
    expect(labelsFetches).toBe(2);
    expect(container.textContent).toContain('brand-new');
    // …and the open form survived with the typed draft intact.
    const after = container.querySelector<HTMLInputElement>('input[name="label-name"]');
    expect(after).not.toBeNull();
    expect(after?.value).toBe('bug-draft');
  });
});
