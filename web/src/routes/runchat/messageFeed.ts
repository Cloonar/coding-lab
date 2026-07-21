// The chat's message-feed state machine (ADR-0016, reshaped by issue #175 /
// ADR-0047): the seq-merged accumulated window, the FULL/LIGHT refetch
// protocol behind one monotonic fetch token and its trailing debounce,
// transcript rotation (issue #34), and the SSE subscriptions that drive it.
// The route injects run identity, the run refetch and scroll behavior via
// MessageFeedOptions; everything else lives here.

import { createEffect, createSignal, on, onCleanup, type Accessor, type Setter } from 'solid-js';
import {
  errorMessage,
  getRunMessages,
  type ChatMessage,
  type ConversationState,
  type Dialog,
  type MessagesResponse,
  type TranscriptStatus,
} from '../../api';
import { maxSeq, mergeMessages, mergeRefetch } from '../../lib/chatStream';
import { type EventsConnection } from '../../sse';

const MESSAGE_LIMIT = 60;

// run.messages.changed fires ~1/s while an agent streams (issue #175): the
// chat coalesces a burst on this trailing edge (mirroring liveResource's
// debounceMs pattern) before its light tail refetch, instead of refetching
// per event.
const MESSAGES_DEBOUNCE_MS = 300;

export interface MessageFeedOptions {
  /** The run id the feed follows — re-read fresh per fetch and per SSE event. */
  runId: () => string;
  /** The app-wide SSE connection (the useEvents() result). */
  events: EventsConnection;
  /** run.changed / resync refresh the run row alongside the FULL refetch. */
  refetchRun: () => void;
  /** The loaded run's repo id (undefined until loaded), for run.changed's repo filter. */
  currentRepoID: () => string | undefined;
  /** Whether the reader is at/near the stream's bottom — the follow gate. */
  isAtBottom: () => boolean;
  scrollToBottom: () => void;
  scrollToPendingCard: () => void;
  /** loadEarlier's prepend anchoring: capture the scroll geometry before the DOM grows… */
  captureAnchor: () => { scrollTop: number; scrollHeight: number } | undefined;
  /** …and restore the visual position after it has. */
  restoreAnchor: (before: { scrollTop: number; scrollHeight: number }) => void;
  /** resetStream's clearing of route-owned stream state (panel selection etc.). */
  onStreamReset: () => void;
}

export interface MessageFeed {
  messages: Accessor<ChatMessage[]>;
  state: Accessor<ConversationState>;
  transcript: Accessor<TranscriptStatus>;
  pendingDialogField: Accessor<Dialog | null>;
  hasMore: Accessor<boolean>;
  exhausted: Accessor<boolean>;
  error: Accessor<string | null>;
  setError: Setter<string | null>;
  refetchMessages: () => Promise<void>;
  loadEarlier: () => Promise<void>;
}

