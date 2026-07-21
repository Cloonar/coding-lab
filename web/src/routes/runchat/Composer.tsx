// The fixed bottom composer (ADR-0029, issue #61): Send is always available
// and fires immediately, with the slash-command autocomplete (issue #51
// decision 5, tiered ranking per issue #122), the §4 jump-to-latest pill, and
// the shared one-tap interrupt action (ADR-0029) that backs the header's turn
// Interrupt and the degraded question-state escape hatch.

import { For, Match, Show, Switch, createEffect, createSignal, on } from 'solid-js';
import {
  errorMessage,
  interruptRun,
  replyRun,
  type ConversationState,
  type Dialog,
  type RunCommand,
  type TranscriptStatus,
} from '../../api';
import Icon from '../../components/Icon';
import { isComposerSend } from '../../lib/composerKeys';
import { capitalize } from './shared';

// Shared one-tap interrupt action (ADR-0029): POST /interrupt (the tmux Escape),
// no confirm, re-entrancy-guarded. Backs the header's turn Interrupt (and its
// mobile menu twin) plus the two question/dialog escape-hatch buttons, so the
// "one tap, no confirm" contract lives in exactly one place.
export function createInterrupt(
  runID: () => string,
  onError: (m: string) => void,
  onDone: () => void,
) {
  const [busy, setBusy] = createSignal(false);
  const run = async () => {
    if (busy()) return;
    setBusy(true);
    try {
      await interruptRun(runID());
    } catch (err) {
      onError(errorMessage(err));
    } finally {
      setBusy(false);
      onDone();
    }
  };
  return { busy, run };
}

