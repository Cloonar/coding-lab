// Secrets section (issue #61 §9, issue #104): a write-only per-repo secret
// store. No endpoint or view here ever fetches or displays a value — create,
// rotate and delete only. Agents consume secrets via `labctl secret exec`.
//
// Every row acts at once, in place: a row opens (a disclosure, nothing over
// the page) to take a new value — "Save new value" — or to be deleted behind
// an inline confirmation; the last row is "+ New secret", which opens the
// same way into the name, description and value of a new one. Problems show
// under the field they are about; the server's refusals too, when it names
// the field, else under the form.
//
// Issue #25 puts the credential-gateway grant picker ABOVE this list and
// issue #39 the SSH-targets picker below it (and still above this list): the
// two secret stores coexist on this page until #27 retires lab's own
// `repo_secrets`, and the pickers leading is what says which one an operator
// should reach for first.

import { A } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal, createUniqueId } from 'solid-js';
import {
  ApiError,
  createRepoSecret,
  deleteRepoSecret,
  errorMessage,
  listRepoSecrets,
  rotateRepoSecret,
  type RepoSecret,
} from '../../../api';
import Banner from '../../../components/Banner';
import EmptyState from '../../../components/EmptyState';
import Icon from '../../../components/Icon';
import InlineConfirm from '../../../components/InlineConfirm';
import { useRepoHome } from '../../repo-home/context';
import RepoSecretGrantsSection from './SecretGrants';
import RepoSSHTargetsSection from './SSHTargets';

function secretUpdatedOn(timestamp: string): string {
  const date = new Date(timestamp);
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleDateString();
}

export default function RepoSecretsSection(props: { repoId: string }) {
  const [secrets, { refetch }] = createResource(() => listRepoSecrets(props.repoId));
  const [error, setError] = createSignal<string | null>(null);
  // One row open at a time: a secret's id, 'new', or nothing.
  const [open, setOpen] = createSignal<string | null>(null);
  const toggle = (id: string): void => {
    setOpen(open() === id ? null : id);
  };

  return (
    <>
      <RepoSecretGrantsSection repoId={props.repoId} />
      <RepoSSHTargetsSection repoId={props.repoId} />
      <section class="card secrets-list" aria-label="Secrets">
        <p class="settings-note">
          Values are write-only: lab never reads or shows them again after saving. Agents use them
          via <code>labctl secret exec</code>.
        </p>
        <Banner message={error()} onDismiss={() => setError(null)} />
        <Switch>
          <Match when={secrets.error !== undefined}>
            <Banner message={errorMessage(secrets.error)} />
          </Match>
          <Match when={secrets()}>
            {(list) => (
              <ul class="secret-rows">
                <Show when={list().length === 0}>
                  <li>
                    <EmptyState>No secrets yet. Add one for agents to use.</EmptyState>
                  </li>
                </Show>
                <For each={list()}>
                  {(secret) => (
                    <SecretRow
                      repoId={props.repoId}
                      secret={secret}
                      open={open() === secret.id}
                      onToggle={() => toggle(secret.id)}
                      onChanged={() => {
                        setOpen(null);
                        void refetch();
                      }}
                      onError={setError}
                    />
                  )}
                </For>
                <NewSecretRow
                  repoId={props.repoId}
                  open={open() === 'new'}
                  onToggle={() => toggle('new')}
                  onCreated={() => {
                    setOpen(null);
                    void refetch();
                  }}
                />
              </ul>
            )}
          </Match>
        </Switch>
      </section>
    </>
  );
}

/** The disclosure head of a row: what it shows and the chevron that opens it. */
function RowHead(props: {
  open: boolean;
  controls: string;
  onToggle: () => void;
  children: import('solid-js').JSX.Element;
}) {
  return (
    <button
      type="button"
      class="secret-row-head"
      aria-expanded={props.open}
      aria-controls={props.controls}
      onClick={() => props.onToggle()}
    >
      <span class="secret-row-text">{props.children}</span>
      <Icon name={props.open ? 'chevron-down' : 'chevron-right'} size={18} />
    </button>
  );
}

