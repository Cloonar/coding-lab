// Dashboard: app header, SSE live-connection dot, empty state (M1) and logout.

import { Navigate } from '@solidjs/router';
import { Match, Switch, createSignal, onCleanup } from 'solid-js';
import { errorMessage, logout } from '../api';
import { useAuth } from '../auth';
import ErrorBanner from '../components/ErrorBanner';
import { connectEvents } from '../sse';

export default function Dashboard() {
  const { auth } = useAuth();
  return (
    <Switch>
      <Match when={auth()?.setup_required}>
        <Navigate href="/setup" />
      </Match>
      <Match when={auth() && !auth()!.authenticated}>
        <Navigate href="/login" />
      </Match>
      <Match when={auth()?.authenticated}>
        <DashboardView />
      </Match>
    </Switch>
  );
}

function DashboardView() {
  const { auth, refresh } = useAuth();
  const events = connectEvents();
  onCleanup(() => events.close());

  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const doLogout = async () => {
    setBusy(true);
    setError(null);
    try {
      await logout();
      await refresh(); // guard redirects to /login
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <main class="page">
      <header class="topbar">
        <span class="brand">
          lab<span class="brand-dot">.</span>
        </span>
        <span
          classList={{ 'live-dot': true, on: events.connected() }}
          role="status"
          aria-label={events.connected() ? 'Live' : 'Reconnecting'}
          title={events.connected() ? 'Live' : 'Reconnecting…'}
        />
        <span class="spacer" />
        <span class="muted">{auth()?.username}</span>
        <button type="button" onClick={() => void doLogout()} disabled={busy()}>
          {busy() ? 'Logging out…' : 'Log out'}
        </button>
      </header>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <p class="empty">No repositories yet — add one from Settings (M2)</p>
    </main>
  );
}
