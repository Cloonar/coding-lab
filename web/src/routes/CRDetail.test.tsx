// CRDetail behavioral contract:
// - the diff view renders classified rows (file header / add / remove /
//   context) with the truncation notice and the head-branch-gone note;
// - closes chips link to the built-in issues the merge will close;
// - Merge is gated behind the confirm panel: nothing POSTs until the
//   operator confirms, Cancel POSTs nothing;
// - a merge 409 body (push rejection) is surfaced VERBATIM — it carries the
//   actionable git/hook message;
// - Close without merging POSTs .../close; a merged CR shows no actions.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { CRDetail as CR } from '../api';
import App from '../App';
import CRDetail from './CRDetail';

const REPO_ID = 'repo_1';
const NUMBER = 3;

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

const SAMPLE_DIFF = [
  'diff --git a/main.go b/main.go',
  'index 1111111..2222222 100644',
  '--- a/main.go',
  '+++ b/main.go',
  '@@ -1,3 +1,4 @@',
  ' package main',
  '-func old() {}',
  '+func new() {}',
  '+// added',
].join('\n');

function baseCR(): CR {
  return {
    number: NUMBER,
    title: 'Add retry loop',
    body: 'Implements the retry.\n\nCloses #7',
    state: 'open',
    head_branch: 'afk/7',
    base_branch: 'main',
    closes: [7, 9],
    created_at: '2026-07-06T00:00:00.000Z',
    merged_at: null,
    merge_commit: null,
    diff: SAMPLE_DIFF,
    diff_truncated: false,
  };
}

