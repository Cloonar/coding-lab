// Parked work card (the repo home's Overview, issue #61) and its discard
// confirm gate: Discard is the one UNGUARDED destruction in lab, so it asks in
// place, and the confirmation's button stays disabled until the operator types
// the branch name exactly — no trim leniency, no near-miss. An unavailable
// parked endpoint renders no block at all.

import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ParkedEntry } from '../api';
import { EventsProvider } from '../events';
import ParkedSection, { parkedSummary } from './ParkedSection';

const REPO_ID = 'repo_1';
const BRANCH = 'lab/foo-20260608-1530';

/** EventSource stand-in so EventsProvider can mount under jsdom; tests push events. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((event: { data: string }) => void)[]>();
  constructor() {
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, listener: (event: { data: string }) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }
  close(): void {}
  emit(type: string, payload: Record<string, unknown>): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data: JSON.stringify(payload) });
    }
  }
}

function emitParkedChanged(): void {
  for (const source of FakeEventSource.instances)
    source.emit('parked.changed', { repoID: REPO_ID });
}

let parkedOnServer: ParkedEntry[] | null;
let discardBodies: Record<string, unknown>[];
let discarded: string[];
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
        if (parkedOnServer === null) {
          return Promise.resolve(jsonResponse(404, { error: 'not found' }));
        }
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
        <ParkedSection repoID={REPO_ID} onDiscarded={(branch) => discarded.push(branch)} />
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
  discarded = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('ParkedSection card', () => {
  it('renders the parked entry with what it holds, in words', async () => {
    await mountParked();

    const card = query<HTMLElement>('section.parked-card');
    expect(card.querySelector('h2')?.textContent).toBe('Parked work (1)');
    expect(card.querySelector('.count')?.textContent).toBe('1');
    expect(query('.parked-branch').textContent).toBe(BRANCH);
    expect(query('.parked-state').textContent).toBe(
      'Worktree has uncommitted changes · 1 commit ahead · 1 unpushed commit',
    );
    expect(query('.parked-path').textContent).toBe('/wt/foo');
    expect(query('button.parked-discard').getAttribute('aria-label')).toBe(`Discard ${BRANCH}`);
  });

  it('says "Nothing parked." with a 0 count while the parked set is empty', async () => {
    parkedOnServer = [];
    await mountParked();

    expect(query('section.parked-card .count').textContent).toBe('0');
    expect(query('section.parked-card').textContent).toContain('Nothing parked.');
    expect(container.querySelector('.parked-entry')).toBeNull();
  });

  it('renders no block at all when the parked endpoint is unavailable', async () => {
    parkedOnServer = null;
    await mountParked();

    expect(container.querySelector('section.parked-card')).toBeNull();
    expect(container.textContent).toBe('');
  });
});

describe('parkedSummary', () => {
  const entry = (over: Partial<ParkedEntry>): ParkedEntry => ({
    branch: BRANCH,
    worktree_path: '/wt/foo',
    dirty: false,
    commits_ahead: 0,
    unpushed: 0,
    ...over,
  });

  it('words each kind of preserved work', () => {
    expect(parkedSummary(entry({ dirty: true }))).toBe('Worktree has uncommitted changes');
    expect(parkedSummary(entry({ dirty: true, worktree_path: '' }))).toBe('Uncommitted changes');
    expect(parkedSummary(entry({ commits_ahead: 2, unpushed: 2 }))).toBe(
      '2 commits ahead · 2 unpushed commits',
    );
    expect(parkedSummary(entry({}))).toBe('Branch is not merged');
  });
});

describe('ParkedSection discard confirm gate', () => {
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
    expect(discarded).toEqual([BRANCH]);
    expect(container.querySelector('.parked-entry')).toBeNull();
    expect(query('section.parked-card').textContent).toContain('Nothing parked.');
  });

  it('asks in place: Cancel and Escape close the confirmation without a request', async () => {
    await mountParked();

    const trigger = query<HTMLButtonElement>('button.parked-discard');
    trigger.click();
    await settle();
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    // The confirmation opens under its entry, with the typed field focused.
    const confirm = query<HTMLElement>('.parked-entry .discard-confirm');
    expect(document.activeElement).toBe(query('input[name="confirm-branch"]'));

    const cancel = Array.from(confirm.querySelectorAll('button')).find(
      (b) => b.textContent === 'Cancel',
    );
    cancel?.click();
    await settle();
    expect(container.querySelector('.discard-confirm')).toBeNull();

    trigger.click();
    await settle();
    query('.discard-confirm').dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }),
    );
    await settle();
    expect(container.querySelector('.discard-confirm')).toBeNull();
    expect(discardBodies).toEqual([]);
  });
});

describe('ParkedSection across refetches and focus', () => {
  const OTHER = 'lab/bar-20260609-0900';
  const two = (): ParkedEntry[] => [
    { branch: BRANCH, worktree_path: '/wt/foo', dirty: true, commits_ahead: 1, unpushed: 1 },
    { branch: OTHER, worktree_path: '', dirty: false, commits_ahead: 2, unpushed: 0 },
  ];
  const trigger = (branch: string) =>
    query<HTMLButtonElement>(`button.parked-discard[aria-label="Discard ${branch}"]`);
  const cancelButton = () =>
    Array.from(container.querySelectorAll<HTMLButtonElement>('.discard-confirm button')).find(
      (b) => b.textContent === 'Cancel',
    )!;

  it('keeps the rows, the open confirmation and the typed text through parked.changed', async () => {
    parkedOnServer = two();
    await mountParked();
    const rows = Array.from(container.querySelectorAll('.parked-entry'));
    trigger(BRANCH).click();
    await settle();
    const input = query<HTMLInputElement>('input[name="confirm-branch"]');
    typeInto(input, 'lab/foo-2026');
    expect(document.activeElement).toBe(input);

    // Another entry changes server-side: a fresh list, fresh objects.
    parkedOnServer = two().map((e) => (e.branch === OTHER ? { ...e, commits_ahead: 3 } : e));
    emitParkedChanged();
    await settle();

    expect(container.textContent).toContain('3 commits ahead');
    expect(Array.from(container.querySelectorAll('.parked-entry'))).toEqual(rows);
    expect(query<HTMLInputElement>('input[name="confirm-branch"]')).toBe(input);
    expect(input.value).toBe('lab/foo-2026');
    expect(document.activeElement).toBe(input);
  });

  it('returns focus to the entry’s Discard on Cancel and on Escape', async () => {
    await mountParked();
    trigger(BRANCH).click();
    await settle();
    cancelButton().click();
    await settle();
    expect(document.activeElement).toBe(trigger(BRANCH));

    trigger(BRANCH).click();
    await settle();
    query('.discard-confirm').dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }),
    );
    await settle();
    expect(document.activeElement).toBe(trigger(BRANCH));
    // A cancelled confirmation starts empty next time.
    trigger(BRANCH).click();
    await settle();
    expect(query<HTMLInputElement>('input[name="confirm-branch"]').value).toBe('');
  });

  it('moves focus to the next entry after a discard, and to the heading after the last', async () => {
    parkedOnServer = two();
    await mountParked();

    trigger(BRANCH).click();
    await settle();
    typeInto(query<HTMLInputElement>('input[name="confirm-branch"]'), BRANCH);
    parkedOnServer = two().slice(1);
    discardButton().click();
    await settle();
    expect(container.textContent).not.toContain(BRANCH);
    expect(document.activeElement).toBe(trigger(OTHER));

    trigger(OTHER).click();
    await settle();
    typeInto(query<HTMLInputElement>('input[name="confirm-branch"]'), OTHER);
    parkedOnServer = [];
    discardButton().click();
    await settle();
    expect(query('section.parked-card').textContent).toContain('Nothing parked.');
    expect(document.activeElement).toBe(query('section.parked-card h2'));
  });
});
