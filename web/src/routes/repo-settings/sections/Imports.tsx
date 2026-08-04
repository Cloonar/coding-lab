// Imports section (issue #261): a repo's declared imports — other registered
// lab repos whose code this repo's instances may read as read-only
// snapshots, mounted at spawn. Directional and consumer-declared: this repo
// (whose settings page this is) is the consumer, and each row names a repo
// it imports FROM. Modeled on Secrets.tsx (issue #198 pattern):
// device-local/immediate — every action (add, remove) talks to the server
// the moment it happens, so there is no useSettingsForm and no
// unsaved-changes guard here.

import { For, Match, Show, Switch, createMemo, createResource, createSignal } from 'solid-js';
import {
  addRepoImport,
  errorMessage,
  listRepoImports,
  listRepos,
  removeRepoImport,
  type RepoImport,
} from '../../../api';
import EmptyState from '../../../components/EmptyState';
import ErrorBanner from '../../../components/ErrorBanner';
import ListRowCard from '../../../components/ListRowCard';
import SectionCard from '../../../components/SectionCard';
import Select, { type SelectOption } from '../../../components/Select';

/**
 * Informational only (issue #261 acceptance): a still-cloning candidate
 * stays selectable here — the spawn-time refusal is the real guard against
 * mounting an unready clone, so this picker must not over-filter. The status
 * text just tells the operator what they're picking.
 */
function cloneStatusHint(status: string): string | undefined {
  if (status === 'error') return 'clone failed';
  if (status === 'cloning') return 'cloning';
  return undefined;
}

export default function ImportsSection(props: { repoId: string }) {
  const [imports, { refetch }] = createResource(() => listRepoImports(props.repoId));
  const [repos] = createResource(() => listRepos());
  const [showAdd, setShowAdd] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  // Candidates for the add-form picker: every registered repo minus this one
  // (self-import is rejected server-side) and minus whatever is already
  // declared (adding again would just be a no-op 201) — the picker only ever
  // offers a target that would actually change something on submit.
  const candidates = createMemo<SelectOption[]>(() => {
    const declared = new Set((imports() ?? []).map((imp) => imp.id));
    return (repos() ?? [])
      .filter((repo) => repo.id !== props.repoId && !declared.has(repo.id))
      .map((repo) => ({
        value: repo.id,
        label: repo.name,
        status: cloneStatusHint(repo.clone_status),
      }));
  });

  return (
    <SectionCard
      title="Imports"
      action={
        <button type="button" class="primary small" onClick={() => setShowAdd(!showAdd())}>
          {showAdd() ? 'Cancel' : '+ Add import'}
        </button>
      }
      hint={
        <>
          Other lab repos this repo's instances may read as read-only snapshots, mounted at spawn.
        </>
      }
    >
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={showAdd()}>
        <AddImportForm
          repoId={props.repoId}
          options={candidates()}
          onAdded={() => {
            setShowAdd(false);
            void refetch();
          }}
        />
      </Show>
      <Switch>
        <Match when={imports.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(imports.error)}</span>
          </div>
        </Match>
        <Match when={imports()?.length === 0}>
          <EmptyState>
            No imports declared — add another lab repo for this repo's instances to read as a
            read-only snapshot.
          </EmptyState>
        </Match>
        <Match when={imports()}>
          <div class="card-list">
            <For each={imports()}>
              {(imp) => (
                <ImportRow
                  repoId={props.repoId}
                  imp={imp}
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

function AddImportForm(props: { repoId: string; options: SelectOption[]; onAdded: () => void }) {
  const [targetId, setTargetId] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    if (targetId() === '') return;
    setBusy(true);
    setError(null);
    try {
      await addRepoImport(props.repoId, targetId());
      setTargetId('');
      props.onAdded();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="card form-card">
      <h2>New import</h2>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <form onSubmit={(e) => void submit(e)}>
        <Select
          skin="field"
          label="Repository"
          name="target_repo_id"
          value={targetId()}
          options={props.options}
          onChange={setTargetId}
        />
        <button type="submit" class="primary wide" disabled={busy() || targetId() === ''}>
          {busy() ? 'Adding…' : 'Add import'}
        </button>
      </form>
    </div>
  );
}

function ImportRow(props: {
  repoId: string;
  imp: RepoImport;
  onChanged: () => void;
  onError: (message: string | null) => void;
}) {
  const [busy, setBusy] = createSignal(false);

  const remove = async () => {
    if (!window.confirm(`Remove import "${props.imp.name}"?`)) return;
    setBusy(true);
    props.onError(null);
    try {
      await removeRepoImport(props.repoId, props.imp.id);
      props.onChanged();
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <ListRowCard
      title={props.imp.name}
      actions={
        <button type="button" class="danger small" onClick={() => void remove()} disabled={busy()}>
          {busy() ? 'Working…' : 'Remove'}
        </button>
      }
    />
  );
}
