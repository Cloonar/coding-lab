// The Needs you block above the repositories list (issue #61): every problem
// the operator has to act on, one entry each, with the action that fixes it —
// Retry for a failed clone, Reset for a three-strikes pause, Fix for a failing
// readiness check (it opens the offending field). The caller renders nothing
// when there is nothing to list. Severity is a word for assistive tech plus an
// icon, never colour alone.
//
// The caller owns the busy state (keyed by entry, so it survives a refetch
// that re-renders the entry) and refuses a second send while one is pending;
// a busy button stays focusable (aria-disabled, not disabled) so focus does
// not drop out of the block mid-request, and its new wording is announced
// through the block's status line. Each action is described by its entry's
// message, so two Fix buttons are told apart by the problem they fix.

import { A, useNavigate } from '@solidjs/router';
import { For, Show, createUniqueId } from 'solid-js';
import Icon from '../../components/Icon';
import { checkTitle, fixHref } from '../../lib/readiness';
import type { NeedsYouEntry } from '../../lib/repoList';

export default function NeedsYou(props: {
  entries: NeedsYouEntry[];
  /** Whether this entry's Retry/Reset is in flight (the caller's state). */
  busy: (entry: NeedsYouEntry) => boolean;
  onRetry: (entry: NeedsYouEntry) => Promise<void>;
  onReset: (entry: NeedsYouEntry) => Promise<void>;
  /** The block's heading (tabindex="-1"): where focus goes when a fixed entry leaves. */
  headingRef?: (el: HTMLHeadingElement) => void;
}) {
  // What is in flight, in words, for screen readers: the busy button's own
  // name change is not reliably announced.
  const status = () =>
    props.entries
      .filter((entry) => props.busy(entry))
      .map((entry) =>
        entry.kind === 'clone'
          ? `Retrying the clone of ${entry.repo.name}…`
          : `Resetting AFK in ${entry.repo.name}…`,
      )
      .join(' ');

  return (
    <section class="needs-you" aria-labelledby="needs-you-heading">
      <div class="repos-eyebrow-row">
        <h2
          class="repos-eyebrow"
          id="needs-you-heading"
          tabIndex={-1}
          ref={(el) => props.headingRef?.(el)}
        >
          Needs you
          <span class="visually-hidden"> ({props.entries.length})</span>
        </h2>
        <span class="count" aria-hidden="true">
          {props.entries.length}
        </span>
      </div>
      <p class="visually-hidden" role="status">
        {status()}
      </p>
      <ul class="needs-you-list">
        <For each={props.entries}>
          {(entry) => (
            <NeedsYouItem
              entry={entry}
              busy={props.busy(entry)}
              onRetry={props.onRetry}
              onReset={props.onReset}
            />
          )}
        </For>
      </ul>
    </section>
  );
}

function NeedsYouItem(props: {
  entry: NeedsYouEntry;
  busy: boolean;
  onRetry: (entry: NeedsYouEntry) => Promise<void>;
  onReset: (entry: NeedsYouEntry) => Promise<void>;
}) {
  const navigate = useNavigate();
  const messageId = `needs-you-${createUniqueId()}-message`;
  // A pause holds runs back (a warning); a failed clone or check blocks them.
  const warning = () => props.entry.kind === 'paused';

  const run = (action: (entry: NeedsYouEntry) => Promise<void>) => {
    if (props.busy) return; // the caller refuses a second send too
    void action(props.entry);
  };

  const fix = () => (props.entry.kind === 'readiness' ? props.entry.check.fix : undefined);
  const checkName = () =>
    props.entry.kind === 'readiness' ? checkTitle(props.entry.check.id).toLowerCase() : '';

  return (
    <li classList={{ 'needs-you-item': true, warning: warning(), problem: !warning() }}>
      <Icon name={warning() ? 'pause' : 'circle-alert'} size={18} class="needs-you-icon" />
      <span class="needs-you-text">
        <A href={`/repos/${props.entry.repo.id}`} class="needs-you-name">
          {props.entry.repo.name}
        </A>
        <span class="needs-you-message" id={messageId}>
          <span class="visually-hidden">{warning() ? 'Warning: ' : 'Problem: '}</span>
          {props.entry.message}
        </span>
      </span>
      <Show when={props.entry.kind === 'clone'}>
        <button
          type="button"
          class="needs-you-action"
          aria-disabled={props.busy ? 'true' : undefined}
          aria-describedby={messageId}
          onClick={() => run(props.onRetry)}
        >
          {props.busy ? 'Retrying' : 'Retry'}
          <span class="visually-hidden"> the clone of {props.entry.repo.name}</span>
          {props.busy ? '…' : ''}
        </button>
      </Show>
      <Show when={props.entry.kind === 'paused'}>
        <button
          type="button"
          class="needs-you-action"
          aria-disabled={props.busy ? 'true' : undefined}
          aria-describedby={messageId}
          onClick={() => run(props.onReset)}
        >
          {props.busy ? 'Resetting' : 'Reset'}
          <span class="visually-hidden"> AFK in {props.entry.repo.name}</span>
          {props.busy ? '…' : ''}
        </button>
      </Show>
      <Show when={props.entry.kind === 'readiness'}>
        <Show
          when={fix()}
          fallback={
            <A
              href={`/repos/${props.entry.repo.id}`}
              class="needs-you-action button-link"
              aria-describedby={messageId}
            >
              Open<span class="visually-hidden"> {props.entry.repo.name}</span>
            </A>
          }
        >
          {(target) => (
            <button
              type="button"
              class="needs-you-action"
              aria-label={`Fix ${checkName()} in ${props.entry.repo.name}`}
              aria-describedby={messageId}
              onClick={() => navigate(fixHref(props.entry.repo.id, target()))}
            >
              Fix
            </button>
          )}
        </Show>
      </Show>
    </li>
  );
}
