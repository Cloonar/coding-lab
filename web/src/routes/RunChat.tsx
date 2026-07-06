// Embedded chat (/runs/:id, issue #7 / ADR-0016): the body IS the chat. A
// compact header (title · conversational state · deep link · Stop), the
// conversation stream (user/assistant text, tool chips, pending dialog,
// lifecycle/errors; thinking behind a toggle), and a fixed bottom composer
// whose state follows the run — locked while a dialog is pending, disabled for
// ended instances, with a "queued" hint while the agent is working. Reads
// through GET /runs/:id/messages and refetches on run.messages.changed; replies
// / answers / interrupts POST back. Phone-first, v0 design language.

import { A, useParams } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import {
  answerRun,
  errorMessage,
  getRun,
  getRunMessages,
  interruptRun,
  replyRun,
  stopInstance,
  type ChatMessage,
  type ConversationState,
  type Dialog,
  type Run,
  type TranscriptStatus,
} from '../api';
import ErrorBanner from '../components/ErrorBanner';
import RequireAuth from '../components/RequireAuth';
import { stateBadge } from '../lib/conversation';
import { instanceTitle, sessionLabel } from '../lib/instanceLabel';
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

  // The message window is accumulated (merged by seq) so scroll-up history and
  // in-place tool-status updates both survive a refetch.
  const [messages, setMessages] = createSignal<ChatMessage[]>([]);
  const [state, setState] = createSignal<ConversationState>('');
  const [transcript, setTranscript] = createSignal<TranscriptStatus>('available');
  const [hasMore, setHasMore] = createSignal(false);
  const [showThinking, setShowThinking] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const loadLatest = async () => {
    try {
      const res = await getRunMessages(params.id, { limit: MESSAGE_LIMIT });
      setState(res.state);
      setTranscript(res.transcript);
      setHasMore(res.has_more);
      setMessages((prev) => mergeMessages(prev, res.messages));
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const loadEarlier = async () => {
    const first = messages()[0];
    if (first === undefined) return;
    try {
      const res = await getRunMessages(params.id, { before: first.seq, limit: MESSAGE_LIMIT });
      setHasMore(res.has_more);
      setMessages((prev) => mergeMessages(prev, res.messages));
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  // Initial load once, then refetch on the tailer's envelope (this run only)
  // and on run.changed (outcome flips → composer state).
  void loadLatest();
  onCleanup(
    events.subscribe('run.messages.changed', (event) => {
      if (event.runID === params.id) void loadLatest();
    }),
  );
  onCleanup(
    events.subscribe('run.changed', () => {
      void refetchRun();
      void loadLatest();
    }),
  );

  const runData = () => resourceValue(run);
  const ended = () => {
    const r = runData();
    return r !== undefined && r.outcome !== 'active';
  };
  const pendingDialog = (): Dialog | null => {
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
        state={state()}
        showThinking={showThinking()}
        onToggleThinking={() => setShowThinking((v) => !v)}
        onError={setError}
        onChanged={() => void refetchRun()}
      />
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />

      <div class="chat-stream" role="log" aria-live="polite">
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
        onSent={() => void loadLatest()}
      />
    </main>
  );
}

function ChatHeader(props: {
  run: Run | undefined;
  state: ConversationState;
  showThinking: boolean;
  onToggleThinking: () => void;
  onError: (message: string) => void;
  onChanged: () => void;
}) {
  const [confirming, setConfirming] = createSignal(false);
  const [stopping, setStopping] = createSignal(false);
  const title = () => {
    const r = props.run;
    if (r === undefined) return 'Chat';
    const label = instanceTitle(sessionLabel(r.session_name));
    return label === '' ? r.branch : label;
  };
  const badge = () => stateBadge(props.state);
  const live = () => props.run !== undefined && props.run.outcome === 'active';

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
      <Show when={props.run?.deep_link_url}>
        {(url) => (
          <a
            href={url()}
            target="_blank"
            rel="noreferrer"
            class="card-link"
            title="Open in claude.ai"
          >
            Open ↗
          </a>
        )}
      </Show>
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
        <Match when={props.ended || props.transcript === 'gone'}>
          <p class="chat-composer-note">This instance has ended — the chat is read-only.</p>
        </Match>
        <Match when={props.dialog !== null}>
          <DialogPanel
            runID={props.runID}
            dialog={props.dialog as Dialog}
            onError={props.onError}
            onAnswered={props.onSent}
          />
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

/** Merge windows by seq (later window wins per seq → in-place tool updates). */
function mergeMessages(prev: ChatMessage[], incoming: ChatMessage[]): ChatMessage[] {
  const bySeq = new Map<number, ChatMessage>();
  for (const m of prev) bySeq.set(m.seq, m);
  for (const m of incoming) bySeq.set(m.seq, m);
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}
