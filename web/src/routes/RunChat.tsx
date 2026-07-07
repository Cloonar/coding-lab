// Embedded chat (/runs/:id, issue #7 / ADR-0016): the body IS the chat. A
// compact header (title · conversational state · deep link · Stop), the
// conversation stream (user/assistant text, tool chips, pending dialog,
// lifecycle/errors; thinking behind a toggle), and a fixed bottom composer
// whose state follows the run — locked while a dialog is pending, disabled for
// ended instances, and morphing its Send button into a one-tap Interrupt while
// the agent is working (ADR-0022). Reads through GET /runs/:id/messages and
// refetches on run.messages.changed; replies / answers / interrupts POST back.
// Phone-first, v0 design language.

import { A, useParams } from '@solidjs/router';
import { Dynamic } from 'solid-js/web';
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
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
import Icon from '../components/Icon';
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
import { parseMarkdown, type Block, type Inline } from '../lib/markdown';
import { resourceValue } from '../lib/resource';
import { groupMessages, toolGroupSummary, type ToolGroup } from '../lib/toolGroups';
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
  // The live pending dialog from the server's top-level pending_dialog field
  // (ADR-0020) — the authoritative source, since Claude Code never flushes a
  // pending tool_use to the transcript. null when none is pending.
  const [pendingDialogField, setPendingDialogField] = createSignal<Dialog | null>(null);
  const [hasMore, setHasMore] = createSignal(false);
  // A before-fetch hit the beginning: never resurrect "Load earlier" from a
  // latest-window has_more, which talks about ITS window, not our accumulated
  // stream.
  const [exhausted, setExhausted] = createSignal(false);
  const [showThinking, setShowThinking] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  // Tool-group open state, keyed by the first tool's seq (decision 12): the
  // group is a derived structure recomputed on every refetch, so native
  // <details> state alone would slam an expanded live group shut on the next
  // SSE tick. seq is the immutable cursor, so it survives the recompute.
  const [openGroups, setOpenGroups] = createSignal<Set<number>>(new Set());
  const setGroupOpen = (key: number, open: boolean) =>
    setOpenGroups((prev) => {
      const next = new Set(prev);
      if (open) next.add(key);
      else next.delete(key);
      return next;
    });

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
      setPendingDialogField(latest.pending_dialog ?? null);
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
        setPendingDialogField(null);
        setTranscript('available');
        setHasMore(false);
        setExhausted(false);
        setError(null);
        setOpenGroups(new Set<number>());
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
  // The pending dialog: the server's top-level pending_dialog field is
  // authoritative (ADR-0020 — the live PreToolUse spool). The messages-scan is
  // the dormant fallback for a future Claude Code that flushes a pending
  // tool_use to the transcript; it is gated on state === 'question' so a dialog
  // answered outside the refetch window can't lock the composer forever.
  const pendingDialog = (): Dialog | null => {
    const field = pendingDialogField();
    if (field) return field;
    if (state() !== 'question') return null;
    const msgs = messages();
    for (let i = msgs.length - 1; i >= 0; i--) {
      const d = msgs[i]?.dialog;
      if (msgs[i]?.kind === 'dialog' && d) return d;
    }
    return null;
  };
  // Group consecutive tool runs at render time (decision 7). Grouping runs on
  // the FULL list — including thinking — so the run boundaries are stable
  // across the thinking toggle; thinking is filtered out at paint, not here.
  const renderItems = () => groupMessages(messages());
  const hiddenThinking = (m: ChatMessage) =>
    m.kind === 'text' && m.thinking === true && !showThinking();

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
            <For each={renderItems()}>
              {(item) => (
                <Switch>
                  <Match when={item.kind === 'toolGroup' && item}>
                    {(group) => (
                      <ToolGroupView
                        group={group()}
                        showThinking={showThinking()}
                        open={openGroups().has(group().key)}
                        onToggle={(o) => setGroupOpen(group().key, o)}
                      />
                    )}
                  </Match>
                  <Match when={item.kind === 'message' && item}>
                    {(msg) => (
                      <Show when={!hiddenThinking(msg().message)}>
                        <MessageView message={msg().message} />
                      </Show>
                    )}
                  </Match>
                </Switch>
              )}
            </For>
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
      <A
        href="/"
        class="crumb chat-back icon-btn"
        aria-label="Back to the dashboard"
        title="Back to the dashboard"
      >
        <Icon name="arrow-left" />
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
            <button
              type="button"
              class="icon-btn danger chat-stop"
              aria-label="Stop the instance"
              title="Stop the instance"
              onClick={() => setConfirming(true)}
            >
              <Icon name="square" />
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
  // Copy-raw (decision 14) is assistant-only: user replies are already plain
  // and selectable, thinking is hidden noise.
  const copyable = () => m().kind === 'text' && m().role !== 'user' && m().thinking !== true;
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
          {/* Markdown render (decision 3): every text/thinking message, any role. */}
          <Markdown source={m().text ?? ''} />
          <Show when={copyable()}>
            <div class="chat-msg-actions">
              <CopyButton text={m().text ?? ''} label="Copy" title="Copy the raw markdown" />
            </div>
          </Show>
        </div>
      </Match>
      <Match when={m().kind === 'tool'}>
        <ToolChip message={m()} />
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

