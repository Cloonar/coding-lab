// Embedded chat (/runs/:id, issue #7 / ADR-0016): the body IS the chat. A
// compact header (title · conversational state · deep link · Stop), the
// conversation stream (user/assistant text, tool chips, pending dialog,
// lifecycle/errors; thinking behind a toggle), and a fixed bottom composer
// whose state follows the run — locked while a dialog is pending, disabled for
// ended instances, with a "queued" hint while the agent is working. Reads
// through GET /runs/:id/messages and refetches on run.messages.changed; replies
// / answers / interrupts POST back. Phone-first, v0 design language.

import { A, useParams } from '@solidjs/router';
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createResource,
  createSignal,
  on,
  onCleanup,
} from 'solid-js';
import {
  answerRun,
  errorMessage,
  getRun,
  getRunMessages,
  interruptRun,
  listProviders,
  replyRun,
  stopInstance,
  type ChatMessage,
  type ConversationState,
  type Dialog,
  type Provider,
  type Run,
  type TranscriptStatus,
} from '../api';
import ErrorBanner from '../components/ErrorBanner';
import OpenAffordance from '../components/OpenAffordance';
import RequireAuth from '../components/RequireAuth';
import {
  anchoredScrollTop,
  isNearBottom,
  maxSeq,
  mergeMessages,
  mergeRefetch,
} from '../lib/chatStream';
import { stateBadge } from '../lib/conversation';
import { openState, providerOpen } from '../lib/deepLink';
import { instanceTitle, sessionLabel, sessionRepo } from '../lib/instanceLabel';
import { resourceValue } from '../lib/resource';
import { useEvents } from '../events';

const MESSAGE_LIMIT = 60;

export default function RunChat() {
  return (
    <RequireAuth>
      <RunChatView />
    </RequireAuth>
  );
}

