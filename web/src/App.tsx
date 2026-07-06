// Root layout: provides the auth resource, wires the global 401 handler,
// gates children on the initial auth-state fetch and mounts the shared SSE
// connection while a session exists.

import { Match, Show, Switch, onCleanup, type ParentProps } from 'solid-js';
import { setUnauthorizedHandler } from './api';
import { AuthProvider, useAuth } from './auth';
import { EventsProvider } from './events';

function Shell(props: ParentProps) {
  const { auth, refresh } = useAuth();

  // Any 401 from the API refreshes the auth state; the route guards then
  // bounce to /login. The api client never fires this handler for /auth/state
  // itself, so the refresh cannot loop even behind an auth gateway that 401s
  // every request (lab's own /auth/state never 401s, but a gateway's can).
  setUnauthorizedHandler(() => {
    void refresh();
  });
  onCleanup(() => setUnauthorizedHandler(null));

  return (
    <Switch
      fallback={
        <main class="page">
          <p class="center-note">Loading…</p>
        </main>
      }
    >
      <Match when={auth.state === 'errored'}>
        <main class="page">
          <div class="banner error" role="alert">
            <span class="banner-text">Network error — is lab still running?</span>
          </div>
          <button type="button" onClick={() => void refresh()}>
            Retry
          </button>
        </main>
      </Match>
      <Match when={auth.state === 'ready' || auth.state === 'refreshing'}>
        {/* One SSE connection for the whole authenticated app; it survives
            route changes and closes on logout when the Show flips. */}
        <Show when={auth()?.authenticated} fallback={props.children}>
          <EventsProvider>{props.children}</EventsProvider>
        </Show>
      </Match>
    </Switch>
  );
}

export default function App(props: ParentProps) {
  return (
    <AuthProvider>
      <Shell>{props.children}</Shell>
    </AuthProvider>
  );
}