export function Composer(props: {
  runID: string;
  state: ConversationState;
  ended: boolean;
  transcript: TranscriptStatus;
  dialog: Dialog | null;
  /** The run's chat-safe slash-command catalog (issue #51 decision 5). */
  commands: RunCommand[];
  /** The provider's display name ('the agent' while metadata loads). */
  agentName: string;
  /** "open it at <host>" / "open the session" — the answer-elsewhere hint. */
  openHint: string;
  jumpVisible: boolean;
  jumpEmphasis: boolean;
  onJump: () => void;
  onError: (message: string) => void;
  /** A reply's informational notice (issue #149) — never an error. */
  onNotice: (message: string) => void;
  onSent: () => void;
}) {
  const [text, setText] = createSignal('');
  const [sending, setSending] = createSignal(false);

  // Send is gated only on a non-empty box and no in-flight POST — never on the
  // run's derived state (ADR-0029, issue #61): the composer no longer morphs and
  // has no Interrupt of its own (that moved to the header).
  const canSend = () => !sending() && text().trim() !== '';

  // Auto-grow (decision 9b): reset to one row then grow to the content height,
  // capped by CSS max-height with internal scroll. Driven from two places: the
  // text-signal effect (typing, and the collapse after a send clears the box)
  // and the ref callback below (a fresh mount — including a remount after a
  // mid-turn dialog — that already carries a compose-ahead draft, where the
  // signal is unchanged so the effect won't refire). In jsdom scrollHeight is 0
  // (no layout) so this is a harmless no-op there.
  let inputEl: HTMLTextAreaElement | undefined;
  const autoGrow = () => {
    const el = inputEl;
    if (el === undefined) return;
    el.style.height = 'auto';
    el.style.height = `${el.scrollHeight}px`;
  };
  createEffect(() => {
    text();
    autoGrow();
  });

  // `body` defaults to the box; the popover's click-to-send path passes the
  // clicked command instead (issue #122) so both routes share one POST +
  // clear-on-success.
  const send = async (body = text().trim()) => {
    if (sending() || body === '') return;
    setSending(true);
    try {
      const result = await replyRun(props.runID, body);
      setText('');
      // 200 with a `notice` body is informational (issue #149), not an error
      // — 204 (the common case) carries none.
      if (result?.notice) props.onNotice(result.notice);
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setSending(false);
      props.onSent();
    }
  };

  // --- Slash-command autocomplete (issue #51 decision 5) --------------------
  // Prefix-only: the popover exists only while the WHOLE input starts with '/'
  // (a mid-message slash never triggers it). Matching is case-insensitive and
  // TIERED (issue #122): exact name, then name prefix, then name substring,
  // then description/arg-hint substring — catalog order within a tier — so a
  // command whose description merely mentions the query can never outrank the
  // command actually named after it. Tab completes "/name " into the box; a
  // click sends the command outright unless it declares an arg hint (then it
  // completes like Tab so the argument can be typed); Enter is NOT an accept
  // gesture — it sends the box as typed. Sending stays the NORMAL reply path —
  // the backend renders the command echo as user text (issue #51 decision 2),
  // so nothing here special-cases a slash message.
  const [acDismissed, setAcDismissed] = createSignal(false); // Escape until the next keystroke
  const [acIndex, setAcIndex] = createSignal(0);
  const acMatches = (): RunCommand[] => {
    const value = text();
    if (!value.startsWith('/')) return [];
    const q = value.slice(1).toLowerCase();
    // Tier 0 exact name, 1 name prefix, 2 name substring, 3 description/arg-
    // hint substring; 4 = no match, dropped. sort() is spec-stable, so catalog
    // order survives within a tier — and the empty query (a bare "/") prefix-
    // matches every name, listing the full catalog in catalog order as before.
    const tier = (c: RunCommand): number => {
      const name = c.name.toLowerCase();
      if (name === q) return 0;
      if (name.startsWith(q)) return 1;
      if (name.includes(q)) return 2;
      const meta = [c.description ?? '', c.arg_hint ?? ''];
      return meta.some((f) => f.toLowerCase().includes(q)) ? 3 : 4;
    };
    return props.commands
      .map((c) => ({ c, t: tier(c) }))
      .filter((e) => e.t < 4)
      .sort((a, b) => a.t - b.t)
      .map((e) => e.c);
  };
  const acOpen = () => !acDismissed() && acMatches().length > 0;
  // Typing resets the highlight to the best (first) match and un-dismisses.
  createEffect(on(text, () => setAcIndex(0), { defer: true }));
  // The list scrolls past 40vh, so cycling must drag the active row into view
  // (optional call: jsdom has no scrollIntoView).
  createEffect(() => {
    if (!acOpen()) return;
    document.getElementById(`chat-cmd-opt-${acIndex()}`)?.scrollIntoView?.({ block: 'nearest' });
  });
  const completeCommand = (cmd: RunCommand) => {
    setText(`/${cmd.name} `);
    // Dismiss until the next keystroke: with the completion landed the picker
    // has done its job — left open it would keep swallowing Tab and the
    // arrows (descriptions often still match "name "). Enter needs no such
    // care: it sends the box as typed whether or not the popover shows
    // (issue #122).
    setAcDismissed(true);
    inputEl?.focus();
  };

  // The keyboard send works in every unlocked state (ADR-0029, issue #61) —
  // idle, needs_input, working — it never gates on `working`. Bare Enter
  // sends only on fine-pointer (mouse/trackpad) setups (ADR-0031, issue #70);
  // Shift+Enter and Alt+Enter stay an explicit newline, and Cmd/Ctrl+Enter
  // keeps sending everywhere regardless of pointer type. Enter during IME
  // composition is ignored (isComposerSend's isComposing/keyCode 229 guard)
  // so committing composed text never fires a send. While the command popover
  // is open, Up/Down cycle, Tab completes and Escape closes — Enter is
  // deliberately NOT captured (ADR-0041, issue #122, reversing ADR-0031's
  // popover-precedence clause): it falls through to the send gate below and
  // posts the box exactly as typed, so Enter never swaps the input for the
  // highlighted row. Cmd/Ctrl+Enter still bypasses the popover branch
  // entirely (the `!(e.metaKey || e.ctrlKey)` gate).
  const onKeyDown = (e: KeyboardEvent) => {
    if (acOpen() && !(e.metaKey || e.ctrlKey)) {
      const matches = acMatches();
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setAcIndex((i) => (i + 1) % matches.length);
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        setAcIndex((i) => (i - 1 + matches.length) % matches.length);
        return;
      }
      if (e.key === 'Escape') {
        // preventDefault also keeps the tool panel's window Esc-close (issue
        // #145, defaultPrevented-guarded) from firing on a popover dismissal.
        e.preventDefault();
        setAcDismissed(true);
        return;
      }
      if (e.key === 'Tab') {
        e.preventDefault();
        const pick = matches[Math.min(acIndex(), matches.length - 1)];
        if (pick !== undefined) completeCommand(pick);
        return;
      }
    }
    if (isComposerSend(e)) {
      e.preventDefault();
      void send();
    }
  };

  return (
    <div class="chat-composer">
      {/* §4 — jump-to-latest pill: floats ~12px above the field, over the
          stream. Kept mounted and toggled via a class so it can fade OUT (not
          just in); accent-emphasized when the content below is a needs-you
          signal. Not focusable while hidden. */}
      <button
        type="button"
        classList={{ 'chat-jump': true, hidden: !props.jumpVisible, emphasis: props.jumpEmphasis }}
        aria-hidden={!props.jumpVisible}
        tabindex={props.jumpVisible ? 0 : -1}
        aria-label={
          props.jumpEmphasis
            ? `${capitalize(props.agentName)} is waiting — jump to latest`
            : 'Jump to latest'
        }
        onClick={() => props.onJump()}
      >
        <Icon name="chevron-down" size={16} class="chat-jump-icon" />
        <span>{props.jumpEmphasis ? `${capitalize(props.agentName)} needs you` : 'Latest'}</span>
      </button>
      <Switch>
        <Match when={props.ended}>
          <p class="chat-composer-note">This instance has ended — the chat is read-only.</p>
        </Match>
        <Match when={props.transcript === 'gone'}>
          <p class="chat-composer-note">Transcript no longer available — the chat is read-only.</p>
        </Match>
        {/* A dialog is pending: the interactive card lives in the STREAM
            (issue #56 decision 1), so the composer collapses to a slim
            waiting note pointing up at it — no textarea, free text can't
            answer a focused picker (decision 2). No Interrupt here (issue
            #165 item 3): an accent square in Send's slot, right next to a
            live interactive card, drew muscle-memory "send" taps that
            declined the focused picker instead. The escape hatch stays
            reachable via the sticky header's turn Interrupt (desktop) and
            the ••• ChatMenu (mobile) — both gated on `live()`, which holds
            while a dialog pends. */}
        <Match when={props.dialog}>
          <p class="chat-composer-note">
            {capitalize(props.agentName)} is waiting on your answer — see the question above.
          </p>
        </Match>
        {/* state 'question' with no structured dialog (a dormant transcript
            flush, or a shape lab can't render): the composer stays locked — a
            free-text reply would land in a focused picker — and the operator
            answers on the provider's own surface or interrupts (decision 5). */}
        <Match when={props.state === 'question'}>
          <div class="chat-dialog">
            <p class="chat-composer-note">
              {capitalize(props.agentName)} needs input — {props.openHint} to respond.
            </p>
            <InterruptButton runID={props.runID} onError={props.onError} onDone={props.onSent} />
          </div>
        </Match>
        <Match when={true}>
          {/* Residual blocked state with no structured dialog — e.g. a plain
              tool-permission prompt or the post-decline "stuck" case (decision
              7). The composer stays usable. The needs-input note now lives in
              the stream as a status line (§3), so needs_input collapses to just
              the input row here. */}
          {/* Slash-command autocomplete popover (issue #51 decision 5): floats
              above the input like the jump pill; rows carry /name, the arg
              hint (dim), the description and a source badge. Keyboard runs
              through onKeyDown; mousedown is swallowed so a click never steals
              the textarea's focus. A click SENDS the command outright when it
              declares no arg hint — hint presence is the one "expects more
              text" signal — otherwise it completes to "/name " and leaves
              focus in the textarea for the argument (issue #122). */}
          <Show when={acOpen()}>
            <div class="chat-cmd-pop" id="chat-cmd-pop" role="listbox" aria-label="Slash commands">
              <For each={acMatches()}>
                {(cmd, i) => (
                  <button
                    type="button"
                    id={`chat-cmd-opt-${i()}`}
                    classList={{ 'chat-cmd-row': true, active: i() === acIndex() }}
                    role="option"
                    aria-selected={i() === acIndex()}
                    title={cmd.description}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => {
                      if (cmd.arg_hint) completeCommand(cmd);
                      else void send(`/${cmd.name}`);
                    }}
                  >
                    <span class="chat-cmd-name mono">/{cmd.name}</span>
                    <Show when={cmd.arg_hint}>
                      <span class="chat-cmd-hint mono">{cmd.arg_hint}</span>
                    </Show>
                    <span class={`chat-cmd-source src-${cmd.source}`}>{cmd.source}</span>
                    <Show when={cmd.description}>
                      <span class="chat-cmd-desc">{cmd.description}</span>
                    </Show>
                  </button>
                )}
              </For>
            </div>
          </Show>
          <div class="chat-composer-row">
            <textarea
              ref={(el) => {
                inputEl = el;
                // Fit an existing draft the moment the element attaches — a
                // remount (e.g. after a mid-turn dialog) keeps the compose-ahead
                // text but resets the height, and the text() effect won't refire
                // for an unchanged signal. Deferred so the value is applied first.
                queueMicrotask(autoGrow);
              }}
              class="chat-input"
              rows={1}
              placeholder="Reply to the agent…"
              value={text()}
              aria-autocomplete="list"
              aria-controls={acOpen() ? 'chat-cmd-pop' : undefined}
              aria-activedescendant={acOpen() ? `chat-cmd-opt-${acIndex()}` : undefined}
              onInput={(e) => {
                // A fresh keystroke revives an Escape-dismissed popover.
                setAcDismissed(false);
                setText(e.currentTarget.value);
              }}
              onKeyDown={onKeyDown}
            />
            {/* Always-Send (ADR-0029, issue #61): Send never reads the derived
                `working` state, because that state can be false — a stale
                transcript tail (issue #38) — and would then hide the operator's
                only way to reply. A genuinely mid-turn reply is queued by the
                agent's own TUI (the backend Reply is unchanged — ADR-0016's
                paste+Enter), with no queue affordance in the UI: the reply
                simply echoes once the transcript reflects it. */}
            <button
              type="button"
              class="icon-btn chat-send"
              classList={{ busy: sending() }}
              aria-label="Send"
              title="Send"
              disabled={!canSend()}
              onClick={() => void send()}
            >
              <Icon name="send" />
            </button>
          </div>
        </Match>
      </Switch>
    </div>
  );
}

