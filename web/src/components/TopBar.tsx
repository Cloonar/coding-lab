// App header shared by all authenticated pages: brand, section nav
// (Repos · Credentials · Settings placeholder), SSE live dot, logout.

import { A } from '@solidjs/router';
import { createSignal } from 'solid-js';
import { errorMessage, logout } from '../api';
import { useAuth } from '../auth';
import { useEvents } from '../events';
import ErrorBanner from './ErrorBanner';

export default function TopBar() {
  const { auth, refresh } = useAuth();
  const events = useEvents();
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
    <>
      <header class="topbar">
        <A href="/" class="brand plain">
          lab<span class="brand-dot">.</span>
        </A>
        <nav class="nav" aria-label="Sections">
          <A href="/" end activeClass="active" class="nav-link">
            Repos
          </A>
          <A href="/credentials" activeClass="active" class="nav-link">
            Credentials
          </A>
          <span class="nav-link disabled" title="Coming in a later milestone" aria-disabled="true">
            Settings
          </span>
        </nav>
        <span class="spacer" />
        <span
          classList={{ 'live-dot': true, on: events.connected() }}
          role="status"
          aria-label={events.connected() ? 'Live' : 'Reconnecting'}
          title={events.connected() ? 'Live' : 'Reconnecting…'}
        />
        <span class="muted user-name">{auth()?.username}</span>
        <button type="button" onClick={() => void doLogout()} disabled={busy()}>
          {busy() ? 'Logging out…' : 'Log out'}
        </button>
      </header>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
    </>
  );
}
