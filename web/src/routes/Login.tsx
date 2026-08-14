// Login form with remember-me. Redirects to /setup while setup is required
// and to / once authenticated — or, when the OneCLI dashboard's login bounce
// sent the operator here, back out to the dashboard (issue #26).

import { Navigate, useSearchParams } from '@solidjs/router';
import { Match, Show, Switch, createSignal, onMount } from 'solid-js';
import { errorMessage, getOneCLIDashboard, login } from '../api';
import { useAuth } from '../auth';
import Banner from '../components/Banner';

/**
 * The fixed keyword port mode's proxy puts in `?next=` when it bounces an
 * unauthenticated browser navigation here (internal/httpapi/onecli_proxy.go).
 * It is a keyword and never a URL, which is the whole open-redirect defense.
 */
const DASHBOARD_NEXT = 'onecli-dashboard';

/**
 * The `path` half of the bounce, narrowed to something that can only ever
 * extend the dashboard's own origin: one leading `/`, and no second separator
 * behind it. `//evil.com` is a protocol-relative URL wearing a path's clothes,
 * and `/\evil.com` is the same thing to a browser, so both fall back to the
 * dashboard's root along with anything absent, absolute or repeated. The
 * origin itself always comes from the authenticated exposure endpoint, never
 * from the query string; oneCLIDashboardGate has the long version.
 */
export function safeDashboardPath(path: unknown): string {
  if (typeof path !== 'string') return '/';
  if (path[0] !== '/' || path[1] === '/' || path[1] === '\\') return '/';
  return path;
}

export default function Login() {
  const { auth } = useAuth();
  return (
    <Switch>
      <Match when={auth()?.setup_required}>
        <Navigate href="/setup" />
      </Match>
      <Match when={auth()?.authenticated}>
        <AuthenticatedRedirect />
      </Match>
      <Match when={auth()}>
        <LoginForm />
      </Match>
    </Switch>
  );
}

/**
 * Where an authenticated operator goes: / as ever, or out to the OneCLI
 * dashboard when the bounce sent them here. It hangs off the authenticated
 * branch rather than off the form's submit so it covers both arrivals — the
 * login that just happened, and a bounce that landed on a session another tab
 * had already made valid.
 */
function AuthenticatedRedirect() {
  const [params] = useSearchParams<{ next?: string }>();
  return (
    <Show when={params.next === DASHBOARD_NEXT} fallback={<Navigate href="/" />}>
      <DashboardBounce />
    </Show>
  );
}

/**
 * The bounce's second half: ask lab where the dashboard is, then hand the
 * BROWSER over — a different origin, so the router cannot do it.
 *
 * Every way this can miss (the exposure call throws, the dashboard is off, no
 * URL) falls back to the ordinary / navigation. The operator is logged in
 * either way, and lab is a better landing place than a spinner that never
 * resolves.
 */
function DashboardBounce() {
  const [params] = useSearchParams<{ path?: string }>();
  const path = safeDashboardPath(params.path);
  const [fellBack, setFellBack] = createSignal(false);

  onMount(() => {
    void (async () => {
      try {
        const exposure = await getOneCLIDashboard();
        if (exposure.mode !== 'off' && typeof exposure.url === 'string' && exposure.url !== '') {
          // replace(), not assign(): the bounce must not sit in the back stack,
          // where Back would land on /login and bounce the operator forward
          // again, forever.
          window.location.replace(exposure.url + path);
          return;
        }
      } catch {
        // A dashboard we cannot locate is not a login failure — fall through.
      }
      setFellBack(true);
    })();
  });

  return (
    <Show
      when={fellBack()}
      fallback={
        <main class="page">
          <p class="center-note">Opening the OneCLI dashboard…</p>
        </main>
      }
    >
      <Navigate href="/" />
    </Show>
  );
}

function LoginForm() {
  const { refresh } = useAuth();
  const [username, setUsername] = createSignal('');
  const [password, setPassword] = createSignal('');
  const [remember, setRemember] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(username(), password(), remember());
      // The refreshed auth state flips authenticated; the guard redirects to /.
      await refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <main class="page">
      <div class="card auth-card">
        <h1 class="brand">
          lab<span class="brand-dot">.</span>
        </h1>
        <h2>Log in</h2>
        <Banner message={error()} onDismiss={() => setError(null)} />
        <form onSubmit={(e) => void submit(e)}>
          <label class="field">
            <span>Username</span>
            <input
              type="text"
              name="username"
              autocomplete="username"
              required
              value={username()}
              onInput={(e) => setUsername(e.currentTarget.value)}
            />
          </label>
          <label class="field">
            <span>Password</span>
            <input
              type="password"
              name="password"
              autocomplete="current-password"
              required
              value={password()}
              onInput={(e) => setPassword(e.currentTarget.value)}
            />
          </label>
          <label class="check">
            <input
              type="checkbox"
              name="remember"
              checked={remember()}
              onChange={(e) => setRemember(e.currentTarget.checked)}
            />
            <span>Remember me for 90 days</span>
          </label>
          <button type="submit" class="primary wide" disabled={busy()}>
            {busy() ? 'Logging in…' : 'Log in'}
          </button>
        </form>
      </div>
    </main>
  );
}