function RunChatView() {
  const params = useParams<{ id: string }>();
  const events = useEvents();

  const [run, { refetch: refetchRun }] = createResource(
    () => params.id,
    (id) => getRun(id),
  );
  // Provider-owned open affordance (ADR-0017): the fallback web link + title,
  // or none for a link-less provider (then the header shows tmux-attach).
  const [providers] = createResource(() => listProviders());

  // The message window is accumulated (merged by seq) so scroll-up history and
  // in-place tool-status updates both survive a refetch.
  const [messages, setMessages] = createSignal<ChatMessage[]>([]);
  const [state, setState] = createSignal<ConversationState>('');
  const [transcript, setTranscript] = createSignal<TranscriptStatus>('available');
  const [hasMore, setHasMore] = createSignal(false);
  // A before-fetch hit the beginning: never resurrect "Load earlier" from a
  // latest-window has_more, which talks about ITS window, not our accumulated
  // stream.
  const [exhausted, setExhausted] = createSignal(false);
  const [showThinking, setShowThinking] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  // The stream is the scroll container (.chat-page bounds the viewport).
  let streamEl: HTMLDivElement | undefined;
  const scrollToBottom = () => {
    if (streamEl !== undefined) streamEl.scrollTop = streamEl.scrollHeight;
  };

  // Monotonic fetch token: only the newest in-flight fetch may apply its
  // result, so a slow stale response can't revert an answered dialog to
  // pending and re-lock the composer.
  let fetchToken = 0;

  // Refetch protocol (ADR-0016): after=<cursor> tail batches make appends
  // gap-free even when >limit messages land between refetches; the latest
  // window is fetched too for back-patched mutations near the tail (tool
  // status flips, answered dialogs). Merged by seq, later response wins.
  // First load (no cursor yet) is just the latest window.
  const refetchMessages = async () => {
    const token = ++fetchToken;
    const cursor = maxSeq(messages());
    try {
      const tail: ChatMessage[] = [];
      let after = cursor;
      while (after > 0) {
        const res = await getRunMessages(params.id, { after, limit: MESSAGE_LIMIT });
        if (token !== fetchToken) return;
        tail.push(...res.messages);
        const top = maxSeq(res.messages);
        // A short window is the whole remaining tail; a stuck seq would loop.
        if (res.messages.length < MESSAGE_LIMIT || top <= after) break;
        after = top;
      }
      const latest = await getRunMessages(params.id, { limit: MESSAGE_LIMIT });
      if (token !== fetchToken) return;
      setState(latest.state);
      setTranscript(latest.transcript);
      if (!exhausted()) setHasMore(latest.has_more);
      // A first window that already covers the whole transcript means there
      // is nothing older to load — ever (messages only append).
      if (cursor === 0 && !latest.has_more && latest.messages.length > 0) setExhausted(true);
      // Follow-bottom only when the reader is already at/near the bottom (or
      // on the initial window), so reading history isn't yanked down.
      const follow = cursor === 0 || (streamEl !== undefined && isNearBottom(streamEl));
      setMessages((prev) => mergeRefetch(prev, tail, latest.messages));
      if (follow) scrollToBottom();
    } catch (err) {
      if (token === fetchToken) setError(errorMessage(err));
    }
  };

  const loadEarlier = async () => {
    const first = messages()[0];
    if (first === undefined) return;
    const token = ++fetchToken;
    try {
      const res = await getRunMessages(params.id, { before: first.seq, limit: MESSAGE_LIMIT });
      if (token !== fetchToken) return;
      setHasMore(res.has_more);
      if (!res.has_more || res.messages.length === 0) setExhausted(true);
      // Prepend anchoring by hand (iOS Safari has no overflow-anchor): capture
      // the geometry, then restore the visual position after the DOM grows.
      const before =
        streamEl === undefined
          ? undefined
          : { scrollTop: streamEl.scrollTop, scrollHeight: streamEl.scrollHeight };
      setMessages((prev) => mergeMessages(prev, res.messages));
      if (streamEl !== undefined && before !== undefined) {
        streamEl.scrollTop = anchoredScrollTop(before, streamEl.scrollHeight);
      }
    } catch (err) {
      if (token === fetchToken) setError(errorMessage(err));
    }
  };

  // All chat state is keyed to the route param: navigating /runs/A → /runs/B
  // re-uses this component, so reset the accumulated stream and reload (the
  // token bump inside refetchMessages orphans any in-flight fetch for A).
  createEffect(
    on(
      () => params.id,
      () => {
        setMessages([]);
        setState('');
        setTranscript('available');
        setHasMore(false);
        setExhausted(false);
        setError(null);
        void refetchMessages();
      },
    ),
  );

  // Refetch on the tailer's envelope (this run only) and on run.changed
  // (outcome flips → composer state). run.changed is repo-scoped on the wire
  // (no runID), so filter by the run's repo — fleet-quiet, at worst
  // sibling-run noise.
  /* eslint-disable solid/reactivity -- handlers re-read params.id / the run fresh per SSE event */
  onCleanup(
    events.subscribe('run.messages.changed', (event) => {
      if (event.runID === params.id) void refetchMessages();
    }),
  );
  onCleanup(
    events.subscribe('run.changed', (event) => {
      const r = resourceValue(run);
      if (r !== undefined && event.repoID !== undefined && event.repoID !== r.repo_id) return;
      void refetchRun();
      void refetchMessages();
    }),
  );
  /* eslint-enable solid/reactivity */

  const runData = () => resourceValue(run);
  const ended = () => {
    const r = runData();
    return r !== undefined && r.outcome !== 'active';
  };
  // The backend defines "pending" as state === 'question': the array scan
  // alone could hold onto a dialog answered externally outside the refetch
  // window and lock the composer forever.
  const pendingDialog = (): Dialog | null => {
    if (state() !== 'question') return null;
    const msgs = messages();
    for (let i = msgs.length - 1; i >= 0; i--) {
      const d = msgs[i]?.dialog;
      if (msgs[i]?.kind === 'dialog' && d) return d;
    }
    return null;
  };
  const visibleMessages = () =>
    messages().filter((m) => showThinking() || !(m.kind === 'text' && m.thinking));

  return (
    <main class="page chat-page">
      <ChatHeader
        run={runData()}
        providers={providers()}
        state={state()}
        showThinking={showThinking()}
        onToggleThinking={() => setShowThinking((v) => !v)}
        onError={setError}
        onChanged={() => void refetchRun()}
      />
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />

      <div class="chat-stream" role="log" aria-live="polite" ref={streamEl}>
        <Switch>
          <Match when={transcript() === 'locating' && messages().length === 0}>
            <p class="empty">Waiting for the transcript…</p>
          </Match>
          <Match when={transcript() === 'gone'}>
            <p class="empty">Transcript no longer available.</p>
          </Match>
          <Match when={messages().length === 0}>
            <p class="empty">No messages yet.</p>
          </Match>
          <Match when={messages().length > 0}>
            <Show when={hasMore()}>
              <button type="button" class="seg chat-earlier" onClick={() => void loadEarlier()}>
                Load earlier
              </button>
            </Show>
            <For each={visibleMessages()}>{(m) => <MessageView message={m} />}</For>
          </Match>
        </Switch>
      </div>

      <Composer
        runID={params.id}
        state={state()}
        ended={ended()}
        transcript={transcript()}
        dialog={pendingDialog()}
        onError={setError}
        onSent={() => void refetchMessages()}
      />
    </main>
  );
}