// A single tool call: a one-line chip expanding on tap to its input/output.
// Tool I/O stays literal <pre> — it is command I/O, not markdown (decision 3).
function ToolChip(props: { message: ChatMessage }) {
  const t = () => props.message.tool;
  return (
    <details class="chat-tool">
      <summary classList={{ 'tool-summary': true, [`tool-${t()?.status ?? 'ok'}`]: true }}>
        <span class="tool-title">{t()?.title}</span>
        <span class="tool-status">{toolStatusMark(t()?.status)}</span>
      </summary>
      <Show when={t()?.input}>
        <pre class="tool-body mono">{t()?.input}</pre>
      </Show>
      <Show when={t()?.output}>
        <pre class="tool-body tool-output mono">{t()?.output}</pre>
      </Show>
    </details>
  );
}

// A run of 2+ tool calls behind one disclosure (decisions 8–11). The collapsed
// summary counts tools only and rolls up any failure; the open state is
// controlled by the parent so it survives SSE refetches (decision 12).
function ToolGroupView(props: {
  group: ToolGroup;
  showThinking: boolean;
  open: boolean;
  onToggle: (open: boolean) => void;
}) {
  const summary = () => toolGroupSummary(props.group);
  // Folded-in thinking renders in order inside the expanded group, but only
  // when the thinking toggle is on (decision 9).
  const items = () =>
    props.group.items.filter((m) => props.showThinking || !(m.kind === 'text' && m.thinking));
  return (
    <details
      class="chat-tool-group"
      open={props.open}
      onToggle={(e) => props.onToggle(e.currentTarget.open)}
    >
      <summary classList={{ 'tool-group-summary': true, 'has-error': props.group.errorCount > 0 }}>
        <span class="tool-group-count">{summary().label}</span>
        <Show when={summary().failed}>
          {(f) => <span class="tool-group-failed"> · {f()}</span>}
        </Show>
        <Show when={summary().running}>
          <span class="tool-group-running"> · running…</span>
        </Show>
      </summary>
      <div class="tool-group-body">
        <For each={items()}>{(m) => <MessageView message={m} />}</For>
      </div>
    </details>
  );
}

// --- Markdown rendering (issue #13) --------------------------------------
// The parser (lib/markdown) emits a plain node tree; the view maps it to Solid
// JSX nodes directly — never an HTML string, never innerHTML — so rendering is
// XSS-safe by construction. Only allowed-scheme hrefs reach an <a>.

function Markdown(props: { source: string }) {
  const blocks = createMemo(() => parseMarkdown(props.source));
  return (
    <div class="chat-msg-text md">
      <For each={blocks()}>{(b) => <BlockView block={b} />}</For>
    </div>
  );
}

function BlockView(props: { block: Block }) {
  return (
    <Switch>
      <Match when={props.block.type === 'heading' && props.block}>
        {(b) => (
          <Dynamic component={`h${Math.min(Math.max(b().level, 1), 6)}`} class="md-h">
            <InlineNodes nodes={b().children} />
          </Dynamic>
        )}
      </Match>
      <Match when={props.block.type === 'paragraph' && props.block}>
        {(b) => (
          <p class="md-p">
            <InlineNodes nodes={b().children} />
          </p>
        )}
      </Match>
      <Match when={props.block.type === 'code' && props.block}>
        {(b) => <CodeBlock lang={b().lang} text={b().text} />}
      </Match>
      <Match when={props.block.type === 'list' && props.block}>
        {(b) => <ListView block={b()} />}
      </Match>
      <Match when={props.block.type === 'blockquote' && props.block}>
        {(b) => (
          <blockquote class="md-quote">
            <For each={b().children}>{(c) => <BlockView block={c} />}</For>
          </blockquote>
        )}
      </Match>
      <Match when={props.block.type === 'table' && props.block}>
        {(b) => <TableView block={b()} />}
      </Match>
      <Match when={props.block.type === 'hr'}>
        <hr class="md-hr" />
      </Match>
    </Switch>
  );
}

