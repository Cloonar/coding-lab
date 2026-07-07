// Conversational-state badges on instance rows (issue #7): the chat tailer's
// per-instance state renders as a chip — working / needs input / question —
// while the idle / ended / empty states carry no badge (the row's other
// affordances already say "not running" or show the outcome).

import { MemoryRouter, Route } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import type { Instance } from '../api';
import InstanceList from './InstanceList';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function instance(overrides: Partial<Instance>): Instance {
  return {
    id: 'run_1',
    repo_id: 'repo_1',
    repo_name: 'proj',
    kind: 'manual',
    provider: 'claude-code',
    issue_number: null,
    branch: 'lab/x',
    worktree_path: '/wt/x',
    session_name: 'proj~dom-20260706-1500',
    model: 'opus[1m]',
    effort: 'max',
    deep_link_url: 'https://claude.ai/code/session_1',
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

function mountList(instances: Instance[]): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(
    () => (
      <MemoryRouter>
        <Route
          path="*"
          component={() => (
            <InstanceList
              instances={instances}
              onStopped={() => {}}
              onChanged={() => {}}
              onError={() => {}}
            />
          )}
        />
      </MemoryRouter>
    ),
    container,
  );
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('InstanceList conversational-state badges', () => {
  it('renders working / needs input / question chips from the tailer state', () => {
    mountList([
      instance({ id: 'run_w', session_name: 'proj~w-20260706-1500', state: 'working' }),
      instance({ id: 'run_n', session_name: 'proj~n-20260706-1501', state: 'needs_input' }),
      instance({ id: 'run_q', session_name: 'proj~q-20260706-1502', state: 'question' }),
    ]);

    const chips = Array.from(container.querySelectorAll('.chip.convo'));
    expect(chips.map((c) => c.textContent?.trim())).toEqual(['working', 'needs input', 'question']);
    expect(container.querySelector('.chip.convo.working')?.getAttribute('title')).toBe(
      'The agent is working',
    );
    expect(container.querySelector('.chip.convo.needs-input')?.getAttribute('title')).toBe(
      'The agent is waiting for you',
    );
    expect(container.querySelector('.chip.convo.question')?.getAttribute('title')).toBe(
      'The agent is asking a question',
    );
  });

  it('carries no badge for the idle / ended / empty states', () => {
    mountList([
      instance({ state: 'idle' }),
      instance({ id: 'run_2', session_name: 'proj~e-20260706-1503', state: 'ended' }),
      instance({ id: 'run_3', session_name: 'proj~x-20260706-1504', state: '' }),
    ]);

    expect(container.querySelectorAll('.instance-row')).toHaveLength(3);
    expect(container.querySelector('.chip.convo')).toBeNull();
  });
});
