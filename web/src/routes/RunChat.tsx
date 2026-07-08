// Embedded chat (/runs/:id, issue #7 / ADR-0016): the body IS the chat. A
// compact header (title · conversational state · deep link · New conversation ·
// Interrupt · Stop), the conversation stream (user/assistant text, tool chips,
// the pending dialog as an interactive inline card (issue #56), lifecycle/errors;
// thinking behind a toggle), and a fixed bottom composer whose state follows the
// run — collapsed to a waiting note while a dialog is pending, disabled for ended
// instances. Send is ALWAYS available and fires immediately (ADR-0029, issue #61);
// the one-tap turn Interrupt lives in the header next to Stop, gated on the live
// outcome — not the derived `working` state, which can be a stale-transcript-tail
// false positive (issue #38). Reads through GET /runs/:id/messages and refetches
// on run.messages.changed; replies / answers / interrupts POST back. Every string
// that names the agent derives from the provider's display metadata (issue #51
// decision 9). Phone-first, v0 design language.

import { A, useParams } from '@solidjs/router';
import { Dynamic, Portal } from 'solid-js/web';
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
  onMount,
} from 'solid-js';
import {
  answerRun,
  errorMessage,
  fetchRunCommands,
  getRun,
  getRunMessages,
  interruptRun,
  listProviders,
  replyRun,
  stopInstance,
  type ChatMessage,
  type ConversationState,
  type Dialog,
  type DialogOption,
  type Provider,
  type Question,
  type QuestionAnswer,
  type QuestionResult,
  type Run,
  type RunCommand,
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
  pillState,
  quickReturnReduce,
  type QuickReturnState,
} from '../lib/chatStream';
import { stateBadge } from '../lib/conversation';
import { openState, providerOpen, type OpenState } from '../lib/deepLink';
import { instanceTitle, sessionLabel, sessionRepo } from '../lib/instanceLabel';
import { parseMarkdown, type Block, type Inline } from '../lib/markdown';
import { clearQueued, peekQueued, queuedVersion, takeQueued } from '../lib/queuedMessage';
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

  // The run's slash-command catalog (issue #51 decision 5): chat-safe commands
  // for the composer autocomplete and the role=clear header affordance.
  // Worktree-dependent, hence per-run; the server caches ~30s. A failing
  // endpoint degrades to an empty catalog (no autocomplete, no affordance)
  // rather than breaking the chat.
  const [commands] = createResource(
    () => params.id,
    (id) => fetchRunCommands(id).catch(() => [] as RunCommand[]),
  );

  // The message window is accumulated (merged by seq) so scroll-up history and
  // in-place tool-status updates both survive a refetch.
  const [messages, setMessages] = createSignal<ChatMessage[]>([]);
  const [state, setState] = createSignal<ConversationState>('');
  const [transcript, setTranscript] = createSignal<TranscriptStatus>('available');
  // The located transcript's opaque identity (issue #34). A change means the
  // run's transcript rotated (a /clear or /rewind → new sessionId → new file),
  // so the accumulated stream must be dropped before the fresh seq-1 messages
  // merge in. undefined until the first response; older servers omit it.
  const [transcriptId, setTranscriptId] = createSignal<string | undefined>(undefined);
  // The live pending dialog from the server's top-level pending_dialog field
  // (ADR-0020) — the authoritative source, since the agent CLI never flushes a
  // pending tool_use to the transcript. null when none is pending.
  const [pendingDialogField, setPendingDialogField] = createSignal<Dialog | null>(null);
  const [hasMore, setHasMore] = createSignal(false);
  // A before-fetch hit the beginning: never resurrect "Load earlier" from a
  // latest-window has_more, which talks about ITS window, not our accumulated
  // stream.
  const [exhausted, setExhausted] = createSignal(false);
  const [showThinking, setShowThinking] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  // Auto-send of the queued first message (issue #41) waits for the first real
  // server response before trusting transcript(): the signal defaults
  // optimistically to 'available' before the initial fetch lands, and a reply
  // sent pre-locate can be lost.
  const [loaded, setLoaded] = createSignal(false);
  // A failed auto-send hands its text to the composer as a draft seed (adopted
  // only into an empty box — never clobbers active typing).
  const [draftSeed, setDraftSeed] = createSignal<string | null>(null);
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

  // --- Single scroll controller (issue #35 §2 + §4) --------------------------
  // One passive listener on .chat-stream drives BOTH the quick-return header
  // (§2, mobile) and the jump-to-latest pill (§4). Reactive outputs:
  const [headerVisible, setHeaderVisible] = createSignal(true); // §2 overlay shown?
  const [nearBottom, setNearBottom] = createSignal(true); // §4 at/near the end?
  // Append-only stream ⇒ "scrolled up" already means "newer content below" (§4).
  const pill = () => pillState(nearBottom(), state() === 'needs_input');

  // Imperative bookkeeping — plain lets, NOT signals: they mutate on every scroll
  // tick and must never schedule a render.
  let headerEl: HTMLElement | undefined; // forwarded up from ChatHeader, for measuring
  let pageEl: HTMLElement | undefined; // .chat-page, carries the --chat-header-h var
  let headerHeightPx = 0; // cached header height (the §2 gate; never read per-tick)
  let quickReturn: QuickReturnState = { lastScrollTop: 0, hideAccum: 0, visible: true };
  // Programmatic-scroll guard (§2 "only user-initiated scroll toggles the header").
  let programmaticScroll = false;
  let programmaticRaf = 0; // instant writes clear on the next frame
  let programmaticTimer = 0; // smooth (tap-to-latest) writes clear on a bounded timer

  const clearProgrammatic = () => {
    programmaticScroll = false;
    programmaticRaf = 0;
    programmaticTimer = 0;
    // Adopt the settled position as the baseline so the first genuine swipe
    // measures its delta from here, not the pre-jump value.
    if (streamEl !== undefined)
      quickReturn = { ...quickReturn, lastScrollTop: streamEl.scrollTop, hideAccum: 0 };
  };

  // Mark the coming scrollTop write(s) as programmatic so §2 ignores them. A
  // scrollTop write dispatches its scroll event asynchronously and browsers
  // coalesce writes into one event — or fire NONE if the value is unchanged — so
  // the flag is cleared by an UNCONDITIONAL rAF/timer, never inside the handler,
  // and thus can't get stuck. Instant writes settle within one frame; a smooth
  // scroll emits a burst over ~300ms, so it holds the flag on a bounded timer.
  const beginProgrammaticScroll = (smooth = false) => {
    programmaticScroll = true;
    if (programmaticRaf !== 0 && typeof cancelAnimationFrame !== 'undefined')
      cancelAnimationFrame(programmaticRaf);
    if (programmaticTimer !== 0) clearTimeout(programmaticTimer);
    programmaticRaf = 0;
    programmaticTimer = 0;
    if (smooth) {
      // A smooth scroll emits a burst of events over ~300ms; a bounded timer
      // outlasts it so the tail isn't misread as a user swipe.
      programmaticTimer = setTimeout(clearProgrammatic, 350);
    } else if (typeof requestAnimationFrame !== 'undefined') {
      programmaticRaf = requestAnimationFrame(clearProgrammatic);
    } else {
      clearProgrammatic();
    }
  };

  const scrollToBottom = () => {
    if (streamEl === undefined) return;
    beginProgrammaticScroll(); // instant — §2 must not read this as a user swipe
    streamEl.scrollTop = streamEl.scrollHeight;
  };

  // The interactive pending-dialog card in the stream (issue #56) — captured
  // for the arrival scroll below; whichever render position mounts it (at its
  // transcript message, or appended) assigns it. Imperative bookkeeping like
  // streamEl, never rendered.
  let dialogCardEl: HTMLDivElement | undefined;
  // The last pending dialog identity a refetch delivered: only a NOT-yet-seen
  // tool_id retargets the follow scroll to the card, so an SSE tick while the
  // same dialog stays pending never re-yanks the viewport (decision 5).
  let seenDialogToolId: string | null = null;

  // Bring the just-arrived dialog card's TOP into view (issue #56 decision 5):
  // the question text, not the stream's bottom, is the thing to read. Deferred
  // a microtask (like the composer's autoGrow) so the card rendered by this
  // refetch's signal writes has attached; guarded like every scripted scroll
  // (§2). jsdom has no scrollIntoView — optional call, like the autocomplete's.
  const scrollToPendingCard = () => {
    queueMicrotask(() => {
      const el = dialogCardEl;
      if (el === undefined || !el.isConnected) {
        // The card didn't render (e.g. the run ended under the dialog) — keep
        // the plain bottom-follow rather than dropping the scroll entirely.
        scrollToBottom();
        return;
      }
      beginProgrammaticScroll();
      el.scrollIntoView?.({ block: 'start' });
    });
  };

  const onScroll = () => {
    const el = streamEl;
    if (el === undefined) return;
    setNearBottom(isNearBottom(el)); // §4 — always, even during programmatic scroll
    if (programmaticScroll) {
      // Track the moving position but leave §2 visibility untouched.
      quickReturn = { ...quickReturn, lastScrollTop: el.scrollTop, hideAccum: 0 };
      return;
    }
    quickReturn = quickReturnReduce(quickReturn, el.scrollTop, headerHeightPx);
    setHeaderVisible(quickReturn.visible);
  };

  const prefersReducedMotion = () =>
    typeof window !== 'undefined' &&
    window.matchMedia?.('(prefers-reduced-motion: reduce)').matches === true;

  // §4 pill tap: smooth-scroll to the latest and EXPLICITLY reveal the header —
  // the jump moves downward (the §2 hide direction), so the reveal can't be
  // derived from scroll direction and must be forced (the programmatic guard
  // suppresses the reducer anyway).
  const jumpToLatest = () => {
    const el = streamEl;
    if (el === undefined) return;
    setHeaderVisible(true);
    quickReturn = { ...quickReturn, visible: true, hideAccum: 0 };
    const smooth = !prefersReducedMotion();
    beginProgrammaticScroll(smooth);
    if (smooth && typeof el.scrollTo === 'function')
      el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' });
    else el.scrollTop = el.scrollHeight;
  };

  // Cache the header height (the §2 gate) once on mount and whenever it resizes
  // (the mobile row wraps; "Confirm stop" changes it). Never read offsetHeight in
  // onScroll — that forces a synchronous reflow and defeats the composited overlay.
  const measureHeader = () => {
    if (headerEl === undefined) return;
    headerHeightPx = headerEl.offsetHeight; // 0 in jsdom → gate = scrollTop ≤ 0, harmless
    pageEl?.style.setProperty('--chat-header-h', `${headerHeightPx}px`);
  };

  onMount(() => {
    const el = streamEl;
    if (el === undefined) return;
    el.addEventListener('scroll', onScroll, { passive: true });
    onCleanup(() => el.removeEventListener('scroll', onScroll));
    measureHeader();
    if (typeof ResizeObserver !== 'undefined' && headerEl !== undefined) {
      const ro = new ResizeObserver(() => measureHeader());
      ro.observe(headerEl);
      onCleanup(() => ro.disconnect());
    }
  });

  onCleanup(() => {
    if (programmaticRaf !== 0 && typeof cancelAnimationFrame !== 'undefined')
      cancelAnimationFrame(programmaticRaf);
    if (programmaticTimer !== 0) clearTimeout(programmaticTimer);
  });

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
      // Does this response bring a pending dialog this client hasn't shown
      // yet? Decided BEFORE the tracking id updates, and tracked regardless of
      // follow — a scrolled-up reader is never yanked (the jump pill covers
      // them), and their card doesn't become "new" again later.
      const dialog = latest.pending_dialog ?? null;
      const newDialog = dialog !== null && dialog.tool_id !== seenDialogToolId;
      seenDialogToolId = dialog?.tool_id ?? null;
      setPendingDialogField(dialog);
      setTranscript(latest.transcript);
      setTranscriptId(latest.transcript_id);
      // Writing transcript_id can synchronously run the rotation effect (Solid
      // flushes user effects on a signal write), which resets the stream and
      // fires a superseding refetch — bumping fetchToken. If that happened, this
      // now-stale refetch must NOT merge its pre-rotation tail/latest back over
      // the reset: a transcript-A tail line whose seq exceeds B's would survive
      // the seq-keyed merge and leak forever (issue #34). Re-guard so the newer
      // refetch owns the stream.
      if (token !== fetchToken) return;
      if (!exhausted()) setHasMore(latest.has_more);
      // A first window that already covers the whole transcript means there
      // is nothing older to load — ever (messages only append).
      if (cursor === 0 && !latest.has_more && latest.messages.length > 0) setExhausted(true);
      // Follow-bottom only when the reader is already at/near the bottom (or
      // on the initial window), so reading history isn't yanked down.
      const follow = cursor === 0 || (streamEl !== undefined && isNearBottom(streamEl));
      setMessages((prev) => mergeRefetch(prev, tail, latest.messages));
      // A just-arrived dialog retargets the follow scroll from the stream's
      // bottom to its card's top (issue #56 decision 5); otherwise today's
      // plain bottom-follow.
      if (follow && newDialog) scrollToPendingCard();
      else if (follow) scrollToBottom();
      setLoaded(true); // the server's transcript status is now trustworthy
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
        // The anchor write adds the prepended height to scrollTop — a large
        // hide-direction delta that would otherwise slam the header shut (§2).
        beginProgrammaticScroll();
        streamEl.scrollTop = anchoredScrollTop(before, streamEl.scrollHeight);
      }
    } catch (err) {
      if (token === fetchToken) setError(errorMessage(err));
    }
  };

  // Reset the accumulated stream + derived state to the empty baseline. Shared
  // by the route-change reset and the transcript-rotation reset below.
  const resetStream = () => {
    setMessages([]);
    setState('');
    setPendingDialogField(null);
    // A fresh stream forgets the seen dialog too: a run navigated back into
    // (or rotated) with a still-pending dialog scrolls its card into view.
    seenDialogToolId = null;
    setTranscript('available');
    setHasMore(false);
    setExhausted(false);
    setError(null);
    setOpenGroups(new Set<number>());
    setLoaded(false); // a fresh transcript is unconfirmed until the next refetch
  };

  // All chat state is keyed to the route param: navigating /runs/A → /runs/B
  // re-uses this component, so reset the accumulated stream and reload (the
  // token bump inside refetchMessages orphans any in-flight fetch for A).
  createEffect(
    on(
      () => params.id,
      () => {
        resetStream();
        // Clear the rotation token too, so run B's first response (undefined →
        // its id) is not mistaken for a rotation by the effect below.
        setTranscriptId(undefined);
        void refetchMessages();
      },
    ),
  );

  // Transcript rotation (issue #34): /clear (or /rewind) starts a new sessionId →
  // transcript that the backend re-points this SAME run at, so the route-change
  // reset above never fires. The fresh transcript restarts seq at 1, which would
  // collide with the stale accumulated messages in the seq-keyed merge — so on a
  // genuine change of transcript_id, reset the stream exactly like a route change
  // and refetch: the chat visibly empties and the composer comes back to life.
  // Guarded to a real A → B change — the initial undefined → id set (and any
  // reset-to-undefined on navigation) is not a rotation.
  createEffect(
    on(
      transcriptId,
      (id, prev) => {
        // Only a real hash → different-hash change is a rotation. The empty
        // string is the server's locating/gone sentinel (transcript_id has no
        // omitempty), so `!id`/`!prev` skips locating→available (the first
        // locate) and available→gone — neither is a rotation, and the seq-1
        // collision only exists between two real transcripts.
        if (!id || !prev || id === prev) return;
        resetStream();
        void refetchMessages();
      },
      { defer: true },
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
  // Reconnect = events lost while the socket was down. Other views recover via
  // createLiveResource (see ../lib/liveResource), but the message accumulator
  // here is a cursor/seq-merge (ADR-0016), not a resource — so resync is wired
  // by hand instead of migrating this view to that helper.
  onCleanup(
    events.subscribe('resync', () => {
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

  // Provider display metadata for this run (issue #51 decision 9): every
  // user-facing string that names the agent derives from display_name, and the
  // "answer it elsewhere" hints derive from fallback_open — no provider brand
  // is ever hardcoded here.
  const runProvider = (): Provider | undefined => {
    const r = runData();
    const list = resourceValue(providers);
    if (r === undefined || list === undefined) return undefined;
    return list.find((p) => p.id === r.provider);
  };
  const agentName = () => runProvider()?.display_name ?? 'the agent';
  // Where to answer when lab can't: the provider's web surface host when it
  // has one (fallback_open), else the generic session wording for a
  // terminal-only provider (whose header offers the tmux attach).
  const openHint = () => {
    const fb = runProvider()?.fallback_open;
    if (fb === undefined || fb.url === '') return 'open the session';
    try {
      return `open it at ${new URL(fb.url).host}`;
    } catch {
      return 'open the session';
    }
  };

  // role=clear affordance (issue #51 decision 2): a clear-role catalog entry
  // binds the header's "New conversation" action; execution rides the NORMAL
  // reply path (the backend echoes the command as user text and the transcript
  // rotation resets the stream). New conversation follows the composer —
  // available whenever not composerLocked() (ended / transcript gone / dialog /
  // question). Its old `working` exclusion existed only to mirror the ADR-0022
  // send-block, which ADR-0029 (issue #61) removed; a mid-turn /clear now rides
  // the same TUI-queued reply path as any send, and a false `working` (a stale
  // transcript tail — issue #38) can no longer hide the affordance.
  const clearCommand = () => (commands() ?? []).find((c) => c.role === 'clear');
  const composerLocked = () =>
    ended() || transcript() === 'gone' || pendingDialog() !== null || state() === 'question';
  const canClear = () => clearCommand() !== undefined && !composerLocked();
  const sendClear = async () => {
    const name = clearCommand()?.name;
    if (name === undefined) return;
    await replyRun(params.id, `/${name}`);
    await refetchMessages();
  };

  // The queued first message parked by the New-run composer for THIS run
  // (issue #41), or undefined. Reactive on the queue's version signal so the
  // pending bubble appears/clears live.
  const queuedText = () => {
    queuedVersion();
    return peekQueued(params.id);
  };

  // Auto-send the queued first message exactly once, when the transcript is
  // confirmed available and the run is live. takeQueued() removes the entry
  // BEFORE the POST, so any re-fire (transcript/state churn, the version bump
  // from the take itself, even the transcript-rotation reset) sees no queued
  // text and cannot double-send; the in-flight boolean is a second belt for the
  // async gap. Keyed by run id, so navigating to another run just reads that
  // run's (absent) entry — nothing to reset here.
  let autoSending = false;
  createEffect(() => {
    queuedVersion(); // re-run when the queue mutates
    const id = params.id;
    const text = peekQueued(id);
    if (text === undefined || autoSending) return;
    if (!loaded()) return; // wait for the first real server response
    if (ended() || transcript() === 'gone') {
      // It can never send now — drop it and say so, rather than parking forever.
      clearQueued(id);
      setError(`Queued message not sent — the run ended before ${agentName()} was ready.`);
      return;
    }
    if (transcript() !== 'available') return; // still locating — a later refetch retries
    autoSending = true;
    const body = takeQueued(id) ?? text;
    void (async () => {
      try {
        await replyRun(id, body);
        await refetchMessages();
      } catch (err) {
        // Hand the text to the composer as a draft seed and surface the error;
        // the operator can retry or edit rather than lose it.
        setError(errorMessage(err));
        setDraftSeed(body);
      } finally {
        autoSending = false;
      }
    })();
  });
  // The pending dialog: the server's top-level pending_dialog field is
  // authoritative (ADR-0020 — the live-signals spool). The messages-scan is
  // the dormant fallback for a future provider CLI that flushes a pending
  // tool_use to the transcript; it is gated on state === 'question' so a dialog
  // answered outside the refetch window can't lock the composer forever.
  const pendingDialog = (): Dialog | null => {
    const field = pendingDialogField();
    if (field) return field;
    if (state() !== 'question') return null;
    const msgs = messages();
    for (let i = msgs.length - 1; i >= 0; i--) {
      const d = msgs[i]?.dialog;
      // An outcome-bearing dialog is ANSWERED (issue #56 decision 3) — the
      // fallback must never resurrect it as pending and re-lock the composer.
      if (msgs[i]?.kind === 'dialog' && d && d.outcome == null) return d;
    }
    return null;
  };
  // The pending dialog the STREAM should offer as an interactive card (issue
  // #56 decision 1): only while the run is live with a transcript — the same
  // gate the composer's Match order has always applied (ended/gone outrank
  // the dialog, an answer POST could land nowhere).
  const activeDialog = (): Dialog | null =>
    ended() || transcript() === 'gone' ? null : pendingDialog();
  // The interactive card replaces a stream dialog message when the two are
  // the SAME dialog (tool_id match): the card renders at the transcript
  // position, fed by the spool-authoritative field data (richer than a
  // transcript echo), and the dialog never shows twice. A stream dialog
  // message NOT matching the pending one keeps its inert prompt-only render.
  const pendingCardFor = (m: ChatMessage): Dialog | null => {
    const d = activeDialog();
    return d !== null && m.kind === 'dialog' && m.dialog?.tool_id === d.tool_id ? d : null;
  };
  // The usual case: the spool-served pending dialog has no transcript message
  // (the agent CLI never flushes a pending tool_use), so the card is appended
  // after the last stream item instead.
  const appendedDialog = (): Dialog | null => {
    const d = activeDialog();
    if (d === null) return null;
    const matched = messages().some((m) => m.kind === 'dialog' && m.dialog?.tool_id === d.tool_id);
    return matched ? null : d;
  };
  // Group consecutive tool runs at render time (decision 7). Grouping runs on
  // the FULL list — including thinking — so the run boundaries are stable
  // across the thinking toggle; thinking is filtered out at paint, not here.
  const renderItems = () => groupMessages(messages());
  const hiddenThinking = (m: ChatMessage) =>
    m.kind === 'text' && m.thinking === true && !showThinking();

  return (
    <main class="page chat-page" ref={pageEl}>
      {/* The banner sits above the body so the mobile overlay header (§2), which
          is absolutely positioned within .chat-body, can never paint over it. */}
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />

      {/* .chat-body is the positioning context for the quick-return header (§2):
          on mobile the header overlays the stream's top and slides via translateY
          while the stream reserves the header's band with padding-top, so hiding
          the header reclaims reading space with no reflow. */}
      <div class="chat-body">
        <ChatHeader
          run={runData()}
          providers={providers()}
          state={state()}
          showThinking={showThinking()}
          onToggleThinking={() => setShowThinking((v) => !v)}
          canClear={canClear()}
          onClear={sendClear}
          onError={setError}
          onChanged={() => void refetchRun()}
          onInterrupted={() => void refetchMessages()}
          hidden={!headerVisible()}
          headerRef={(el) => (headerEl = el)}
        />

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
                        // A dialog message matching the pending dialog renders
                        // as the interactive card AT ITS TRANSCRIPT POSITION
                        // (issue #56 decision 1) — never also as the inert
                        // prompt line, and never a second time below.
                        <Show
                          when={pendingCardFor(msg().message)}
                          fallback={
                            <Show when={!hiddenThinking(msg().message)}>
                              <MessageView message={msg().message} />
                            </Show>
                          }
                        >
                          {(d) => (
                            <DialogCard
                              runID={params.id}
                              dialog={d()}
                              openHint={openHint()}
                              onError={setError}
                              onAnswered={() => void refetchMessages()}
                              cardRef={(el) => (dialogCardEl = el)}
                            />
                          )}
                        </Show>
                      )}
                    </Match>
                  </Switch>
                )}
              </For>
            </Match>
          </Switch>
          {/* The pending dialog as an interactive stream card (issue #56
              decision 1), appended after the last item when no transcript
              message carries its tool_id — the usual case: the spool-served
              dialog never reaches the transcript. When one does, the card
              renders at that message's position above instead. */}
          <Show when={appendedDialog()}>
            {(d) => (
              <DialogCard
                runID={params.id}
                dialog={d()}
                openHint={openHint()}
                onError={setError}
                onAnswered={() => void refetchMessages()}
                cardRef={(el) => (dialogCardEl = el)}
              />
            )}
          </Show>
          {/* §3 — the needs-input note lives in the stream, not the composer: a
            subtle centered status line (styled like .chat-lifecycle, not a
            bubble), the last stream child so it only shows at the bottom.
            Reactive on state(); slides off with the rest as you scroll up. */}
          <Show when={state() === 'needs_input'}>
            <p class="chat-lifecycle chat-needs-input">
              {capitalize(agentName())} is waiting for your reply.
            </p>
          </Show>
          {/* Queued first message (issue #41): the operator's message parked by
              the New-run composer, shown as the LAST stream item — below the
              needs-input note, nearest the composer it collapses into — as a
              muted pending user bubble until the auto-send fires. Last-position
              mirrors chat order: history, then the "waiting" status, then the
              about-to-send message. The .pending hook is Phase 3's to restyle. */}
          <Show when={queuedText() !== undefined}>
            <div class="chat-msg role-user pending">
              <span class="chat-msg-tag">Sends when {agentName()} is ready</span>
              <Markdown source={queuedText() ?? ''} />
            </div>
          </Show>
        </div>
      </div>

      <Composer
        runID={params.id}
        state={state()}
        ended={ended()}
        transcript={transcript()}
        dialog={pendingDialog()}
        draftSeed={draftSeed()}
        commands={commands() ?? []}
        agentName={agentName()}
        openHint={openHint()}
        jumpVisible={pill().visible}
        jumpEmphasis={pill().emphasized}
        onJump={jumpToLatest}
        onError={setError}
        onSent={() => void refetchMessages()}
      />
    </main>
  );
}

