// The Needs you block above the repositories list (issue #61): every problem
// the operator has to act on, one entry each, with the action that fixes it —
// Retry for a failed clone, Reset for a three-strikes pause, Fix for a failing
// readiness check (it opens the offending field). The caller renders nothing
// when there is nothing to list. Severity is a word for assistive tech plus an
// icon, never colour alone.

import { A, useNavigate } from '@solidjs/router';
import { For, Show, createSignal } from 'solid-js';
import Icon from '../../components/Icon';
import { checkTitle, fixHref } from '../../lib/readiness';
import type { NeedsYouEntry } from '../../lib/repoList';

export default function NeedsYou(props: {
  entries: NeedsYouEntry[];
  onRetry: (entry: NeedsYouEntry) => Promise<void>;
  onReset: (entry: NeedsYouEntry) => Promise<void>;
}) {
  return (
    <section class="needs-you" aria-labelledby="needs-you-heading">
      <div class="repos-eyebrow-row">
        <h2 class="repos-eyebrow" id="needs-you-heading">
          Needs you
          <span class="visually-hidden"> ({props.entries.length})</span>
        </h2>
        <span class="count" aria-hidden="true">
          {props.entries.length}
        </span>
      </div>
      <ul class="needs-you-list">
        <For each={props.entries}>
          {(entry) => (
            <NeedsYouItem entry={entry} onRetry={props.onRetry} onReset={props.onReset} />
          )}
        </For>
      </ul>
    </section>
  );
}

function NeedsYouItem(props: {
  entry: NeedsYouEntry;
  onRetry: (entry: NeedsYouEntry) => Promise<void>;
  onReset: (entry: NeedsYouEntry) => Promise<void>;
}) {
  const navigate = useNavigate();
  const [busy, setBusy] = createSignal(false);
  // A pause holds runs back (a warning); a failed clone or check blocks them.
  const warning = () => props.entry.kind === 'paused';

  const run = async (action: (entry: NeedsYouEntry) => Promise<void>) => {
    setBusy(true);
    try {
      await action(props.entry);
    } finally {
      setBusy(false);
    }
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
        <span class="needs-you-message">
          <span class="visually-hidden">{warning() ? 'Warning: ' : 'Problem: '}</span>
          {props.entry.message}
        </span>
      </span>
      <Show when={props.entry.kind === 'clone'}>
        <button
          type="button"
          class="needs-you-action"
          aria-label={`Retry the clone of ${props.entry.repo.name}`}
          disabled={busy()}
          onClick={() => void run(props.onRetry)}
        >
          {busy() ? 'Retrying…' : 'Retry'}
        </button>
      </Show>
      <Show when={props.entry.kind === 'paused'}>
        <button
          type="button"
          class="needs-you-action"
          aria-label={`Reset AFK in ${props.entry.repo.name}`}
          disabled={busy()}
          onClick={() => void run(props.onReset)}
        >
          {busy() ? 'Resetting…' : 'Reset'}
        </button>
      </Show>
      <Show when={props.entry.kind === 'readiness'}>
        <Show
          when={fix()}
          fallback={
            <A href={`/repos/${props.entry.repo.id}`} class="needs-you-action button-link">
              Open
            </A>
          }
        >
          {(target) => (
            <button
              type="button"
              class="needs-you-action"
              aria-label={`Fix ${checkName()} in ${props.entry.repo.name}`}
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