// One-tap Interrupt escape hatch (ADR-0029, superseding ADR-0016's confirm tap):
// an accent `pause` icon-button that fires interruptRun (Escape) immediately, no
// confirmation — interrupt is non-destructive (the agent survives, idles, and is
// re-promptable), so a confirm tap is friction. Rendered in the composer's
// degraded question-state branch (issue #56 decision 5) only — there it's the
// primary action, with no interactive card above to draw a miss-tap. The
// dialog-pending branch dropped it (issue #165 item 3): an accent square in
// Send's slot, next to a live interactive card, drew muscle-memory "send" taps
// that declined the focused picker instead; the header's turn Interrupt and the
// ••• ChatMenu keep the hatch reachable there. It shares the `pause` glyph with
// the header's turn Interrupt (the composer Send no longer morphs — ADR-0029),
// and stays distinct from the danger `square` Stop, which is two-step (destructive
// teardown, ADR-0019).
function InterruptButton(props: {
  runID: string;
  onError: (message: string) => void;
  onDone: () => void;
}) {
  const interrupt = createInterrupt(
    () => props.runID,
    (m) => props.onError(m),
    () => props.onDone(),
  );
  return (
    <button
      type="button"
      class="chat-interrupt icon-btn"
      classList={{ busy: interrupt.busy() }}
      aria-label="Interrupt"
      title="Interrupt the agent (Escape)"
      onClick={() => void interrupt.run()}
    >
      <Show when={interrupt.busy()} fallback={<Icon name="pause" />}>
        <span class="chat-interrupt-busy" aria-hidden="true">
          …
        </span>
      </Show>
    </button>
  );
}