/** Sentence-start capitalization for the "the agent" display-name fallback. */
function capitalize(s: string): string {
  return s === '' ? s : s.charAt(0).toUpperCase() + s.slice(1);
}

function ChatHeader(props: {
  run: Run | undefined;
  providers: Provider[] | undefined;
  state: ConversationState;
  showThinking: boolean;
  onToggleThinking: () => void;
  /** role=clear command available AND the composer is unlocked (issue #51). */
  canClear: boolean;
  /** Sends the provider's clear command via the normal reply path. */
  onClear: () => Promise<void>;
  onError: (message: string) => void;
  onChanged: () => void;
  /** One-tap turn Interrupt fired from the header/menu (ADR-0029) — refetch on done. */
  onInterrupted: () => void;
  hidden: boolean;
  headerRef: (el: HTMLElement) => void;
}) {
  const [confirming, setConfirming] = createSignal(false);
  const [stopping, setStopping] = createSignal(false);
  // "New conversation" (issue #51 decision 2): inline confirm — the chat
  // clears for real — then the clear command rides the normal reply path.
  const [clearConfirming, setClearConfirming] = createSignal(false);
  const [clearing, setClearing] = createSignal(false);
  // The affordance can vanish mid-confirm (a dialog locks the composer, the
  // run ends): drop the armed confirm so it can't linger invisibly and fire
  // on the next appearance.
  createEffect(() => {
    if (!props.canClear) setClearConfirming(false);
  });
  const doClear = async () => {
    if (clearing()) return;
    setClearing(true);
    try {
      await props.onClear();
      setClearConfirming(false);
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setClearing(false);
    }
  };
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

  // The header's one-tap turn Interrupt (ADR-0029), shared by the inline
  // desktop button and its `•••` menu twin.
  const interrupt = createInterrupt(
    () => props.run?.id ?? '',
    (m) => props.onError(m),
    () => props.onInterrupted(),
  );

  return (
    <header
      ref={props.headerRef}
      classList={{ 'chat-header': true, 'chat-header--hidden': props.hidden }}
    >
      <A href="/" class="crumb chat-back icon-btn" aria-label="Back to home" title="Back to home">
        <Icon name="arrow-left" />
      </A>
      <span class="chat-title">{title()}</span>

      {/* <640px: the convo chip distilled to a colored dot beside the title. A
          labeled graphic (role=img), not a live region — an aria-label swap
          wouldn't announce, and the full chip is display:none here so the dot is
          the only state cue a screen reader can reach on mobile. */}
      <Show when={badge()}>
        {(b) => (
          <span
            classList={{ 'chat-state-dot': true, [b().cls]: true }}
            title={b().title}
            aria-label={b().title}
            role="img"
          />
        )}
      </Show>
      {/* >=640px: the full text chip (same markup as before, now display-gated). */}
      <Show when={badge()}>
        {(b) => (
          <span
            classList={{ chip: true, convo: true, 'chat-state-chip': true, [b().cls]: true }}
            title={b().title}
          >
            {b().label}
          </span>
        )}
      </Show>

      <span class="spacer" />

      {/* >=640px: the original inline controls, verbatim. display:contents at
          >=640px makes this wrapper transparent (identical flex row to before);
          <640px the wrapper is display:none and the controls live in the menu. */}
      <div class="chat-desktop-actions">
        <Show when={props.canClear}>
          <Switch>
            <Match when={!clearConfirming()}>
              <button
                type="button"
                class="seg chat-new-convo"
                title="Clear this chat and start a fresh conversation"
                onClick={() => setClearConfirming(true)}
              >
                New conversation
              </button>
            </Match>
            <Match when={clearConfirming()}>
              <button
                type="button"
                class="seg chat-new-convo-confirm"
                title="This clears the chat history for real"
                disabled={clearing()}
                onClick={() => void doClear()}
              >
                {clearing() ? 'Clearing…' : 'Confirm clear'}
              </button>
              <button type="button" class="seg" onClick={() => setClearConfirming(false)}>
                Cancel
              </button>
            </Match>
          </Switch>
        </Show>
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
        {/* One-tap turn Interrupt (ADR-0029) — gated on live(), not the derived
            (possibly false) `working` state; accent + `pause`, deliberately
            distinct from the adjacent danger two-step `square` Stop. */}
        <Show when={live()}>
          <button
            type="button"
            class="icon-btn accent chat-turn-interrupt"
            classList={{ busy: interrupt.busy() }}
            aria-label="Interrupt"
            title="Interrupt the current turn (keeps the session)"
            onClick={() => void interrupt.run()}
          >
            <Show when={interrupt.busy()} fallback={<Icon name="pause" />}>
              <span class="chat-interrupt-busy" aria-hidden="true">
                …
              </span>
            </Show>
          </button>
        </Show>
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
      </div>

      {/* <640px: everything the one-line row can't hold moves into `•••`. */}
      <ChatMenu
        showThinking={props.showThinking}
        onToggleThinking={props.onToggleThinking}
        openState={open()}
        live={live()}
        confirming={confirming()}
        stopping={stopping()}
        onInterrupt={() => void interrupt.run()}
        onRequestStop={() => setConfirming(true)}
        onCancelStop={() => setConfirming(false)}
        onStop={() => void stop()}
        canClear={props.canClear}
        clearConfirming={clearConfirming()}
        clearing={clearing()}
        onRequestClear={() => setClearConfirming(true)}
        onCancelClear={() => setClearConfirming(false)}
        onClear={() => void doClear()}
      />
    </header>
  );
}

