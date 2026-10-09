// AppShell (issues #41, #175, #76). The shell owns the single listInstances
// resource and hands it down through ShellInstancesContext; these tests pin
// that run.messages.changed patches one row in place (no refetch, untouched
// rows keep their identity) while run.changed refetches the list; the bottom
// TabBar's route rules (hidden on the Chat and the Schedule editor, shown on
// the schedules section), its Runs badge and More dot as fed by the shell;
// the `(N) lab` document title and the app badge; and that the retired mobile
// chrome (top strip, hamburger, drawer, scrim) no longer renders.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { For, Show, useContext } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Instance, Run } from '../api';
import { AuthProvider } from '../auth';
import { EventsProvider } from '../events';
import { ShellInstancesContext } from '../lib/shellInstances';
import AppShell from './AppShell';

// FakeEventSource + emit (crib: RunChat.test.tsx) — a registry of live instances
// and a per-type addEventListener/emit so tests can fire SSE events at the
// component the way sse.ts's connectEvents() actually consumes them.
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

function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 4; i += 1) await flush();
}

/** A page under the shell that reads its instances through the context, so
 *  the tests see exactly what the Runs page / rail would. AppShell renders its
 *  children bare until auth resolves (outside the provider), hence the guard. */
function Probe() {
  const shell = useContext(ShellInstancesContext);
  return (
    <Show when={shell}>
      {(ctx) => (
        <ul id="probe" data-loaded={String(ctx().loaded())}>
          <For each={ctx().all()}>
            {(instance) => (
              <li
                data-id={instance.id}
                data-state={instance.state}
                data-detail={instance.state_detail ?? ''}
              >
                {instance.id}
              </li>
            )}
          </For>
        </ul>
      )}
    </Show>
  );
}

async function mount(path = '/'): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: path });
  dispose = render(
    () => (
      <AuthProvider>
        <EventsProvider>
          <MemoryRouter history={history}>
            <Route
              path="*"
              component={() => (
                <AppShell>
                  <Probe />
                </AppShell>
              )}
            />
          </MemoryRouter>
        </EventsProvider>
      </AuthProvider>
    ),
    container,
  );
  await settle();
}

// --- instances fixture (issue #175) -----------------------------------------

const RUN_ID = 'run_1';
const OTHER_RUN_ID = 'run_2';

/** Crib of RunChat.test.tsx's baseRun(), parameterized by id since this file
 *  needs two distinct rows. */
function baseRun(id: string): Run {
  return {
    id,
    repo_id: 'repo_1',
    kind: 'manual',
    provider: 'claude-code',
    issue_number: null,
    pull_number: null,
    branch: `lab/${id}`,
    worktree_path: `/wt/${id}`,
    session_name: `proj~dom-20260706-1500-${id}`,
    title: null,
    model: 'opus[1m]',
    effort: 'max',
    remote: false,
    deep_link_url: null,
    started_at: '2026-07-06T15:00:00.000Z',
    budget_deadline: null,
    ended_at: null,
    outcome: 'active',
    failure_reason: null,
  };
}

/** Instance = Run + repo_name/live/connecting/state (see api.ts). */
function baseInstance(id: string, overrides: Partial<Instance> = {}): Instance {
  return {
    ...baseRun(id),
    repo_name: 'proj',
    live: true,
    connecting: false,
    state: 'working',
    ...overrides,
  };
}

let instancesOnServer: Instance[];
let instancesFetchCount = 0;

function emitMessagesChanged(payload: Record<string, unknown>): void {
  FakeEventSource.instances[0]?.emit('run.messages.changed', {
    type: 'run.messages.changed',
    ...payload,
  });
}

function emitRunChanged(payload: Record<string, unknown> = {}): void {
  FakeEventSource.instances[0]?.emit('run.changed', { type: 'run.changed', ...payload });
}

let providerLoggedIn = true;

const probeRow = (id: string) => document.querySelector<HTMLElement>(`#probe li[data-id="${id}"]`);
const tabBar = () => document.querySelector<HTMLElement>('nav[aria-label="Tabs"]');
const runsTab = () => tabBar()?.querySelector<HTMLAnchorElement>('a[href="/"]') ?? null;
const runsBadge = () => runsTab()?.querySelector('.tab-badge') ?? null;
const moreDot = () => tabBar()?.querySelector('a[href="/more"] .tab-dot') ?? null;
const shell = () => document.querySelector<HTMLElement>('.shell');

