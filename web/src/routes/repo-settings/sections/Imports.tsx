// Imports section (issue #61 §9, issue #261): a repo's declared imports —
// other registered lab repos whose code this repo's instances may read as
// read-only snapshots, mounted at spawn. Directional and consumer-declared:
// this repo (whose settings page this is) is the consumer, and each row names
// a repo it imports FROM.
//
// Every action is immediate: Remove takes the import away at once and the
// toast offers Undo (which declares it again); Add is a pick and a button,
// and says what happened in the toast. Nothing opens over the page and no
// browser confirm is involved.

import { For, Show, createMemo, createResource, createSignal, createUniqueId } from 'solid-js';
import {
  addRepoImport,
  errorMessage,
  listRepoImports,
  listRepos,
  removeRepoImport,
  type RepoImport,
} from '../../../api';
import Banner from '../../../components/Banner';
import EmptyState from '../../../components/EmptyState';
import Select, { type SelectOption } from '../../../components/Select';
import { useRepoHome } from '../../repo-home/context';

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
  const home = useRepoHome();
  const uid = createUniqueId();
  const pickLabelId = `imports-${uid}-pick`;
  const pickErrorId = `imports-${uid}-pick-error`;
  const [imports, { refetch }] = createResource(() => listRepoImports(props.repoId));
  const [repos] = createResource(() => listRepos());
  const [error, setError] = createSignal<string | null>(null);
  const [targetId, setTargetId] = createSignal('');
  const [pickProblem, setPickProblem] = createSignal<string | null>(null);
  const [busy, setBusy] = createSignal<string | null>(null);

  // Candidates for the picker: every registered repo minus this one
  // (self-import is rejected server-side) and minus whatever is already
  // declared (adding again would just be a no-op 201) — the picker only ever
  // offers a target that would actually change something.
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

  const add = async (event: SubmitEvent): Promise<void> => {
    event.preventDefault();
    if (busy() !== null) return;
    if (targetId() === '') {
      setPickProblem('Choose a repository first.');
      return;
    }
    const target = targetId();
    setBusy('add');
    setError(null);
    try {
      const added = await addRepoImport(props.repoId, target);
      setTargetId('');
      await refetch();
      home.notify(`${home.repo()?.name ?? 'This repository'} can now read ${added.name}`);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  // Removed at once; the toast's Undo declares the import again. A re-add
  // that fails says so where the list is.
  const remove = async (imp: RepoImport): Promise<void> => {
    setBusy(imp.id);
    setError(null);
    try {
      await removeRepoImport(props.repoId, imp.id);
      await refetch();
      home.notify(`Removed the import of ${imp.name}`, {
        action: {
          label: 'Undo',
          run: () => {
            void addRepoImport(props.repoId, imp.id)
              .then(() => refetch())
              .catch((err: unknown) => {
                setError(`Could not import ${imp.name} again: ${errorMessage(err)}`);
              });
          },
        },
      });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  return (
    <section class="card imports-list" aria-label="Imports">
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Show when={imports.error}>{(err) => <Banner message={errorMessage(err())} />}</Show>
      <Show when={imports()}>
        {(list) => (
          <Show
            when={list().length > 0}
            fallback={<EmptyState>No imports. Runs only see this repository.</EmptyState>}
          >
            <ul class="import-rows">
              <For each={list()}>
                {(imp) => (
                  <li class="import-row">
                    <span class="import-row-text">
                      <strong>{imp.name}</strong>
                      <span class="import-row-meta">
                        Read-only snapshot of its default branch, refreshed at every spawn.
                      </span>
                    </span>
                    <button
                      type="button"
                      class="small"
                      aria-label={`Remove the import of ${imp.name}`}
                      disabled={busy() === imp.id}
                      onClick={() => void remove(imp)}
                    >
                      {busy() === imp.id ? 'Removing…' : 'Remove'}
                    </button>
                  </li>
                )}
              </For>
            </ul>
          </Show>
        )}
      </Show>
      <form class="imports-add" novalidate onSubmit={(e) => void add(e)}>
        <span id={pickLabelId} class="visually-hidden">
          Repository to import
        </span>
        <div class="imports-add-row">
          <Select
            skin="field"
            label="Repository to import"
            labelledBy={pickLabelId}
            name="target_repo_id"
            value={targetId()}
            options={candidates()}
            inheritLabel="Choose a repository to import"
            describedBy={pickProblem() !== null ? pickErrorId : undefined}
            invalid={pickProblem() !== null}
            onChange={(value) => {
              setTargetId(value);
              setPickProblem(null);
            }}
          />
          <button type="submit" disabled={busy() === 'add'}>
            {busy() === 'add' ? 'Adding…' : 'Add'}
          </button>
        </div>
        <Show when={pickProblem()}>
          {(message) => (
            <p class="sfield-error" id={pickErrorId} role="alert">
              {message()}
            </p>
          )}
        </Show>
      </form>
    </section>
  );
}
