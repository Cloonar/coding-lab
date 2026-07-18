// AppShell drawer-gesture binding (issue #140): the state machine is unit-
// tested in lib/drawerGesture.test.ts; these tests exercise the WINDOW-level
// listener binding, and in particular its recovery from leaked touch
// sequences. Touch events fire at the element the finger went DOWN on, so
// when an SSE-driven re-render detaches that node mid-touch the
// touchend/touchcancel dispatch on the detached node and never bubble to
// window. Before the self-heal in onTouchStart, that pinned activeTouchId
// forever (gesture permanently dead until the PWA was killed) and — when a
// drag was claimed — left the full-viewport scrim mounted, eating every tap.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Instance, Run } from '../api';
import { AuthProvider } from '../auth';
import { EventsProvider } from '../events';
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

// --- Touch fabrication (jsdom has no TouchEvent; ToolPanel.test precedent) --

type TouchInit = { identifier: number; clientX: number; clientY: number };

function touchList(touches: TouchInit[]): TouchList {
  const list: Record<string | number, unknown> = {
    length: touches.length,
    item: (i: number) => touches[i] ?? null,
  };
  touches.forEach((t, i) => {
    list[i] = t;
  });
  return list as unknown as TouchList;
}

/** A fake single-finger touch event; `id` distinguishes sequences (iOS-style
 *  fresh ids vs Android-style reuse are both exercised below). */
function touchEvent(
  type: 'touchstart' | 'touchmove' | 'touchend' | 'touchcancel',
  touch: { x: number; y: number; id?: number } | null,
  o: { t?: number } = {},
): Event {
  const e = new Event(type, { bubbles: true, cancelable: true });
  const touches = touch ? [{ identifier: touch.id ?? 1, clientX: touch.x, clientY: touch.y }] : [];
  const list = touchList(touches);
  Object.defineProperty(e, 'touches', { value: list, configurable: true });
  Object.defineProperty(e, 'changedTouches', { value: list, configurable: true });
  if (o.t !== undefined) Object.defineProperty(e, 'timeStamp', { value: o.t, configurable: true });
  return e;
}

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 4; i += 1) await flush();
}

const shell = () => document.querySelector<HTMLElement>('.shell');
const scrim = () => document.querySelector<HTMLElement>('.shell-scrim');
const drawerOpen = () => shell()?.classList.contains('drawer-open') ?? false;
const dragging = () => shell()?.classList.contains('drawer-dragging') ?? false;

