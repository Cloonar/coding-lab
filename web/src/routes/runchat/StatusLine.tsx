// The Chat status line (issue #58 §2): one worded line docked above the
// composer whenever the run is live, its transcript is available and no
// dialog is pending. It replaces the header's state badge (gone at every
// width) and its inline turn Interrupt: the state is always named in WORDS —
// "Working", "Waiting for you", "Idle" — with the dot as a redundant cue
// (filled + pulsing / filled / hollow), never colour alone.
//
//   working     pulsing dot · "Working" · the running tool call's title ·
//               elapsed since the turn began — right: one-tap Interrupt
//   needs_input dot · "Waiting for you" · the reason when the server names
//               one (state_detail, issue #79: "permission request", a
//               permission prompt's own text…; ellipsis-clipped like the
//               tool title) (the in-stream "…is waiting for your reply."
//               line stays)
//   idle / ''   hollow dot · "Idle" · how long ago the last message arrived —
//               right: "Pull base · N behind" while the run is behind its base
//
// The line never gates Send (ADR-0029): it is information plus two shortcuts,
// and the composer below it behaves the same in every state. Interrupt is the
// SAME one-tap action as everywhere else — the composer hands in the
// createInterrupt controller it owns, so the no-confirm contract still lives
// in exactly one place. Pull base is the shared createPullBase action (the
// `/pull-base` lab command down the ordinary reply path), not an endpoint.

import { Match, Show, Switch, createEffect, createMemo, createSignal, onCleanup } from 'solid-js';
import type { ChatMessage, ConversationState } from '../../api';
import Icon from '../../components/Icon';
import { createPullBase } from './pullBase';

/** `0:23`, `12:05`, `1:02:03` — a running turn's elapsed time. */
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (n: number) => String(n).padStart(2, '0');
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

/** `just now`, `4m ago`, `3h ago`, `2d ago` — how long the run has idled. */
export function formatAgo(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000)); // a skewed future stamp reads "just now"
  if (s < 60) return 'just now';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

const stamp = (m: ChatMessage): number | null => {
  if (m.time === undefined || m.time === '') return null;
  const t = Date.parse(m.time);
  return Number.isNaN(t) ? null : t;
};

/**
 * The title of the tool call running right now: the LAST tool message still
 * `running` (an earlier one stuck at running — a lost back-patch — must not
 * shadow the live one). Null when nothing runs, e.g. while the agent writes.
 */
export function runningToolTitle(messages: readonly ChatMessage[]): string | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m?.kind === 'tool' && m.tool?.status === 'running') {
      const title = m.tool.title !== '' ? m.tool.title : m.tool.name;
      return title !== '' ? title : null;
    }
  }
  return null;
}

/**
 * When the running turn began: the latest operator (user-role text) message's
 * timestamp — every turn opens with one, the first prompt included. Null when
 * the loaded window holds no such message or it carries no usable time; the
 * line then simply omits the elapsed time rather than guess.
 */
export function turnStartTime(messages: readonly ChatMessage[]): number | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m?.kind === 'text' && m.role === 'user') return stamp(m);
  }
  return null;
}

/** When the newest message carrying a usable timestamp arrived. */
export function lastMessageTime(messages: readonly ChatMessage[]): number | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    const t = m === undefined ? null : stamp(m);
    if (t !== null) return t;
  }
  return null;
}

type StatusMode = 'working' | 'waiting' | 'idle';

/** Elapsed ticks once a second; the idle "ago" only needs a slow refresh. */
const WORKING_TICK_MS = 1_000;
const IDLE_TICK_MS = 30_000;

