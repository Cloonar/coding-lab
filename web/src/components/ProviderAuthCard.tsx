// Provider auth card (Credentials page): machine-level login status for one
// agent provider, rendered from its auth-flow descriptor (issue #51 decision
// 7) — the card knows flows, never providers:
//   - oauth-code: the full login flow — start-login yields a tappable
//     authorize link plus a code paste field; the pasted code answers 202 and
//     completion lands via the provider.auth.changed SSE event (the card
//     refetches on it, so the status flips live) — plus logout.
//   - oauth-redirect: status + the descriptor's operator instructions (the
//     login happens in a browser against the lab host) + logout.
//   - api-key: status + a note that the vault credential is injected at spawn
//     (schema-only for now — no form).
//   - external: status only; the account is managed outside lab.
// Logout sits behind a confirm dialog that names the running-instance count
// and warns that AFK auto stays on. Starting login again doubles as
// cancel/retry (v0). All copy derives from the provider's display_name.

import { Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import {
  errorMessage,
  providerAuthStatus,
  providerLoginCode,
  providerLoginStart,
  providerLogout,
  type Provider,
  type ProviderAuthStatus,
} from '../api';
import { useEvents } from '../events';
import ErrorBanner from './ErrorBanner';

export default function ProviderAuthCard(props: { provider: Provider; activeRuns?: number }) {
  const events = useEvents();
  const [status, { refetch, mutate }] = createResource(
    () => props.provider.id,
    (id) => providerAuthStatus(id),
  );
  onCleanup(
    // eslint-disable-next-line solid/reactivity -- the handler re-reads props.provider.id fresh on every SSE event
    events.subscribe('provider.auth.changed', (event) => {
      // The payload names the provider; refetch on a match (or on an old
      // payload without one, defensively).
      const id = event['provider'];
      if (typeof id !== 'string' || id === props.provider.id) void refetch();
    }),
  );

  const [error, setError] = createSignal<string | null>(null);
  const [busy, setBusy] = createSignal(false);
  const [oauthUrl, setOauthUrl] = createSignal<string | null>(null);
  const [code, setCode] = createSignal('');
  const [waiting, setWaiting] = createSignal(false);
  const [confirmingLogout, setConfirmingLogout] = createSignal(false);

  const name = () => props.provider.display_name;
  const kind = () => props.provider.auth.kind;
  // Logout exists only for the flows lab drives itself; api-key material lives
  // in the vault and external accounts are managed outside lab entirely.
  const canLogout = () => kind() === 'oauth-code' || kind() === 'oauth-redirect';
  const loggedIn = () => status()?.logged_in === true;

  // Login landed (SSE-driven refetch flipped the status): clear the flow.
  const resetFlowIfDone = (s: ProviderAuthStatus | undefined) => {
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
      mutate(resetFlowIfDone(await providerAuthStatus(props.provider.id, true)));
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
      const res = await providerLoginStart(props.provider.id);
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
      await providerLoginCode(props.provider.id, trimmed);
      setWaiting(true); // 202 accepted — provider.auth.changed finishes the job
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
      // flips even before the provider.auth.changed refetch lands.
      mutate(await providerLogout(props.provider.id));
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
        <span class="card-title">{name()}</span>
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
          <Show when={kind() === 'api-key'}>
            <ApiKeyNote name={name()} />
          </Show>
          <Show when={canLogout()}>
            <Show
              when={confirmingLogout()}
              fallback={
                <div class="card-actions">
                  <button
                    type="button"
                    class="danger provider-logout"
                    onClick={() => setConfirmingLogout(true)}
                    disabled={busy()}
                  >
                    Log out
                  </button>
                </div>
              }
            >
              <LogoutConfirm
                providerName={name()}
                activeRuns={props.activeRuns ?? 0}
                busy={busy()}
                onCancel={() => setConfirmingLogout(false)}
                onConfirm={() => void doLogout()}
              />
            </Show>
          </Show>
        </Match>
        <Match when={status() !== undefined}>
          <Switch>
            {/* oauth-code: start-login → authorize link + code paste form. */}
            <Match when={kind() === 'oauth-code'}>
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
                      {busy() ? 'Starting…' : `Log in to ${name()}`}
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
                        name="provider-login-code"
                        class="mono"
                        value={code()}
                        onInput={(e) => setCode(e.currentTarget.value)}
                        autocomplete="off"
                        spellcheck={false}
                      />
                    </label>
                    <div class="card-actions">
                      <button
                        type="submit"
                        class="primary"
                        disabled={busy() || code().trim() === ''}
                      >
                        Submit code
                      </button>
                      <button type="button" onClick={() => void startLogin()} disabled={busy()}>
                        Restart login
                      </button>
                      <Show when={waiting()}>
                        <span class="muted pulse">waiting for {name()}…</span>
                      </Show>
                    </div>
                  </form>
                </div>
              </Show>
            </Match>
            {/* oauth-redirect: the login runs in a browser against the lab
                host; the descriptor's instructions tell the operator how. */}
            <Match when={kind() === 'oauth-redirect'}>
              <p class="card-sub auth-instructions">
                {props.provider.auth.instructions ||
                  `Log in from a browser that can reach the lab host; the status updates once ${name()} records the login.`}
              </p>
            </Match>
            <Match when={kind() === 'api-key'}>
              <ApiKeyNote name={name()} />
            </Match>
            {/* external: status only — the account is managed outside lab. */}
          </Switch>
        </Match>
      </Switch>
    </section>
  );
}

/** api-key flows carry no login form here — spawn injects the vault secret. */
function ApiKeyNote(props: { name: string }) {
  return (
    <p class="muted card-sub auth-apikey-note">
      {props.name} authenticates with a stored credential injected at spawn — there is nothing to
      log in here.
    </p>
  );
}

/**
 * Logout confirm gate: machine-wide logout is bare by decision — it does NOT
 * stop running instances, so the dialog names how many keep running and warns
 * that AFK auto stays on (expect failed-spawn churn until a fresh login). A
 * plain confirm, not a typed-confirm: logout is reversible by logging back in.
 */
function LogoutConfirm(props: {
  providerName: string;
  activeRuns: number;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const runs = () => props.activeRuns;
  const plural = () => (runs() === 1 ? '' : 's');
  return (
    <div class="logout-confirm" role="alertdialog" aria-label={`Log out of ${props.providerName}`}>
      <p class="logout-warning">
        This drops the machine's {props.providerName} account so you can log in a fresh one.{' '}
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
          class="danger provider-logout-confirm"
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
