// The app's spine (issue #41, Phase 1; reshaped by issue #76): brand, a
// `+ New run` entry to the composer at /new, the grouped run list (RunList in
// its 'rail' variant — Needs you / Working / Idle with the attention count),
// the section nav (Runs, Repos, Credentials, Tokens, Settings), and the
// signed-in footer with Log out and the SSE live dot. AppShell renders it as
// the persistent desktop rail (>=1024px; the phone has the tab bar instead).
// The instances arrive as a prop because AppShell owns the single
// listInstances resource (lib/shellInstances). History has no rail link any
// more: it is the Runs page's Ended side, so the Runs link stays lit on
// /history too. Rows carry NO destructive action — Stop lives in the chat
// header and on the Repos page.

import { A, useLocation } from '@solidjs/router';
import { createSignal } from 'solid-js';
import { errorMessage, logout, type Instance } from '../api';
import { useAuth } from '../auth';
import { useEvents } from '../events';
import Banner from './Banner';
import Icon from './Icon';
import RunList from './RunList';

export default function SideNav(props: {
  instances: Instance[];
  /** Desktop collapse chevron. */
  onCollapse?: () => void;
}) {
  const { auth, refresh } = useAuth();
  const events = useEvents();
  const location = useLocation();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  // The Runs link is lit on both sides of the Runs page: Live (/) and Ended
  // (/history). The router's own active match can't say that — `end` stops at
  // `/`, and without it `/` would match every path — so the classes are set
  // here and override the router's.
  const runsLit = () => location.pathname === '/' || location.pathname === '/history';

  const doLogout = async () => {
    setBusy(true);
    setError(null);
    try {
      await logout();
      await refresh(); // the route guard bounces to /login
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <nav class="rail" aria-label="Main">
      <div class="rail-brand">
        <A href="/" class="brand plain">
          lab<span class="brand-dot">.</span>
        </A>
        <button
          type="button"
          class="icon-btn rail-collapse"
          aria-label="Collapse sidebar"
          title="Collapse sidebar (Ctrl/Cmd+B)"
          onClick={() => props.onCollapse?.()}
        >
          <Icon name="chevrons-left" />
        </button>
      </div>

      <A href="/new" end class="rail-newrun">
        + New run
      </A>

      <RunList instances={props.instances} variant="rail" />

      <span class="rail-spacer" />

      <div class="rail-nav">
        <A
          href="/"
          end
          class="rail-nav-link"
          classList={{ active: runsLit(), inactive: !runsLit() }}
        >
          <Icon name="inbox" class="rail-nav-icon" />
          <span>Runs</span>
        </A>
        <A href="/repos" activeClass="active" class="rail-nav-link">
          <Icon name="folder" class="rail-nav-icon" />
          <span>Repos</span>
        </A>
        <A href="/credentials" activeClass="active" class="rail-nav-link">
          <Icon name="key" class="rail-nav-icon" />
          <span>Credentials</span>
        </A>
        <A href="/tokens" activeClass="active" class="rail-nav-link">
          <Icon name="ticket" class="rail-nav-icon" />
          <span>Tokens</span>
        </A>
        <A href="/settings" activeClass="active" class="rail-nav-link">
          <Icon name="settings" class="rail-nav-icon" />
          <span>Settings</span>
        </A>
      </div>

      <Banner message={error()} onDismiss={() => setError(null)} />
      <div class="rail-foot">
        <span class="muted rail-user">{auth()?.username}</span>
        <button type="button" class="rail-logout" onClick={() => void doLogout()} disabled={busy()}>
          {busy() ? 'Logging out…' : 'Log out'}
        </button>
        <span
          classList={{ 'live-dot': true, on: events.connected() }}
          role="status"
          aria-label={events.connected() ? 'Live' : 'Reconnecting'}
          title={events.connected() ? 'Live' : 'Reconnecting…'}
        />
      </div>
    </nav>
  );
}
