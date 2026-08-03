// SideNav (issue #41): the ACTIVE rail renders from the instances prop —
// attention rows first (see lib/railOrder), each an <A> to /runs/:id with the
// current route highlighted via aria-current. Only live instances appear, and
// rows carry no Stop control (that lives in the chat header + Repos page).

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Instance } from '../api';
import { AuthProvider } from '../auth';
import { EventsProvider } from '../events';
import SideNav from './SideNav';

/** Stand-in for EventSource so EventsProvider can mount (SSE live dot). */
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
    started_at: '2026-07-06T15:00:00.000Z',
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

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 4; i += 1) await flush();
}

function mount(instances: Instance[], path = '/'): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: path });
  dispose = render(
    () => (
      <AuthProvider>
        <EventsProvider>
          <MemoryRouter history={history}>
            <Route path="*" component={() => <SideNav instances={instances} />} />
          </MemoryRouter>
        </EventsProvider>
      </AuthProvider>
    ),
    container,
  );
}

function railRows(): HTMLAnchorElement[] {
  return Array.from(container.querySelectorAll<HTMLAnchorElement>('a.rail-row'));
}

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown) => {
      const url = String(input);
      if (url === '/api/v1/auth/state') {
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
        );
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    }),
  );
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('SideNav ACTIVE list', () => {
  it('renders a row per live instance, each linking to /runs/:id', async () => {
    mount([
      instance({ id: 'run_a', session_name: 'proj~a-20260706-1500' }),
      instance({ id: 'run_b', session_name: 'proj~b-20260706-1501' }),
    ]);
    await settle();
    const rows = railRows();
    expect(rows).toHaveLength(2);
    expect(rows.map((r) => r.getAttribute('href')).sort()).toEqual(['/runs/run_a', '/runs/run_b']);
  });

  it('drops ended/dead runs — only live instances appear', async () => {
    mount([
      instance({ id: 'run_live', session_name: 'proj~live-20260706-1500', live: true }),
      instance({ id: 'run_dead', session_name: 'proj~dead-20260706-1501', live: false }),
    ]);
    await settle();
    const rows = railRows();
    expect(rows).toHaveLength(1);
    expect(rows[0]?.getAttribute('href')).toBe('/runs/run_live');
  });

  it('orders attention rows before working before the rest', async () => {
    mount([
      instance({ id: 'run_idle', session_name: 'proj~idle-20260706-1500', state: 'idle' }),
      instance({ id: 'run_working', session_name: 'proj~work-20260706-1501', state: 'working' }),
      instance({ id: 'run_attn', session_name: 'proj~attn-20260706-1502', state: 'needs_input' }),
    ]);
    await settle();
    expect(railRows().map((r) => r.getAttribute('href'))).toEqual([
      '/runs/run_attn',
      '/runs/run_working',
      '/runs/run_idle',
    ]);
  });

  it('marks the current run with aria-current="page" (and only that one)', async () => {
    mount(
      [
        instance({ id: 'run_a', session_name: 'proj~a-20260706-1500', state: 'needs_input' }),
        instance({ id: 'run_b', session_name: 'proj~b-20260706-1501' }),
      ],
      '/runs/run_b',
    );
    await settle();
    const current = container.querySelectorAll('a.rail-row[aria-current="page"]');
    expect(current).toHaveLength(1);
    expect(current[0]?.getAttribute('href')).toBe('/runs/run_b');
  });

  it('puts no Stop control in rail rows', async () => {
    mount([instance({ id: 'run_a', session_name: 'proj~a-20260706-1500', state: 'working' })]);
    await settle();
    const row = railRows()[0]!;
    expect(row.querySelector('button')).toBeNull();
  });

  it("carries the run's state in the row's accessible name (the dot is color-only)", async () => {
    mount([
      instance({ id: 'run_attn', session_name: 'proj~a-20260706-1500', state: 'needs_input' }),
      instance({ id: 'run_plain', session_name: 'proj~b-20260706-1501', state: 'idle' }),
    ]);
    await settle();
    const byHref = (href: string) => railRows().find((r) => r.getAttribute('href') === href)!;
    expect(byHref('/runs/run_attn').getAttribute('aria-label')).toBe(
      'a · 15:00 — proj — needs input',
    );
    // No live badge (idle) → no trailing state word.
    expect(byHref('/runs/run_plain').getAttribute('aria-label')).toBe('b · 15:01 — proj');
  });

  it('shows a user-set title alone in the row and its accessible name (issue #111)', async () => {
    mount([
      instance({ id: 'run_titled', session_name: 'proj~a-20260706-1500', title: 'Fix the tests' }),
    ]);
    await settle();
    const row = railRows()[0]!;
    expect(row.querySelector('.rail-row-title')?.textContent).toBe('Fix the tests');
    expect(row.textContent).not.toContain('15:00'); // the title ALONE, no raw label
    expect(row.getAttribute('aria-label')).toBe('Fix the tests — proj');
  });

  it('shows the AFK budget countdown on the secondary line, and only for AFK rows', async () => {
    const deadline = new Date(Date.now() + 30 * 60_000).toISOString();
    mount([
      instance({ id: 'run_afk', session_name: 'proj~afk-12', budget_deadline: deadline }),
      // A manual run never shows a budget, even with a (stray) deadline.
      instance({
        id: 'run_manual',
        session_name: 'proj~m-20260706-1500',
        budget_deadline: deadline,
      }),
    ]);
    await settle();
    const byHref = (href: string) => railRows().find((r) => r.getAttribute('href') === href)!;
    expect(byHref('/runs/run_afk').textContent).toMatch(/left/);
    expect(byHref('/runs/run_manual').textContent).not.toMatch(/left/);
  });

  it('shows the "N behind" chip only for a row with commits_behind > 0, and folds it into the accessible name (issue #149)', async () => {
    mount([
      instance({ id: 'run_behind', session_name: 'proj~a-20260706-1500', commits_behind: 2 }),
      instance({ id: 'run_current', session_name: 'proj~b-20260706-1501', commits_behind: 0 }),
      instance({ id: 'run_unset', session_name: 'proj~c-20260706-1502' }),
    ]);
    await settle();
    const byHref = (href: string) => railRows().find((r) => r.getAttribute('href') === href)!;

    const behindRow = byHref('/runs/run_behind');
    expect(behindRow.querySelector('.rail-behind-chip')?.textContent).toBe('2 behind');
    expect(behindRow.getAttribute('aria-label')).toBe('a · 15:00 — proj — 2 behind');

    expect(byHref('/runs/run_current').querySelector('.rail-behind-chip')).toBeNull();
    expect(byHref('/runs/run_unset').querySelector('.rail-behind-chip')).toBeNull();
  });
});