function ListView(props: { block: Extract<Block, { type: 'list' }> }) {
  const items = () => props.block.items;
  return (
    <Switch>
      <Match when={props.block.ordered}>
        <ol class="md-list" start={props.block.start}>
          <For each={items()}>
            {(item) => (
              <li>
                <For each={item}>{(c) => <BlockView block={c} />}</For>
              </li>
            )}
          </For>
        </ol>
      </Match>
      <Match when={!props.block.ordered}>
        <ul class="md-list">
          <For each={items()}>
            {(item) => (
              <li>
                <For each={item}>{(c) => <BlockView block={c} />}</For>
              </li>
            )}
          </For>
        </ul>
      </Match>
    </Switch>
  );
}

function TableView(props: { block: Extract<Block, { type: 'table' }> }) {
  const align = (i: number) => props.block.align[i] ?? 'none';
  // Own overflow-x scroll container: faithful shape, swipe sideways on mobile
  // (decision 5) — never forces the message column to scroll horizontally.
  return (
    <div class="md-table-wrap">
      <table class="md-table">
        <thead>
          <tr>
            <For each={props.block.header}>
              {(cell, i) => (
                <th classList={{ [`md-align-${align(i())}`]: true }}>
                  <InlineNodes nodes={cell} />
                </th>
              )}
            </For>
          </tr>
        </thead>
        <tbody>
          <For each={props.block.rows}>
            {(row) => (
              <tr>
                <For each={row}>
                  {(cell, i) => (
                    <td classList={{ [`md-align-${align(i())}`]: true }}>
                      <InlineNodes nodes={cell} />
                    </td>
                  )}
                </For>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </div>
  );
}

function InlineNodes(props: { nodes: Inline[] }) {
  return <For each={props.nodes}>{(n) => <InlineNode node={n} />}</For>;
}

function InlineNode(props: { node: Inline }) {
  return (
    <Switch>
      <Match when={props.node.type === 'text' && props.node}>{(n) => <>{n().value}</>}</Match>
      <Match when={props.node.type === 'break'}>
        <br />
      </Match>
      <Match when={props.node.type === 'code' && props.node}>
        {(n) => <code class="md-code">{n().value}</code>}
      </Match>
      <Match when={props.node.type === 'strong' && props.node}>
        {(n) => (
          <strong>
            <InlineNodes nodes={n().children} />
          </strong>
        )}
      </Match>
      <Match when={props.node.type === 'em' && props.node}>
        {(n) => (
          <em>
            <InlineNodes nodes={n().children} />
          </em>
        )}
      </Match>
      <Match when={props.node.type === 'link' && props.node}>
        {(n) => (
          <a class="md-link" href={n().href} target="_blank" rel="noopener noreferrer">
            <InlineNodes nodes={n().children} />
          </a>
        )}
      </Match>
    </Switch>
  );
}

// A fenced code block with a claude.ai-style header bar: language label left,
// copy-raw button right, always visible (mobile has no hover). The copy source
// is the parser-retained literal fence content (decision 13).
function CodeBlock(props: { lang: string; text: string }) {
  return (
    <div class="md-codeblock">
      <div class="md-codeblock-bar">
        <span class="md-codeblock-lang">{props.lang || 'text'}</span>
        <CopyButton text={props.text} title="Copy the code" />
      </div>
      <pre class="md-pre mono">{props.text}</pre>
    </div>
  );
}

// Copy-to-clipboard with icon→check feedback (decision 15): inline SVG icons,
// navigator.clipboard (the embedded server is a secure context). Silent on a
// missing/blocked clipboard rather than throwing.
function CopyButton(props: { text: string; label?: string; title?: string }) {
  const [copied, setCopied] = createSignal(false);
  let timer: ReturnType<typeof setTimeout> | undefined;
  const copy = async () => {
    try {
      await navigator.clipboard?.writeText(props.text);
      setCopied(true);
      clearTimeout(timer);
      timer = setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable or denied — leave the button unchanged */
    }
  };
  onCleanup(() => clearTimeout(timer));
  return (
    <button
      type="button"
      class="copy-btn"
      classList={{ copied: copied() }}
      aria-label={props.label ?? 'Copy'}
      title={props.title ?? 'Copy'}
      onClick={() => void copy()}
    >
      <Switch>
        <Match when={copied()}>
          <Icon name="check" size={15} class="copy-icon" />
          <span class="copy-label">Copied</span>
        </Match>
        <Match when={!copied()}>
          <Icon name="copy" size={15} class="copy-icon" />
          <Show when={props.label}>
            <span class="copy-label">{props.label}</span>
          </Show>
        </Match>
      </Switch>
    </button>
  );
}

// Shared one-tap interrupt action (ADR-0022): POST /interrupt (the tmux Escape),
// no confirm, re-entrancy-guarded. Backs both the working-state morph button and
// the question/dialog escape-hatch squares, so the "one tap, no confirm" contract
// lives in exactly one place.
function createInterrupt(runID: () => string, onError: (m: string) => void, onDone: () => void) {
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
  const interrupt = createInterrupt(
    () => props.runID,
    (m) => props.onError(m),
    () => props.onSent(),
  );

  // The morph pivot (ADR-0022): while the agent is working the composer button
  // is a one-tap Interrupt, never a Send — nothing can be sent mid-turn, so the
  // "queued" affordance is gone from the UI (the backend Reply is untouched, it
  // is simply unreachable here until the agent idles). The textarea stays
  // editable throughout for compose-ahead.
  const working = () => props.state === 'working';
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

  const send = async () => {
    const body = text().trim();
    if (sending() || body === '') return;
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

  // Cmd/Ctrl+Enter sends in the sendable states only (decision 9a). Bare Enter
  // is never a send — a phone's return key must insert a newline — and while
  // working there is no keyboard interrupt (the square is the only interrupt).
  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !working()) {
      e.preventDefault();
      void send();
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
        {/* state 'question' with no structured dialog (a dormant transcript
            flush, or a shape lab can't render): the composer stays locked — a
            free-text reply would land in a focused picker — and the operator
            answers in claude.ai or interrupts (decision 5). */}
        <Match when={props.state === 'question'}>
          <div class="chat-dialog">
            <p class="chat-composer-note">Claude needs input — open it in claude.ai to respond.</p>
            <InterruptButton runID={props.runID} onError={props.onError} onDone={props.onSent} />
          </div>
        </Match>
        <Match when={true}>
          {/* Residual blocked state with no structured dialog — e.g. a plain
              tool-permission prompt or the post-decline "stuck" case (decision
              7). The composer stays usable, with a hint. */}
          <Show when={props.state === 'needs_input'}>
            <p class="chat-composer-note">
              Claude needs input — reply below, or open it in claude.ai.
            </p>
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
              onInput={(e) => setText(e.currentTarget.value)}
              onKeyDown={onKeyDown}
            />
            {/* One morphing button (decision 2): the SAME <button> element in
                both states — glyph, action, accessible name, and enabled/busy
                swap on working() — so keyboard focus is never dropped across the
                flip. Send (disabled when empty / in flight) when idle/needs_input;
                a one-tap Interrupt (always enabled, no confirm, pulsing) while
                working, showing the busy "…" while /interrupt is in flight. */}
            <button
              type="button"
              classList={{
                'icon-btn': true,
                'chat-send': !working(),
                'chat-interrupt': working(),
                pulse: working() && !interrupt.busy(),
                busy: working() ? interrupt.busy() : sending(),
              }}
              aria-label={working() ? 'Interrupt' : 'Send'}
              title={working() ? 'Interrupt the agent (Escape)' : 'Send'}
              disabled={working() ? false : !canSend()}
              onClick={() => void (working() ? interrupt.run() : send())}
            >
              <Show
                when={working() && interrupt.busy()}
                fallback={<Icon name={working() ? 'square' : 'send'} />}
              >
                <span class="chat-interrupt-busy" aria-hidden="true">
                  …
                </span>
              </Show>
            </button>
          </div>
          <Show when={working()}>
            <p class="chat-composer-hint">The agent is working — tap to interrupt.</p>
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

// One-tap Interrupt escape hatch (ADR-0022, superseding ADR-0016's confirm tap):
// a square accent icon-button that fires interruptRun (Escape) immediately, no
// confirmation — interrupt is non-destructive (the agent survives, idles, and is
// re-promptable), so a confirm tap is friction. Rendered in the locked
// question-state and in the DialogPanel (decision 5); it stays inert (no pulse),
// unlike the working-state morph button in Composer, which shares createInterrupt
// but draws its own pulsing square. Distinct from the header Stop, which stays
// danger-red + two-step (destructive teardown, ADR-0019).
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
      <Show when={interrupt.busy()} fallback={<Icon name="square" />}>
        <span class="chat-interrupt-busy" aria-hidden="true">
          …
        </span>
      </Show>
    </button>
  );
}

function toolStatusMark(status: string | undefined): string {
  if (status === 'ok') return '✓';
  if (status === 'error') return '✕';
  return '…';
}
