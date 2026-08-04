// Secrets section, moved verbatim from the RepoSettings monolith (issue
// #198). Device-local/immediate by design: every action (create, rotate,
// delete) talks to the server the moment it happens, so there is no
// useSettingsForm and no unsaved-changes guard here.

import { A } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal } from 'solid-js';
import {
  createRepoSecret,
  deleteRepoSecret,
  errorMessage,
  listRepoSecrets,
  rotateRepoSecret,
  type RepoSecret,
} from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';
import SectionCard from '../../../components/SectionCard';

function secretUpdatedOn(timestamp: string): string {
  const date = new Date(timestamp);
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleDateString();
}

/**
 * Secrets section (issue #104): a write-only per-repo secret store. No
 * endpoint or view here ever fetches or displays a value — create, rotate
 * and delete only. Agents consume secrets via `labctl secret exec`, not
 * through this UI.
 */
export default function RepoSecretsSection(props: { repoId: string }) {
  const [secrets, { refetch }] = createResource(() => listRepoSecrets(props.repoId));
  const [showCreate, setShowCreate] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  return (
    <SectionCard
      title="Secrets"
      action={
        <button type="button" class="primary small" onClick={() => setShowCreate(!showCreate())}>
          {showCreate() ? 'Cancel' : '+ Add secret'}
        </button>
      }
      hint={
        <>
          Values are write-only — lab never reads or shows them again after saving. Agents use them
          via <code>labctl secret exec</code>.
        </>
      }
    >
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={showCreate()}>
        <CreateSecretForm
          repoId={props.repoId}
          onCreated={() => {
            setShowCreate(false);
            void refetch();
          }}
        />
      </Show>
      <Switch>
        <Match when={secrets.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(secrets.error)}</span>
          </div>
        </Match>
        <Match when={secrets()?.length === 0}>
          <p class="empty">No secrets yet — add one for agents to use via labctl secret exec.</p>
        </Match>
        <Match when={secrets()}>
          <div class="card-list">
            <For each={secrets()}>
              {(secret) => (
                <SecretRow
                  repoId={props.repoId}
                  secret={secret}
                  onChanged={() => void refetch()}
                  onError={setError}
                />
              )}
            </For>
          </div>
        </Match>
      </Switch>
    </SectionCard>
  );
}

function CreateSecretForm(props: { repoId: string; onCreated: () => void }) {
  const [name, setName] = createSignal('');
  const [description, setDescription] = createSignal('');
  const [value, setValue] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await createRepoSecret(props.repoId, name().trim(), description().trim(), value());
      setName('');
      setDescription('');
      setValue('');
      props.onCreated();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="card form-card">
      <h2>New secret</h2>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <form onSubmit={(e) => void submit(e)}>
        <label class="field">
          <span>Name</span>
          <input
            type="text"
            name="secret-name"
            required
            autocomplete="off"
            spellcheck={false}
            class="mono"
            placeholder="API_KEY"
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
          />
          <small class="hint">
            Uppercase letters, digits, underscores; must start with a letter.
          </small>
        </label>
        <label class="field">
          <span>Description</span>
          <input
            type="text"
            name="secret-description"
            autocomplete="off"
            value={description()}
            onInput={(e) => setDescription(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Value</span>
          <input
            type="password"
            name="secret-value"
            required
            autocomplete="off"
            value={value()}
            onInput={(e) => setValue(e.currentTarget.value)}
          />
        </label>
        <button type="submit" class="primary wide" disabled={busy()}>
          {busy() ? 'Saving…' : 'Save secret'}
        </button>
      </form>
    </div>
  );
}

function SecretRow(props: {
  repoId: string;
  secret: RepoSecret;
  onChanged: () => void;
  onError: (message: string | null) => void;
}) {
  const [rotating, setRotating] = createSignal(false);
  const [newValue, setNewValue] = createSignal('');
  const [busy, setBusy] = createSignal(false);

  const rotate = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    props.onError(null);
    try {
      await rotateRepoSecret(props.repoId, props.secret.id, newValue());
      setNewValue('');
      setRotating(false);
      props.onChanged();
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!window.confirm(`Delete secret "${props.secret.name}"?`)) return;
    setBusy(true);
    props.onError(null);
    try {
      await deleteRepoSecret(props.repoId, props.secret.id);
      props.onChanged();
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <article class="card">
      <div class="card-head">
        <span class="card-title mono">{props.secret.name}</span>
        <span class="spacer" />
        <Show when={!rotating()}>
          <button type="button" class="small" onClick={() => setRotating(true)} disabled={busy()}>
            Rotate
          </button>
          <button
            type="button"
            class="danger small"
            onClick={() => void remove()}
            disabled={busy()}
          >
            {busy() ? 'Working…' : 'Delete'}
          </button>
        </Show>
      </div>
      <Show when={props.secret.description}>
        <p class="muted card-sub">{props.secret.description}</p>
      </Show>
      <p class="muted card-sub">Updated {secretUpdatedOn(props.secret.updated_at)}</p>
      {/* The sticky exposure warning (issue #108): exposed_run_id/exposed_at
          are both null until a run's transcript surfaces the value, then both
          set until the next rotate clears them — the refetch after a
          successful rotate (props.onChanged, wired above) is what makes this
          disappear, no separate clear-exposure call needed. */}
      <Show when={props.secret.exposed_run_id}>
        {(runID) => (
          <p class="secret-exposed">
            <span class="chip exposed">Exposed</span>{' '}
            <span>
              Exposed in run <A href={`/runs/${runID()}`}>{runID()}</A>
              <Show when={props.secret.exposed_at}>
                {(exposedAt) => <> · {secretUpdatedOn(exposedAt())}</>}
              </Show>{' '}
              — rotate to clear.
            </span>
          </p>
        )}
      </Show>
      <Show when={rotating()}>
        {/* Rotation: the field starts blank by construction — the old value is
            never fetched, so there is nothing to prefill. */}
        <form onSubmit={(e) => void rotate(e)}>
          <label class="field">
            <span>New value</span>
            <input
              type="password"
              name="secret-rotate-value"
              required
              autocomplete="off"
              value={newValue()}
              onInput={(e) => setNewValue(e.currentTarget.value)}
            />
          </label>
          <div class="card-actions">
            <button type="submit" class="primary" disabled={busy()}>
              {busy() ? 'Rotating…' : 'Save new value'}
            </button>
            <button
              type="button"
              onClick={() => {
                setNewValue('');
                setRotating(false);
              }}
              disabled={busy()}
            >
              Cancel
            </button>
          </div>
        </form>
      </Show>
    </article>
  );
}
