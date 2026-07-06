// Parked work per repo: managed branches the guarded teardown preserved.
// Rendered as a collapsed strip (nothing at all while empty, v0 parity);
// entries show the dirty badge, ahead/unpushed counts and the worktree path.
// Discard is the ONE unguarded destruction in lab — the dialog says so
// honestly and only arms the button once the exact branch name is typed.

import { For, Show, createResource, createSignal, onCleanup } from 'solid-js';
import { discardParked, errorMessage, listParked, type ParkedEntry } from '../api';
import { useEvents } from '../events';
import ErrorBanner from './ErrorBanner';

export default function ParkedSection(props: { repoID: string }) {
  const events = useEvents();
  const [parked, { refetch }] = createResource(
    () => props.repoID,
    (repoID) => listParked(repoID),
  );
  onCleanup(
    // eslint-disable-next-line solid/reactivity -- the handler re-reads props.repoID fresh on every SSE event
    events.subscribe('parked.changed', (event) => {
      if (event.repoID === props.repoID) void refetch();
    }),
  );

  const [error, setError] = createSignal<string | null>(null);
  const [confirming, setConfirming] = createSignal<string | null>(null);

  const discard = async (branch: string) => {
    setError(null);
    try {
      await discardParked(props.repoID, branch);
      setConfirming(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      void refetch();
    }
  };

  return (
    <Show when={(parked()?.length ?? 0) > 0}>
      <details class="parked">
        <summary>
          Parked · <span class="parked-count">{parked()!.length}</span>
        </summary>
        <ErrorBanner message={error()} onDismiss={() => setError(null)} />
        <ul class="parked-list">
          <For each={parked()}>
            {(entry) => (
              <li class="parked-entry">
                <div class="parked-line">
                  <span class="mono parked-branch">{entry.branch}</span>
                  <Show when={entry.dirty}>
                    <span class="chip status-error">● dirty</span>
                  </Show>
                  <Show when={entry.commits_ahead > 0}>
                    <span class="chip" title={`${entry.commits_ahead} commit(s) ahead`}>
                      ↑{entry.commits_ahead}
                    </span>
                  </Show>
                  <Show when={entry.unpushed > 0}>
                    <span class="chip status-cloning">⚠ {entry.unpushed} unpushed</span>
                  </Show>
                  <span class="spacer" />
                  <button
                    type="button"
                    class="danger parked-discard"
                    onClick={() =>
                      setConfirming(confirming() === entry.branch ? null : entry.branch)
                    }
                  >
                    Discard
                  </button>
                </div>
                <Show when={entry.worktree_path !== ''}>
                  <p class="muted mono parked-path">{entry.worktree_path}</p>
                </Show>
                <Show when={confirming() === entry.branch}>
                  <DiscardConfirm
                    entry={entry}
                    onCancel={() => setConfirming(null)}
                    onDiscard={() => void discard(entry.branch)}
                  />
                </Show>
              </li>
            )}
          </For>
        </ul>
      </details>
    </Show>
  );
}

/**
 * Typed-confirm gate: Discard stays disabled until the operator types the
 * branch name exactly. No trimming leniency — this is the unguarded path.
 */
function DiscardConfirm(props: {
  entry: ParkedEntry;
  onCancel: () => void;
  onDiscard: () => void;
}) {
  const [typed, setTyped] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const armed = () => typed() === props.entry.branch;

  return (
    <div class="discard-confirm" role="alertdialog" aria-label={`Discard ${props.entry.branch}`}>
      <p class="discard-warning">
        This permanently deletes the branch
        {props.entry.worktree_path !== '' ? ' and its worktree' : ''}
        {props.entry.dirty ? ', including uncommitted changes' : ''}
        {props.entry.unpushed > 0
          ? ` and ${props.entry.unpushed} unpushed commit${props.entry.unpushed === 1 ? '' : 's'}`
          : ''}
        . No safety checks apply and there is no undo.
      </p>
      <label class="field">
        <span>
          Type <span class="mono">{props.entry.branch}</span> to confirm
        </span>
        <input
          name="confirm-branch"
          class="mono"
          value={typed()}
          onInput={(e) => setTyped(e.currentTarget.value)}
          autocomplete="off"
          spellcheck={false}
        />
      </label>
      <div class="card-actions">
        <button
          type="button"
          class="danger"
          disabled={!armed() || busy()}
          onClick={() => {
            setBusy(true);
            props.onDiscard();
          }}
        >
          {busy() ? 'Discarding…' : 'Discard forever'}
        </button>
        <button type="button" onClick={() => props.onCancel()} disabled={busy()}>
          Cancel
        </button>
      </div>
    </div>
  );
}
