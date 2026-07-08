// Claude auth card (dashboard top): machine-level login status, force
// refresh, the OAuth login flow, and a machine-wide logout for switching
// accounts (issue #46) — Login yields a tappable authorize link plus a code
// paste field; the pasted code answers 202 and completion lands via the
// claude.auth.changed SSE event (the card refetches on it, so the status flips
// live). Logout sits behind a confirm dialog that names the running-instance
// count and warns that AFK auto stays on. Starting login again doubles as
// cancel/retry (v0).

import { Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import {
  claudeAuthStatus,
  claudeLoginCode,
  claudeLoginStart,
  claudeLogout,
  errorMessage,
  type ClaudeAuthStatus,
} from '../api';
import { useEvents } from '../events';
import ErrorBanner from './ErrorBanner';

export default function ClaudeAuthCard(props: { activeRuns?: number }) {
  const events = useEvents();
  const [status, { refetch, mutate }] = createResource(() => claudeAuthStatus());
  onCleanup(
    events.subscribe('claude.auth.changed', () => {
      void refetch();
    }),
  );

  const [error, setError] = createSignal<string | null>(null);
  const [busy, setBusy] = createSignal(false);
  const [oauthUrl, setOauthUrl] = createSignal<string | null>(null);
  const [code, setCode] = createSignal('');
  const [waiting, setWaiting] = createSignal(false);
  const [confirmingLogout, setConfirmingLogout] = createSignal(false);

  const loggedIn = () => status()?.logged_in === true;

  // Login landed (SSE-driven refetch flipped the status): clear the flow.
  const resetFlowIfDone = (s: ClaudeAuthStatus | undefined) => {
    if (s?.logged_in) {
      setOauthUrl(null);
      setCode('');
      setWaiting(false);
    }
    return s;
  };

  const forceRefresh = async () => {
    setBusy(true);
    setError(null);
    try {
      mutate(resetFlowIfDone(await claudeAuthStatus(true)));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const startLogin = async () => {
    setBusy(true);
    setError(null);
    setWaiting(false);
    try {
      const res = await claudeLoginStart();
      setOauthUrl(res.oauth_url);
      setCode('');
    } catch (err) {
      // 409 = already logged in — resync instead of surfacing an error.
      setOauthUrl(null);
      setError(errorMessage(err));
      void refetch();
    } finally {
      setBusy(false);
    }
  };

  const submitCode = async (event: SubmitEvent) => {
    event.preventDefault();
    const trimmed = code().trim();
    if (trimmed === '') return;
    setBusy(true);
    setError(null);
    try {
      await claudeLoginCode(trimmed);
      setWaiting(true); // 202 accepted — claude.auth.changed finishes the job
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const doLogout = async () => {
    setBusy(true);
    setError(null);
    try {
      // 200 echoes the now-logged-out status; adopt it directly so the card
      // flips even before the claude.auth.changed refetch lands.
      mutate(await claudeLogout());
      setConfirmingLogout(false);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section class="card auth-status-card">
      <div class="card-head">
        <span class="card-title">Claude</span>
        <span class="spacer" />
        <Switch>
          <Match when={status.loading && status() === undefined}>
            <span class="muted">checking…</span>
          </Match>
          <Match when={loggedIn()}>
            <span class="chip in-use">logged in</span>
          </Match>
          <Match when={status() !== undefined}>
            <span class="chip status-error">logged out</span>
          </Match>
        </Switch>
        <button
          type="button"
          onClick={() => void forceRefresh()}
          disabled={busy()}
          title="Bypass the 30s status cache"
        >
          Refresh
        </button>
      </div>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Switch>
        <Match when={status.error !== undefined}>
          <p class="muted card-sub">{errorMessage(status.error)}</p>
        </Match>
        <Match when={loggedIn()}>
          <p class="muted card-sub">
            Logged in as <span class="mono">{status()!.email}</span>
            <Show when={status()!.method !== ''}> via {status()!.method}</Show>
          </p>
          <Show
            when={confirmingLogout()}
            fallback={
              <div class="card-actions">
                <button
                  type="button"
                  class="danger claude-logout"
                  onClick={() => setConfirmingLogout(true)}
                  disabled={busy()}
                >
                  Log out
                </button>
              </div>
            }
          >
            <LogoutConfirm
              activeRuns={props.activeRuns ?? 0}
              busy={busy()}
              onCancel={() => setConfirmingLogout(false)}
              onConfirm={() => void doLogout()}
            />
          </Show>
        </Match>
        <Match when={status() !== undefined}>
          <Show
            when={oauthUrl()}
            fallback={
              <div class="card-actions">
                <button
                  type="button"
                  class="primary"
                  onClick={() => void startLogin()}
                  disabled={busy()}
                >
                  {busy() ? 'Starting…' : 'Log in to Claude'}
                </button>
              </div>
            }
          >
            <div class="login-flow">
              <p class="card-sub">
                1. Open the authorize page and approve access:
                <br />
                <a href={oauthUrl()!} target="_blank" rel="noreferrer" class="oauth-link">
                  Open authorize page ↗
                </a>
              </p>
              <form onSubmit={(e) => void submitCode(e)}>
                <label class="field">
                  <span>2. Paste the code from the authorize page</span>
                  <input
                    name="claude-login-code"
                    class="mono"
                    value={code()}
                    onInput={(e) => setCode(e.currentTarget.value)}
                    autocomplete="off"
                    spellcheck={false}
                  />
                </label>
                <div class="card-actions">
                  <button type="submit" class="primary" disabled={busy() || code().trim() === ''}>
                    Submit code
                  </button>
                  <button type="button" onClick={() => void startLogin()} disabled={busy()}>
                    Restart login
                  </button>
                  <Show when={waiting()}>
                    <span class="muted pulse">waiting for Claude…</span>
                  </Show>
                </div>
              </form>
            </div>
          </Show>
        </Match>
      </Switch>
    </section>
  );
}

/**
 * Logout confirm gate: machine-wide logout is bare by decision — it does NOT
 * stop running instances, so the dialog names how many keep running and warns
 * that AFK auto stays on (expect failed-spawn churn until a fresh login). A
 * plain confirm, not a typed-confirm: logout is reversible by logging back in.
 */
function LogoutConfirm(props: {
  activeRuns: number;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const runs = () => props.activeRuns;
  const plural = () => (runs() === 1 ? '' : 's');
  return (
    <div class="logout-confirm" role="alertdialog" aria-label="Log out of Claude">
      <p class="logout-warning">
        This drops the machine's Claude account so you can log in a fresh one.{' '}
        <Show when={runs() > 0} fallback={<>No instances are running right now.</>}>
          <span class="logout-runs">
            {runs()} running instance{plural()}
          </span>{' '}
          keep working on the current token until it refreshes, then fail.
        </Show>{' '}
        AFK auto stays on — expect failed-spawn churn until you log in fresh.
      </p>
      <div class="card-actions">
        <button
          type="button"
          class="danger claude-logout-confirm"
          disabled={props.busy}
          onClick={() => props.onConfirm()}
        >
          {props.busy ? 'Logging out…' : 'Log out'}
        </button>
        <button type="button" onClick={() => props.onCancel()} disabled={props.busy}>
          Cancel
        </button>
      </div>
    </div>
  );
}
