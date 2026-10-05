// Danger zone (issue #61): what deleting this repository removes, and the
// "Delete repository" button that opens the in-page delete dialog (a bottom
// sheet below 1024px, a centered dialog from 1024px — the shared Dialog). No
// browser confirm, and no force checkbox that appears after a refused attempt.
//
// The section renders its card only: the one-page settings supplies the
// section heading ("Danger zone") above it, as it does for every section.
//
// Opening the dialog first loads what the delete would touch — the repos that
// import this one, the live instances, parked work, Schedules and secrets —
// so the dialog says everything before any attempt:
// - Other repos import this one: the server refuses that delete even when
//   forced, so the dialog names the importers, offers no way to delete and
//   links to the first importer's Imports section.
// - Otherwise it lists only the consequences that apply, with counts. A count
//   that cannot be loaded (an optional endpoint answering 404/501, a network
//   error) is left out — never shown as 0 — and never blocks the dialog. The
//   operator types the repo name to enable Delete; with live instances or a
//   running clone the confirmation sends the forced delete at once, because
//   the operator has just read that consequence. A plain delete the server
//   still refuses with a 409 (state changed meanwhile) shows the server's
//   message in the dialog and reloads the consequences — it is never retried
//   with force behind the operator's back.
// After the delete the app returns to the list, which confirms by name.

import { A, useNavigate } from '@solidjs/router';
import { For, Show, createEffect, createSignal, on, onCleanup } from 'solid-js';
import type { Accessor } from 'solid-js';
import {
  ApiError,
  deleteRepo,
  errorMessage,
  listInstances,
  listParked,
  listRepoImporters,
  listRepoSchedules,
  listRepoSecrets,
  type RepoImport,
  type Repo,
} from '../../../api';
import Banner from '../../../components/Banner';
import Dialog from '../../../components/Dialog';
import { parseRemote } from '../../../lib/remoteUrl';
import { deleteConsequences, needsForce, type DeleteFacts } from '../../../lib/repoDelete';
import { noticeState } from '../../../lib/routeNotice';

/** What the dialog loaded; null = that lookup failed and its line is left out. */
interface Loaded {
  importers: RepoImport[] | null;
  live: number | null;
  parked: DeleteFacts['parked'];
  schedules: number | null;
  secrets: number | null;
}

function settled<T>(result: PromiseSettledResult<T>): T | null {
  return result.status === 'fulfilled' ? result.value : null;
}

async function loadFacts(repoID: string): Promise<Loaded> {
  const [importers, instances, parked, schedules, secrets] = await Promise.allSettled([
    listRepoImporters(repoID),
    listInstances(),
    listParked(repoID),
    listRepoSchedules(repoID),
    listRepoSecrets(repoID),
  ]);
  const live = settled(instances);
  return {
    importers: settled(importers),
    live: live === null ? null : live.filter((i) => i.live && i.repo_id === repoID).length,
    parked: settled(parked),
    schedules: settled(schedules)?.length ?? null,
    secrets: settled(secrets)?.length ?? null,
  };
}

