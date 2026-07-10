// The app's spine (issue #41, Phase 1): brand, a New-run entry, the ACTIVE list
// of live instances (attention-first — see lib/railOrder), the footer section
// nav, and the signed-in footer with Log out and the SSE live dot. AppShell
// renders it as a persistent rail (desktop) or an overlay drawer (mobile). The
// instances arrive as a prop because AppShell owns the single listInstances
// resource shared with the mobile hamburger's attention badge. Rows carry NO
// destructive action — Stop lives in the chat header and on the Repos page.

import { A } from '@solidjs/router';
import { For, Show, createSignal, onCleanup } from 'solid-js';
import { errorMessage, logout, type Instance } from '../api';
import { useAuth } from '../auth';
import { useEvents } from '../events';
import { budgetRemaining, parseAFKLabel } from '../lib/afk';
import { stateBadge } from '../lib/conversation';
import { runDisplayTitle, sessionLabel } from '../lib/instanceLabel';
import { orderRail } from '../lib/railOrder';
import ErrorBanner from './ErrorBanner';
import Icon from './Icon';

export default function SideNav(props: {
  instances: Instance[];
  /** Tap on any nav target — the mobile drawer closes on it. */
  onNavigate?: () => void;
  /** Desktop collapse chevron. */
  onCollapse?: () => void;
}) {
  const { auth, refresh } = useAuth();
  const events = useEvents();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  // ACTIVE = live instances only; ended/dead runs drop out (History is the
  // archive). orderRail sorts attention → working → rest, newest first.
  const rows = () => orderRail(props.instances.filter((instance) => instance.live));

  // Display-only countdown tick for AFK budget hints (the spec's one sanctioned
  // interval, inherited from the old InstanceList rows).
  const [now, setNow] = createSignal(Date.now());
  const ticker = setInterval(() => setNow(Date.now()), 30_000);
  onCleanup(() => clearInterval(ticker));

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
        <A href="/" class="brand plain" onClick={() => props.onNavigate?.()}>
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

      <A href="/" end class="rail-newrun" onClick={() => props.onNavigate?.()}>
        + New run
      </A>

      <p class="rail-section-label">Active</p>
      <Show when={rows().length > 0} fallback={<p class="rail-empty">No live runs.</p>}>
        <ul class="rail-active">
          <For each={rows()}>
            {(instance) => (
              <RailRow instance={instance} now={now()} onNavigate={props.onNavigate} />
            )}
          </For>
        </ul>
      </Show>

      <span class="rail-spacer" />

      <div class="rail-nav">
        <A
          href="/repos"
          activeClass="active"
          class="rail-nav-link"
          onClick={() => props.onNavigate?.()}
        >
          <Icon name="folder" class="rail-nav-icon" />
          <span>Repos</span>
        </A>
        <A
          href="/history"
          activeClass="active"
          class="rail-nav-link"
          onClick={() => props.onNavigate?.()}
        >
          <Icon name="history" class="rail-nav-icon" />
          <span>History</span>
        </A>
        <A
          href="/credentials"
          activeClass="active"
          class="rail-nav-link"
          onClick={() => props.onNavigate?.()}
        >
          <Icon name="key" class="rail-nav-icon" />
          <span>Credentials</span>
        </A>
        <A
          href="/tokens"
          activeClass="active"
          class="rail-nav-link"
          onClick={() => props.onNavigate?.()}
        >
          <Icon name="ticket" class="rail-nav-icon" />
          <span>Tokens</span>
        </A>
        <A
          href="/settings"
          activeClass="active"
          class="rail-nav-link"
          onClick={() => props.onNavigate?.()}
        >
          <Icon name="settings" class="rail-nav-icon" />
          <span>Settings</span>
        </A>
      </div>

      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
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

function RailRow(props: { instance: Instance; now: number; onNavigate?: () => void }) {
  const state = () => props.instance.state;
  // A user-set title wins over the parsed label (issue #111 rename overlay).
  const title = () => runDisplayTitle(props.instance);
  const badge = () => stateBadge(state());
  // AFK rows keep their budget countdown (re-homed from the old InstanceList):
  // the deadline is the run's only time pressure and lives nowhere else.
  const afk = () => parseAFKLabel(sessionLabel(props.instance.session_name));
  const budget = () =>
    afk() === null ? null : budgetRemaining(props.instance.budget_deadline, props.now);
  // The dot is color-only, so the link's accessible name carries the state
  // word a screen reader would otherwise miss.
  const ariaLabel = () => {
    const base = `${title()} — ${props.instance.repo_name}`;
    const b = badge();
    return b === null ? base : `${base} — ${b.label}`;
  };
  return (
    <li>
      <A
        href={`/runs/${props.instance.id}`}
        end
        class="rail-row"
        aria-label={ariaLabel()}
        onClick={() => props.onNavigate?.()}
      >
        <span
          class="rail-row-dot"
          classList={{
            working: state() === 'working',
            'needs-input': state() === 'needs_input',
            question: state() === 'question',
          }}
          title={badge()?.title}
        />
        <span class="rail-row-body">
          <span class="rail-row-title">{title()}</span>
          <span class="rail-row-repo">
            {props.instance.repo_name}
            <Show when={budget()}>
              {(b) => (
                <span
                  classList={{ 'rail-row-budget': true, over: b() === 'over budget' }}
                  title="Time left on this AFK run's budget"
                >
                  {' · '}
                  {b()}
                </span>
              )}
            </Show>
          </span>
        </span>
      </A>
    </li>
  );
}
