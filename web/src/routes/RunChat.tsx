// Embedded chat (/runs/:id, issue #7 / ADR-0016): the body IS the chat. A
// compact header (title · conversational state · spawn-time model chip (issue
// #68) · deep link · Interrupt · Stop), the conversation stream (user/assistant
// text, tool chips, the pending dialog as an interactive inline card (issue
// #56), lifecycle/errors; thinking permanently hidden at paint — issue #68),
// and a fixed bottom composer whose state follows the run — collapsed to a
// waiting note while a dialog is pending, disabled for ended instances. Tool
// chips and group summaries are buttons whose click branches on the breakpoint
// (issue #154): on desktop (>=1024px) they toggle a RICH inline expansion in
// place (a lone chip reveals its ToolViewBody, a group reveals its member
// chips); on phones they open the tool detail bottom sheet exactly as before
// (issue #145) — while the pending-dialog card stays inline. Send is
// ALWAYS available and fires immediately (ADR-0029, issue #61);
// the one-tap turn Interrupt lives in the header next to Stop, gated on the live
// outcome — not the derived `working` state, which can be a stale-transcript-tail
// false positive (issue #38). Reads through GET /runs/:id/messages; the tailer's
// ~1/s run.messages.changed ticks coalesce on a trailing debounce into a LIGHT
// tail-only refetch (after=min(cursor, backpatchSeq-1) — issue #175, no
// per-second latest-window re-merge), while route changes, transcript rotation,
// resync, run.changed (sibling-run events filtered by runID) and the POST
// callbacks (send/answer/interrupt) take the FULL protocol; replies / answers /
// interrupts POST back. Every string that names the agent derives from the
// provider's display metadata (issue #51 decision 9). Phone-first, v0 design
// language.

import { useParams } from '@solidjs/router';
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createResource,
  createSignal,
  onCleanup,
  onMount,
} from 'solid-js';
import {
  fetchRunCommands,
  getRepo,
  getRun,
  listProviders,
  type ChatMessage,
  type Dialog,
  type Provider,
  type RunCommand,
} from '../api';
import EmptyState from '../components/EmptyState';
import Banner from '../components/Banner';
import RequireAuth from '../components/RequireAuth';
import ToolPanel, { type PanelTarget } from '../components/ToolPanel';
import { isFileView } from '../components/ToolViews';
import {
  anchoredScrollTop,
  isNearBottom,
  pillState,
  quickReturnReduce,
  type QuickReturnState,
} from '../lib/chatStream';
import { remoteGated } from '../lib/deepLink';
import { resourceValue } from '../lib/resource';
import { groupMessages, reconcileRenderItems, type RenderItem } from '../lib/toolGroups';
import { useEvents } from '../events';
import { ChatHeader } from './runchat/ChatHeader';
import { Composer } from './runchat/Composer';
import { DialogCard } from './runchat/Dialogs';
import { MessageView, ToolGroupView } from './runchat/Messages';
import { createMessageFeed } from './runchat/messageFeed';
import { capitalize } from './runchat/shared';