function ChatHeader(props: {
  run: Run | undefined;
  providers: Provider[] | undefined;
  state: ConversationState;
  showThinking: boolean;
  onToggleThinking: () => void;
  onError: (message: string) => void;
  onChanged: () => void;
}) {
  const [confirming, setConfirming] = createSignal(false);
  const [stopping, setStopping] = createSignal(false);
  // "repo · label" — the session name is `<repo>~<label>` and the label alone
  // is ambiguous across repos.
  const title = () => {
    const r = props.run;
    if (r === undefined) return 'Chat';
    const repo = sessionRepo(r.session_name);
    const label = instanceTitle(sessionLabel(r.session_name));
    if (label === '') return r.branch;
    return repo === '' ? label : `${repo} · ${label}`;
  };
  const badge = () => stateBadge(props.state);
  const live = () => props.run !== undefined && props.run.outcome === 'active';
  // The open affordance (ADR-0017): the exact deep link when captured, else the
  // provider's generic web fallback, else a tmux-attach for a link-less
  // provider — same source of truth as the dashboard rows, never hardcoded.
  const open = () => {
    const r = props.run;
    if (r === undefined) return null;
    return openState(
      { connecting: false, deep_link_url: r.deep_link_url, session_name: r.session_name },
      providerOpen(props.providers, r.provider),
    );
  };

  const stop = async () => {
    const r = props.run;
    if (r === undefined) return;
    setStopping(true);
    try {
      await stopInstance(r.session_name);
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setStopping(false);
      setConfirming(false);
      props.onChanged();
    }
  };

  return (
    <header class="chat-header">
      <A href="/" class="crumb chat-back" title="Back to the dashboard">
        ←
      </A>
      <span class="chat-title">{title()}</span>
      <Show when={badge()}>
        {(b) => (
          <span classList={{ chip: true, convo: true, [b().cls]: true }} title={b().title}>
            {b().label}
          </span>
        )}
      </Show>
      <span class="spacer" />
      <button
        type="button"
        class="seg chat-thinking-toggle"
        aria-pressed={props.showThinking}
        onClick={() => props.onToggleThinking()}
        title="Show or hide the agent's thinking"
      >
        {props.showThinking ? 'Hide thinking' : 'Show thinking'}
      </button>
      <Show when={open()}>{(s) => <OpenAffordance state={s()} />}</Show>
      <Show when={live()}>
        <Switch>
          <Match when={!confirming()}>
            <button type="button" class="danger chat-stop" onClick={() => setConfirming(true)}>
              Stop
            </button>
          </Match>
          <Match when={confirming()}>
            <button
              type="button"
              class="danger chat-stop-confirm"
              disabled={stopping()}
              onClick={() => void stop()}
            >
              {stopping() ? 'Stopping…' : 'Confirm stop'}
            </button>
            <button type="button" class="seg" onClick={() => setConfirming(false)}>
              Cancel
            </button>
          </Match>
        </Switch>
      </Show>
    </header>
  );
}