beforeEach(() => {
  instancesFetchCount = 0;
  providerLoggedIn = true;
  instancesOnServer = [
    baseInstance(RUN_ID, { state: 'working' }),
    baseInstance(OTHER_RUN_ID, { state: 'idle' }),
  ];
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal('scrollTo', vi.fn());
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/auth/state'))
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated: true, username: 'op' }),
        );
      if (url === '/api/v1/instances') {
        instancesFetchCount += 1;
        return Promise.resolve(jsonResponse(200, { instances: instancesOnServer }));
      }
      if (url === '/api/v1/providers')
        return Promise.resolve(jsonResponse(200, { providers: [{ id: 'claude-code' }] }));
      if (url === '/api/v1/settings') return Promise.resolve(jsonResponse(200, {}));
      if (url === '/api/v1/providers/claude-code/auth/status')
        return Promise.resolve(jsonResponse(200, { logged_in: providerLoggedIn }));
      return Promise.resolve(jsonResponse(200, []));
    }),
  );
});

afterEach(() => {
  FakeEventSource.instances = [];
  dispose?.();
  dispose = undefined;
  container?.remove();
  vi.unstubAllGlobals();
  document.title = 'lab';
});

describe('AppShell instances: run.messages.changed patches in place (issue #175)', () => {
  it('provides the one list to the tree and marks it loaded', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1);
    expect(document.querySelector('#probe')?.getAttribute('data-loaded')).toBe('true');
    expect(probeRow(RUN_ID)?.dataset.state).toBe('working');
    expect(probeRow(OTHER_RUN_ID)?.dataset.state).toBe('idle');
  });

  it('a known runID + state patches that row without refetching the list', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1); // one fetch on mount
    expect(runsBadge()).toBeNull(); // nothing needs attention yet
    const untouched = probeRow(OTHER_RUN_ID);

    emitMessagesChanged({ runID: RUN_ID, state: 'question' });
    await settle();

    // No refetch — the row was patched in place.
    expect(instancesFetchCount).toBe(1);
    expect(probeRow(RUN_ID)?.dataset.state).toBe('question');
    // The other row kept its object identity, so <For> kept its node.
    expect(probeRow(OTHER_RUN_ID)).toBe(untouched);
    // The attention count flips: live && state is needs_input|question.
    expect(runsBadge()?.textContent).toBe('1');
  });

  it('patches state_detail in place, also on a same-state tick, and clears it when omitted', async () => {
    await mount();

    emitMessagesChanged({ runID: RUN_ID, state: 'needs_input', state_detail: 'input needed' });
    await settle();
    expect(probeRow(RUN_ID)?.dataset.state).toBe('needs_input');
    expect(probeRow(RUN_ID)?.dataset.detail).toBe('input needed');

    emitMessagesChanged({
      runID: RUN_ID,
      state: 'needs_input',
      state_detail: 'permission request',
    });
    await settle();
    expect(probeRow(RUN_ID)?.dataset.detail).toBe('permission request');

    emitMessagesChanged({ runID: RUN_ID, state: 'working' });
    await settle();
    expect(probeRow(RUN_ID)?.dataset.state).toBe('working');
    expect(probeRow(RUN_ID)?.dataset.detail).toBe('');
    expect(instancesFetchCount).toBe(1); // never a refetch
  });

  it('run.changed refetches the whole instances list exactly once', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1);

    emitRunChanged({ repoID: 'repo_1' });
    await settle();

    expect(instancesFetchCount).toBe(2);
  });

  it('an unknown runID is ignored: no fetch, no crash, list unchanged', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1);

    emitMessagesChanged({ runID: 'run_ghost', state: 'question' });
    await settle();

    expect(instancesFetchCount).toBe(1);
    expect(runsBadge()).toBeNull();
    expect(probeRow(RUN_ID)?.dataset.state).toBe('working');
  });

  it('a missing state field is ignored: no fetch, no crash, list unchanged', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1);

    emitMessagesChanged({ runID: RUN_ID }); // no `state` key at all
    await settle();

    expect(instancesFetchCount).toBe(1);
    expect(runsBadge()).toBeNull();
    expect(probeRow(RUN_ID)?.dataset.state).toBe('working');
  });
});

