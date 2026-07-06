// Root layout: provides the auth resource, wires the global 401 handler and
// gates children on the initial auth-state fetch.

import { Match, Switch, onCleanup, type ParentProps } from 'solid-js';
import { setUnauthorizedHandler } from './api';
import { AuthProvider, useAuth } from './auth';

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
      <Match when={auth.state === 'ready' || auth.state === 'refreshing'}>{props.children}</Match>
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