export function StatusLine(props: {
  runID: string;
  state: ConversationState;
  /** The reason behind `state` (issue #79); '' or absent renders nothing. */
  stateDetail?: string;
  messages: readonly ChatMessage[];
  /** Run.commits_behind — 0 when absent (nothing to report, issue #149). */
  commitsBehind: number;
  /** The composer's createInterrupt controller (the one-tap contract). */
  interrupt: { busy: () => boolean; run: () => Promise<void> };
  onError: (message: string) => void;
  /** A reply's informational notice (issue #149) — /pull-base's "already up to date…". */
  onNotice: (message: string) => void;
  /** After a Pull base: refetch the run (commits_behind) and the stream. */
  onPulled: () => void;
}) {
  // 'idle' covers '' (no state composed yet) and any other residual state —
  // 'question' and 'ended' never reach the line (the composer's earlier
  // branches own them).
  const mode = (): StatusMode =>
    props.state === 'working' ? 'working' : props.state === 'needs_input' ? 'waiting' : 'idle';

  const toolTitle = createMemo(() => runningToolTitle(props.messages));
  const turnStart = createMemo(() => turnStartTime(props.messages));
  const lastAt = createMemo(() => lastMessageTime(props.messages));

  // A wall clock that only ticks while it is shown: once a second while
  // working (elapsed), every 30s while idle ("4m ago"), never while waiting
  // or without a timestamp to measure from. The effect re-runs on a mode or
  // anchor change and its cleanup clears the previous interval.
  const [now, setNow] = createSignal(Date.now());
  createEffect(() => {
    const m = mode();
    const anchor = m === 'working' ? turnStart() : m === 'idle' ? lastAt() : null;
    if (anchor === null) return;
    setNow(Date.now());
    const id = setInterval(
      () => setNow(Date.now()),
      m === 'working' ? WORKING_TICK_MS : IDLE_TICK_MS,
    );
    onCleanup(() => clearInterval(id));
  });

  const label = () =>
    mode() === 'working' ? 'Working' : mode() === 'waiting' ? 'Waiting for you' : 'Idle';
  // Working: what runs right now. Clipped first when the row is tight.
  const title = (): string | null => (mode() === 'working' ? toolTitle() : null);
  // Waiting: why (issue #79). Only when non-empty, so no dangling separator.
  const reason = (): string | null => {
    if (mode() !== 'waiting') return null;
    const d = (props.stateDetail ?? '').trim();
    return d === '' ? null : d;
  };
  // The time part — elapsed while working, "4m ago" while idle — never clips.
  const time = (): string | null => {
    if (mode() === 'working') {
      const start = turnStart();
      return start === null ? null : formatElapsed(now() - start);
    }
    if (mode() === 'idle') {
      const at = lastAt();
      return at === null ? null : formatAgo(now() - at);
    }
    return null;
  };

  const pull = createPullBase(
    () => props.runID,
    (m) => props.onError(m),
    (m) => props.onNotice(m),
    () => props.onPulled(),
  );

  return (
    <div class="chat-status" data-state={mode()}>
      <span class={`chat-status-dot ${mode()}`} aria-hidden="true" />
      {/* One text run so the line reads "Working · go build · 0:23" to a
          screen reader too. Only the tool title clips (ellipsis); the label,
          the time and the action never do. */}
      <span class="chat-status-text">
        <b class="chat-status-label">{label()}</b>
        <Show when={title()}>
          {(t) => <span class="chat-status-detail chat-status-title"> · {t()}</span>}
        </Show>
        <Show when={reason()}>
          {(r) => (
            <span class="chat-status-detail chat-status-title chat-status-reason" title={r()}>
              {' · '}
              {r()}
            </span>
          )}
        </Show>
        <Show when={time()}>
          {(t) => <span class="chat-status-detail chat-status-time"> · {t()}</span>}
        </Show>
      </span>
      <Switch>
        {/* Working: the one-tap turn Interrupt (ADR-0029), in thumb reach.
            The derived `working` can be a stale-transcript false positive
            (issue #38), so the ••• menu keeps its own live-gated Interrupt;
            this one is the convenient copy where the state says it matters. */}
        <Match when={mode() === 'working'}>
          <button
            type="button"
            class="chat-status-action chat-status-interrupt"
            classList={{ busy: props.interrupt.busy() }}
            aria-busy={props.interrupt.busy()}
            onClick={() => void props.interrupt.run()}
          >
            <Icon name="pause" size={16} />
            <span>Interrupt</span>
          </button>
        </Match>
        {/* Idle and behind the base: Pull base, the same action Run details
            offers — only while there IS something to pull. */}
        <Match when={mode() === 'idle' && props.commitsBehind > 0}>
          <button
            type="button"
            class="chat-status-action chat-status-pull"
            classList={{ busy: pull.busy() }}
            disabled={pull.busy()}
            onClick={() => void pull.run()}
          >
            <Icon name="git-branch" size={16} />
            <span>Pull base · {props.commitsBehind} behind</span>
          </button>
        </Match>
      </Switch>
    </div>
  );
}