describe('AppShell tab bar (issue #76)', () => {
  it.each(['/', '/history', '/new', '/repos/x/issues', '/repos/x/settings/schedules', '/more'])(
    'renders the tab bar on %s',
    async (path) => {
      await mount(path);
      expect(tabBar()).toBeTruthy();
      expect(shell()?.classList.contains('has-tabbar')).toBe(true);
    },
  );

  it.each(['/runs/run_1', '/repos/x/settings/schedules/new', '/repos/x/settings/schedules/sch_1'])(
    'hides the tab bar on %s',
    async (path) => {
      await mount(path);
      expect(tabBar()).toBeNull();
      expect(shell()?.classList.contains('has-tabbar')).toBe(false);
    },
  );

  it('badges Runs with the attention count, hidden at zero', async () => {
    await mount();
    expect(runsBadge()).toBeNull();
    expect(runsTab()?.getAttribute('aria-label')).toBe('Runs');

    emitMessagesChanged({ runID: RUN_ID, state: 'needs_input' });
    emitMessagesChanged({ runID: OTHER_RUN_ID, state: 'question' });
    await settle();
    expect(runsBadge()?.textContent).toBe('2');
    expect(runsTab()?.getAttribute('aria-label')).toBe('Runs, 2 need you');

    emitMessagesChanged({ runID: RUN_ID, state: 'idle' });
    emitMessagesChanged({ runID: OTHER_RUN_ID, state: 'working' });
    await settle();
    expect(runsBadge()).toBeNull();
  });

  it('does not count ended runs that were left in an attention state', async () => {
    instancesOnServer = [baseInstance(RUN_ID, { live: false, state: 'question' })];
    await mount();
    expect(runsBadge()).toBeNull();
  });

  it('dots More only when the agent provider is logged out', async () => {
    await mount();
    expect(moreDot()).toBeNull();
    dispose?.();
    container.remove();

    providerLoggedIn = false;
    await mount();
    expect(moreDot()).toBeTruthy();
  });

  it('renders no top strip, hamburger, drawer or scrim', async () => {
    await mount();
    expect(document.querySelector('.shell-topstrip')).toBeNull();
    expect(document.querySelector('.strip-hamburger')).toBeNull();
    expect(document.querySelector('[aria-label="Open menu"]')).toBeNull();
    expect(document.querySelector('.attn-badge')).toBeNull();
    expect(document.querySelector('.shell-scrim')).toBeNull();
    expect(shell()?.classList.contains('drawer-open')).toBe(false);
    expect(shell()?.classList.contains('drawer-dragging')).toBe(false);
  });
});

describe('AppShell attention outside the page (issue #76)', () => {
  it('titles the document `(2) lab` while two runs need you, `lab` otherwise', async () => {
    await mount();
    expect(document.title).toBe('lab');

    emitMessagesChanged({ runID: RUN_ID, state: 'needs_input' });
    emitMessagesChanged({ runID: OTHER_RUN_ID, state: 'question' });
    await settle();
    expect(document.title).toBe('(2) lab');

    emitMessagesChanged({ runID: RUN_ID, state: 'working' });
    emitMessagesChanged({ runID: OTHER_RUN_ID, state: 'idle' });
    await settle();
    expect(document.title).toBe('lab');
  });

  it('restores the base title when the shell unmounts', async () => {
    instancesOnServer = [baseInstance(RUN_ID, { state: 'question' })];
    await mount();
    expect(document.title).toBe('(1) lab');
    dispose?.();
    dispose = undefined;
    expect(document.title).toBe('lab');
  });

  it('mirrors the count on the app badge where the Badging API exists', async () => {
    const setAppBadge = vi.fn(() => Promise.resolve());
    const clearAppBadge = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'setAppBadge', { value: setAppBadge, configurable: true });
    Object.defineProperty(navigator, 'clearAppBadge', { value: clearAppBadge, configurable: true });
    try {
      await mount();
      expect(clearAppBadge).toHaveBeenCalled();

      emitMessagesChanged({ runID: RUN_ID, state: 'question' });
      await settle();
      expect(setAppBadge).toHaveBeenLastCalledWith(1);
    } finally {
      delete (navigator as { setAppBadge?: unknown }).setAppBadge;
      delete (navigator as { clearAppBadge?: unknown }).clearAppBadge;
    }
  });

  it('swallows a rejected app-badge call', async () => {
    const setAppBadge = vi.fn(() => Promise.reject(new Error('NotAllowedError')));
    Object.defineProperty(navigator, 'setAppBadge', { value: setAppBadge, configurable: true });
    try {
      instancesOnServer = [baseInstance(RUN_ID, { state: 'question' })];
      await mount();
      expect(setAppBadge).toHaveBeenCalledWith(1);
      // An unhandled rejection would fail the run; the title still updates.
      expect(document.title).toBe('(1) lab');
    } finally {
      delete (navigator as { setAppBadge?: unknown }).setAppBadge;
    }
  });
});
