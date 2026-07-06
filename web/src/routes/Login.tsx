// Login form with remember-me. Redirects to /setup while setup is required
// and to / once authenticated.

import { Navigate } from '@solidjs/router';
import { Match, Switch, createSignal } from 'solid-js';
import { errorMessage, login } from '../api';
import { useAuth } from '../auth';
import ErrorBanner from '../components/ErrorBanner';

export default function Login() {
  const { auth } = useAuth();
  return (
    <Switch>
      <Match when={auth()?.setup_required}>
        <Navigate href="/setup" />
      </Match>
      <Match when={auth()?.authenticated}>
        <Navigate href="/" />
      </Match>
      <Match when={auth()}>
        <LoginForm />
      </Match>
    </Switch>
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
        <ErrorBanner message={error()} onDismiss={() => setError(null)} />
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