async function mount(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/' });
  dispose = render(
    () => (
      <AuthProvider>
        <EventsProvider>
          <MemoryRouter history={history}>
            <Route
              path="*"
              component={() => (
                <AppShell>
                  <div id="content">
                    <button id="victim" type="button">
                      a row that re-renders away
                    </button>
                  </div>
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
  // Mobile viewport + a measurable drawer (jsdom lays out at 0).
  const rail = document.querySelector<HTMLElement>('.shell-rail');
  expect(rail).toBeTruthy();
  Object.defineProperty(rail!, 'offsetWidth', { value: 260, configurable: true });
}

/** Swipe rightward from `el`, far enough to claim + commit-open on release. */
function swipeOpen(el: EventTarget, id: number, t0 = 0): void {
  el.dispatchEvent(touchEvent('touchstart', { x: 10, y: 200, id }, { t: t0 }));
  el.dispatchEvent(touchEvent('touchmove', { x: 80, y: 202, id }, { t: t0 + 40 }));
  el.dispatchEvent(touchEvent('touchmove', { x: 200, y: 204, id }, { t: t0 + 80 }));
  el.dispatchEvent(touchEvent('touchend', { x: 200, y: 204, id }, { t: t0 + 120 }));
}

/** Orphan a touch: finger down on #victim, node detached (an SSE re-render),
 *  so the touchend dispatches on the detached node and never reaches window.
 *  `claim` first drags past INTENT_PX so the leak pins a claimed drag. */
function leakTouch(id: number, claim: boolean): void {
  const victim = document.getElementById('victim')!;
  victim.dispatchEvent(touchEvent('touchstart', { x: 10, y: 300, id }, { t: 0 }));
  if (claim) victim.dispatchEvent(touchEvent('touchmove', { x: 60, y: 302, id }, { t: 40 }));
  victim.remove();
  victim.dispatchEvent(touchEvent('touchend', { x: 60, y: 302, id }, { t: 80 }));
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

const attnBadge = () => document.querySelector('.attn-badge');
const railDot = (id: string) =>
  document.querySelector(`a[href="/runs/${id}"] .rail-row-dot`);

beforeEach(() => {
  instancesFetchCount = 0;
  instancesOnServer = [
    baseInstance(RUN_ID, { state: 'working' }),
    baseInstance(OTHER_RUN_ID, { state: 'idle' }),
  ];
  vi.stubGlobal('EventSource', FakeEventSource);
  Object.defineProperty(window, 'innerWidth', { value: 390, configurable: true });
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
});

describe('AppShell drawer gesture binding', () => {
  it('a rightward swipe opens the drawer', async () => {
    await mount();
    swipeOpen(document.getElementById('victim')!, 1);
    await settle();
    expect(drawerOpen()).toBe(true);
  });

  it('recovers after a tracked touch is orphaned by its target detaching', async () => {
    await mount();
    leakTouch(7, false);

    // A later, ordinary swipe (fresh id — iOS never reuses them) must still
    // open the drawer: the stale id is detected as gone from e.touches and
    // settled at the next touchstart.
    swipeOpen(document.body, 8, 1000);
    await settle();
    expect(drawerOpen()).toBe(true);
  });

  it('recovers a claimed drag: the pinned scrim clears on the next touch', async () => {
    await mount();
    leakTouch(7, true);
    await settle();
    // The leak left a claimed drag pinned: scrim mounted over the viewport.
    expect(dragging()).toBe(true);
    expect(scrim()).toBeTruthy();

    // Any next touch self-heals at touchstart — the drag settles as cancelled…
    document.body.dispatchEvent(touchEvent('touchstart', { x: 200, y: 400, id: 9 }, { t: 1000 }));
    document.body.dispatchEvent(touchEvent('touchend', { x: 200, y: 400, id: 9 }, { t: 1040 }));
    await settle();
    expect(dragging()).toBe(false);
    expect(scrim()).toBeNull();
    expect(drawerOpen()).toBe(false);

    // …and the gesture works again.
    swipeOpen(document.body, 10, 2000);
    await settle();
    expect(drawerOpen()).toBe(true);
  });

  it('recovers when a reused identifier starts a new sequence (Android)', async () => {
    await mount();
    leakTouch(7, false);

    // Android hands the next finger the SAME identifier: seeing "our" id go
    // down again in changedTouches proves the old sequence ended unheard.
    swipeOpen(document.body, 7, 1000);
    await settle();
    expect(drawerOpen()).toBe(true);
  });

  it('backgrounding mid-drag (visibilitychange, no touchcancel) resets the drag', async () => {
    await mount();
    const victim = document.getElementById('victim')!;
    victim.dispatchEvent(touchEvent('touchstart', { x: 10, y: 300, id: 7 }, { t: 0 }));
    victim.dispatchEvent(touchEvent('touchmove', { x: 60, y: 302, id: 7 }, { t: 40 }));
    await settle();
    expect(dragging()).toBe(true);

    Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true });
    try {
      document.dispatchEvent(new Event('visibilitychange'));
    } finally {
      Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
    }
    await settle();
    expect(dragging()).toBe(false);
    expect(scrim()).toBeNull();

    swipeOpen(document.body, 8, 1000);
    await settle();
    expect(drawerOpen()).toBe(true);
  });
});

describe('AppShell instances: run.messages.changed patches in place (issue #175)', () => {
  it('a known runID + state patches that row without refetching the list', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1); // one fetch on mount
    expect(attnBadge()).toBeNull(); // nothing needs attention yet
    expect(railDot(RUN_ID)?.classList.contains('working')).toBe(true);

    emitMessagesChanged({ runID: RUN_ID, state: 'question' });
    await settle();

    // No refetch — the row was patched in place.
    expect(instancesFetchCount).toBe(1);
    // hasAttention flips: live && state is needs_input|question.
    expect(attnBadge()).toBeTruthy();
    // The row's own dot swaps class too.
    expect(railDot(RUN_ID)?.classList.contains('question')).toBe(true);
    expect(railDot(RUN_ID)?.classList.contains('working')).toBe(false);
  });

  it('run.changed refetches the whole instances list exactly once', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1);

    emitRunChanged({ repoID: 'repo_1' });
    await settle();

    expect(instancesFetchCount).toBe(2);
  });

  it('an unknown runID is ignored: no fetch, no crash, rail unchanged', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1);

    emitMessagesChanged({ runID: 'run_ghost', state: 'question' });
    await settle();

    expect(instancesFetchCount).toBe(1);
    expect(attnBadge()).toBeNull();
    expect(railDot(RUN_ID)?.classList.contains('working')).toBe(true);
  });

  it('a missing state field is ignored: no fetch, no crash, rail unchanged', async () => {
    await mount();
    expect(instancesFetchCount).toBe(1);

    emitMessagesChanged({ runID: RUN_ID }); // no `state` key at all
    await settle();

    expect(instancesFetchCount).toBe(1);
    expect(attnBadge()).toBeNull();
    expect(railDot(RUN_ID)?.classList.contains('working')).toBe(true);
  });
});