export function createMessageFeed(opts: MessageFeedOptions): MessageFeed {
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
  const [error, setError] = createSignal<string | null>(null);
  // The last pending dialog identity a refetch delivered: only a NOT-yet-seen
  // tool_id retargets the follow scroll to the card, so an SSE tick while the
  // same dialog stays pending never re-yanks the viewport (decision 5).
  let seenDialogToolId: string | null = null;

  // Monotonic fetch token: only the newest in-flight fetch may apply its
  // result, so a slow stale response can't revert an answered dialog to
  // pending and re-lock the composer. Shared by BOTH refetch modes below (and
  // loadEarlier), so a full refetch supersedes an in-flight light one and
  // vice versa.
  let fetchToken = 0;

  // The run.messages.changed trailing debounce (issue #175): the pending
  // flush accumulates the MINIMUM backpatchSeq announced across the burst —
  // the lowest seq whose content changed — so one light refetch covers every
  // mutation the burst named. A full refetch supersedes a queued light one
  // (route change, rotation, resync, run.changed and the POST callbacks all
  // route there) but ABSORBS the queued seq into its own tail start rather
  // than discarding it — backpatchSeq is a delta the server never re-sends
  // (ADR-0047). lastTickState tracks the envelope state across ticks for the
  // turn-settle escalation below.
  let messagesDebounceTimer: ReturnType<typeof setTimeout> | undefined;
  let pendingBackpatchSeq: number | undefined;
  let lastTickState: string | undefined;
  const clearMessagesDebounce = () => {
    clearTimeout(messagesDebounceTimer);
    messagesDebounceTimer = undefined;
    pendingBackpatchSeq = undefined;
  };
  onCleanup(clearMessagesDebounce);

  // One response envelope's non-window state, applied identically by both
  // refetch modes: conversational state, the pending dialog, transcript
  // status and the rotation id. Returns null when this refetch was superseded
  // mid-application (see the transcript_id re-guard), otherwise whether the
  // response brought a pending dialog this client hasn't shown yet.
  const applyEnvelope = (res: MessagesResponse, token: number): { newDialog: boolean } | null => {
    setState(res.state);
    // Does this response bring a pending dialog this client hasn't shown
    // yet? Decided BEFORE the tracking id updates, and tracked regardless of
    // follow — a scrolled-up reader is never yanked (the jump pill covers
    // them), and their card doesn't become "new" again later.
    const dialog = res.pending_dialog ?? null;
    const newDialog = dialog !== null && dialog.tool_id !== seenDialogToolId;
    seenDialogToolId = dialog?.tool_id ?? null;
    // Keep the dialog's OBJECT identity stable while the same dialog stays
    // pending (a dialog's content is immutable for its tool_id — the hook
    // spools it once). Each response is a fresh parse; adopting it would
    // ripple a no-op change through every downstream computation — the
    // option <For> would recreate its rows and the Other input would lose
    // focus (and, before the identity-memo guards below, its text) under
    // the operator's fingers on every SSE tick.
    setPendingDialogField((prev) =>
      prev !== null && dialog !== null && prev.tool_id === dialog.tool_id ? prev : dialog,
    );
    setTranscript(res.transcript);
    setTranscriptId(res.transcript_id);
    // Writing transcript_id can synchronously run the rotation effect (Solid
    // flushes user effects on a signal write), which resets the stream and
    // fires a superseding refetch — bumping fetchToken. If that happened, the
    // caller must NOT merge its pre-rotation tail back over the reset: a
    // transcript-A tail line whose seq exceeds B's would survive the
    // seq-keyed merge and leak forever (issue #34). Re-guard so the newer
    // refetch owns the stream.
    return token === fetchToken ? { newDialog } : null;
  };

  // The shared after=<start> tail pager: gap-free appends even when >limit
  // messages land between refetches. Returns null when a newer refetch
  // superseded this one mid-loop; `last` is the final page's envelope
  // (undefined only for start <= 0, which neither caller passes).
  const fetchTail = async (
    start: number,
    token: number,
  ): Promise<{ tail: ChatMessage[]; last: MessagesResponse | undefined } | null> => {
    const tail: ChatMessage[] = [];
    let last: MessagesResponse | undefined;
    let after = start;
    while (after > 0) {
      const res = await getRunMessages(opts.runId(), { after, limit: MESSAGE_LIMIT });
      if (token !== fetchToken) return null;
      last = res;
      tail.push(...res.messages);
      const top = maxSeq(res.messages);
      // A short window is the whole remaining tail; a stuck seq would loop.
      if (res.messages.length < MESSAGE_LIMIT || top <= after) break;
      after = top;
    }
    return { tail, last };
  };

  // Refetch protocol (ADR-0016, reshaped by issue #175): two modes over the
  // shared token guard and tail pager.
  //
  // FULL — after=<cursor> tail batches for gap-free appends, PLUS the latest
  // window for back-patched mutations near the tail (the only way to see them
  // when the trigger can't name what changed, or on a server predating
  // backpatchSeq). Merged by seq, later response wins — except unchanged
  // content, which keeps its identity (chatStream.ts). First load (no cursor
  // yet) is just the latest window. Used by first load, route change,
  // transcript rotation, resync, run.changed, and the send/answer/interrupt
  // callbacks.
  const refetchMessages = async () => {
    // A full refetch supersedes a queued light one — absorbing, never
    // discarding, its queued backpatchSeq: the latest window below only
    // reaches the newest 60 messages, so dropping a deeper announced seq
    // would leave it stale until navigation (ADR-0047).
    const pendingSeq = pendingBackpatchSeq;
    clearMessagesDebounce();
    const token = ++fetchToken;
    const cursor = maxSeq(messages());
    const start = pendingSeq === undefined ? cursor : Math.min(cursor, pendingSeq - 1);
    try {
      const paged = await fetchTail(start, token);
      if (paged === null) return;
      const latest = await getRunMessages(opts.runId(), { limit: MESSAGE_LIMIT });
      if (token !== fetchToken) return;
      const applied = applyEnvelope(latest, token);
      if (applied === null) return;
      if (!exhausted()) setHasMore(latest.has_more);
      // A first window that already covers the whole transcript means there
      // is nothing older to load — ever (messages only append).
      if (cursor === 0 && !latest.has_more && latest.messages.length > 0) setExhausted(true);
      // Follow-bottom only when the reader is already at/near the bottom (or
      // on the initial window), so reading history isn't yanked down.
      const follow = cursor === 0 || opts.isAtBottom();
      setMessages((prev) => mergeRefetch(prev, paged.tail, latest.messages));
      // A just-arrived dialog retargets the follow scroll from the stream's
      // bottom to its card's top (issue #56 decision 5); otherwise today's
      // plain bottom-follow.
      if (follow && applied.newDialog) opts.scrollToPendingCard();
      else if (follow) opts.scrollToBottom();
    } catch (err) {
      if (token === fetchToken) setError(errorMessage(err));
    }
  };

  // LIGHT — the run.messages.changed path (issue #175): tail pages only, from
  // after=min(cursor, backpatchSeq - 1), so appends AND every announced
  // back-patch arrive in one paging pass with NO latest-window request — the
  // per-second stream tick no longer re-downloads (and re-merges) a window of
  // unchanged messages. hasMore/exhausted are deliberately not written:
  // appends can never change whether OLDER history exists. Falls back to FULL
  // before the first window (cursor 0) and for a backpatch at seq 1 (after=0
  // means "latest window" on the wire, not "everything").
  const refetchMessagesLight = async (backpatchSeq?: number) => {
    const cursor = maxSeq(messages());
    const start = backpatchSeq !== undefined ? Math.min(cursor, backpatchSeq - 1) : cursor;
    if (start <= 0) {
      await refetchMessages();
      return;
    }
    const token = ++fetchToken;
    try {
      const paged = await fetchTail(start, token);
      if (paged === null || paged.last === undefined) return;
      // Same envelope post-processing as FULL, from the FINAL tail response:
      // dialog identity tracking, rotation re-guard, follow-bottom/newDialog.
      const applied = applyEnvelope(paged.last, token);
      if (applied === null) return;
      const follow = opts.isAtBottom();
      setMessages((prev) => mergeMessages(prev, paged.tail));
      if (follow && applied.newDialog) opts.scrollToPendingCard();
      else if (follow) opts.scrollToBottom();
    } catch (err) {
      if (token === fetchToken) setError(errorMessage(err));
    }
  };

  const loadEarlier = async () => {
    const first = messages()[0];
    if (first === undefined) return;
    const token = ++fetchToken;
    try {
      const res = await getRunMessages(opts.runId(), { before: first.seq, limit: MESSAGE_LIMIT });
      if (token !== fetchToken) return;
      setHasMore(res.has_more);
      if (!res.has_more || res.messages.length === 0) setExhausted(true);
      // Prepend anchoring by hand (iOS Safari has no overflow-anchor): capture
      // the geometry, then restore the visual position after the DOM grows.
      const before = opts.captureAnchor();
      setMessages((prev) => mergeMessages(prev, res.messages));
      if (before !== undefined) opts.restoreAnchor(before);
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
    opts.onStreamReset();
    // The settle-escalation tracker is stream state too: run A's `working`
    // must not read as a turn ending on run B's first tick.
    lastTickState = undefined;
  };

  // All chat state is keyed to the route param: navigating /runs/A → /runs/B
  // re-uses this component, so reset the accumulated stream and reload (the
  // token bump inside refetchMessages orphans any in-flight fetch for A).
  createEffect(
    on(
      () => opts.runId(),
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

  // The tailer's envelope (this run only) coalesces on a trailing debounce
  // (issue #175): while an agent streams it fires ~1/s, and refetching per
  // event rebuilt the view every second. The flush takes the LIGHT path with
  // the burst's minimum backpatchSeq; a tick that leaves `working` skips the
  // wait and escalates to FULL — the turn-settle self-heal (ADR-0047).
  // run.changed (outcome flips → composer
  // state) refetches FULL: it carries runID when the event concerns exactly
  // one run — a sibling run's flip is ignored — and an event WITHOUT runID
  // stays a conservative repo-filtered refetch (stop-all, AFK reaper, CR
  // merge, parked cleanup are genuinely repo-scoped).
  /* eslint-disable solid/reactivity -- handlers re-read runId() / the repo fresh per SSE event */
  onCleanup(
    opts.events.subscribe('run.messages.changed', (event) => {
      if (event.runID !== opts.runId()) return;
      // event.state rides the envelope too; this view keeps reading state
      // from the refetch responses so one source of truth feeds the composer.
      if (typeof event.backpatchSeq === 'number') {
        pendingBackpatchSeq =
          pendingBackpatchSeq === undefined
            ? event.backpatchSeq
            : Math.min(pendingBackpatchSeq, event.backpatchSeq);
      }
      // Turn-settle escalation (ADR-0047): the bus drops events for slow
      // subscribers and a dropped backpatchSeq is never re-sent, so a tick
      // that LEAVES `working` escalates to one FULL refetch — the
      // latest-window re-read the per-tick loop no longer does — repairing
      // whatever a lost announcement failed to name, at turn cadence instead
      // of tick cadence. Accumulate-then-escalate order matters: the FULL
      // absorbs the seq queued above, so an announcement riding this very
      // tick survives the escalation too.
      const settled =
        lastTickState === 'working' && typeof event.state === 'string' && event.state !== 'working';
      if (typeof event.state === 'string') lastTickState = event.state;
      if (settled) {
        void refetchMessages();
        return;
      }
      clearTimeout(messagesDebounceTimer);
      messagesDebounceTimer = setTimeout(() => {
        messagesDebounceTimer = undefined;
        const seq = pendingBackpatchSeq;
        pendingBackpatchSeq = undefined;
        void refetchMessagesLight(seq);
      }, MESSAGES_DEBOUNCE_MS);
    }),
  );
  onCleanup(
    opts.events.subscribe('run.changed', (event) => {
      // runID present = the event names exactly one run (issue #175): a
      // sibling run's flip must not refetch this open chat.
      if (typeof event.runID === 'string' && event.runID !== opts.runId()) return;
      const repoID = opts.currentRepoID();
      if (repoID !== undefined && event.repoID !== undefined && event.repoID !== repoID) return;
      opts.refetchRun();
      void refetchMessages();
    }),
  );
  // Reconnect = events lost while the socket was down. Other views recover via
  // createLiveResource (see ../../lib/liveResource), but the message accumulator
  // here is a cursor/seq-merge (ADR-0016), not a resource — so resync is wired
  // by hand instead of migrating this view to that helper.
  onCleanup(
    opts.events.subscribe('resync', () => {
      opts.refetchRun();
      void refetchMessages();
    }),
  );
  /* eslint-enable solid/reactivity */

  return {
    messages,
    state,
    transcript,
    pendingDialogField,
    hasMore,
    exhausted,
    error,
    setError,
    refetchMessages,
    loadEarlier,
  };
}