function MessageView(props: { message: ChatMessage }) {
  const m = () => props.message;
  return (
    <Switch>
      <Match when={m().kind === 'text'}>
        <div
          classList={{
            'chat-msg': true,
            [`role-${m().role ?? 'assistant'}`]: true,
            thinking: m().thinking === true,
          }}
        >
          <Show when={m().thinking}>
            <span class="chat-msg-tag">thinking</span>
          </Show>
          <p class="chat-msg-text">{m().text}</p>
        </div>
      </Match>
      <Match when={m().kind === 'tool'}>
        <details class="chat-tool">
          <summary classList={{ 'tool-summary': true, [`tool-${m().tool?.status ?? 'ok'}`]: true }}>
            <span class="tool-title">{m().tool?.title}</span>
            <span class="tool-status">{toolStatusMark(m().tool?.status)}</span>
          </summary>
          <Show when={m().tool?.input}>
            <pre class="tool-body mono">{m().tool?.input}</pre>
          </Show>
          <Show when={m().tool?.output}>
            <pre class="tool-body tool-output mono">{m().tool?.output}</pre>
          </Show>
        </details>
      </Match>
      <Match when={m().kind === 'dialog'}>
        <div class="chat-dialog-inline">
          <p class="chat-dialog-prompt">{m().dialog?.prompt}</p>
        </div>
      </Match>
      <Match when={m().kind === 'lifecycle'}>
        <p classList={{ 'chat-lifecycle': true, error: m().error === true }}>{m().text}</p>
      </Match>
    </Switch>
  );
}

function Composer(props: {
  runID: string;
  state: ConversationState;
  ended: boolean;
  transcript: TranscriptStatus;
  dialog: Dialog | null;
  onError: (message: string) => void;
  onSent: () => void;
}) {
  const [text, setText] = createSignal('');
  const [sending, setSending] = createSignal(false);

  const send = async () => {
    const body = text().trim();
    if (body === '') return;
    setSending(true);
    try {
      await replyRun(props.runID, body);
      setText('');
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setSending(false);
      props.onSent();
    }
  };

  return (
    <div class="chat-composer">
      <Switch>
        <Match when={props.ended}>
          <p class="chat-composer-note">This instance has ended — the chat is read-only.</p>
        </Match>
        <Match when={props.transcript === 'gone'}>
          <p class="chat-composer-note">Transcript no longer available — the chat is read-only.</p>
        </Match>
        <Match when={props.dialog}>
          {(d) => (
            <DialogPanel
              runID={props.runID}
              dialog={d()}
              onError={props.onError}
              onAnswered={props.onSent}
            />
          )}
        </Match>
        <Match when={true}>
          <div class="chat-composer-row">
            <textarea
              class="chat-input"
              rows={1}
              placeholder="Reply to the agent…"
              value={text()}
              onInput={(e) => setText(e.currentTarget.value)}
            />
            <button
              type="button"
              class="chat-send"
              disabled={sending() || text().trim() === ''}
              onClick={() => void send()}
            >
              {sending() ? '…' : 'Send'}
            </button>
            <InterruptButton runID={props.runID} onError={props.onError} onDone={props.onSent} />
          </div>
          <Show when={props.state === 'working'}>
            <p class="chat-composer-hint">The agent is working — your reply will be queued.</p>
          </Show>
        </Match>
      </Switch>
    </div>
  );
}

