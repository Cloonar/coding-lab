// Add-repo agent pick (issue #66 / ADR-0030): the always-visible "Agent"
// select defaults to the inherit entry (Global default) and POST /repos only
// carries `provider` when the operator explicitly chose one.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Provider } from '../api';
import App from '../App';
import AddRepo from './AddRepo';

const PROVIDERS: Provider[] = [
  {
    id: 'claude-code',
    display_name: 'Claude Code',
    supports_remote: true,
    auth: { kind: 'oauth-code' },
    models: [{ value: 'sonnet', label: 'Sonnet', efforts: [] }],
    efforts: [{ value: 'high', label: 'high' }],
    options: [],
  },
  {
    id: 'codex',
    display_name: 'Codex',
    supports_remote: false,
    auth: { kind: 'api-key' },
    models: [{ value: 'gpt-5-codex', label: 'GPT-5 Codex', efforts: [] }],
    efforts: [],
    options: [],
  },
];

/** Stand-in for EventSource so the authenticated App shell can mount. */
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

let createBodies: Record<string, unknown>[];
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
      if (url === '/api/v1/credentials' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { credentials: [] }));
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: PROVIDERS }));
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      if (url === '/api/v1/repos' && method === 'POST') {
        createBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return Promise.resolve(jsonResponse(201, { id: 'repo_1', clone_status: 'cloning' }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountAddRepo(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/repos/new' });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/repos/new" component={AddRepo} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function typeInto(el: HTMLInputElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/** The unified Select trigger button (field skin) for a form field name. */
function selectTrigger(name: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(`button[name="${name}"]`);
  if (!el) throw new Error(`missing select trigger button[name="${name}"]`);
  return el;
}

/** Opens the named Select and clicks the option with the given label. */
async function chooseFromSelect(name: string, optionLabel: string): Promise<void> {
  selectTrigger(name).click();
  await settle();
  const row = Array.from(container.querySelectorAll<HTMLButtonElement>('[role="option"]')).find(
    (r) => r.querySelector('.select-option-label')?.textContent === optionLabel,
  );
  if (!row) throw new Error(`missing option ${JSON.stringify(optionLabel)} in ${name}`);
  row.click();
  await settle();
}

async function submitForm(): Promise<void> {
  const form = container.querySelector('form');
  if (!form) throw new Error('missing add-repo form');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
  await settle();
}

beforeEach(() => {
  createBodies = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('AddRepo agent pick', () => {
  it('POSTs the chosen provider alongside the remote URL', async () => {
    await mountAddRepo();
    const url = container.querySelector<HTMLInputElement>('input[name="remote_url"]');
    expect(url).not.toBeNull();
    typeInto(url!, 'git@h:o/r.git');

    // The select defaults to the inherit entry before the pick.
    expect(selectTrigger('provider').textContent).toContain('Global default');
    await chooseFromSelect('provider', 'Codex');
    await submitForm();

    expect(createBodies).toEqual([{ remote_url: 'git@h:o/r.git', provider: 'codex' }]);
  });

  it('omits provider entirely when left on the Global default entry', async () => {
    await mountAddRepo();
    typeInto(
      container.querySelector<HTMLInputElement>('input[name="remote_url"]')!,
      'git@h:o/r.git',
    );

    await submitForm();

    expect(createBodies).toEqual([{ remote_url: 'git@h:o/r.git' }]);
  });
});
