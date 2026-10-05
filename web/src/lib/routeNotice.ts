// Cross-route notices (issue #61): a page that navigates away after an action
// hands the destination a one-line confirmation through router state, e.g.
//
//   navigate('/repos', { state: noticeState('Deleted coding-lab from lab') });
//
// The destination reads it with takeRouteNotice(location) and shows it once in
// a toast. useRouteNotice wires both halves: it shows each arriving notice once
// and then replaces the history entry's state, so a reload or a Back/Forward
// onto that entry does not show the same notice again.

import { useLocation, useNavigate } from '@solidjs/router';
import { createEffect, on } from 'solid-js';

/** The router-state shape a notice travels in. */
export interface RouteNoticeState {
  notice: string;
}

/** Builds the router state that carries `message` to the next page. */
export function noticeState(message: string): RouteNoticeState {
  return { notice: message };
}

/**
 * Reads the notice from a router location's state: the message, or null when
 * the state carries none (absent, not an object, or an empty string).
 */
export function takeRouteNotice(location: { readonly state?: unknown }): string | null {
  const state = location.state;
  if (state === null || typeof state !== 'object') return null;
  const notice = (state as { notice?: unknown }).notice;
  return typeof notice === 'string' && notice.trim() !== '' ? notice : null;
}

/**
 * Shows every notice that arrives with a navigation exactly once: calls
 * `onNotice` with the message, then clears it from the current history entry
 * (a replace to the same URL with empty state). Call it in a component body
 * under the router.
 */
export function useRouteNotice(onNotice: (message: string) => void): void {
  const location = useLocation();
  const navigate = useNavigate();
  createEffect(
    on(
      () => location.state,
      () => {
        const message = takeRouteNotice(location);
        if (message === null) return;
        onNotice(message);
        navigate(`${location.pathname}${location.search}${location.hash}`, {
          replace: true,
          scroll: false,
          state: null,
        });
      },
    ),
  );
}