// The mobile (<640px) header overflow menu (§1): an anchored dropdown mirroring
// TopBar's nav-menu/nav-scrim pattern — outside-tap + Escape + tap-to-close —
// carrying the controls the one-line header can't hold inline: the thinking
// toggle, the open affordance (when available), New conversation, the one-tap
// turn Interrupt and the two-step Stop (both live only). `•••` is always
// present; the items are contextual. CSS hides the whole wrapper at >=640px,
// where the inline header controls render instead.
function ChatMenu(props: {
  showThinking: boolean;
  onToggleThinking: () => void;
  openState: OpenState | null;
  live: boolean;
  confirming: boolean;
  stopping: boolean;
  onInterrupt: () => void;
  onRequestStop: () => void;
  onCancelStop: () => void;
  onStop: () => void;
  canClear: boolean;
  clearConfirming: boolean;
  clearing: boolean;
  onRequestClear: () => void;
  onCancelClear: () => void;
  onClear: () => void;
}) {
  const [menuOpen, setMenuOpen] = createSignal(false);
  // Closing always abandons a half-armed Stop/Clear, so a confirm can't
  // linger invisibly and fire on the next open.
  const close = () => {
    setMenuOpen(false);
    props.onCancelStop();
    props.onCancelClear();
  };

  // Close on Escape while open (mirrors TopBar).
  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Escape' && menuOpen()) close();
  };
  window.addEventListener('keydown', onKeyDown);
  onCleanup(() => window.removeEventListener('keydown', onKeyDown));

  return (
    <div class="chat-menu">
      <button
        type="button"
        class="icon-btn chat-menu-toggle"
        aria-label="More actions"
        title="More actions"
        aria-haspopup="menu"
        aria-expanded={menuOpen()}
        aria-controls={menuOpen() ? 'chat-menu-panel' : undefined}
        onClick={() => (menuOpen() ? close() : setMenuOpen(true))}
      >
        <Icon name="more-horizontal" />
      </button>

      <Show when={menuOpen()}>
        <div class="chat-menu-panel" id="chat-menu-panel" role="menu">
          <button
            type="button"
            class="chat-menu-item"
            role="menuitem"
            onClick={() => {
              props.onToggleThinking();
              close();
            }}
          >
            <Icon name="message-square" class="chat-menu-icon" />
            <span>{props.showThinking ? 'Hide thinking' : 'Show thinking'}</span>
          </button>

          {/* Presentational wrapper (role=none): the OpenAffordance renders its
              own focusable <a>/<button>, so nesting it inside a role=menuitem
              would be invalid ARIA. Closing on click still works via bubbling. */}
          <Show when={props.openState}>
            {(s) => (
              <div class="chat-menu-item chat-menu-open" role="none" onClick={close}>
                <OpenAffordance state={s()} />
              </div>
            )}
          </Show>

          {/* New conversation (issue #51): same two-step confirm as Stop —
              the clear command wipes the chat for real. */}
          <Show when={props.canClear}>
            <Switch>
              <Match when={!props.clearConfirming}>
                <button
                  type="button"
                  class="chat-menu-item"
                  role="menuitem"
                  onClick={() => props.onRequestClear()}
                >
                  <Icon name="plus" class="chat-menu-icon" />
                  <span>New conversation…</span>
                </button>
              </Match>
              <Match when={props.clearConfirming}>
                <button
                  type="button"
                  class="chat-menu-item"
                  role="menuitem"
                  disabled={props.clearing}
                  onClick={() => props.onClear()}
                >
                  <Icon name="plus" class="chat-menu-icon" />
                  <span>{props.clearing ? 'Clearing…' : 'Confirm clear'}</span>
                </button>
                <button
                  type="button"
                  class="chat-menu-item"
                  role="menuitem"
                  onClick={() => props.onCancelClear()}
                >
                  <span class="chat-menu-icon" aria-hidden="true" />
                  <span>Cancel</span>
                </button>
              </Match>
            </Switch>
          </Show>

          {/* One-tap turn Interrupt (ADR-0029): no confirm step, unlike the
              two-step Stop below — it keeps the session, just ends the turn. */}
          <Show when={props.live}>
            <button
              type="button"
              class="chat-menu-item accent"
              role="menuitem"
              title="Interrupt the current turn (keeps the session)"
              onClick={() => {
                props.onInterrupt();
                close();
              }}
            >
              <Icon name="pause" class="chat-menu-icon" />
              <span>Interrupt</span>
            </button>
          </Show>

          <Show when={props.live}>
            <Switch>
              <Match when={!props.confirming}>
                <button
                  type="button"
                  class="chat-menu-item danger"
                  role="menuitem"
                  onClick={() => props.onRequestStop()}
                >
                  <Icon name="square" class="chat-menu-icon" />
                  <span>Stop run…</span>
                </button>
              </Match>
              <Match when={props.confirming}>
                <button
                  type="button"
                  class="chat-menu-item danger"
                  role="menuitem"
                  disabled={props.stopping}
                  onClick={() => props.onStop()}
                >
                  <Icon name="square" class="chat-menu-icon" />
                  <span>{props.stopping ? 'Stopping…' : 'Confirm stop'}</span>
                </button>
                <button
                  type="button"
                  class="chat-menu-item"
                  role="menuitem"
                  onClick={() => props.onCancelStop()}
                >
                  <span class="chat-menu-icon" aria-hidden="true" />
                  <span>Cancel</span>
                </button>
              </Match>
            </Switch>
          </Show>
        </div>
      </Show>

      {/* Portal the scrim to document.body: the mobile header is its own
          stacking context (position:absolute; z-index:2), so a scrim nested
          inside it would dim the header's own back/title. At body level it sits
          below the header and dims only the page behind — like TopBar's scrim. */}
      <Show when={menuOpen()}>
        <Portal>
          <div class="chat-menu-scrim" aria-hidden="true" onClick={close} />
        </Portal>
      </Show>
    </div>
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
      {/* An ANSWERED dialog stays in history as a compact, inert Q→A summary
          (issue #56 decision 3). Outcome PRESENCE is the answered signal —
          never the inner fields: a plan rejected without typed feedback
          serializes as an all-omitempty {}. A dialog message WITHOUT an
          outcome keeps the prompt-only line below (the pending interactive
          card is intercepted upstream by pendingCardFor, not here). */}
      <Match when={m().kind === 'dialog' && m().dialog?.outcome ? m().dialog : null}>
        {(d) => <AnsweredDialog dialog={d()} />}
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

// A fenced code block with a chat-app-style header bar: language label left,
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

// Shared one-tap interrupt action (ADR-0029): POST /interrupt (the tmux Escape),
// no confirm, re-entrancy-guarded. Backs the header's turn Interrupt (and its
// mobile menu twin) plus the two question/dialog escape-hatch buttons, so the
// "one tap, no confirm" contract lives in exactly one place.
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
  /** A handed-down draft (a failed queued-message auto-send), or null. */
  draftSeed: string | null;
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
  onSent: () => void;
}) {
  const [text, setText] = createSignal('');
  const [sending, setSending] = createSignal(false);

  // Adopt a handed-down draft seed (issue #41: a failed auto-send's text) ONLY
  // into an empty box, so it never clobbers what the operator is actively
  // typing. `on`+defer reacts to seed CHANGES only, and reads text() untracked
  // so keystrokes don't re-trigger the adoption.
  createEffect(
    on(
      () => props.draftSeed,
      (seed) => {
        if (seed != null && seed !== '' && text().trim() === '') setText(seed);
      },
      { defer: true },
    ),
  );
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

  // --- Slash-command autocomplete (issue #51 decision 5) --------------------
  // Prefix-only: the popover exists only while the WHOLE input starts with '/'
  // (a mid-message slash never triggers it). The filter is a case-insensitive
  // substring match across name, description and arg hint. Accepting inserts
  // "/name " and keeps focus in the textarea; sending stays the NORMAL reply
  // path — the backend renders the command echo as user text (issue #51
  // decision 2), so nothing here special-cases a slash message.
  const [acDismissed, setAcDismissed] = createSignal(false); // Escape until the next keystroke
  const [acIndex, setAcIndex] = createSignal(0);
  const acMatches = (): RunCommand[] => {
    const value = text();
    if (!value.startsWith('/')) return [];
    const q = value.slice(1).toLowerCase();
    return props.commands.filter((c) =>
      [c.name, c.description ?? '', c.arg_hint ?? ''].some((f) => f.toLowerCase().includes(q)),
    );
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
  const acceptCommand = (cmd: RunCommand) => {
    setText(`/${cmd.name} `);
    // Dismiss until the next keystroke: Enter must go back to being a newline
    // once the pick landed (descriptions often still match "name ").
    setAcDismissed(true);
    inputEl?.focus();
  };

  // Cmd/Ctrl+Enter now sends in every unlocked state (ADR-0029, issue #61): Send
  // is always available, so the shortcut no longer gates on `working`. Bare Enter
  // is never a send — a phone's return key must insert a newline. While the
  // command popover is open, Up/Down cycle, Enter/Tab accept and Escape closes;
  // Cmd/Ctrl+Enter still sends over it.
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
        e.preventDefault();
        setAcDismissed(true);
        return;
      }
      if (e.key === 'Enter' || e.key === 'Tab') {
        e.preventDefault();
        const pick = matches[Math.min(acIndex(), matches.length - 1)];
        if (pick !== undefined) acceptCommand(pick);
        return;
      }
    }
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
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
            waiting note pointing up at it plus the Interrupt escape hatch —
            no textarea, free text can't answer a focused picker (decision 2).
            Mirrors the degraded question-state branch below. */}
        <Match when={props.dialog}>
          <div class="chat-dialog">
            <p class="chat-composer-note">
              {capitalize(props.agentName)} is waiting on your answer — see the question above.
            </p>
            <InterruptButton runID={props.runID} onError={props.onError} onDone={props.onSent} />
          </div>
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
              the textarea's focus. */}
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
                    onClick={() => acceptCommand(cmd)}
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