function SecretRow(props: {
  repoId: string;
  secret: RepoSecret;
  open: boolean;
  onToggle: () => void;
  onChanged: () => void;
  onError: (message: string | null) => void;
}) {
  const home = useRepoHome();
  const uid = createUniqueId();
  const bodyId = `secret-${uid}-body`;
  const valueId = `secret-${uid}-value`;
  const errorId = `secret-${uid}-error`;
  const hintId = `secret-${uid}-hint`;
  const [newValue, setNewValue] = createSignal('');
  const [problem, setProblem] = createSignal<string | null>(null);
  const [busy, setBusy] = createSignal(false);

  const rotate = async (event: SubmitEvent): Promise<void> => {
    event.preventDefault();
    if (busy()) return;
    if (newValue() === '') {
      setProblem('Paste the new value first.');
      document.getElementById(valueId)?.focus();
      return;
    }
    setBusy(true);
    setProblem(null);
    props.onError(null);
    try {
      await rotateRepoSecret(props.repoId, props.secret.id, newValue());
      setNewValue('');
      props.onChanged();
      home.notify(`${props.secret.name} has a new value`);
    } catch (err) {
      setProblem(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (): Promise<void> => {
    setBusy(true);
    props.onError(null);
    try {
      await deleteRepoSecret(props.repoId, props.secret.id);
      props.onChanged();
      home.notify(`Deleted ${props.secret.name}`);
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <li class="secret-row" classList={{ open: props.open }}>
      <RowHead open={props.open} controls={bodyId} onToggle={props.onToggle}>
        <span class="secret-row-name mono">{props.secret.name}</span>
        <span class="secret-row-meta">
          <Show when={props.secret.description}>{props.secret.description} · </Show>
          updated {secretUpdatedOn(props.secret.updated_at)}
        </span>
      </RowHead>
      {/* The sticky exposure warning (issue #108): exposed_run_id/exposed_at
          are both null until a run's transcript surfaces the value, then both
          set until the next rotate clears them — the refetch after a
          successful rotate (props.onChanged) is what makes this disappear. */}
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
      <Show when={props.open}>
        {/* Rotation: the field starts blank by construction — the old value is
            never fetched, so there is nothing to prefill. */}
        <form class="secret-row-body" id={bodyId} novalidate onSubmit={(e) => void rotate(e)}>
          <div class="sfield" classList={{ invalid: problem() !== null }}>
            <div class="sfield-label">
              <label for={valueId}>New value</label>
            </div>
            <input
              type="password"
              id={valueId}
              name="secret-rotate-value"
              autocomplete="off"
              placeholder="Paste the new value"
              value={newValue()}
              aria-invalid={problem() !== null ? 'true' : undefined}
              aria-describedby={problem() !== null ? `${errorId} ${hintId}` : hintId}
              onInput={(e) => {
                setNewValue(e.currentTarget.value);
                setProblem(null);
              }}
            />
            <Show when={problem()}>
              {(message) => (
                <p class="sfield-error" id={errorId} role="alert">
                  {message()}
                </p>
              )}
            </Show>
            <small class="sfield-hint" id={hintId}>
              The current value is never shown.
            </small>
          </div>
          <div class="secret-row-actions">
            <InlineConfirm
              label="Delete"
              confirmLabel="Delete for good"
              busyLabel="Deleting…"
              prompt={`Delete ${props.secret.name}?`}
              disabled={busy()}
              onConfirm={remove}
            />
            <span class="spacer" />
            <button type="submit" class="primary" disabled={busy()}>
              {busy() ? 'Saving…' : 'Save new value'}
            </button>
          </div>
        </form>
      </Show>
    </li>
  );
}

function NewSecretRow(props: {
  repoId: string;
  open: boolean;
  onToggle: () => void;
  onCreated: () => void;
}) {
  const home = useRepoHome();
  const uid = createUniqueId();
  const bodyId = `secret-${uid}-body`;
  const id = (key: string): string => `secret-${uid}-${key}`;
  const [name, setName] = createSignal('');
  const [description, setDescription] = createSignal('');
  const [value, setValue] = createSignal('');
  const [problems, setProblems] = createSignal<{ name?: string; value?: string; form?: string }>(
    {},
  );
  const [busy, setBusy] = createSignal(false);

  const submit = async (event: SubmitEvent): Promise<void> => {
    event.preventDefault();
    if (busy()) return;
    const found: { name?: string; value?: string } = {};
    if (name().trim() === '') found.name = 'Give the secret a name.';
    if (value() === '') found.value = 'Paste the value first.';
    setProblems(found);
    if (found.name !== undefined || found.value !== undefined) {
      document.getElementById(id(found.name !== undefined ? 'name' : 'value'))?.focus();
      return;
    }
    setBusy(true);
    try {
      const created = await createRepoSecret(
        props.repoId,
        name().trim(),
        description().trim(),
        value(),
      );
      setName('');
      setDescription('');
      setValue('');
      props.onCreated();
      home.notify(`Added ${created.name}`);
    } catch (err) {
      // A refusal that names the field (the name's shape) shows under it.
      const field = err instanceof ApiError ? err.field : undefined;
      if (field === 'name' || field === 'value') setProblems({ [field]: errorMessage(err) });
      else setProblems({ form: errorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  const textField = (
    key: 'name' | 'description' | 'value',
    label: string,
    input: import('solid-js').JSX.Element,
    hint?: string,
  ) => {
    const problem = (): string | undefined => (key === 'description' ? undefined : problems()[key]);
    return (
      <div class="sfield" classList={{ invalid: problem() !== undefined }}>
        <div class="sfield-label">
          <label for={id(key)}>{label}</label>
        </div>
        {input}
        <Show when={problem()}>
          {(message) => (
            <p class="sfield-error" id={id(`${key}-error`)} role="alert">
              {message()}
            </p>
          )}
        </Show>
        <Show when={hint}>
          <small class="sfield-hint" id={id(`${key}-hint`)}>
            {hint}
          </small>
        </Show>
      </div>
    );
  };
  const describedBy = (key: 'name' | 'value', hint: boolean): string | undefined => {
    const ids = [
      problems()[key] !== undefined ? id(`${key}-error`) : null,
      hint ? id(`${key}-hint`) : null,
    ].filter(Boolean);
    return ids.length > 0 ? ids.join(' ') : undefined;
  };

  return (
    <li class="secret-row secret-row-new" classList={{ open: props.open }}>
      <RowHead open={props.open} controls={bodyId} onToggle={props.onToggle}>
        <span class="secret-row-name">
          <Icon name="plus" size={16} />
          New secret
        </span>
      </RowHead>
      <Show when={props.open}>
        <form class="secret-row-body" id={bodyId} novalidate onSubmit={(e) => void submit(e)}>
          {textField(
            'name',
            'Name',
            <input
              type="text"
              id={id('name')}
              name="secret-name"
              autocomplete="off"
              spellcheck={false}
              class="mono"
              placeholder="API_KEY"
              value={name()}
              aria-invalid={problems().name !== undefined ? 'true' : undefined}
              aria-describedby={describedBy('name', true)}
              onInput={(e) => {
                setName(e.currentTarget.value);
                setProblems({ ...problems(), name: undefined });
              }}
            />,
            'Uppercase letters, digits, underscores; must start with a letter.',
          )}
          {textField(
            'description',
            'Description',
            <input
              type="text"
              id={id('description')}
              name="secret-description"
              autocomplete="off"
              value={description()}
              onInput={(e) => setDescription(e.currentTarget.value)}
            />,
          )}
          {textField(
            'value',
            'Value',
            <input
              type="password"
              id={id('value')}
              name="secret-value"
              autocomplete="off"
              value={value()}
              aria-invalid={problems().value !== undefined ? 'true' : undefined}
              aria-describedby={describedBy('value', false)}
              onInput={(e) => {
                setValue(e.currentTarget.value);
                setProblems({ ...problems(), value: undefined });
              }}
            />,
          )}
          <Banner
            message={problems().form ?? null}
            onDismiss={() => setProblems({ ...problems(), form: undefined })}
          />
          <div class="secret-row-actions">
            <span class="spacer" />
            <button type="submit" class="primary" disabled={busy()}>
              {busy() ? 'Adding…' : 'Add secret'}
            </button>
          </div>
        </form>
      </Show>
    </li>
  );
}
