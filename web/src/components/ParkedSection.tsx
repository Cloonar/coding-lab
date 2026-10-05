// Parked work per repo: managed branches the guarded teardown preserved. The
// repo home's Overview shows it as a card (issue #61): the heading with the
// count, one row per entry — the branch, what is in it in words (uncommitted
// changes, commits ahead, unpushed commits) and the worktree path — or
// "Nothing parked.". If the parked endpoint is unavailable (it is mounted only
// when the server wires reconcile), the block renders nothing at all.
//
// Discard is the ONE unguarded destruction in lab, so it asks in place before
// it acts: the confirmation opens under the entry, says so honestly and only
// arms the button once the exact branch name is typed.

import { For, Show, createSignal, createUniqueId } from 'solid-js';
import { discardParked, errorMessage, listParked, type ParkedEntry } from '../api';
import { createLiveResource } from '../lib/liveResource';
import { resourceValue } from '../lib/resource';
import Banner from './Banner';

/**
 * What a parked entry holds, in words: "Worktree has uncommitted changes ·
 * 2 commits ahead · 1 unpushed commit". An entry with none of these is a
 * branch the teardown kept because it is not merged.
 */
export function parkedSummary(entry: ParkedEntry): string {
  const parts: string[] = [];
  if (entry.dirty) {
    parts.push(
      entry.worktree_path !== '' ? 'Worktree has uncommitted changes' : 'Uncommitted changes',
    );
  }
  if (entry.commits_ahead > 0) {
    parts.push(`${entry.commits_ahead} commit${entry.commits_ahead === 1 ? '' : 's'} ahead`);
  }
  if (entry.unpushed > 0) {
    parts.push(`${entry.unpushed} unpushed commit${entry.unpushed === 1 ? '' : 's'}`);
  }
  return parts.length === 0 ? 'Branch is not merged' : parts.join(' · ');
}

export default function ParkedSection(props: {
  repoID: string;
  /** A discard succeeded — the parent confirms it (a toast). */
  onDiscarded?: (branch: string) => void;
}) {
  const [parked, { refetch }] = createLiveResource(
    () => props.repoID,
    (repoID) => listParked(repoID),
    [{ type: 'parked.changed', match: (event) => event.repoID === props.repoID }],
  );

  const [error, setError] = createSignal<string | null>(null);
  const [confirming, setConfirming] = createSignal<string | null>(null);
  const headingId = `parked-${createUniqueId()}`;

  const entries = (): ParkedEntry[] | undefined => resourceValue(parked);
  const count = () => entries()?.length ?? 0;

  const discard = async (branch: string) => {
    setError(null);
    try {
      await discardParked(props.repoID, branch);
      setConfirming(null);
      props.onDiscarded?.(branch);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      void refetch();
    }
  };

  return (
    <Show when={entries()}>
      {(list) => (
        <section class="overview-card parked-card" aria-labelledby={headingId}>
          <div class="overview-card-head">
            <h2 id={headingId}>
              Parked work
              <span class="visually-hidden"> ({count()})</span>
            </h2>
            <span class="spacer" />
            <span class="count parked-count" aria-hidden="true">
              {count()}
            </span>
          </div>
          <Banner message={error()} onDismiss={() => setError(null)} />
          <Show
            when={list().length > 0}
            fallback={<p class="muted overview-empty">Nothing parked.</p>}
          >
            <ul class="parked-list">
              <For each={list()}>
                {(entry) => (
                  <li class="parked-entry">
                    <div class="parked-line">
                      <span class="parked-text">
                        <span class="mono parked-branch">{entry.branch}</span>
                        <span class="parked-state">{parkedSummary(entry)}</span>
                        <Show when={entry.worktree_path !== ''}>
                          <span class="muted mono parked-path">{entry.worktree_path}</span>
                        </Show>
                      </span>
                      <button
                        type="button"
                        class="danger parked-discard"
                        aria-expanded={confirming() === entry.branch}
                        aria-label={`Discard ${entry.branch}`}
                        onClick={() =>
                          setConfirming(confirming() === entry.branch ? null : entry.branch)
                        }
                      >
                        Discard
                      </button>
                    </div>
                    <Show when={confirming() === entry.branch}>
                      <DiscardConfirm
                        entry={entry}
                        onCancel={() => setConfirming(null)}
                        onDiscard={() => discard(entry.branch)}
                      />
                    </Show>
                  </li>
                )}
              </For>
            </ul>
          </Show>
        </section>
      )}
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
  onDiscard: () => Promise<void>;
}) {
  const [typed, setTyped] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const armed = () => typed() === props.entry.branch;
  let input: HTMLInputElement | undefined;

  return (
    <div
      class="discard-confirm"
      role="group"
      aria-label={`Discard ${props.entry.branch}`}
      ref={(el) => queueMicrotask(() => (el.isConnected ? input?.focus() : undefined))}
      onKeyDown={(event) => {
        if (event.key !== 'Escape' || busy()) return;
        event.preventDefault();
        props.onCancel();
      }}
    >
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
        {/* <code>, not a span: `.field span` would turn the branch into a block. */}
        <span>
          Type <code class="discard-branch">{props.entry.branch}</code> to confirm
        </span>
        <input
          ref={input}
          name="confirm-branch"
          class="mono"
          value={typed()}
          onInput={(e) => setTyped(e.currentTarget.value)}
          autocomplete="off"
          spellcheck={false}
        />
      </label>
      <div class="discard-actions">
        <button type="button" onClick={() => props.onCancel()} disabled={busy()}>
          Cancel
        </button>
        <button
          type="button"
          class="solid-danger"
          disabled={!armed() || busy()}
          onClick={() => {
            setBusy(true);
            void props.onDiscard().finally(() => setBusy(false));
          }}
        >
          {busy() ? 'Discarding…' : 'Discard forever'}
        </button>
      </div>
    </div>
  );
}
