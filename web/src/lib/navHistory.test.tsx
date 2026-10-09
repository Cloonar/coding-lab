// navHistory contract (issue #76): Back is possible only while the current
// history entry sits ABOVE the app's first entry. The router's `_depth` stamp
// on that first entry is `history.length - 1`, which is > 0 whenever the tab
// had history before lab loaded — so a cold deep link must still read "no
// in-app entry" until the app itself pushes one. The last case drives the
// real @solidjs/router against jsdom's history to pin the stamp behaviour this
// module relies on.

import { Route, Router, useNavigate, type Navigator } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { canGoBack, initNavHistory, resetNavHistory } from './navHistory';

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

/** Resolves on the next popstate (jsdom fires history.go asynchronously). */
const popped = () =>
  new Promise<void>((resolve) =>
    window.addEventListener('popstate', () => resolve(), { once: true }),
  );

beforeEach(() => {
  resetNavHistory();
});

afterEach(() => {
  resetNavHistory();
  vi.restoreAllMocks();
});

describe('navHistory', () => {
  it('cannot go back before initNavHistory has recorded a floor', () => {
    window.history.replaceState({ _depth: 5 }, '');
    expect(canGoBack()).toBe(false);
  });

  it('treats the boot entry as the floor, even at a depth above zero', () => {
    // A deep link in a tab with three earlier (foreign) entries.
    window.history.replaceState({ _depth: 3 }, '');
    initNavHistory();
    expect(canGoBack()).toBe(false);

    // The app pushes an entry: now there is an in-app one below it.
    window.history.replaceState({ _depth: 4 }, '');
    expect(canGoBack()).toBe(true);

    // Back on the boot entry again (or a replace onto it): the floor.
    window.history.replaceState({ _depth: 3 }, '');
    expect(canGoBack()).toBe(false);
  });

  it('records the floor once — later calls do not move it', () => {
    window.history.replaceState({ _depth: 1 }, '');
    initNavHistory();
    window.history.replaceState({ _depth: 2 }, '');
    initNavHistory();
    expect(canGoBack()).toBe(true);
  });

  it('falls back to history.length - 1 when the boot entry has no stamp', () => {
    window.history.replaceState(null, '');
    initNavHistory();
    window.history.replaceState({ _depth: window.history.length - 1 }, '');
    expect(canGoBack()).toBe(false);
    window.history.replaceState({ _depth: window.history.length }, '');
    expect(canGoBack()).toBe(true);
  });

  it('reads an entry without a stamp as "cannot go back"', () => {
    window.history.replaceState({ _depth: 0 }, '');
    initNavHistory();
    window.history.replaceState({ other: true }, '');
    expect(canGoBack()).toBe(false);
  });

  it('follows the real router: a cold deep link, an in-app push, then back', async () => {
    // The router scrolls to the top on a push; jsdom has no scrollTo.
    vi.spyOn(window, 'scrollTo').mockImplementation(() => {});
    // Foreign history before lab: the tab already holds earlier entries.
    window.history.pushState(null, '', '/elsewhere-1');
    window.history.pushState(null, '', '/elsewhere-2');
    window.history.replaceState(null, '', '/runs/run_1');
    // What the router module does at load (saveCurrentDepth) for a fresh
    // document: stamp the boot entry with history.length - 1. Its module was
    // evaluated before this test reshaped the history, so redo it here.
    window.history.replaceState({ _depth: window.history.length - 1 }, '');

    let navigate: Navigator | undefined;
    const Capture = () => {
      navigate = useNavigate();
      return null;
    };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const dispose = render(
      () => (
        <Router root={(props) => props.children}>
          <Route path="*" component={Capture} />
        </Router>
      ),
      container,
    );
    try {
      await flush();
      // The boot entry's stamp is > 0: depth alone would claim a back entry.
      const bootStamp = (window.history.state as { _depth?: number } | null)?._depth;
      expect(bootStamp).toBeGreaterThan(0);
      initNavHistory();
      expect(canGoBack()).toBe(false);

      // A replace (e.g. a <Navigate> redirect) keeps the depth: still the floor.
      navigate!('/runs/run_1?x=1', { replace: true });
      await flush();
      expect(canGoBack()).toBe(false);

      navigate!('/');
      await flush();
      expect(canGoBack()).toBe(true);

      const back = popped();
      navigate!(-1);
      await back;
      await flush();
      expect(window.location.pathname).toBe('/runs/run_1');
      expect(canGoBack()).toBe(false);
    } finally {
      dispose();
      container.remove();
    }
  });
});
