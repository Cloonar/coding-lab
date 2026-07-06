// Discard confirm gate: Discard is the one UNGUARDED destruction in lab, so
// the dialog's button must stay disabled until the operator types the branch
// name exactly — no trim leniency, no near-miss.

import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ParkedEntry } from '../api';
import { EventsProvider } from '../events';
import ParkedSection from './ParkedSection';

const REPO_ID = 'repo_1';
const BRANCH = 'lab/foo-20260608-1530';

/** Minimal EventSource stand-in so EventsProvider can mount under jsdom. */
class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

let parkedOnServer: ParkedEntry[];
let discardBodies: Record<string, unknown>[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

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

function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === `/api/v1/repos/${REPO_ID}/parked` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { parked: parkedOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/parked/discard` && method === 'POST') {
        discardBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return Promise.resolve(jsonResponse(204));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountParked(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(
    () => (
      <EventsProvider>
        <ParkedSection repoID={REPO_ID} />
      </EventsProvider>
    ),
    container,
  );
  await settle();
}

function query<T extends Element>(selector: string): T {
  const el = container.querySelector<T>(selector);
  if (!el) throw new Error(`missing ${selector}`);
  return el;
}

function typeInto(el: HTMLInputElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

function discardButton(): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'));
  const el = buttons.find((b) => b.textContent?.includes('Discard forever'));
  if (!el) throw new Error('missing Discard forever button');
  return el;
}

beforeEach(() => {
  parkedOnServer = [
    { branch: BRANCH, worktree_path: '/wt/foo', dirty: true, commits_ahead: 1, unpushed: 1 },
  ];
  discardBodies = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('ParkedSection discard confirm gate', () => {
  it('renders the parked entry with its badges', async () => {
    await mountParked();

    expect(container.textContent).toContain(BRANCH);
    expect(container.textContent).toContain('● dirty');
    expect(container.textContent).toContain('↑1');
    expect(container.textContent).toContain('⚠ 1 unpushed');
    expect(container.textContent).toContain('/wt/foo');
  });

  it('renders nothing at all while the parked set is empty', async () => {
    parkedOnServer = [];
    await mountParked();

    expect(container.querySelector('details.parked')).toBeNull();
  });

  it('keeps Discard disabled until the exact branch name is typed', async () => {
    await mountParked();

    query<HTMLButtonElement>('button.parked-discard').click();
    await settle();

    const button = discardButton();
    const input = query<HTMLInputElement>('input[name="confirm-branch"]');

    // Untyped → disabled.
    expect(button.disabled).toBe(true);

    // Near-misses stay disabled: prefix, wrong case, trailing whitespace.
    typeInto(input, 'lab/foo');
    expect(discardButton().disabled).toBe(true);
    typeInto(input, BRANCH.toUpperCase());
    expect(discardButton().disabled).toBe(true);
    typeInto(input, `${BRANCH} `);
    expect(discardButton().disabled).toBe(true);

    // Exact match arms it.
    typeInto(input, BRANCH);
    expect(discardButton().disabled).toBe(false);

    // And editing away disarms again.
    typeInto(input, `${BRANCH}x`);
    expect(discardButton().disabled).toBe(true);

    expect(discardBodies).toEqual([]); // nothing fired while disarmed
  });

  it('POSTs the discard only after the exact confirmation', async () => {
    await mountParked();

    query<HTMLButtonElement>('button.parked-discard').click();
    await settle();
    typeInto(query<HTMLInputElement>('input[name="confirm-branch"]'), BRANCH);
    discardButton().click();
    parkedOnServer = []; // the server-side refetch now sees it gone
    await settle();

    expect(discardBodies).toEqual([{ branch: BRANCH }]);
    expect(container.querySelector('details.parked')).toBeNull();
  });
});
