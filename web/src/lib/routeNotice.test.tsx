// Route notices (issue #61): noticeState builds the router state a page hands
// to the next one; takeRouteNotice reads it back defensively; useRouteNotice
// shows each arriving notice once and clears it from the history entry.

import {
  MemoryRouter,
  Route,
  createMemoryHistory,
  useLocation,
  useNavigate,
} from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { noticeState, takeRouteNotice, useRouteNotice } from './routeNotice';

describe('noticeState / takeRouteNotice', () => {
  it('round-trips a message through router state', () => {
    expect(noticeState('Deleted coding-lab from lab')).toEqual({
      notice: 'Deleted coding-lab from lab',
    });
    expect(takeRouteNotice({ state: noticeState('Deleted coding-lab from lab') })).toBe(
      'Deleted coding-lab from lab',
    );
  });

  it('answers null for state that carries no usable notice', () => {
    expect(takeRouteNotice({ state: null })).toBeNull();
    expect(takeRouteNotice({ state: undefined })).toBeNull();
    expect(takeRouteNotice({})).toBeNull();
    expect(takeRouteNotice({ state: 'text' })).toBeNull();
    expect(takeRouteNotice({ state: { notice: 42 } })).toBeNull();
    expect(takeRouteNotice({ state: { notice: '   ' } })).toBeNull();
    expect(takeRouteNotice({ state: { other: 'x' } })).toBeNull();
  });
});

describe('useRouteNotice', () => {
  let dispose: (() => void) | undefined;
  let container: HTMLDivElement;
  afterEach(() => {
    dispose?.();
    dispose = undefined;
    container.remove();
  });

  const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

  it('reports each arriving notice once and clears it from the entry', async () => {
    const seen = vi.fn();
    const history = createMemoryHistory();
    history.set({ value: '/from' });

    function From() {
      const navigate = useNavigate();
      return (
        <button
          type="button"
          onClick={() => navigate('/to?tab=1', { state: noticeState('Saved 2 changes') })}
        >
          go
        </button>
      );
    }
    function To() {
      const location = useLocation();
      useRouteNotice(seen);
      return <p class="state">{JSON.stringify(location.state)}</p>;
    }

    container = document.createElement('div');
    document.body.appendChild(container);
    dispose = render(
      () => (
        <MemoryRouter history={history}>
          <Route path="/from" component={From} />
          <Route path="/to" component={To} />
        </MemoryRouter>
      ),
      container,
    );
    await flush();

    container.querySelector('button')?.click();
    for (let i = 0; i < 4; i += 1) await flush();

    expect(seen).toHaveBeenCalledTimes(1);
    expect(seen).toHaveBeenCalledWith('Saved 2 changes');
    // Cleared by a replace to the same URL (query kept): nothing left to re-show.
    expect(history.get()).toBe('/to?tab=1');
    expect(container.querySelector('.state')?.textContent).toBe('null');

    // Leaving and coming back without state shows nothing new.
    history.back();
    await flush();
    history.forward();
    for (let i = 0; i < 4; i += 1) await flush();
    expect(seen).toHaveBeenCalledTimes(1);
  });
});