let crOnServer: CR;
let mergeStatus: number;
let mergeError: string;
let mergePosts: number;
let closePosts: number;
let crFetches: number;
let repoFails: boolean;
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
      // The breadcrumb's repo-name lookup — non-throwing: a failure degrades
      // to the placeholder name, it must never blank the CR view.
      if (url === `/api/v1/repos/${REPO_ID}` && method === 'GET') {
        if (repoFails) {
          return Promise.resolve(jsonResponse(500, { error: 'repo lookup failed' }));
        }
        return Promise.resolve(
          jsonResponse(200, { id: REPO_ID, name: 'coding-lab', tracker_binding: 'builtin' }),
        );
      }
      if (url === `/api/v1/repos/${REPO_ID}/crs/${NUMBER}` && method === 'GET') {
        crFetches += 1;
        return Promise.resolve(jsonResponse(200, { ...crOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/crs/${NUMBER}/merge` && method === 'POST') {
        mergePosts += 1;
        if (mergeStatus !== 200) {
          return Promise.resolve(jsonResponse(mergeStatus, { error: mergeError }));
        }
        crOnServer = {
          ...crOnServer,
          state: 'merged',
          merged_at: '2026-07-06T02:00:00.000Z',
          merge_commit: 'abc1234def5678',
        };
        return Promise.resolve(jsonResponse(200, { cr: { ...crOnServer } }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/crs/${NUMBER}/close` && method === 'POST') {
        closePosts += 1;
        crOnServer = { ...crOnServer, state: 'closed' };
        return Promise.resolve(jsonResponse(200, { cr: { ...crOnServer } }));
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

async function mountDetail(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/repos/${REPO_ID}/crs/${NUMBER}` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/repos/:id/crs/:number" component={CRDetail} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function buttonByText(text: string): HTMLButtonElement | null {
  const buttons = Array.from(container.querySelectorAll('button'));
  return buttons.find((b) => b.textContent?.trim() === text) ?? null;
}

function mustButton(text: string): HTMLButtonElement {
  const el = buttonByText(text);
  if (!el) throw new Error(`missing button ${text}`);
  return el;
}

beforeEach(() => {
  crOnServer = baseCR();
  mergeStatus = 200;
  mergeError = '';
  mergePosts = 0;
  closePosts = 0;
  crFetches = 0;
  repoFails = false;
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('CRDetail (breadcrumb)', () => {
  it('renders the trail (Repos / <repo> / Change requests / #N), leaf inert', async () => {
    await mountDetail();

    const crumb = container.querySelector('p.crumb');
    expect(crumb?.textContent).toBe('Repos / coding-lab / Change requests / #3');
    expect(crumb?.querySelector('a[href="/repos"]')).not.toBeNull();
    expect(crumb?.querySelector(`a[href="/repos/${REPO_ID}/issues"]`)).not.toBeNull();
    expect(crumb?.querySelector(`a[href="/repos/${REPO_ID}/crs"]`)).not.toBeNull();
    // The #3 leaf is the current page: inert text, not a link.
    expect(crumb?.querySelector(`a[href="/repos/${REPO_ID}/crs/${NUMBER}"]`)).toBeNull();
  });

  it('degrades to the placeholder name and keeps the CR readable when getRepo fails', async () => {
    repoFails = true;
    await mountDetail();

    // The repo failure must not blank the loaded CR.
    expect(container.textContent).toContain('CR #3');
    expect(container.textContent).toContain('Add retry loop');
    // The trail still renders, with the placeholder standing in for the name.
    expect(container.querySelector('p.crumb')?.textContent).toBe(
      'Repos / Repository / Change requests / #3',
    );
  });
});

describe('CRDetail (open CR)', () => {
  it('renders title, branches, body, closes chips and the classified diff', async () => {
    await mountDetail();

    expect(container.textContent).toContain('CR #3');
    expect(container.textContent).toContain('Add retry loop');
    expect(container.textContent).toContain('afk/7 → main');
    expect(container.textContent).toContain('Implements the retry.');

    // Closes chips link to the issues the merge will close.
    const chip7 = container.querySelector(`a[href="/repos/${REPO_ID}/issues/7"]`);
    const chip9 = container.querySelector(`a[href="/repos/${REPO_ID}/issues/9"]`);
    expect(chip7?.textContent).toContain('closes #7');
    expect(chip9?.textContent).toContain('closes #9');

    // Diff rows carry the classifier's classes: tinting is CSS on these.
    expect(container.querySelector('.diff-file-head')?.textContent).toBe('main.go');
    expect(container.querySelectorAll('.diff-row.diff-add')).toHaveLength(2);
    expect(container.querySelectorAll('.diff-row.diff-remove')).toHaveLength(1);
    expect(container.querySelectorAll('.diff-row.diff-hunk')).toHaveLength(1);
    expect(container.querySelectorAll('.diff-row.diff-context')).toHaveLength(1);
    // The '--- a/main.go' header row stays meta — never tinted as a removal.
    expect(container.querySelectorAll('.diff-row.diff-meta')).toHaveLength(3);
    // Horizontal scrolling happens INSIDE the diff container.
    expect(container.querySelector('.diff-scroll')).not.toBeNull();
    expect(container.textContent).not.toContain('Diff truncated');
  });

  it('gates Merge behind the confirm panel and POSTs only on confirm', async () => {
    await mountDetail();

    // No confirm panel, no POST yet.
    expect(container.querySelector('.merge-confirm')).toBeNull();
    mustButton('Merge').click();
    await settle();

    expect(mergePosts).toBe(0); // opening the panel POSTs nothing
    expect(container.querySelector('.merge-confirm')).not.toBeNull();
    expect(container.textContent).toContain('closes issues #7, #9');

    mustButton('Confirm merge').click();
    await settle();

    expect(mergePosts).toBe(1);
    // The refetched CR is merged: state chip updates, actions disappear.
    expect(container.querySelector('.chip.state-merged')).not.toBeNull();
    expect(buttonByText('Merge')).toBeNull();
    expect(buttonByText('Close without merging')).toBeNull();
    expect(container.textContent).toContain('abc1234def');
  });

  it('POSTs nothing when the confirm panel is cancelled', async () => {
    await mountDetail();

    mustButton('Merge').click();
    await settle();
    mustButton('Cancel').click();
    await settle();

    expect(mergePosts).toBe(0);
    expect(container.querySelector('.merge-confirm')).toBeNull();
    expect(buttonByText('Merge')).not.toBeNull();
  });

  it('surfaces the push-rejection 409 body verbatim', async () => {
    mergeStatus = 409;
    mergeError = 'push rejected: remote: GH006: Protected branch update failed for main';
    await mountDetail();

    mustButton('Merge').click();
    await settle();
    mustButton('Confirm merge').click();
    await settle();

    expect(mergePosts).toBe(1);
    const banner = container.querySelector('.banner.error');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain(
      'push rejected: remote: GH006: Protected branch update failed for main',
    );
    // The CR is still open — the confirm panel stays for a retry.
    expect(container.querySelector('.merge-confirm')).not.toBeNull();
  });

  it('POSTs .../close on Close without merging', async () => {
    await mountDetail();

    mustButton('Close without merging').click();
    await settle();

    expect(closePosts).toBe(1);
    expect(mergePosts).toBe(0);
    expect(container.querySelector('.chip.state-closed')).not.toBeNull();
  });

  it('refetches on cr.changed for this repo only', async () => {
    await mountDetail();
    expect(crFetches).toBe(1);

    for (const source of FakeEventSource.instances) {
      source.emit('cr.changed', { repoID: 'repo_other' });
    }
    await settle();
    expect(crFetches).toBe(1);

    for (const source of FakeEventSource.instances) {
      source.emit('cr.changed', { repoID: REPO_ID });
    }
    await settle();
    expect(crFetches).toBe(2);
  });
});

describe('CRDetail (diff edge cases)', () => {
  it('shows the truncation notice when the server bounded the diff', async () => {
    crOnServer = { ...baseCR(), diff_truncated: true };
    await mountDetail();

    expect(container.textContent).toContain('Diff truncated');
  });

  it('shows the server note when the head branch is gone and the diff is omitted', async () => {
    const cr = baseCR();
    delete cr.diff;
    delete cr.diff_truncated;
    crOnServer = { ...cr, note: 'head branch afk/7 no longer exists' };
    await mountDetail();

    expect(container.querySelector('.diff-row')).toBeNull();
    expect(container.textContent).toContain('head branch afk/7 no longer exists');
  });
});

describe('CRDetail (merged CR)', () => {
  it('shows the merge record and no action buttons', async () => {
    crOnServer = {
      ...baseCR(),
      state: 'merged',
      merged_at: '2026-07-06T02:00:00.000Z',
      merge_commit: 'abc1234def5678',
    };
    await mountDetail();

    expect(container.querySelector('.chip.state-merged')).not.toBeNull();
    expect(container.textContent).toContain('abc1234def');
    expect(buttonByText('Merge')).toBeNull();
    expect(buttonByText('Close without merging')).toBeNull();
  });
});
