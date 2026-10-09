// More (/more) — the phone's fourth tab (issue #76; reference
// docs/reference/tab-bar-mockup.html, "More" and "More, logged out"). Four
// rows, each with one line of state so the page doubles as a glance at the
// setup: Credentials (the default agent provider's login), Tokens (how many),
// Settings (today's category index, one level below) and Install app (only
// when the PWA is installable — the same gate as the Settings row). Then the
// account: who is signed in, the SSE live dot and Log out, which moved here
// from the drawer the tab bar replaced.
//
// While the agent provider is logged out — the one setup fault that silently
// breaks every new run, and the only thing the More tab's dot reports — an
// alert with Reconnect sits above the rows and the Credentials row carries a
// `logged out` chip instead of its hint. The alert has an icon and a bold
// title over its sentence (the mockup), which Banner's text-only message
// cannot carry, so it reuses the Repos page's "Needs you" problem item — the
// same icon · title · message · action row the mockup draws for both.
//
// From 1024px the rail holds all of this, so the route redirects to /settings
// (live, should the viewport grow while it is open). No version line: no API
// reports the lab version (it is only logged at startup), and issue #76 adds
// no API.

import { A, Navigate } from '@solidjs/router';
import { Show, createResource, createSignal, type JSX } from 'solid-js';
import { errorMessage, listTokens, logout } from '../api';
import { useAuth } from '../auth';
import Banner from '../components/Banner';
import Icon, { type IconName } from '../components/Icon';
import RequireAuth from '../components/RequireAuth';
import SectionHead from '../components/SectionHead';
import { useEvents } from '../events';
import { install } from '../lib/install';
import { createMediaQuery } from '../lib/media';
import { createProviderLogin } from '../lib/providerLogin';
import { resourceValue } from '../lib/resource';
import { settingsSummary } from './settings/categories';

/** The shell breakpoint (AppShell's DESKTOP_MIN_PX): the rail replaces More. */
const DESKTOP_QUERY = '(min-width: 1024px)';

/** The Settings row's hint: the section names, in the row's own fixed order. */
const SETTINGS_HINT = settingsSummary();

export default function More() {
  const desktop = createMediaQuery(DESKTOP_QUERY);
  return (
    <RequireAuth>
      <Show when={!desktop()} fallback={<Navigate href="/settings" />}>
        <MoreView />
      </Show>
    </RequireAuth>
  );
}

function MoreView() {
  const { auth, refresh } = useAuth();
  const events = useEvents();
  const login = createProviderLogin();
  const providerName = () => login.provider()?.display_name ?? 'The agent provider';

  // ApiToken has no expiry or revocation: every listed token is active.
  const [tokens] = createResource(() => listTokens());
  const tokensHint = (): string | undefined => {
    const list = resourceValue(tokens);
    if (list === undefined) return undefined;
    return list.length === 0 ? 'None' : `${list.length} active`;
  };

  // Same flow as the rail's Log out: end the session, then refresh the auth
  // state so the route guard bounces to /login.
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const doLogout = async () => {
    setBusy(true);
    setError(null);
    try {
      await logout();
      await refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <main class="page more-page">
      <SectionHead title="More" />
      <Show when={login.loggedOut()}>
        <div class="needs-you-item problem more-alert" role="alert">
          <Icon name="circle-alert" size={18} class="needs-you-icon" />
          <span class="needs-you-text">
            <strong class="more-alert-title">{providerName()} is logged out</strong>
            <span class="needs-you-message">
              New runs will fail at the login wall until you reconnect.
            </span>
          </span>
          <A href="/credentials" class="needs-you-action">
            Reconnect
          </A>
        </div>
      </Show>

      <nav class="settings-index more-list" aria-label="More">
        <MoreRow href="/credentials" icon="key" title="Credentials">
          <Show
            when={login.loggedOut()}
            fallback={
              <Show when={login.loggedIn()}>
                <span class="more-row-hint">{providerName()} logged in</span>
              </Show>
            }
          >
            <span class="chip status-error">logged out</span>
          </Show>
        </MoreRow>
        <MoreRow href="/tokens" icon="ticket" title="Tokens">
          <Show when={tokensHint()}>{(hint) => <span class="more-row-hint">{hint()}</span>}</Show>
        </MoreRow>
        <MoreRow href="/settings" icon="settings" title="Settings">
          <span class="more-row-hint">{SETTINGS_HINT}</span>
        </MoreRow>
        {/* Reopens the install sheet AppShell mounts (issue #142) — the same
            gate and action as the Install app card in Settings. */}
        <Show when={install.settingsRowVisible()}>
          <button
            type="button"
            class="settings-index-row more-row-button"
            onClick={() => install.openFromSettings()}
          >
            <Icon name="arrow-down-to-line" size={20} class="settings-index-icon" />
            <span class="settings-index-text">
              <span class="settings-index-title">Install app</span>
            </span>
            <span class="more-row-hint">Add to Home Screen</span>
            <Icon name="chevron-right" size={18} class="settings-index-chevron" />
          </button>
        </Show>
      </nav>

      <Banner message={error()} onDismiss={() => setError(null)} />
      <div class="more-account">
        <span class="more-account-who">
          <span class="more-account-name">{auth()?.username}</span>
          <span class="more-account-live">
            {/* The rail's live dot, verbatim: the status lives on the dot;
                the visible word only repeats it for sighted users. */}
            <span
              classList={{ 'live-dot': true, on: events.connected() }}
              role="status"
              aria-label={events.connected() ? 'Live' : 'Reconnecting'}
              title={events.connected() ? 'Live' : 'Reconnecting…'}
            />
            <span aria-hidden="true">{events.connected() ? 'Live' : 'Reconnecting…'}</span>
          </span>
        </span>
        <button type="button" class="more-logout" onClick={() => void doLogout()} disabled={busy()}>
          {busy() ? 'Logging out…' : 'Log out'}
        </button>
      </div>
    </main>
  );
}

/** One link row: the settings index row's look, with a one-line state hint. */
function MoreRow(props: {
  href: string;
  icon: IconName;
  title: string;
  /** The row's state: a hint or a chip, or nothing while unknown. */
  children?: JSX.Element;
}) {
  return (
    <A href={props.href} class="settings-index-row">
      <Icon name={props.icon} size={20} class="settings-index-icon" />
      <span class="settings-index-text">
        <span class="settings-index-title">{props.title}</span>
      </span>
      {props.children}
      <Icon name="chevron-right" size={18} class="settings-index-chevron" />
    </A>
  );
}