// The pending dialog as a full-width stream card (issue #56 decision 1): the
// interactive DialogPanel wrapped in house card chrome, rendered inside
// .chat-stream — at its matching transcript message's position when one
// exists, else appended after the last item. cardRef hands the element up for
// the arrival scroll (decision 5).
function DialogCard(props: {
  runID: string;
  dialog: Dialog;
  openHint: string;
  onError: (message: string) => void;
  onAnswered: () => void;
  cardRef: (el: HTMLDivElement) => void;
}) {
  return (
    <div class="chat-dialog-card" ref={(el) => props.cardRef(el)}>
      <DialogPanel
        runID={props.runID}
        dialog={props.dialog}
        openHint={props.openHint}
        onError={props.onError}
        onAnswered={props.onAnswered}
      />
    </div>
  );
}

/**
 * An answered dialog message in history (issue #56 decision 3): a compact,
 * inert Q→A summary — visibly quieter than the interactive card, no buttons,
 * no unchosen options. The caller guards on outcome PRESENCE (the answered
 * signal); this component never re-derives it from inner fields.
 */
function AnsweredDialog(props: { dialog: Dialog }) {
  const outcome = () => props.dialog.outcome ?? {};
  const results = () => outcome().results ?? [];
  // Question texts for the renders that have no per-question answers to show
  // (dismissed, or an empty outcome): the outcome's own texts when recorded,
  // else the dialog's questions, else the prompt.
  const questionTexts = () => {
    if (results().length > 0) return results().map((r) => r.question);
    const questions = props.dialog.questions ?? [];
    if (questions.length > 0) return questions.map((q) => q.text);
    return [props.dialog.prompt];
  };
  const planMarker = () =>
    outcome().dismissed === true
      ? 'Plan dismissed'
      : outcome().approved === true
        ? 'Plan approved'
        : 'Plan rejected';
  return (
    <div class="chat-dialog-answered">
      <Switch>
        {/* Plan review: the FULL plan markdown stays readable in history
            (same .chat-dialog-plan render as the live card), followed by a
            one-line resolution marker; typed rejection feedback reads as an
            operator quote. */}
        <Match when={props.dialog.dialog_kind === 'plan'}>
          <div class="chat-dialog-plan">
            <Markdown source={props.dialog.prompt} />
          </div>
          <p class="chat-dialog-outcome">{planMarker()}</p>
          <Show when={outcome().dismissed !== true && outcome().approved !== true}>
            <Show when={outcome().feedback}>
              {(f) => (
                <p class="dialog-qa-answer">
                  <span class="dialog-qa-other">“{f()}”</span>
                </p>
              )}
            </Show>
          </Show>
        </Match>
        {/* Dismissed question/approval: the question text(s) with ONE
            dismissed marker — there is no answer to show. */}
        <Match when={outcome().dismissed === true}>
          <For each={questionTexts()}>{(text) => <p class="dialog-qa-question">{text}</p>}</For>
          <p class="chat-dialog-outcome">Dismissed</p>
        </Match>
        {/* Question kind (and the reserved approval kind): one Q→A pair per
            recorded result, dialog order. */}
        <Match when={results().length > 0}>
          <For each={results()}>
            {(r) => (
              <div class="dialog-qa">
                <p class="dialog-qa-question">{r.question}</p>
                <AnswerLine result={r} />
              </div>
            )}
          </For>
        </Match>
        {/* Answered but nothing recorded (an empty, non-dismissed outcome on
            a question) — mark the questions unanswered rather than vanish. */}
        <Match when={true}>
          <For each={questionTexts()}>
            {(text) => (
              <div class="dialog-qa">
                <p class="dialog-qa-question">{text}</p>
                <p class="dialog-qa-answer">
                  <span class="dialog-qa-none">No answer recorded</span>
                </p>
              </div>
            )}
          </For>
        </Match>
      </Switch>
    </div>
  );
}

