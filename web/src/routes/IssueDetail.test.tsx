// IssueDetail behavioral contract:
// - builtin repos expose the add-comment box, the open/close toggle and the
//   label editor; forge-bound repos hide all three behind the managed-on-the-
//   forge note while body and comments stay readable;
// - the comment form POSTs {body}; the state toggle PATCHes {state}; the
//   label editor PUTs the replaced set built with the toggle algebra.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { IssueDetail as Issue, Label, TrackerBinding } from '../api';
import App from '../App';
import IssueDetail from './IssueDetail';

const REPO_ID = 'repo_1';
const NUMBER = 7;

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
let issueOnServer: Issue;
let commentBodies: Record<string, unknown>[];
let labelPuts: Record<string, unknown>[];
let patches: Record<string, unknown>[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function baseIssue(): Issue {
  return {
    number: NUMBER,
    title: 'Fix login',
    body: 'Steps to reproduce…',
    state: 'open',
    labels: ['bug'],
    comments: [{ author: 'operator', body: 'first', created_at: '2026-07-06T01:00:00.000Z' }],
    created_at: '2026-07-06T00:00:00.000Z',
    updated_at: '2026-07-06T01:00:00.000Z',
  };
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
      if (url === `/api/v1/repos/${REPO_ID}/issues/${NUMBER}` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...issueOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/issues/${NUMBER}` && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as Record<string, unknown>;
        patches.push(patch);
        issueOnServer = { ...issueOnServer, ...patch };
        return Promise.resolve(jsonResponse(200, { ...issueOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/issues/${NUMBER}/comments` && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as { body: string };
        commentBodies.push(body);
        const comment = {
          author: 'operator',
          body: body.body,
          created_at: '2026-07-06T02:00:00.000Z',
        };
        issueOnServer = { ...issueOnServer, comments: [...issueOnServer.comments, comment] };
        return Promise.resolve(jsonResponse(201, comment));
      }
      if (url === `/api/v1/repos/${REPO_ID}/issues/${NUMBER}/labels` && method === 'PUT') {
        const body = JSON.parse(String(init?.body)) as { labels: string[] };
        labelPuts.push(body);
        issueOnServer = { ...issueOnServer, labels: body.labels };
        return Promise.resolve(jsonResponse(200, { ...issueOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/labels` && method === 'GET') {
        if (binding !== 'builtin') {
          return Promise.reject(new Error('labels endpoint hit for a forge repo'));
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

async function mountDetail(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/repos/${REPO_ID}/issues/${NUMBER}` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/repos/:id/issues/:number" component={IssueDetail} />
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
  binding = 'builtin';
  repoFails = false;
  issueOnServer = baseIssue();
  commentBodies = [];
  labelPuts = [];
  patches = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('IssueDetail (builtin repo)', () => {
  it('renders body, labels and comments with the mutation controls', async () => {
    await mountDetail();

    expect(container.textContent).toContain('#7');
    expect(container.textContent).toContain('Fix login');
    expect(container.textContent).toContain('Steps to reproduce…');
    expect(container.textContent).toContain('first');
    expect(container.querySelector('textarea[name="comment"]')).not.toBeNull();
    expect(buttonByText('Close issue')).not.toBeNull();
    expect(buttonByText('Edit labels')).not.toBeNull();
    expect(container.textContent).not.toContain('Managed on the forge');
  });

  it('POSTs the comment body and shows the refreshed thread', async () => {
    await mountDetail();

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[name="comment"]');
    if (!textarea) throw new Error('missing comment textarea');
    textarea.value = 'looks good';
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    const form = container.querySelector('form.comment-form');
    if (!form) throw new Error('missing comment form');
    form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
    await settle();

    expect(commentBodies).toEqual([{ body: 'looks good' }]);
    expect(container.textContent).toContain('looks good');
    expect(container.textContent).toContain('Comments (2)');
  });

  it('PATCHes the state on Close issue', async () => {
    await mountDetail();

    mustButton('Close issue').click();
    await settle();

    expect(patches).toEqual([{ state: 'closed' }]);
    expect(buttonByText('Reopen issue')).not.toBeNull();
  });

  it('PUTs the replaced label set from the editor', async () => {
    await mountDetail();

    mustButton('Edit labels').click();
    await settle();

    // Save is disabled until the selection differs from the issue's labels.
    expect(mustButton('Save labels').disabled).toBe(true);

    // Toggle 'ui' on: picker chips are the repo's labels (bug, ui).
    const chips = Array.from(container.querySelectorAll<HTMLButtonElement>('button.chip-toggle'));
    const ui = chips.find((b) => b.textContent?.includes('ui'));
    if (!ui) throw new Error('missing ui picker chip');
    ui.click();
    await settle();

    expect(mustButton('Save labels').disabled).toBe(false);
    mustButton('Save labels').click();
    await settle();

    expect(labelPuts).toEqual([{ labels: ['bug', 'ui'] }]);
  });
});

describe('IssueDetail (repo fetch fails)', () => {
  beforeEach(() => {
    repoFails = true;
  });

  it('keeps the loaded issue readable and surfaces the repo error', async () => {
    await mountDetail();

    // The successfully loaded issue must not be blanked by the repo failure.
    expect(container.textContent).toContain('#7');
    expect(container.textContent).toContain('Fix login');
    expect(container.textContent).toContain('Steps to reproduce…');
    expect(container.textContent).toContain('first');
    // The failure is surfaced in a banner…
    const banner = container.querySelector('.banner.error');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain('repo lookup failed');
    // …and the view degrades to read-only: the binding is unknown, so no
    // mutation controls and no misleading forge note.
    expect(container.querySelector('textarea[name="comment"]')).toBeNull();
    expect(buttonByText('Close issue')).toBeNull();
    expect(buttonByText('Edit labels')).toBeNull();
    expect(container.textContent).not.toContain('Managed on the forge');
  });
});

describe('IssueDetail (forge repo)', () => {
  beforeEach(() => {
    binding = 'forge';
  });

  it('hides comment box, state toggle and label editor behind the forge note', async () => {
    await mountDetail();

    expect(container.textContent).toContain('Managed on the forge');
    expect(container.querySelector('textarea[name="comment"]')).toBeNull();
    expect(buttonByText('Close issue')).toBeNull();
    expect(buttonByText('Edit labels')).toBeNull();
    // The read view keeps working.
    expect(container.textContent).toContain('Steps to reproduce…');
    expect(container.textContent).toContain('first');
  });
});