// Mirrors the app shell's rail breakpoint (AppShell.tsx DESKTOP_MIN_PX, not
// exported): the tool panel flips sheet → sidebar at exactly the width the
// persistent rail appears, so the page never mixes phone and desktop chrome.
const DESKTOP_MIN_PX = 1024;

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

  // The run's repo, fetched once the run is loaded, for the header's repo links
  // (issue #132): the git-icon's hosted forge URL. Gated on a loaded run with a
  // non-empty repo_id (an undefined source skips the fetch). A failed fetch is
  // read through resourceValue below, never onto the error banner — it just
  // means no forge icon.
  const [repo] = createResource(
    () => {
      const r = resourceValue(run);
      return r !== undefined && r.repo_id !== '' ? r.repo_id : undefined;
    },
    (id) => getRepo(id),
  );

  // The run's slash-command catalog (issue #51 decision 5): chat-safe commands
  // feeding the composer autocomplete. Worktree-dependent, hence per-run; the
  // server caches ~30s. A failing endpoint degrades to an empty catalog (no
  // autocomplete) rather than breaking the chat.
  const [commands] = createResource(
    () => params.id,
    (id) => fetchRunCommands(id).catch(() => [] as RunCommand[]),
  );

  // A reply's informational notice (issue #149) — e.g. "already up to date
  // with origin/main". Never an error: its own dismissible banner, separate
  // signal, neutral styling (Banner's `notice` variant).
  const [notice, setNotice] = createSignal<string | null>(null);
  // What the tool detail panel is showing (issue #145, superseding the inline
  // <details> expansion): a group (keyed by its first tool's seq — decision 12)
  // or a lone chip (its seq). Identity by immutable seq, NOT by object: groups
  // and messages are re-derived/replaced wholesale on every SSE refetch, and
  // only seq survives the recompute. The panel never opens or retargets itself
  // — every write goes through a chip/group tap or the close affordances.
  const [panelSel, setPanelSel] = createSignal<
    null | { kind: 'group'; key: number } | { kind: 'tool'; seq: number }
  >(null);

  // Desktop inline expansion state (issue #154), the controlled successor to the
  // pre-#145 uncontrolled <details>: a group is a derived structure recomputed
  // on every refetch and a lone chip's message is replaced wholesale, so native
  // <details> state would slam an expanded run shut on the next SSE tick. seq is
  // the immutable cursor that survives the recompute — groups key by their first
  // tool's seq (decision 12), lone chips by their own. Both are stream state,
  // reset alongside panelSel below. Mobile never writes them (its click opens
  // the sheet); the group/chip bodies only render when desktopPanel() is true.
  const [openGroups, setOpenGroups] = createSignal<Set<number>>(new Set());
  const [openTools, setOpenTools] = createSignal<Set<number>>(new Set());
  const toggleInSet = (set: Set<number>, key: number) => {
    const next = new Set(set);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    return next;
  };
  const groupOpen = (key: number) => openGroups().has(key);
  const toggleGroup = (key: number) => setOpenGroups((prev) => toggleInSet(prev, key));
  const toolExpanded = (seq: number) => openTools().has(seq);
  const toggleTool = (seq: number) => setOpenTools((prev) => toggleInSet(prev, seq));

  // >=1024px renders the panel as the in-flow sidebar instead of the sheet — a
  // pure render switch on shared state, so crossing the breakpoint while open
  // keeps the panel open. Test stubs may hand back a bare MediaQueryList with
  // no addEventListener, hence the optional calls.
  const [desktopPanel, setDesktopPanel] = createSignal(false);
  const desktopQuery =
    typeof window !== 'undefined'
      ? window.matchMedia?.(`(min-width: ${DESKTOP_MIN_PX}px)`)
      : undefined;
  if (desktopQuery !== undefined) {
    setDesktopPanel(desktopQuery.matches === true);
    const onBreakpoint = () => setDesktopPanel(desktopQuery.matches === true);
    desktopQuery.addEventListener?.('change', onBreakpoint);
    onCleanup(() => desktopQuery.removeEventListener?.('change', onBreakpoint));
  }

  // The stream is the scroll container (.chat-page bounds the viewport).
  let streamEl: HTMLDivElement | undefined;

  // --- Single scroll controller (issue #35 §2 + §4) --------------------------
  // One passive listener on .chat-stream drives BOTH the quick-return header
  // (§2, mobile) and the jump-to-latest pill (§4). Reactive outputs:
  const [headerVisible, setHeaderVisible] = createSignal(true); // §2 overlay shown?
  const [nearBottom, setNearBottom] = createSignal(true); // §4 at/near the end?
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

  // The message feed (ADR-0016 / ADR-0047, issue #175): the accumulated
  // window, the FULL/LIGHT refetch protocol and the SSE wiring live in the
  // factory; the route injects run identity, the run refetch and the scroll
  // behavior it owns.
  const {
    messages,
    state,
    transcript,
    pendingDialogField,
    contextUsage,
    hasMore,
    error,
    setError,
    refetchMessages,
    loadEarlier,
  } = createMessageFeed({
    runId: () => params.id,
    events,
    refetchRun: () => void refetchRun(),
    currentRepoID: () => resourceValue(run)?.repo_id,
    isAtBottom: () => streamEl !== undefined && isNearBottom(streamEl),
    scrollToBottom,
    scrollToPendingCard,
    captureAnchor: () =>
      streamEl === undefined
        ? undefined
        : { scrollTop: streamEl.scrollTop, scrollHeight: streamEl.scrollHeight },
    restoreAnchor: (before) => {
      if (streamEl === undefined) return;
      // The anchor write adds the prepended height to scrollTop — a large
      // hide-direction delta that would otherwise slam the header shut (§2).
      beginProgrammaticScroll();
      streamEl.scrollTop = anchoredScrollTop(before, streamEl.scrollHeight);
    },
    onStreamReset: () => {
      // Panel selection AND the desktop inline expansions are stream state (they
      // name a seq): route navigation and transcript rotation both come through
      // here, so they reset for free.
      setPanelSel(null);
      setOpenGroups(new Set<number>());
      setOpenTools(new Set<number>());
    },
  });
  // Append-only stream ⇒ "scrolled up" already means "newer content below" (§4).
  const pill = () => pillState(nearBottom(), state() === 'needs_input');

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
  //
  // A remote-off run of a remote-capable provider has NO web session (issue
  // #163), so it takes the generic wording too: the very same gate that hides
  // the Open affordance must mute this prose, or the hint names a web session
  // that was never registered. These degraded branches are exactly where a
  // non-remote run LANDS — without remote control the agent flushes its pending
  // tool_use, so the dialog arrives by transcript scan rather than the spool.
  const openHint = () => {
    const r = runData();
    const p = runProvider();
    if (r !== undefined && remoteGated(r, p?.supports_remote)) return 'open the session';
    const fb = p?.fallback_open;
    if (fb === undefined || fb.url === '') return 'open the session';
    try {
      return `open it at ${new URL(fb.url).host}`;
    } catch {
      return 'open the session';
    }
  };

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
  // Group consecutive tool runs at render time (decision 7). Thinking is
  // permanently dropped at paint (issue #68) — never shown, transcripts on
  // disk keep everything — while grouping still runs on the FULL list
  // (including thinking) so tool-run boundaries and seq keys stay stable.
  // Memoized WITH reconciliation (issue #175): groupMessages builds fresh
  // wrapper objects per call, which would tear down every rendered item even
  // when the underlying messages kept identity — and panelTarget below reads
  // renderItems repeatedly, which used to re-group on every read. The memo
  // reuses the previous run's items (message wrappers by reference, groups by
  // key + member identity) so the reference-keyed <For> leaves settled DOM
  // alone.
  const renderItems = createMemo<RenderItem[]>((prev = []) =>
    reconcileRenderItems(prev, groupMessages(messages())),
  );
  const hiddenThinking = (m: ChatMessage) => m.kind === 'text' && m.thinking === true;

  // The panel's live target (issue #145), re-resolved from the CURRENT stream
  // on every read: the panel must never hold captured message objects — a
  // refetch replaces them wholesale, and a captured tool would freeze its
  // output while the real one keeps growing. The `key` stays stable across
  // refetches (it derives from panelSel's seq) so the panel's internal page
  // state survives SSE ticks and only resets on a genuine retarget.
  const panelTarget = (): PanelTarget | null => {
    const sel = panelSel();
    if (sel === null) return null;
    if (sel.kind === 'group') {
      const item = renderItems().find((i) => i.kind === 'toolGroup' && i.key === sel.key);
      if (item === undefined || item.kind !== 'toolGroup') return null;
      return {
        key: `group:${sel.key}`,
        entry: 'list',
        // Thinking folds into the run for grouping but never renders (issue
        // #68) — the panel gets tools only.
        tools: item.items.filter((m) => m.kind === 'tool'),
      };
    }
    // A lone chip resolves by seq from messages(), NOT from the render items:
    // a chip that later coalesces into a group must keep its open detail
    // working across the recompute.
    const m = messages().find((x) => x.seq === sel.seq && x.kind === 'tool');
    if (m === undefined) return null;
    return { key: `tool:${sel.seq}`, entry: 'detail', tools: [m] };
  };
  // A dissolved target — e.g. load-earlier prepends tools and renumbers a
  // group's first seq — must not leave a ghost selection behind a closed
  // panel, or the "same" group could silently reopen later under a new shape.
  createEffect(() => {
    if (panelSel() !== null && panelTarget() === null) setPanelSel(null);
  });
  // Desktop sidebar shows FILE details only (issue #154 §2): never a list, never
  // a command. A group selection, or a non-file tool selection, is reachable by
  // opening the sheet on mobile and then crossing the breakpoint — clear it so
  // the sidebar can't be asked to render something it doesn't show.
  createEffect(() => {
    if (!desktopPanel()) return;
    const sel = panelSel();
    if (sel === null) return;
    if (sel.kind === 'group') {
      setPanelSel(null);
      return;
    }
    const m = messages().find((x) => x.seq === sel.seq && x.kind === 'tool');
    if (m === undefined || !isFileView(m)) setPanelSel(null);
  });
  const groupSelected = (key: number) => {
    const sel = panelSel();
    return sel !== null && sel.kind === 'group' && sel.key === key;
  };
  const toolSelected = (m: ChatMessage) => {
    const sel = panelSel();
    return sel !== null && sel.kind === 'tool' && m.kind === 'tool' && sel.seq === m.seq;
  };

  return (
    // .chat-page is a flex ROW (issue #145): the chat column plus, on desktop,
    // the in-flow tool panel sidebar. panel-open widens the page cap so the
    // column keeps its 760px where the viewport allows; closed, the layout is
    // exactly the pre-panel single column.
    <main class="page chat-page" classList={{ 'panel-open': panelTarget() !== null }} ref={pageEl}>
      <div class="chat-column">
        {/* The banners sit above the body so the mobile overlay header (§2), which
          is absolutely positioned within .chat-body, can never paint over them. */}
        <Banner message={error()} onDismiss={() => setError(null)} />
        <Banner message={notice()} onDismiss={() => setNotice(null)} variant="notice" />

        {/* .chat-body is the positioning context for the quick-return header (§2):
          on mobile the header overlays the stream's top and slides via translateY
          while the stream reserves the header's band with padding-top, so hiding
          the header reclaims reading space with no reflow. */}
        <div class="chat-body">
          <ChatHeader
            run={runData()}
            repo={resourceValue(repo)}
            providers={providers()}
            state={state()}
            contextUsage={contextUsage()}
            onError={setError}
            onChanged={() => void refetchRun()}
            onInterrupted={() => void refetchMessages()}
            hidden={!headerVisible()}
            headerRef={(el) => (headerEl = el)}
          />

          <div class="chat-stream" role="log" aria-live="polite" ref={streamEl}>
            <Switch>
              {/* An active run whose adapter-composed state is idle with no
                messages and an unlocated transcript (codex writes its transcript
                only at first-turn start — issue #96): trust the adapter state
                (ADR-0038) over the transcript-derived placeholder and invite the
                first message, which starts the conversation. */}
              <Match
                when={
                  !ended() &&
                  transcript() === 'locating' &&
                  state() === 'idle' &&
                  messages().length === 0
                }
              >
                <EmptyState>Ready — your first message starts the conversation.</EmptyState>
              </Match>
              <Match when={transcript() === 'locating' && messages().length === 0}>
                <EmptyState>Waiting for the transcript…</EmptyState>
              </Match>
              <Match when={transcript() === 'gone'}>
                <EmptyState>Transcript no longer available.</EmptyState>
              </Match>
              <Match when={messages().length === 0}>
                <EmptyState>No messages yet.</EmptyState>
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
                            desktop={desktopPanel()}
                            selected={groupSelected(group().key)}
                            onOpen={() => setPanelSel({ kind: 'group', key: group().key })}
                            open={groupOpen(group().key)}
                            onToggle={() => toggleGroup(group().key)}
                            toolExpanded={toolExpanded}
                            onToggleTool={toggleTool}
                            toolSelected={toolSelected}
                            onOpenTool={(seq) => setPanelSel({ kind: 'tool', seq })}
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
                                <MessageView
                                  message={msg().message}
                                  desktop={desktopPanel()}
                                  toolSelected={toolSelected(msg().message)}
                                  onOpenTool={() =>
                                    setPanelSel({ kind: 'tool', seq: msg().message.seq })
                                  }
                                  toolExpanded={toolExpanded(msg().message.seq)}
                                  onToggleTool={() => toggleTool(msg().message.seq)}
                                />
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
          </div>
        </div>

        <Composer
          runID={params.id}
          state={state()}
          ended={ended()}
          transcript={transcript()}
          dialog={pendingDialog()}
          commands={commands() ?? []}
          agentName={agentName()}
          openHint={openHint()}
          jumpVisible={pill().visible}
          jumpEmphasis={pill().emphasized}
          onJump={jumpToLatest}
          onError={setError}
          onNotice={setNotice}
          onSent={() => void refetchMessages()}
        />
      </div>
      {/* The tool detail panel (issue #145): a pure view over panelTarget —
          resolved live above — mounted beside the column (the desktop sidebar
          is in-flow; the phone sheet is position:fixed and escapes the row). */}
      <ToolPanel
        target={panelTarget()}
        desktop={desktopPanel()}
        onClose={() => setPanelSel(null)}
      />
    </main>
  );
}
