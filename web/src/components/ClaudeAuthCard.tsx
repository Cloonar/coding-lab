// Claude auth card (dashboard top): machine-level login status, force
// refresh, and the OAuth login flow — Login yields a tappable authorize link
// plus a code paste field; the pasted code answers 202 and completion lands
// via the claude.auth.changed SSE event (the card refetches on it, so the
// status flips live). Starting login again doubles as cancel/retry (v0).

import { Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import {
  claudeAuthStatus,
  claudeLoginCode,
  claudeLoginStart,
  errorMessage,
  type ClaudeAuthStatus,
} from '../api';
import { useEvents } from '../events';
import ErrorBanner from './ErrorBanner';

export default function ClaudeAuthCard() {
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