/**
 * One recorded answer line: the chosen labels joined ", " (recorded toggle
 * order), then any typed Other text as a quoted span — visually distinct
 * from a listed label; a multi-select result can carry both. Neither means
 * that question got no recorded answer → the unanswered marker.
 */
function AnswerLine(props: { result: QuestionResult }) {
  const chosen = () => props.result.chosen ?? [];
  const other = () => props.result.other_text ?? '';
  return (
    <p class="dialog-qa-answer">
      <Show when={chosen().length > 0}>
        <span class="dialog-qa-chosen">{chosen().join(', ')}</span>
      </Show>
      <Show when={other() !== ''}>
        <span class="dialog-qa-other">“{other()}”</span>
      </Show>
      <Show when={chosen().length === 0 && other() === ''}>
        <span class="dialog-qa-none">No answer recorded</span>
      </Show>
    </p>
  );
}

function DialogPanel(props: {
  runID: string;
  dialog: Dialog;
  /** "open it at <host>" / "open the session" — for the degraded note. */
  openHint: string;
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
  const questions = () => props.dialog.questions ?? [];
  const answer = async (payload: {
    index?: number;
    selected?: number[];
    other_text?: string;
    answers?: QuestionAnswer[];
  }) => {
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
      {/* A plan review's prompt IS the plan body — rendered as markdown in
          full (issue #56 decision 4): the chat pane is the only scrollbar, so
          approve/reject sit after the whole plan. Every other kind keeps the
          plain one-line prompt. */}
      <Show
        when={props.dialog.dialog_kind === 'plan'}
        fallback={<p class="chat-dialog-prompt">{props.dialog.prompt}</p>}
      >
        <div class="chat-dialog-plan">
          <Markdown source={props.dialog.prompt} />
        </div>
      </Show>
      <Show
        when={props.dialog.answerable}
        fallback={
          <p class="chat-composer-note">
            This dialog can't be answered here — {props.openHint} to respond.
          </p>
        }
      >
        <Switch>
          {/* Multi-question form (issue #51 decision 3): one atomic submit. */}
          <Match when={questions().length > 0}>
            <MultiQuestionForm
              questions={questions()}
              resetKey={props.dialog.tool_id}
              busy={busy()}
              onSubmit={(answers) => void answer({ answers })}
            />
          </Match>
          {/* Multi-select: toggle cards (issue #56 decision 7 — the former
              bare checkbox rows dropped the option descriptions) + a single
              confirm. The selection signal and Submit payload are unchanged. */}
          <Match when={props.dialog.multi}>
            <ul class="dialog-options">
              <For each={options()}>
                {(opt, i) => (
                  <li>
                    <OptionToggle
                      option={opt}
                      pressed={selected().includes(i())}
                      disabled={busy()}
                      onToggle={() => toggle(i())}
                    />
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
          {/* Single-select: one button per option; the Other row takes text.
              Plan approval and the generic 'approval' kind land here too —
              their pinned options answer as a flat single-select index. */}
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
                            aria-label="Other — type your answer"
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
                        class="dialog-option"
                        disabled={busy()}
                        onClick={() => void answer({ index: i() })}
                      >
                        <OptionContent option={opt} />
                      </button>
                    </Show>
                  </li>
                )}
              </For>
            </ul>
          </Match>
        </Switch>
      </Show>
    </div>
  );
}

/**
 * One option's face — bold label over the ALWAYS-visible muted description
 * (issue #56 decision 7) — shared by every option-card shape so single-select
 * rows, multi-select toggles and plan approve/reject rows read alike. The
 * body wrapper takes the card's flexible column so a check indicator can ride
 * the row's trailing edge.
 */
function OptionContent(props: { option: DialogOption }) {
  return (
    <span class="dialog-option-body">
      <span class="dialog-option-label">{props.option.label}</span>
      <Show when={props.option.description}>
        <span class="dialog-option-desc">{props.option.description}</span>
      </Show>
    </span>
  );
}

/**
 * A toggleable option card (issue #56 decision 7): the shared card face plus
 * a pressed state — accent border, subtle tint and a check indicator —
 * announced via aria-pressed. Used by the flat multi-select path and the
 * multi-question form; tapping only toggles, submission stays with the
 * form's own Submit.
 */
function OptionToggle(props: {
  option: DialogOption;
  pressed: boolean;
  disabled: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      classList={{ 'dialog-option': true, selected: props.pressed }}
      aria-pressed={props.pressed}
      disabled={props.disabled}
      onClick={() => props.onToggle()}
    >
      <OptionContent option={props.option} />
      <Show when={props.pressed}>
        <span class="dialog-option-check" aria-hidden="true">
          <Icon name="check" size={16} />
        </span>
      </Show>
    </button>
  );
}

/**
 * The multi-question form (issue #51 decision 3), rendered the way the
 * provider's own app renders question forms: stacked questions in order, each
 * a small header chip + the question text + option buttons; multi_select
 * questions toggle; the synthesized "Other" row (is_other) opens a free-text
 * input for a single-select question. ONE submit answers all questions
 * atomically via answers[] — disabled until every question is answered (Other
 * requires non-empty text).
 *
 * Answer encoding (provider.go QuestionAnswer is authoritative): association
 * is POSITIONAL — answers[i] answers questions[i]. Each entry mirrors the
 * flat single-question shape: a single-select question sends `index` = the
 * chosen OPTION index (+ `other_text` when that row is the is_other row); a
 * multi_select question sends `selected` = the toggled option indices
 * ascending, `index` absent. The adapter drives real picker rows from these
 * fields, so this shape is a wire contract, not a UI convenience.
 */
function MultiQuestionForm(props: {
  questions: Question[];
  /** The dialog identity (tool_id): selections reset when it changes. */
  resetKey: string;
  busy: boolean;
  onSubmit: (answers: QuestionAnswer[]) => void;
}) {
  const [picks, setPicks] = createSignal<ReadonlyMap<number, number[]>>(new Map());
  const [others, setOthers] = createSignal<ReadonlyMap<number, string>>(new Map());

  createEffect(
    on(
      () => props.resetKey,
      () => {
        setPicks(new Map());
        setOthers(new Map());
      },
      { defer: true },
    ),
  );

  // The renderable options of a question, keeping their ORIGINAL option
  // indices (the adapter navigates picker rows by index). Multi-select
  // questions drop the synthesized Other row: the adapter rejects Other
  // inside a multi-select answer by policy (normSelected — the TUI's
  // Other-in-multi-select recipe is unverified), so offering the row here
  // would only collect an answer the backend bounces.
  const visibleOptions = (q: Question): { opt: DialogOption; idx: number }[] =>
    q.options
      .map((opt, idx) => ({ opt, idx }))
      .filter(({ opt }) => !(q.multi_select === true && opt.is_other === true));

  const picked = (qi: number) => picks().get(qi) ?? [];
  const otherText = (qi: number) => others().get(qi) ?? '';
  const isPicked = (qi: number, oi: number) => picked(qi).includes(oi);
  const togglePick = (qi: number, oi: number, multi: boolean) =>
    setPicks((prev) => {
      const next = new Map(prev);
      const current = prev.get(qi) ?? [];
      if (multi) {
        next.set(
          qi,
          current.includes(oi)
            ? current.filter((x) => x !== oi)
            : [...current, oi].sort((a, b) => a - b),
        );
      } else {
        // Single select: picking replaces; re-picking clears.
        next.set(qi, current.includes(oi) ? [] : [oi]);
      }
      return next;
    });
  const setOther = (qi: number, value: string) => setOthers((prev) => new Map(prev).set(qi, value));

  const otherPicked = (qi: number, q: Question) =>
    picked(qi).some((oi) => q.options[oi]?.is_other === true);
  const questionAnswered = (qi: number, q: Question) =>
    picked(qi).length > 0 && (!otherPicked(qi, q) || otherText(qi).trim() !== '');
  const complete = () => props.questions.every((q, qi) => questionAnswered(qi, q));

  const submit = () => {
    if (!complete()) return;
    props.onSubmit(
      props.questions.map((q, qi): QuestionAnswer => {
        if (q.multi_select === true) return { selected: picked(qi) }; // ascending by construction
        const idx = picked(qi)[0] ?? 0; // complete() guarantees exactly one pick
        const a: QuestionAnswer = { index: idx };
        if (q.options[idx]?.is_other === true) a.other_text = otherText(qi).trim();
        return a;
      }),
    );
  };

  return (
    <>
      <div class="dialog-questions">
        <For each={props.questions}>
          {(q, qi) => (
            <div class="dialog-question" role="group" aria-label={q.text}>
              <Show when={q.header}>
                <span class="chip dialog-question-header">{q.header}</span>
              </Show>
              <p class="chat-dialog-prompt">{q.text}</p>
              <ul class="dialog-options">
                <For each={visibleOptions(q)}>
                  {(entry) => (
                    <li>
                      <OptionToggle
                        option={entry.opt}
                        pressed={isPicked(qi(), entry.idx)}
                        disabled={props.busy}
                        onToggle={() => togglePick(qi(), entry.idx, q.multi_select === true)}
                      />
                      <Show when={entry.opt.is_other && isPicked(qi(), entry.idx)}>
                        <input
                          class="chat-input dialog-other-input"
                          placeholder="Type your answer…"
                          aria-label={`Other answer for: ${q.text}`}
                          value={otherText(qi())}
                          onInput={(e) => setOther(qi(), e.currentTarget.value)}
                        />
                      </Show>
                    </li>
                  )}
                </For>
              </ul>
            </div>
          )}
        </For>
      </div>
      {/* ONE atomic submit for every question (issue #51 decision 3). */}
      <button type="button" class="chat-send" disabled={props.busy || !complete()} onClick={submit}>
        Submit
      </button>
    </>
  );
}

// One-tap Interrupt escape hatch (ADR-0029, superseding ADR-0016's confirm tap):
// an accent `pause` icon-button that fires interruptRun (Escape) immediately, no
// confirmation — interrupt is non-destructive (the agent survives, idles, and is
// re-promptable), so a confirm tap is friction. Rendered in the composer's two
// locked branches — dialog-pending waiting note (issue #56 decision 2) and the
// degraded question state (decision 5) — so exactly one always-visible escape
// hatch exists however far the stream card scrolls. It shares the `pause` glyph
// with the header's turn Interrupt (the composer Send no longer morphs — ADR-0029),
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

function toolStatusMark(status: string | undefined): string {
  if (status === 'ok') return '✓';
  if (status === 'error') return '✕';
  return '…';
}
