// The instance open affordance (ADR-0017), shared by the dashboard rows and the
// chat header: the exact/fallback web link as an external-link anchor, a
// "connecting…" pulse while deep-link capture runs, or — for a provider with no
// web session — a copyable `tmux attach` command, since that session is driven
// from a terminal on the lab host.
//
// Responsive (issue #15): the link renders the shared .action-icon (external
// link) + .action-label ("Open") pair with a stable accessible name, so it is
// icon-only on phones and in the chat header, and text on the desktop row —
// the CSS breakpoints/context decide which of the two is shown.

import { Match, Switch, createSignal, onCleanup } from 'solid-js';
import type { OpenState } from '../lib/deepLink';
import Icon from './Icon';

type LinkState = Extract<OpenState, { kind: 'link' }>;
type AttachState = Extract<OpenState, { kind: 'attach' }>;

const linkTitle = (l: LinkState): string | undefined => (l.exact ? undefined : l.title);

export default function OpenAffordance(props: { state: OpenState }) {
  const linkState = (): LinkState | false => (props.state.kind === 'link' ? props.state : false);
  const attachState = (): AttachState | false =>
    props.state.kind === 'attach' ? props.state : false;

  return (
    <Switch>
      <Match when={props.state.kind === 'connecting'}>
        <span class="chip connecting pulse" title="Waiting for the deep link">
          connecting…
        </span>
      </Match>
      <Match when={linkState()}>
        {(s) => (
          <a
            href={s().url}
            target="_blank"
            rel="noreferrer"
            class="card-link"
            aria-label="Open the session"
            title={linkTitle(s()) ?? 'Open the session'}
          >
            <Icon name="external-link" class="action-icon" />
            <span class="action-label">Open</span>
          </a>
        )}
      </Match>
      <Match when={attachState()}>
        {(s) => <AttachButton command={s().command} title={s().title} />}
      </Match>
    </Switch>
  );
}

/** A copyable `tmux attach` command; the tooltip also spells out the command. */
function AttachButton(props: { command: string; title: string }) {
  const [copied, setCopied] = createSignal(false);
  let timer: ReturnType<typeof setTimeout> | undefined;
  onCleanup(() => clearTimeout(timer));

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(props.command);
      setCopied(true);
      clearTimeout(timer);
      timer = setTimeout(() => setCopied(false), 2_000);
    } catch {
      // Clipboard unavailable (http, permissions) — the tooltip still shows the command.
    }
  };

  return (
    <button
      type="button"
      class="card-link attach-copy"
      title={`${props.title}: ${props.command}`}
      onClick={() => void copy()}
    >
      {copied() ? 'Copied ✓' : 'Copy attach'}
    </button>
  );
}
