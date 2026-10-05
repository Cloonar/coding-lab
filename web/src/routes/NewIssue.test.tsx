// NewIssue behavioral contract for fetch failures (it renders inside the repo
// home frame's Issues tab, issue #61, with an "Issues / New issue" trail):
// - a failed getRepo renders the frame's error banner instead of a dead page
//   with neither form nor feedback;
// - a failed listLabels keeps the form usable — only the label picker is
//   dropped.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Label, TrackerBinding } from '../api';
import App from '../App';
import RepoRoutes from './repo-home/routes';

const REPO_ID = 'repo_1';

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

const LABELS: Label[] = [
  { id: 'lbl_1', name: 'bug', color: '#d73a4a', description: '' },
  { id: 'lbl_2', name: 'ui', color: '#1d76db', description: '' },
];

let binding: TrackerBinding;
let repoFails: boolean;
let labelsFail: boolean;
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
          jsonResponse(200, {
            id: REPO_ID,
            name: 'coding-lab',
            tracker_binding: binding,
            remote_url: 'git@git.cloonar.com:Cloonar/coding-lab.git',
          }),
        );
      }
      if (url === `/api/v1/repos/${REPO_ID}/labels` && method === 'GET') {
        if (labelsFail) {
          return Promise.resolve(jsonResponse(500, { error: 'labels unavailable' }));
        }
        return Promise.resolve(jsonResponse(200, { labels: LABELS }));
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountNewIssue(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/repos/${REPO_ID}/issues/new` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <RepoRoutes />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

beforeEach(() => {
  binding = 'builtin';
  repoFails = false;
  labelsFail = false;
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('NewIssue (builtin repo)', () => {
  it('renders the form with the label picker', async () => {
    await mountNewIssue();

    // The frame names the repo; the page keeps its own heading and a trail
    // within the Issues tab (Issues links back to the list, the leaf is inert).
    expect(container.querySelector('.repo-head h1')?.textContent).toBe('coding-lab');
    expect(container.querySelector('.section-head h2')?.textContent).toBe('New issue');
    const crumb = container.querySelector('p.crumb');
    expect(crumb?.textContent).toBe('Issues / New issue');
    expect(
      Array.from(crumb?.querySelectorAll('a') ?? []).map((a) => a.getAttribute('href')),
    ).toEqual([`/repos/${REPO_ID}/issues`]);
    expect(container.querySelector('input[name="title"]')).not.toBeNull();
    expect(container.querySelectorAll('button.chip-toggle').length).toBeGreaterThan(0);
  });
});

describe('NewIssue (fetch failures)', () => {
  it('shows the repo error banner instead of a dead page', async () => {
    repoFails = true;
    await mountNewIssue();

    // The page heading stays and the failure is visible — not a silent blank.
    expect(container.querySelector('.section-head h2')?.textContent).toBe('New issue');
    const banner = container.querySelector('.banner.error');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain('repo lookup failed');
    expect(container.querySelector('form.form-card')).toBeNull();
  });

  it('keeps the form usable when the labels fetch fails', async () => {
    labelsFail = true;
    await mountNewIssue();

    expect(container.querySelector('form.form-card')).not.toBeNull();
    expect(container.querySelector('input[name="title"]')).not.toBeNull();
    // Only the picker is dropped.
    expect(container.querySelectorAll('button.chip-toggle')).toHaveLength(0);
  });
});
