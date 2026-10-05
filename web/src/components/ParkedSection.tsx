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
//
// Rows render from a store reconciled by branch, so a parked.changed refetch
// (fresh objects for every entry) patches them in place; the open
// confirmation, the text typed into it and its busy state live here, keyed
// by branch, so no refetch can wipe them. Focus: Cancel or Escape returns it
// to that entry's Discard; after a discard the entry is gone, so it moves to
// the next entry's Discard, else the card's heading.

import { For, Show, createComputed, createSignal, createUniqueId } from 'solid-js';
import { createStore, reconcile } from 'solid-js/store';
import { discardParked, errorMessage, listParked, type ParkedEntry } from '../api';
import { rescueFocus } from '../lib/focus';
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
  // The branch whose confirmation is open, what was typed into it, and the
  // branch being discarded right now — all above the rows (see the header).
  const [confirming, setConfirming] = createSignal<string | null>(null);
  const [typed, setTyped] = createSignal<Record<string, string>>({});
  const [busy, setBusy] = createSignal<string | null>(null);
  const headingId = `parked-${createUniqueId()}`;
  let heading: HTMLHeadingElement | undefined;
  const triggers = new Map<string, HTMLButtonElement>();

  // One store object per branch. `loaded` stays true after a failed refetch
  // (the card keeps its last list); an endpoint that never answered (it is
  // not mounted) leaves the card out entirely.
  const [rows, setRows] = createStore<ParkedEntry[]>([]);
  const [loaded, setLoaded] = createSignal(false);
  createComputed(() => {
    const next = resourceValue(parked);
    if (next === undefined) return;
    setRows(reconcile(next, { key: 'branch' }));
    setLoaded(true);
  });
  const count = () => rows.length;

  const typedFor = (branch: string) => typed()[branch] ?? '';
  const setTypedFor = (branch: string, value: string) =>
    setTyped((prev) => ({ ...prev, [branch]: value }));
  const forget = (branch: string) =>
    setTyped((prev) => {
      const next = { ...prev };
      delete next[branch];
      return next;
    });

  const open = (branch: string) => setConfirming(branch);
  const cancel = (branch: string) => {
    setConfirming(null);
    forget(branch);
    triggers.get(branch)?.focus();
  };

  const discard = async (branch: string) => {
    if (busy() !== null) return;
    const repoID = props.repoID;
    const onDiscarded = props.onDiscarded;
    // Where focus goes once this entry is gone: the next entry, else the
    // previous one, else the heading.
    const index = rows.findIndex((entry) => entry.branch === branch);
    const neighbour = rows[index + 1]?.branch ?? rows[index - 1]?.branch;
    setError(null);
    setBusy(branch);
    try {
      await discardParked(repoID, branch);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(null);
      void refetch();
      return;
    }
    setBusy(null);
    setConfirming(null);
    forget(branch);
    onDiscarded?.(branch);
    // The confirmation (and the button that had focus) closed: back to the
    // entry's Discard while it is still listed…
    triggers.get(branch)?.focus();
    try {
      await refetch();
    } catch {
      // The card keeps its last list.
    }
    // …and once the entry is gone, to its neighbour or the heading.
    rescueFocus(neighbour === undefined ? undefined : triggers.get(neighbour), heading);
  };

  return (
    <Show when={loaded()}>
      <section class="overview-card parked-card" aria-labelledby={headingId}>
        <div class="overview-card-head">
          <h2 id={headingId} tabIndex={-1} ref={heading}>
            Parked work
            <span class="visually-hidden"> ({count()})</span>
          </h2>
          <span class="spacer" />
          <span class="count parked-count" aria-hidden="true">
            {count()}
          </span>
        </div>
        <Banner message={error()} onDismiss={() => setError(null)} />
        <Show when={count() > 0} fallback={<p class="muted overview-empty">Nothing parked.</p>}>
          <ul class="parked-list">
            <For each={rows}>
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
                      ref={(el) => triggers.set(entry.branch, el)}
                      aria-expanded={confirming() === entry.branch}
                      aria-label={`Discard ${entry.branch}`}
                      disabled={busy() === entry.branch}
                      onClick={() =>
                        confirming() === entry.branch ? cancel(entry.branch) : open(entry.branch)
                      }
                    >
                      Discard
                    </button>
                  </div>
                  <Show when={confirming() === entry.branch}>
                    <DiscardConfirm
                      entry={entry}
                      typed={typedFor(entry.branch)}
                      onType={(value) => setTypedFor(entry.branch, value)}
                      busy={busy() === entry.branch}
                      onCancel={() => cancel(entry.branch)}
                      onDiscard={() => void discard(entry.branch)}
                    />
                  </Show>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </section>
    </Show>
  );
}

/**
 * Typed-confirm gate: Discard stays disabled until the operator types the
 * branch name exactly. No trimming leniency — this is the unguarded path.
 * The typed text and the busy state are the parent's (kept per branch).
 */
function DiscardConfirm(props: {
  entry: ParkedEntry;
  typed: string;
  onType: (value: string) => void;
  busy: boolean;
  onCancel: () => void;
  onDiscard: () => void;
}) {
  const armed = () => props.typed === props.entry.branch;
  let input: HTMLInputElement | undefined;

  return (
    <div
      class="discard-confirm"
      role="group"
      aria-label={`Discard ${props.entry.branch}`}
      ref={(el) => queueMicrotask(() => (el.isConnected ? input?.focus() : undefined))}
      onKeyDown={(event) => {
        if (event.key !== 'Escape' || props.busy) return;
        event.preventDefault();
        event.stopPropagation();
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
          value={props.typed}
          onInput={(e) => props.onType(e.currentTarget.value)}
          autocomplete="off"
          spellcheck={false}
        />
      </label>
      <div class="discard-actions">
        <button type="button" onClick={() => props.onCancel()} disabled={props.busy}>
          Cancel
        </button>
        <button
          type="button"
          class="solid-danger"
          disabled={!armed() || props.busy}
          onClick={() => props.onDiscard()}
        >
          {props.busy ? 'Discarding…' : 'Discard forever'}
        </button>
      </div>
    </div>
  );
}
