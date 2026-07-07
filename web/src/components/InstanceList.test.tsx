// Conversational-state badges on instance rows (issue #7): the chat tailer's
// per-instance state renders as a chip — working / needs input / question —
// while the idle / ended / empty states carry no badge (the row's other
// affordances already say "not running" or show the outcome).

import { MemoryRouter, Route } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import type { Instance, Provider } from '../api';
import InstanceList from './InstanceList';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

const claudeFallback = {
  url: 'https://claude.ai/code',
  title: "Opens the claude.ai session picker — the exact deep link wasn't captured",
};

// claude-code has a web surface (fallback_open); codex is link-less (none).
const PROVIDERS: Provider[] = [
  { id: 'claude-code', models: [], efforts: [], fallback_open: claudeFallback },
  { id: 'codex', models: [], efforts: [] },
];

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

function mountList(instances: Instance[], providers: Provider[] = PROVIDERS): void {
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
              providers={providers}
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

/** The "Open ↗" open-link anchor of the first row, if present. */
function openLink(): HTMLAnchorElement | null {
  return (
    Array.from(container.querySelectorAll('a')).find((a) => a.textContent?.includes('Open ↗')) ??
    null
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

describe('InstanceList open affordance (ADR-0017)', () => {
  it('links the exact captured deep link', () => {
    mountList([instance({ deep_link_url: 'https://claude.ai/code/session_1' })]);
    expect(openLink()?.getAttribute('href')).toBe('https://claude.ai/code/session_1');
  });

  it('falls back to the provider generic web link on a missed capture', () => {
    mountList([instance({ deep_link_url: null, connecting: false })]);
    const link = openLink();
    expect(link?.getAttribute('href')).toBe(claudeFallback.url);
    expect(link?.getAttribute('title')).toBe(claudeFallback.title);
  });

  it('pulses while capture is in flight', () => {
    mountList([instance({ deep_link_url: null, connecting: true })]);
    expect(openLink()).toBeNull();
    expect(container.querySelector('.chip.connecting.pulse')?.textContent).toContain('connecting');
  });

  it('offers a copyable tmux-attach for a link-less provider', () => {
    mountList([instance({ provider: 'codex', deep_link_url: null, connecting: false })]);
    expect(openLink()).toBeNull();
    const attach = container.querySelector('button.attach-copy');
    expect(attach?.textContent).toContain('Copy attach');
    expect(attach?.getAttribute('title')).toContain('tmux attach -t proj~dom-20260706-1500');
  });

  it('renders no open affordance on a miss while the providers list is loading', () => {
    // providers=undefined (resource in flight): a missed-capture web-provider row
    // must show neither a link nor tmux-attach — no misleading flash (rendered
    // directly because a defaulted param cannot forward undefined).
    container = document.createElement('div');
    document.body.appendChild(container);
    dispose = render(
      () => (
        <MemoryRouter>
          <Route
            path="*"
            component={() => (
              <InstanceList
                instances={[instance({ deep_link_url: null, connecting: false })]}
                providers={undefined}
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
    expect(openLink()).toBeNull();
    expect(container.querySelector('button.attach-copy')).toBeNull();
    expect(container.querySelector('.chip.connecting')).toBeNull();
  });
});