function DialogPanel(props: {
  runID: string;
  dialog: Dialog;
  onError: (message: string) => void;
  onAnswered: () => void;
}) {
  const [busy, setBusy] = createSignal(false);
  const [selected, setSelected] = createSignal<number[]>([]);
  const [otherText, setOtherText] = createSignal('');

  // Selection state is keyed to the dialog's identity: if the pending dialog
  // changes while the panel is mounted, stale picks must not carry over and
  // answer the new dialog.
  createEffect(
    on(
      () => props.dialog.tool_id,
      () => {
        setSelected([]);
        setOtherText('');
      },
      { defer: true },
    ),
  );

  const options = () => props.dialog.options ?? [];
  const answer = async (payload: { index?: number; selected?: number[]; other_text?: string }) => {
    setBusy(true);
    try {
      await answerRun(props.runID, { tool_id: props.dialog.tool_id, ...payload });
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
      props.onAnswered();
    }
  };
  const toggle = (i: number) =>
    setSelected((prev) => (prev.includes(i) ? prev.filter((x) => x !== i) : [...prev, i]));

  return (
    <div class="chat-dialog">
      <p class="chat-dialog-prompt">{props.dialog.prompt}</p>
      <Show
        when={props.dialog.answerable}
        fallback={
          <p class="chat-composer-note">
            This dialog can't be answered here — open it in claude.ai.
          </p>
        }
      >
        <Switch>
          {/* Multi-select: checkboxes + a single confirm. */}
          <Match when={props.dialog.multi}>
            <ul class="dialog-options">
              <For each={options()}>
                {(opt, i) => (
                  <li>
                    <label class="dialog-check">
                      <input
                        type="checkbox"
                        checked={selected().includes(i())}
                        onChange={() => toggle(i())}
                      />
                      {opt.label}
                    </label>
                  </li>
                )}
              </For>
            </ul>
            <button
              type="button"
              class="chat-send"
              disabled={busy() || selected().length === 0}
              onClick={() => void answer({ selected: selected() })}
            >
              Submit
            </button>
          </Match>
          {/* Single-select: one button per option; the Other row takes text. */}
          <Match when={true}>
            <ul class="dialog-options">
              <For each={options()}>
                {(opt, i) => (
                  <li>
                    <Show
                      when={!opt.is_other}
                      fallback={
                        <div class="dialog-other">
                          <input
                            class="chat-input"
                            placeholder="Other…"
                            value={otherText()}
                            onInput={(e) => setOtherText(e.currentTarget.value)}
                          />
                          <button
                            type="button"
                            class="chat-send"
                            disabled={busy() || otherText().trim() === ''}
                            onClick={() => void answer({ index: i(), other_text: otherText() })}
                          >
                            Send
                          </button>
                        </div>
                      }
                    >
                      <button
                        type="button"
                        class="dialog-option seg"
                        disabled={busy()}
                        onClick={() => void answer({ index: i() })}
                        title={opt.description}
                      >
                        {opt.label}
                      </button>
                    </Show>
                  </li>
                )}
              </For>
            </ul>
          </Match>
        </Switch>
      </Show>
      <InterruptButton runID={props.runID} onError={props.onError} onDone={props.onAnswered} />
    </div>
  );
}

function InterruptButton(props: {
  runID: string;
  onError: (message: string) => void;
  onDone: () => void;
}) {
  const [confirming, setConfirming] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const interrupt = async () => {
    setBusy(true);
    try {
      await interruptRun(props.runID);
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
      setConfirming(false);
      props.onDone();
    }
  };
  return (
    <Switch>
      <Match when={!confirming()}>
        <button
          type="button"
          class="seg chat-interrupt"
          onClick={() => setConfirming(true)}
          title="Interrupt the agent (Escape)"
        >
          Interrupt
        </button>
      </Match>
      <Match when={confirming()}>
        <button
          type="button"
          class="danger chat-interrupt-confirm"
          disabled={busy()}
          onClick={() => void interrupt()}
        >
          {busy() ? '…' : 'Confirm'}
        </button>
        <button type="button" class="seg" onClick={() => setConfirming(false)}>
          Cancel
        </button>
      </Match>
    </Switch>
  );
}

function toolStatusMark(status: string | undefined): string {
  if (status === 'ok') return '✓';
  if (status === 'error') return '✕';
  return '…';
}
