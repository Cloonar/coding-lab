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
//
// Rows keep their identity across a reload (lib/rowStore.ts), and focus never
// falls off the page: a removed row hands it to the row that takes its place
// (else to the pick), Undo to the row it brought back, and an Add with
// nothing picked to the pick it is asking for.

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
import { rescueFocus } from '../../../lib/focus';
import { resourceValue } from '../../../lib/resource';
import { createRowStore, rescueRowFocus, type OwnedRows } from '../../../lib/rowStore';
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
  const [fetched, { refetch }] = createResource(
    () => props.repoId,
    async (repoId): Promise<OwnedRows<RepoImport>> => ({
      owner: repoId,
      rows: await listRepoImports(repoId),
    }),
  );
  // One stable object per import, and only ever this repo's.
  const imports = createRowStore(
    () => resourceValue(fetched),
    () => props.repoId,
  );
  const [repos] = createResource(() => listRepos());
  const [error, setError] = createSignal<string | null>(null);
  const [targetId, setTargetId] = createSignal('');
  const [pickProblem, setPickProblem] = createSignal<string | null>(null);
  // What has a request in flight ('add', or an import's id): checked before
  // sending, so the buttons can stay enabled — a disabled one drops the focus.
  const [busy, setBusy] = createSignal<ReadonlySet<string>>(new Set());
  const setKeyBusy = (key: string, on: boolean): void => {
    const next = new Set(busy());
    if (on) next.add(key);
    else next.delete(key);
    setBusy(next);
  };

  let section: HTMLElement | undefined;
  const removeButtons = (): HTMLElement[] =>
    Array.from(section?.querySelectorAll<HTMLElement>('.import-row button') ?? []);
  const pick = (): HTMLElement | null =>
    section?.querySelector<HTMLElement>('button[name="target_repo_id"]') ?? null;

  // Candidates for the picker: every registered repo minus this one
  // (self-import is rejected server-side) and minus whatever is already
  // declared (adding again would just be a no-op 201) — the picker only ever
  // offers a target that would actually change something.
  const candidates = createMemo<SelectOption[]>(() => {
    const declared = new Set(imports.rows.map((imp) => imp.id));
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
    if (busy().has('add')) return;
    if (targetId() === '') {
      setPickProblem('Choose a repository first.');
      // To the control the problem is about.
      pick()?.focus();
      return;
    }
    const target = targetId();
    setKeyBusy('add', true);
    setError(null);
    try {
      const added = await addRepoImport(props.repoId, target);
      setTargetId('');
      await refetch();
      home.notify(`${home.repo()?.name ?? 'This repository'} can now read ${added.name}`);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setKeyBusy('add', false);
    }
  };

  // Removed at once; the toast's Undo declares the import again. A re-add
  // that fails says so where the list is.
  const remove = async (imp: RepoImport): Promise<void> => {
    // Captured before the await: the row is a live object.
    const { id, name } = imp;
    if (busy().has(id)) return;
    setKeyBusy(id, true);
    setError(null);
    try {
      await removeRepoImport(props.repoId, id);
      const at = imports.rows.findIndex((row) => row.id === id);
      imports.remove(id);
      void refetch();
      // The row took its Remove button — and the focus — with it.
      rescueRowFocus(removeButtons(), at, pick());
      home.notify(`Removed the import of ${name}`, {
        action: {
          label: 'Undo',
          run: () => {
            void addRepoImport(props.repoId, id)
              .then(() => refetch())
              .then(() => {
                // The toast is gone with the click that ran this.
                rescueFocus(
                  removeButtons().find(
                    (button) => button.getAttribute('aria-label') === removeLabel(name),
                  ),
                  pick(),
                );
              })
              .catch((err: unknown) => {
                setError(`Could not import ${name} again: ${errorMessage(err)}`);
                rescueFocus(pick());
              });
          },
        },
      });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setKeyBusy(id, false);
    }
  };
  const removeLabel = (name: string): string => `Remove the import of ${name}`;

  return (
    <section class="card imports-list" aria-label="Imports" ref={section}>
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Show when={fetched.error}>{(err) => <Banner message={errorMessage(err())} />}</Show>
      <Show when={imports.loaded()}>
        <Show
          when={imports.rows.length > 0}
          fallback={<EmptyState>No imports. Runs only see this repository.</EmptyState>}
        >
          <ul class="import-rows">
            <For each={imports.rows}>
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
                    aria-label={removeLabel(imp.name)}
                    aria-busy={busy().has(imp.id) ? 'true' : undefined}
                    onClick={() => void remove(imp)}
                  >
                    {busy().has(imp.id) ? 'Removing…' : 'Remove'}
                  </button>
                </li>
              )}
            </For>
          </ul>
        </Show>
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
          <button type="submit" aria-busy={busy().has('add') ? 'true' : undefined}>
            {busy().has('add') ? 'Adding…' : 'Add'}
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
