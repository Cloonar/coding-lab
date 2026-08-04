// First-run setup: create the admin account. Only reachable while the server
// reports setup_required; otherwise redirects to /login or /.

import { Navigate } from '@solidjs/router';
import { Match, Switch, createSignal } from 'solid-js';
import { errorMessage, setup } from '../api';
import { useAuth } from '../auth';
import Banner from '../components/Banner';

export default function Setup() {
  const { auth } = useAuth();
  return (
    <Switch>
      <Match when={auth() && !auth()!.setup_required}>
        <Navigate href={auth()!.authenticated ? '/' : '/login'} />
      </Match>
      <Match when={auth()?.setup_required}>
        <SetupForm />
      </Match>
    </Switch>
  );
}

function SetupForm() {
  const { refresh } = useAuth();
  const [username, setUsername] = createSignal('');
  const [password, setPassword] = createSignal('');
  const [confirm, setConfirm] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    if (password() !== confirm()) {
      setError('Passwords do not match.');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await setup(username(), password());
      // The refreshed auth state flips setup_required; the guard above then
      // redirects to /login (or / if the server started a session).
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
        <h2>First-run setup</h2>
        <p class="muted">Create the admin account for this lab instance.</p>
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
              autocomplete="new-password"
              required
              value={password()}
              onInput={(e) => setPassword(e.currentTarget.value)}
            />
          </label>
          <label class="field">
            <span>Confirm password</span>
            <input
              type="password"
              name="confirm"
              autocomplete="new-password"
              required
              value={confirm()}
              onInput={(e) => setConfirm(e.currentTarget.value)}
            />
          </label>
          <button type="submit" class="primary wide" disabled={busy()}>
            {busy() ? 'Creating…' : 'Create account'}
          </button>
        </form>
      </div>
    </main>
  );
}
