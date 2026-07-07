// App header shared by all authenticated pages: brand, section nav
// (Repos · Credentials · Runs · Tokens · Settings), SSE live dot, logout.
// Phone-first (<640px): brand · live dot · hamburger; the section links,
// username and Log out live in a full-width dropdown menu over a scrim that
// closes on outside tap, Escape, and route navigation. At >=640px the inline
// nav renders (username always visible) and the hamburger/menu are hidden.

import { A, useLocation } from '@solidjs/router';
import { createEffect, createSignal, on, onCleanup, Show } from 'solid-js';
import { errorMessage, logout } from '../api';
import { useAuth } from '../auth';
import { useEvents } from '../events';
import ErrorBanner from './ErrorBanner';
import Icon from './Icon';

export default function TopBar() {
  const { auth, refresh } = useAuth();
  const events = useEvents();
  const location = useLocation();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [open, setOpen] = createSignal(false);

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

  // Close the mobile menu on route navigation. The pathname accessor registers
  // the dependency; the initial (immediate) run is a harmless no-op.
  createEffect(
    on(
      () => location.pathname,
      () => setOpen(false),
    ),
  );

  // Close on Escape while the menu is open.
  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Escape' && open()) setOpen(false);
  };
  window.addEventListener('keydown', onKeyDown);
  onCleanup(() => window.removeEventListener('keydown', onKeyDown));

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
          <A href="/runs" activeClass="active" class="nav-link">
            Runs
          </A>
          <A href="/tokens" activeClass="active" class="nav-link">
            Tokens
          </A>
          <A href="/settings" activeClass="active" class="nav-link">
            Settings
          </A>
        </nav>
        <span class="spacer" />
        <span
          classList={{ 'live-dot': true, on: events.connected() }}
          role="status"
          aria-label={events.connected() ? 'Live' : 'Reconnecting'}
          title={events.connected() ? 'Live' : 'Reconnecting…'}
        />
        <span class="muted user-name">{auth()?.username}</span>
        <button
          type="button"
          class="desktop-logout"
          onClick={() => void doLogout()}
          disabled={busy()}
        >
          {busy() ? 'Logging out…' : 'Log out'}
        </button>
        <button
          type="button"
          class="icon-btn nav-toggle"
          aria-label="Menu"
          title="Menu"
          aria-expanded={open()}
          aria-controls="nav-menu"
          onClick={() => setOpen((v) => !v)}
        >
          <Icon name="menu" />
        </button>

        <Show when={open()}>
          <div class="nav-menu" id="nav-menu">
            {/* Close on any link tap — the route-change effect misses a tap on
                the already-active link (no navigation fires). */}
            <nav class="nav-menu-links" aria-label="Sections" onClick={() => setOpen(false)}>
              <A href="/" end activeClass="active" class="nav-menu-link">
                <Icon name="folder" class="nav-menu-icon" />
                <span>Repos</span>
              </A>
              <A href="/credentials" activeClass="active" class="nav-menu-link">
                <Icon name="key" class="nav-menu-icon" />
                <span>Credentials</span>
              </A>
              <A href="/runs" activeClass="active" class="nav-menu-link">
                <Icon name="play" class="nav-menu-icon" />
                <span>Runs</span>
              </A>
              <A href="/tokens" activeClass="active" class="nav-menu-link">
                <Icon name="ticket" class="nav-menu-icon" />
                <span>Tokens</span>
              </A>
              <A href="/settings" activeClass="active" class="nav-menu-link">
                <Icon name="settings" class="nav-menu-icon" />
                <span>Settings</span>
              </A>
            </nav>
            <div class="nav-menu-foot">
              <span class="muted nav-menu-user">{auth()?.username}</span>
              <button
                type="button"
                class="nav-menu-logout"
                onClick={() => void doLogout()}
                disabled={busy()}
              >
                <Icon name="log-out" class="nav-menu-icon" />
                <span>{busy() ? 'Logging out…' : 'Log out'}</span>
              </button>
            </div>
          </div>
        </Show>
      </header>

      <Show when={open()}>
        <div class="nav-scrim" onClick={() => setOpen(false)} aria-hidden="true" />
      </Show>

      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
    </>
  );
}
