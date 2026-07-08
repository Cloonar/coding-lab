// Credentials page: list (name, kind badge, created, in-use indicator),
// create with kind-switched payload fields, rename, replace-secret rotation
// and delete with the server's 409 message surfaced. Secrets are write-only:
// no payload is ever fetched, shown or prefilled anywhere on this page.

import { For, Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import {
  createCredential,
  deleteCredential,
  errorMessage,
  listCredentials,
  listInstances,
  updateCredential,
  type CredentialKind,
  type CredentialListItem,
} from '../api';
import ClaudeAuthCard from '../components/ClaudeAuthCard';
import ErrorBanner from '../components/ErrorBanner';
import PayloadFields, { createPayloadDraft } from '../components/PayloadFields';
import RequireAuth from '../components/RequireAuth';
import { useEvents } from '../events';
import { resourceValue } from '../lib/resource';

export const KIND_LABELS: Record<CredentialKind, string> = {
  ssh_key: 'SSH key',
  https_token: 'HTTPS token',
  forge_token: 'Forge token',
};

function createdOn(timestamp: string): string {
  const date = new Date(timestamp);
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleDateString();
}

export default function Credentials() {
  return (
    <RequireAuth>
      <CredentialsView />
    </RequireAuth>
  );
}

function CredentialsView() {
  const events = useEvents();
  const [credentials, { refetch }] = createResource(() => listCredentials());
  // The referenced flag follows repo rows (create/delete/PATCH change FKs).
  onCleanup(events.subscribe('repo.changed', () => void refetch()));

  // Live instance count for the Claude logout confirm copy (issue #46): running
  // instances survive a logout on their in-memory token until it refreshes.
  const [instances, { refetch: refetchInstances }] = createResource(() => listInstances());
  onCleanup(events.subscribe('run.changed', () => void refetchInstances()));
  const liveCount = () => (resourceValue(instances) ?? []).filter((i) => i.live).length;

  const [showCreate, setShowCreate] = createSignal(false);

  return (
    <main class="page">
      <div class="stack">
        <ClaudeAuthCard activeRuns={liveCount()} />
      </div>
      <div class="section-head">
        <h2>Credentials</h2>
        <button type="button" class="primary" onClick={() => setShowCreate(!showCreate())}>
          {showCreate() ? 'Cancel' : '+ Add credential'}
        </button>
      </div>
      <Show when={showCreate()}>
        <CreateCredentialCard
          onCreated={() => {
            setShowCreate(false);
            void refetch();
          }}
        />
      </Show>
      <Switch>
        <Match when={credentials.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(credentials.error)}</span>
          </div>
        </Match>
        <Match when={credentials()?.length === 0}>
          <p class="empty">No credentials yet — add an SSH key or token to clone private repos.</p>
        </Match>
        <Match when={credentials()}>
          <div class="card-list">
            <For each={credentials()}>
              {(credential) => (
                <CredentialCard credential={credential} onChanged={() => void refetch()} />
              )}
            </For>
          </div>
        </Match>
      </Switch>
    </main>
  );
}

function CreateCredentialCard(props: { onCreated: () => void }) {
  const [name, setName] = createSignal('');
  const [kind, setKind] = createSignal<CredentialKind>('ssh_key');
  const draft = createPayloadDraft();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await createCredential(name().trim(), kind(), draft.build(kind()));
      setName('');
      draft.reset();
      props.onCreated();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="card form-card">
      <h2>New credential</h2>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <form onSubmit={(e) => void submit(e)}>
        <label class="field">
          <span>Name</span>
          <input
            type="text"
            name="name"
            required
            autocomplete="off"
            placeholder="forgejo-deploy-key"
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Kind</span>
          <select
            name="kind"
            value={kind()}
            onChange={(e) => setKind(e.currentTarget.value as CredentialKind)}
          >
            <option value="ssh_key">SSH key (git over SSH)</option>
            <option value="https_token">HTTPS token (git over HTTPS)</option>
            <option value="forge_token">Forge API token (issues + PRs)</option>
          </select>
        </label>
        <PayloadFields kind={kind()} draft={draft} />
        <button type="submit" class="primary wide" disabled={busy()}>
          {busy() ? 'Saving…' : 'Save credential'}
        </button>
      </form>
    </div>
  );
}

function CredentialCard(props: { credential: CredentialListItem; onChanged: () => void }) {
  const [mode, setMode] = createSignal<'view' | 'rename' | 'replace'>('view');
  const [newName, setNewName] = createSignal('');
  const draft = createPayloadDraft();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const run = async (action: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try {
      await action();
      setMode('view');
      draft.reset();
      props.onChanged();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // Reactive reads happen in the handlers themselves (tracked as event
  // handlers); the closures passed to run() only see captured plain values.
  const rename = (event: SubmitEvent) => {
    event.preventDefault();
    const id = props.credential.id;
    const name = newName().trim();
    void run(async () => {
      await updateCredential(id, { name });
    });
  };

  const replaceSecret = (event: SubmitEvent) => {
    event.preventDefault();
    const id = props.credential.id;
    const payload = draft.build(props.credential.kind);
    void run(async () => {
      await updateCredential(id, { payload });
    });
  };

  const remove = () => {
    const id = props.credential.id;
    if (!window.confirm(`Delete credential "${props.credential.name}"?`)) return;
    void run(async () => {
      await deleteCredential(id);
    });
  };

  return (
    <article class="card">
      <div class="card-head">
        <span class="card-title">{props.credential.name}</span>
        <span class="chip mono">{KIND_LABELS[props.credential.kind]}</span>
        <Show when={props.credential.referenced}>
          <span class="chip in-use">in use</span>
        </Show>
      </div>
      <p class="muted card-sub">Created {createdOn(props.credential.created_at)}</p>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Switch>
        <Match when={mode() === 'rename'}>
          <form onSubmit={rename}>
            <label class="field">
              <span>New name</span>
              <input
                type="text"
                name="name"
                required
                autocomplete="off"
                value={newName()}
                onInput={(e) => setNewName(e.currentTarget.value)}
              />
            </label>
            <div class="card-actions">
              <button type="submit" class="primary" disabled={busy()}>
                {busy() ? 'Saving…' : 'Save name'}
              </button>
              <button type="button" onClick={() => setMode('view')} disabled={busy()}>
                Cancel
              </button>
            </div>
          </form>
        </Match>
        <Match when={mode() === 'replace'}>
          {/* Rotation: fields start blank by construction — the old secret is
              never fetched, so there is nothing to prefill. */}
          <form onSubmit={replaceSecret}>
            <PayloadFields kind={props.credential.kind} draft={draft} />
            <div class="card-actions">
              <button type="submit" class="primary" disabled={busy()}>
                {busy() ? 'Replacing…' : 'Replace secret'}
              </button>
              <button
                type="button"
                onClick={() => {
                  draft.reset();
                  setMode('view');
                }}
                disabled={busy()}
              >
                Cancel
              </button>
            </div>
          </form>
        </Match>
        <Match when={mode() === 'view'}>
          <div class="card-actions">
            <button
              type="button"
              onClick={() => {
                setNewName(props.credential.name);
                setMode('rename');
              }}
              disabled={busy()}
            >
              Rename
            </button>
            <button type="button" onClick={() => setMode('replace')} disabled={busy()}>
              Replace secret
            </button>
            <button type="button" class="danger" onClick={remove} disabled={busy()}>
              {busy() ? 'Working…' : 'Delete'}
            </button>
          </div>
        </Match>
      </Switch>
    </article>
  );
}
