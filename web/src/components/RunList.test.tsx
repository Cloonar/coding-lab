// RunList (issue #76): live instances render in Needs you / Working / Idle
// groups (empty groups omitted, ended rows dropped), each label carrying its
// count with the Needs you count accent-styled; every row is one <A> to
// /runs/:id whose accessible name carries title, repo, state and behind; the
// page variant shows the state phrase and the age, the rail variant drops
// both; AFK rows show the budget; no row carries a button; empty reads
// "No live runs.".

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import type { Instance } from '../api';
import RunList, { type RunListVariant } from './RunList';

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
    started_at: new Date(Date.now() - 4.5 * 60_000).toISOString(),
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

function mount(instances: Instance[], variant: RunListVariant = 'page'): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/' });
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route path="*" component={() => <RunList instances={instances} variant={variant} />} />
      </MemoryRouter>
    ),
    container,
  );
}

const rows = () => Array.from(container.querySelectorAll<HTMLAnchorElement>('a.runlist-row'));
const byHref = (href: string) => rows().find((r) => r.getAttribute('href') === href)!;
const labels = () =>
  Array.from(container.querySelectorAll('.runlist-label')).map((l) => l.textContent);

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('RunList', () => {
  it('groups live runs Needs you → Working → Idle with each group count', () => {
    mount([
      instance({ id: 'idle', session_name: 'proj~i-20260706-1500', state: 'idle' }),
      instance({ id: 'work', session_name: 'proj~w-20260706-1501', state: 'working' }),
      instance({ id: 'q', session_name: 'proj~q-20260706-1502', state: 'question' }),
      instance({ id: 'n', session_name: 'proj~n-20260706-1503', state: 'needs_input' }),
      instance({ id: 'dead', session_name: 'proj~d-20260706-1504', live: false }),
    ]);
    expect(labels()).toEqual(['Needs you2', 'Working1', 'Idle1']);
    expect(rows().map((r) => r.getAttribute('href'))).toEqual([
      '/runs/q',
      '/runs/n',
      '/runs/work',
      '/runs/idle',
    ]);
    // Only the Needs you count is accent-styled.
    const counts = Array.from(container.querySelectorAll('.runlist-count'));
    expect(counts.map((c) => c.classList.contains('attn'))).toEqual([true, false, false]);
  });

  it('omits empty groups', () => {
    mount([instance({ id: 'work', state: 'working' })]);
    expect(labels()).toEqual(['Working1']);
  });

  it('reads "No live runs." with nothing live', () => {
    mount([instance({ id: 'dead', live: false })]);
    expect(rows()).toHaveLength(0);
    expect(container.textContent).toContain('No live runs.');
  });

  it('shows repo · state phrase and the age on a page row, with no button', () => {
    mount([instance({ id: 'n', state: 'needs_input' })]);
    const row = byHref('/runs/n');
    expect(row.querySelector('.runlist-sub')?.textContent).toBe('proj · waiting for you');
    expect(row.querySelector('.runlist-state')?.classList.contains('attn')).toBe(true);
    expect(row.querySelector('.runlist-age')?.textContent).toBe('4m');
    expect(row.querySelector('button')).toBeNull();
  });

  it('drops the state phrase and the age in the rail variant', () => {
    mount([instance({ id: 'n', state: 'needs_input' })], 'rail');
    const row = byHref('/runs/n');
    expect(row.querySelector('.runlist-sub')?.textContent).toBe('proj');
    expect(row.querySelector('.runlist-age')).toBeNull();
    expect(container.querySelector('.runlist-rail')).not.toBeNull();
  });

  it('carries title, repo, state and behind in the accessible name', () => {
    mount([
      instance({ id: 'n', state: 'needs_input', commits_behind: 3 }),
      instance({ id: 'i', session_name: 'proj~b-20260706-1501', state: 'idle' }),
    ]);
    expect(byHref('/runs/n').getAttribute('aria-label')).toBe(
      'dom · 15:00 — proj — needs input — 3 behind',
    );
    expect(byHref('/runs/n').querySelector('.runlist-behind')?.textContent).toBe('3 behind');
    expect(byHref('/runs/i').getAttribute('aria-label')).toBe('b · 15:01 — proj');
    expect(byHref('/runs/i').querySelector('.runlist-behind')).toBeNull();
  });

  it('shows the AFK budget only on AFK rows', () => {
    const deadline = new Date(Date.now() + 30 * 60_000).toISOString();
    mount([
      instance({ id: 'afk', session_name: 'proj~afk-12', budget_deadline: deadline }),
      instance({ id: 'm', session_name: 'proj~m-20260706-1500', budget_deadline: deadline }),
    ]);
    expect(byHref('/runs/afk').textContent).toMatch(/left/);
    expect(byHref('/runs/m').textContent).not.toMatch(/left/);
  });
});