export default function DangerZone(props: { repo: Accessor<Repo> }) {
  const navigate = useNavigate();
  const [open, setOpen] = createSignal(false);
  const [checking, setChecking] = createSignal(false);
  const [loaded, setLoaded] = createSignal<Loaded | null>(null);
  const [typed, setTyped] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  // Reloading the consequences after a refusal: Delete waits for the new list.
  const [refreshing, setRefreshing] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  let nameInput: HTMLInputElement | undefined;
  let closeButton: HTMLButtonElement | undefined;
  // Bumped on every load and on close, so a lookup that answers after the
  // dialog closed (or after a newer lookup started) is dropped.
  let generation = 0;
  let alive = true;
  onCleanup(() => {
    alive = false;
  });

  // The frame can stay mounted while the route moves to another repo: a
  // dialog opened for the previous one closes rather than speak for it.
  createEffect(
    on(
      () => props.repo().id,
      () => {
        generation += 1;
        setChecking(false);
        setOpen(false);
        setBusy(false);
        setRefreshing(false);
        setError(null);
        setTyped('');
      },
      { defer: true },
    ),
  );

  const name = () => props.repo().name;
  const importers = () => loaded()?.importers ?? [];
  const blocked = () => importers().length > 0;
  const facts = (): DeleteFacts => {
    const l = loaded();
    return {
      cloneStatus: props.repo().clone_status,
      trackerBinding: props.repo().tracker_binding,
      live: l?.live ?? null,
      parked: l?.parked ?? null,
      schedules: l?.schedules ?? null,
      secrets: l?.secrets ?? null,
    };
  };
  const matches = () => typed() === name();
  const remoteSentence = () => {
    const host = parseRemote(props.repo().remote_url)?.host ?? null;
    return host !== null ? `The remote on ${host} is not touched.` : 'The remote is not touched.';
  };

  const load = async (): Promise<boolean> => {
    const mine = ++generation;
    const result = await loadFacts(props.repo().id);
    if (!alive || mine !== generation) return false;
    setLoaded(result);
    return true;
  };

  const openDialog = async () => {
    if (checking() || open()) return;
    setChecking(true);
    setError(null);
    setTyped('');
    const ok = await load();
    if (!alive) return;
    setChecking(false);
    if (ok) setOpen(true);
  };

  const close = () => {
    if (busy()) return;
    generation += 1;
    setOpen(false);
    setError(null);
    setTyped('');
  };

  const confirm = async () => {
    if (!matches() || busy() || refreshing()) return;
    const target = props.repo();
    const force = needsForce(facts());
    setBusy(true);
    setError(null);
    try {
      await deleteRepo(target.id, force);
      if (!alive) return;
      navigate('/repos', { state: noticeState(`Deleted ${target.name} from lab`) });
    } catch (err) {
      if (!alive) return;
      setError(errorMessage(err));
      setBusy(false);
      // The server saw something the dialog did not (a run started, an
      // import was declared): show its reason and what applies now.
      if (err instanceof ApiError && err.status === 409) {
        setRefreshing(true);
        await load();
        if (!alive) return;
        setRefreshing(false);
        if (blocked()) closeButton?.focus();
      }
    }
  };

  return (
    <>
      <div class="card danger-zone danger-box">
        <p>
          Removes <b>{name()}</b> from lab, with its clone, settings, run history, Schedules and
          secrets. The remote repository is not touched.
        </p>
        <button
          type="button"
          class="danger"
          aria-haspopup="dialog"
          aria-busy={checking() ? 'true' : undefined}
          onClick={() => void openDialog()}
        >
          {checking() ? 'Checking…' : 'Delete repository'}
        </button>
      </div>

      {/* A sibling of the card, so the card's danger heading colour never
          reaches the dialog's title. */}
      <Dialog
        open={open()}
        onClose={close}
        role="alertdialog"
        dismissable={!busy()}
        class={blocked() ? 'repo-delete-dialog delete-blocked-dialog' : 'repo-delete-dialog'}
        initialFocus={() => (blocked() ? closeButton : nameInput)}
        title={blocked() ? `${name()} cannot be deleted yet` : `Delete ${name()}?`}
        actions={
          <Show
            when={blocked()}
            fallback={
              <>
                <button type="button" onClick={close} disabled={busy()}>
                  Cancel
                </button>
                <button
                  type="button"
                  class="solid-danger"
                  disabled={!matches() || busy() || refreshing()}
                  onClick={() => void confirm()}
                >
                  {busy() ? 'Deleting…' : 'Delete repository'}
                </button>
              </>
            }
          >
            <button type="button" ref={closeButton} onClick={close}>
              Close
            </button>
            <A
              href={`/repos/${importers()[0]?.id ?? ''}/settings/imports`}
              class="link-button primary"
              onClick={close}
            >
              Open {importers()[0]?.name} imports
            </A>
          </Show>
        }
      >
        <Banner message={error()} onDismiss={() => setError(null)} />
        <Show
          when={blocked()}
          fallback={
            <>
              <ul class="delete-consequences">
                <For each={deleteConsequences(facts())}>{(line) => <li>{line}</li>}</For>
              </ul>
              <p class="muted">{remoteSentence()} This cannot be undone.</p>
              <div class="field delete-confirm">
                <label for="delete-repo-name">
                  Type <code>{name()}</code> to confirm
                </label>
                <input
                  ref={nameInput}
                  id="delete-repo-name"
                  name="confirm_name"
                  type="text"
                  class="mono"
                  autocomplete="off"
                  autocapitalize="off"
                  spellcheck={false}
                  value={typed()}
                  disabled={busy()}
                  onInput={(e) => setTyped(e.currentTarget.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault();
                      void confirm();
                    }
                  }}
                />
              </div>
            </>
          }
        >
          <p class="delete-blocked">
            <For each={importers()}>
              {(importer, index) => (
                <>
                  <Show when={index() > 0}>
                    {index() === importers().length - 1 ? ' and ' : ', '}
                  </Show>
                  <b>{importer.name}</b>
                </>
              )}
            </For>{' '}
            {importers().length === 1
              ? 'imports this repository. Remove that import first.'
              : 'import this repository. Remove those imports first.'}
          </p>
        </Show>
      </Dialog>
    </>
  );
}
