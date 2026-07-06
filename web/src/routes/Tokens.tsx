// API tokens (/tokens, D7 PATs): list (name, created, last used), create
// (the secret is shown ONCE in a copy-able field with a store-it-now
// warning — lab keeps only a hash), delete with confirm. The token string
// never appears anywhere else: the list endpoint serves metadata only.

import { For, Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import {
  createToken,
  deleteToken,
  errorMessage,
  listTokens,
  type ApiToken,
  type CreatedApiToken,
} from '../api';
import ErrorBanner from '../components/ErrorBanner';
import RequireAuth from '../components/RequireAuth';
import TopBar from '../components/TopBar';

function onDate(timestamp: string): string {
  const date = new Date(timestamp);
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleDateString();
}

export default function Tokens() {
  return (
    <RequireAuth>
      <TokensView />
    </RequireAuth>
  );
}

function TokensView() {
  const [tokens, { refetch }] = createResource(() => listTokens());
  const [created, setCreated] = createSignal<CreatedApiToken | null>(null);
  const [error, setError] = createSignal<string | null>(null);

  return (
    <main class="page">
      <TopBar />
      <div class="section-head">
        <h2>API tokens</h2>
      </div>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <CreateTokenCard
        onCreated={(token) => {
          setCreated(token);
          void refetch();
        }}
      />
      <Show when={created()}>
        {(token) => <TokenReveal token={token()} onDone={() => setCreated(null)} />}
      </Show>
      <Switch>
        <Match when={tokens.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(tokens.error)}</span>
          </div>
        </Match>
        <Match when={tokens()?.length === 0}>
          <p class="empty">No API tokens yet — create one for scripts and automation.</p>
        </Match>
        <Match when={tokens()}>
          <div class="card-list">
            <For each={tokens()}>
              {(token) => (
                <TokenCard token={token} onChanged={() => void refetch()} onError={setError} />
              )}
            </For>
          </div>
        </Match>
      </Switch>
    </main>
  );
}

function CreateTokenCard(props: { onCreated: (token: CreatedApiToken) => void }) {
  const [name, setName] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const token = await createToken(name().trim());
      setName('');
      props.onCreated(token);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="card form-card">
      <h2>New token</h2>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <form onSubmit={(e) => void submit(e)}>
        <label class="field">
          <span>Name</span>
          <input
            type="text"
            name="token-name"
            required
            autocomplete="off"
            placeholder="ci-deploy"
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
          />
        </label>
        <button type="submit" class="primary" disabled={busy()}>
          {busy() ? 'Creating…' : 'Create token'}
        </button>
      </form>
    </div>
  );
}

/**
 * The one and only appearance of the secret. Once dismissed it is gone for
 * good — lab stores a hash and cannot show it again.
 */
function TokenReveal(props: { token: CreatedApiToken; onDone: () => void }) {
  const [copied, setCopied] = createSignal(false);
  let timer: ReturnType<typeof setTimeout> | undefined;
  onCleanup(() => clearTimeout(timer));

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(props.token.token);
      setCopied(true);
      clearTimeout(timer);
      timer = setTimeout(() => setCopied(false), 2_000);
    } catch {
      // Clipboard unavailable (http, permissions) — the field is selectable.
    }
  };

  return (
    <div class="card token-reveal">
      <div class="banner success" role="status">
        <span class="banner-text">
          Token "{props.token.name}" created — copy it and store it now. It will not be shown again.
        </span>
      </div>
      <label class="field">
        <span>Token (shown once)</span>
        <input
          type="text"
          name="token-value"
          class="mono"
          readonly
          value={props.token.token}
          onFocus={(e) => e.currentTarget.select()}
          spellcheck={false}
          autocomplete="off"
        />
      </label>
      <div class="card-actions">
        <button type="button" class="primary" onClick={() => void copy()}>
          {copied() ? 'Copied' : 'Copy'}
        </button>
        <button type="button" onClick={() => props.onDone()}>
          Done
        </button>
      </div>
    </div>
  );
}

function TokenCard(props: {
  token: ApiToken;
  onChanged: () => void;
  onError: (message: string) => void;
}) {
  const [busy, setBusy] = createSignal(false);

  const remove = async () => {
    const { id, name } = props.token;
    if (!window.confirm(`Delete token "${name}"? Anything using it stops working immediately.`)) {
      return;
    }
    setBusy(true);
    try {
      await deleteToken(id);
      props.onChanged();
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <article class="card token-card">
      <div class="card-head">
        <span class="card-title">{props.token.name}</span>
        <span class="spacer" />
        <button type="button" class="danger small" onClick={() => void remove()} disabled={busy()}>
          {busy() ? 'Deleting…' : 'Delete'}
        </button>
      </div>
      <p class="muted card-sub">
        Created {onDate(props.token.created_at)} ·{' '}
        {props.token.last_used_at === null
          ? 'never used'
          : `last used ${onDate(props.token.last_used_at)}`}
      </p>
    </article>
  );
}
