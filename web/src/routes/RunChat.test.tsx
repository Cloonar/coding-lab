// RunChat behavioral contract (issue #7):
// - a run.messages.changed for THIS run coalesces on a 300ms trailing debounce
//   and then refetches LIGHT (issue #175): tail pages only, from
//   after=min(cursor, backpatchSeq-1), no latest-window request; other runs are
//   ignored; run.changed for other repos — or carrying a SIBLING runID — is
//   ignored too; a FULL refetch absorbs a queued backpatchSeq, and a tick that
//   leaves `working` escalates to FULL — the turn-settle self-heal (ADR-0047);
// - a refetch tails with after=<cursor> (paginating past the window limit) so
//   appends accumulate gap-free, and a stale in-flight response never applies
//   over a newer one (request-token guard); unchanged content (equal
//   content_hash) keeps message/DOM identity across refetches;
// - an ended run is read-only (no composer, no reply POST); a gone transcript
//   on a live run gets transcript-specific copy;
// - "Load earlier" never resurrects once paging-up hit the beginning.

import { describe, expect, it, vi } from 'vitest';
import type { ChatMessage, MessagesResponse } from '../api';
import {
  FakeEventSource,
  MESSAGES_DEBOUNCE_MS,
  RUN_ID,
  baseRun,
  buttonByText,
  container,
  emitMessagesChanged,
  emitMessagesChangedSettled,
  h,
  hashed,
  installChatHooks,
  jsonResponse,
  mountChat,
  settle,
  withAssistantText,
} from './runchat/harness';

installChatHooks();

describe('RunChat', () => {
  it('refetches only for this run on run.messages.changed (past the debounce)', async () => {
    await mountChat();
    const before = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length;

    // Another run's tick never refetches — not even after the debounce window
    // (the filter runs before any timer is scheduled).
    await emitMessagesChangedSettled('run_other');
    expect((globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length).toBe(before);

    await emitMessagesChangedSettled();
    expect((globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(
      before,
    );
  });

  function messagesUrls(): string[] {
    return (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls
      .map((c) => String(c[0]))
      .filter((u) => u.includes(`/runs/${RUN_ID}/messages`));
  }

  it('coalesces a burst into ONE tail request and never fetches a latest window (issue #175)', async () => {
    await mountChat(); // seq 1..4 accumulated, cursor 4
    const before = messagesUrls().length;

    // The server gains an append mid-burst.
    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: [
        ...h.messagesOnServer.messages,
        hashed({ seq: 5, kind: 'text', role: 'assistant', text: 'streamed line 5' }),
      ],
      cursor: 5,
    };

    // Three ticks inside one trailing window, then the flush.
    vi.useFakeTimers();
    emitMessagesChanged();
    await vi.advanceTimersByTimeAsync(100);
    emitMessagesChanged();
    await vi.advanceTimersByTimeAsync(100);
    emitMessagesChanged();
    await vi.advanceTimersByTimeAsync(MESSAGES_DEBOUNCE_MS + 100);
    vi.useRealTimers();
    await settle();

    const urls = messagesUrls().slice(before);
    expect(urls).toHaveLength(1); // ONE request for the whole burst
    expect(urls[0]).toContain('after=4&'); // the append cursor
    // No request without a cursor param (no latest window) after the initial load.
    expect(urls.every((u) => u.includes('after='))).toBe(true);
    expect(container.textContent).toContain('streamed line 5');
  });

  it('reaches a back-patched mutation via after=backpatchSeq-1 and renders the flip (issue #175)', async () => {
    await mountChat(); // hashed fixture: the seq-3 tool is status ok
    expect(container.querySelector('.tool-summary.tool-ok')).not.toBeNull();
    const before = messagesUrls().length;

    // The seq-3 tool flips ok → error on the server: its content_hash
    // re-derives and the tailer announces backpatchSeq 3.
    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: h.messagesOnServer.messages.map((m) =>
        m.seq === 3 ? hashed({ ...m, tool: { ...m.tool!, status: 'error' as const } }) : m,
      ),
    };
    await emitMessagesChangedSettled(RUN_ID, { backpatchSeq: 3 });

    const urls = messagesUrls().slice(before);
    expect(urls).toHaveLength(1); // still a single fetch
    expect(urls[0]).toContain('after=2&'); // min(cursor 4, backpatchSeq - 1)
    expect(container.querySelector('.tool-summary.tool-error')).not.toBeNull();
    expect(container.querySelector('.tool-summary.tool-ok')).toBeNull();
  });

  it('keeps the rendered DOM node when a light refetch redelivers unchanged content (issue #175)', async () => {
    await mountChat(); // hashed fixture
    const node = Array.from(container.querySelectorAll('.chat-msg')).find((el) =>
      el.textContent?.includes('all done'),
    );
    expect(node).toBeDefined();

    // backpatchSeq 2 redelivers seq 2..4 as FRESH parses with UNCHANGED
    // hashes: the merge keeps every prev object — and the array identity —
    // so no signal fires and the settled DOM survives by reference.
    await emitMessagesChangedSettled(RUN_ID, { backpatchSeq: 2 });

    expect(node!.isConnected).toBe(true); // never torn down
    expect(
      Array.from(container.querySelectorAll('.chat-msg')).find((el) =>
        el.textContent?.includes('all done'),
      ),
    ).toBe(node); // the SAME node object, not an equal re-render
  });

  it('a FULL refetch inside the debounce window absorbs the queued backpatchSeq (ADR-0047)', async () => {
    // 70 hashed messages with the ONLY tool at seq 2 — after paging up, the
    // accumulated stream reaches below what any latest window (newest 60) can
    // re-deliver, so a discarded announcement would leave seq 2 stale forever.
    h.messagesOnServer = {
      messages: [
        hashed({ seq: 1, kind: 'text', role: 'user', text: 'kick off' }),
        hashed({
          seq: 2,
          kind: 'tool',
          tool: { name: 'Bash', title: 'long task', status: 'running' as const },
        }),
        ...Array.from({ length: 68 }, (_, i) =>
          hashed({
            seq: i + 3,
            kind: 'text' as const,
            role: 'assistant' as const,
            text: `progress ${i + 3}`,
          }),
        ),
      ],
      state: 'needs_input',
      cursor: 70,
      has_more: true,
      transcript: 'available',
    };
    await mountChat(); // latest window seq 11..70
    buttonByText('Load earlier')!.click();
    await settle(); // accumulated 1..70; the seq-2 tool renders as running
    expect(container.querySelector('.tool-summary.tool-ok')).toBeNull();
    const before = messagesUrls().length;

    // The seq-2 tool completes; the tailer announces backpatchSeq 2 — but a
    // repo-scoped run.changed lands INSIDE the 300ms window and forces a FULL
    // refetch. The FULL must absorb the queued seq into its tail start:
    // backpatchSeq is a delta the server never re-sends, and seq 2 is beyond
    // the latest window's reach.
    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: h.messagesOnServer.messages.map((m) =>
        m.seq === 2
          ? hashed({ ...m, tool: { ...m.tool!, status: 'ok' as const, output: 'PASS' } })
          : m,
      ),
    };
    vi.useFakeTimers();
    emitMessagesChanged(RUN_ID, { backpatchSeq: 2 });
    await vi.advanceTimersByTimeAsync(100); // still inside the debounce window
    FakeEventSource.instances[0]?.emit('run.changed', { type: 'run.changed', repoID: 'repo_1' });
    await vi.advanceTimersByTimeAsync(MESSAGES_DEBOUNCE_MS + 100);
    vi.useRealTimers();
    await settle();

    // Tail from min(cursor 70, backpatchSeq − 1) = 1, paged past the window
    // limit, plus the latest window — and NO third fetch (the queued light
    // flush was superseded, its seq absorbed rather than re-scheduled).
    const urls = messagesUrls().slice(before);
    expect(urls).toHaveLength(3);
    expect(urls[0]).toContain('after=1&');
    expect(urls[1]).toContain('after=61&');
    expect(urls[2]).not.toContain('after=');
    expect(container.querySelector('.tool-summary.tool-ok')).not.toBeNull();
  });

  it('escalates to a FULL refetch when the envelope state leaves working (ADR-0047)', async () => {
    await mountChat(); // hashed fixture: the seq-3 tool is status ok
    // Mid-turn tick: the light path, no latest-window request.
    await emitMessagesChangedSettled(RUN_ID, { state: 'working' });
    let urls = messagesUrls();
    expect(urls[urls.length - 1]).toContain('after=4&');
    const before = urls.length;

    // The seq-3 flip's announcement is LOST — the bus drops events for slow
    // subscribers, and a dropped backpatchSeq is never re-sent. The server
    // mutated; no tick names the seq.
    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: h.messagesOnServer.messages.map((m) =>
        m.seq === 3 ? hashed({ ...m, tool: { ...m.tool!, status: 'error' as const } }) : m,
      ),
    };

    // The settle tick (working → needs_input) must escalate to FULL — the
    // latest-window re-read is the turn-cadence self-heal for whatever the
    // lost announcements failed to name.
    await emitMessagesChangedSettled(RUN_ID, { state: 'needs_input' });

    urls = messagesUrls().slice(before);
    expect(urls.some((u) => !u.includes('after='))).toBe(true);
    expect(container.querySelector('.tool-summary.tool-error')).not.toBeNull();
    expect(container.querySelector('.tool-summary.tool-ok')).toBeNull();
  });

  it('ignores run.changed carrying a SIBLING runID; a repo-scoped one still refetches (issue #175)', async () => {
    await mountChat();
    const before = messagesUrls().length;

    // Same repo, different run: filtered before the repo check.
    FakeEventSource.instances[0]?.emit('run.changed', {
      type: 'run.changed',
      repoID: 'repo_1',
      runID: 'run_other',
    });
    await settle();
    expect(messagesUrls().length).toBe(before);

    // No runID = genuinely repo-scoped (stop-all, AFK reaper, CR merge):
    // the conservative full refetch stays.
    FakeEventSource.instances[0]?.emit('run.changed', { type: 'run.changed', repoID: 'repo_1' });
    await settle();
    expect(messagesUrls().length).toBeGreaterThan(before);
  });

  it('makes zero requests while the chat idles with no events (issue #175)', async () => {
    await mountChat();
    const total = () => (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length;
    const before = total();

    vi.useFakeTimers();
    await vi.advanceTimersByTimeAsync(5_000);
    vi.useRealTimers();
    await settle();

    expect(total()).toBe(before); // no polling, no stray debounce flush
  });

  it('resets the stream and unlocks the composer when the transcript rotates', async () => {
    // Pre-clear: two accumulated messages (seq 1–2) and a pending dialog that
    // locks the composer.
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'text', role: 'assistant', text: 'pre-clear reasoning' },
        { seq: 2, kind: 'text', role: 'assistant', text: 'stale tail line' },
      ],
      state: 'question',
      cursor: 2,
      has_more: false,
      transcript: 'available',
      transcript_id: 'sess-A',
      pending_dialog: {
        tool_id: 'toolu_pre',
        dialog_kind: 'question',
        prompt: 'Which fix?',
        answerable: true,
        options: [{ label: 'Revert' }, { label: 'Other', is_other: true }],
      },
    };
    await mountChat();
    expect(container.textContent).toContain('stale tail line');
    expect(container.querySelector('.chat-composer-row')).toBeNull(); // locked by the dialog
    expect(container.textContent).toContain('Which fix?');

    // /clear rotates: a brand-new transcript restarts seq at 1 with fresh
    // content, a new identity, and no dialog. Its lone seq-1 message would
    // otherwise leave the stale seq-2 tail behind in the seq-keyed merge.
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'user', text: 'fresh start' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      transcript_id: 'sess-B',
      pending_dialog: null,
    };
    await emitMessagesChangedSettled();
    await settle();

    // The whole pre-clear stream is dropped (no stale seq-2 tail, no stale
    // dialog), only the fresh conversation shows, and the composer is usable.
    expect(container.textContent).toContain('fresh start');
    expect(container.textContent).not.toContain('stale tail line');
    expect(container.textContent).not.toContain('pre-clear reasoning');
    expect(container.textContent).not.toContain('Which fix?');
    expect(container.querySelector('.chat-composer-row')).not.toBeNull();
    expect(container.querySelector('.chat-input')).not.toBeNull();
  });

  it('does not leak a stale high-seq tail when a rotation straddles a refetch', async () => {
    // The hard case: a rotation lands BETWEEN two pages of one refetch's tail
    // loop (issue #175 made the SSE path tail-only, so the straddle now sits
    // between paging GETs). Accumulated stream on transcript A is [seq1,
    // seq2]. The light refetch's after=2 page still sees A (a FULL window,
    // seq 3..62, so the loop pages on); then /clear rotates and the NEXT page
    // (and every later GET) sees the fresh transcript B (seq1). Applying the
    // final page's transcript_id resets the stream and fires a superseding
    // full refetch mid-function; the stale outer refetch must not merge its
    // seq 3..62 A-lines back in — seqs that B (max seq 1) never overwrites
    // would otherwise persist forever (issue #34).
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'text', role: 'user', text: 'A-one' },
        { seq: 2, kind: 'text', role: 'assistant', text: 'A-two' },
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
      transcript_id: 'sess-A',
      pending_dialog: null,
    };
    await mountChat();
    expect(container.textContent).toContain('A-two');

    const aTail: MessagesResponse = {
      // A full window (60 = the page limit), so the tail loop pages again.
      messages: Array.from({ length: 60 }, (_, i) => ({
        seq: 3 + i,
        kind: 'text' as const,
        role: 'assistant' as const,
        text: `A-stale-${3 + i}`,
      })),
      state: 'working',
      cursor: 62,
      has_more: true,
      transcript: 'available',
      transcript_id: 'sess-A',
      pending_dialog: null,
    };
    const bWindow: MessagesResponse = {
      messages: [{ seq: 1, kind: 'text', role: 'user', text: 'B-fresh-one' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      transcript_id: 'sess-B',
      pending_dialog: null,
    };
    // The after=2 page sees A; the next page (after=62) and all later GETs
    // see rotated B — forcing the rotation to land inside a single refetch.
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown, init?: RequestInit) => {
        const url = String(input);
        const method = init?.method ?? 'GET';
        if (url === '/api/v1/auth/state')
          return Promise.resolve(
            jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
          );
        if (url === '/api/v1/providers')
          return Promise.resolve(jsonResponse(200, { providers: h.providersOnServer }));
        if (url === `/api/v1/runs/${RUN_ID}` && method === 'GET')
          return Promise.resolve(jsonResponse(200, { ...h.runOnServer }));
        if (url === `/api/v1/repos/${h.runOnServer.repo_id}` && method === 'GET')
          return Promise.resolve(jsonResponse(200, { ...h.repoOnServer }));
        if (url.startsWith(`/api/v1/runs/${RUN_ID}/messages`)) {
          const after = Number(new URL(url, 'http://lab').searchParams.get('after') ?? '0');
          return Promise.resolve(jsonResponse(200, after === 2 ? aTail : bWindow));
        }
        // AppShell mounts the side rail once authenticated; it fetches the
        // instance list for the ACTIVE rail + attention badge.
        if (url === '/api/v1/instances' && method === 'GET') {
          return Promise.resolve(jsonResponse(200, { instances: [] }));
        }
        return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
      }),
    );

    await emitMessagesChangedSettled();
    await settle();

    // Only the fresh transcript B shows; every stale A tail line is gone.
    expect(container.textContent).toContain('B-fresh-one');
    expect(container.textContent).not.toContain('A-stale-');
    expect(container.textContent).not.toContain('A-two');
  });

  it('does not reset the stream on the first transcript locate (locating -> available)', async () => {
    // The server always sends transcript_id (no omitempty): "" while locating.
    // The locating -> available transition ("" -> hash) is NOT a rotation and
    // must not trigger a spurious reset + refetch.
    h.messagesOnServer = {
      messages: [],
      state: 'idle',
      cursor: 0,
      has_more: false,
      transcript: 'locating',
      transcript_id: '',
    };
    await mountChat();

    const messagesGets = () =>
      (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.filter((c) =>
        String(c[0]).includes('/messages'),
      ).length;
    const before = messagesGets();

    // The transcript is located: a real id and the first messages arrive.
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'located content' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      transcript_id: 'sess-A',
    };
    await emitMessagesChangedSettled();

    expect(container.textContent).toContain('located content');
    // Exactly one refetch cycle (the SSE-driven one); a spurious rotation reset
    // would fire a second refetch (cursor 0 → one more /messages GET).
    expect(messagesGets() - before).toBe(1);
  });

  it('ignores run.changed for other repos', async () => {
    await mountChat();
    const fetchMock = globalThis.fetch as ReturnType<typeof vi.fn>;
    // Count only this run's own fetches: the app shell's rail refetches the
    // instance list on every run.changed (any repo), which is orthogonal to
    // RunChat's repo-scoped refetch under test here.
    const runFetches = () =>
      fetchMock.mock.calls.filter((call) => String(call[0]).includes(`/runs/${RUN_ID}`)).length;
    const before = runFetches();

    FakeEventSource.instances[0]?.emit('run.changed', {
      type: 'run.changed',
      repoID: 'repo_other',
    });
    await settle();
    expect(runFetches()).toBe(before);

    FakeEventSource.instances[0]?.emit('run.changed', { type: 'run.changed', repoID: 'repo_1' });
    await settle();
    expect(runFetches()).toBeGreaterThan(before);
  });

  it('tails a refetch with after=<cursor>, paginating past the window limit gap-free', async () => {
    await mountChat(); // seq 1..4 accumulated, cursor 4

    // 61 appended messages force the after= loop to page twice.
    const appended: ChatMessage[] = Array.from({ length: 61 }, (_, i) => ({
      seq: 5 + i,
      kind: 'text',
      role: 'assistant',
      text: `tail message ${5 + i}`,
    }));
    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: [...h.messagesOnServer.messages, ...appended],
      cursor: 65,
    };

    await emitMessagesChangedSettled();

    const urls = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.map((c) => String(c[0]));
    expect(urls.some((u) => u.includes('after=4&'))).toBe(true);
    expect(urls.some((u) => u.includes('after=64&'))).toBe(true);
    // Gap-free accumulation: the old head, the mid-tail beyond one window,
    // and the tip are all present.
    expect(container.textContent).toContain('do the thing');
    expect(container.textContent).toContain('tail message 34');
    expect(container.textContent).toContain('tail message 65');
  });

  it('drops a stale in-flight refetch once a newer one applied (request-token guard)', async () => {
    await mountChat(); // seq 1..4 accumulated

    // Hold every messages GET so resolution order is ours to pick.
    const held: ((body: MessagesResponse) => void)[] = [];
    const baseFetch = globalThis.fetch;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown, init?: RequestInit) => {
        const url = String(input);
        if (
          url.startsWith(`/api/v1/runs/${RUN_ID}/messages`) &&
          (init?.method ?? 'GET') === 'GET'
        ) {
          return new Promise((resolve) => {
            held.push((body) => resolve(jsonResponse(200, body)));
          });
        }
        return (baseFetch as typeof fetch)(input as RequestInfo, init);
      }),
    );

    // Two debounce windows, so each emit flushes into its OWN light refetch
    // (a single window would coalesce them — that's the burst test's job).
    await emitMessagesChangedSettled(); // refetch A — its after=4 page is held[0]
    await emitMessagesChangedSettled(); // refetch B — its after=4 page is held[1]
    expect(held).toHaveLength(2);

    // B finishes first: its tail page carries the fresh seq-5 content, and the
    // light path applies the FINAL tail response's envelope (issue #175 — no
    // latest-window request follows).
    held[1]!({
      messages: [{ seq: 5, kind: 'text', role: 'assistant', text: 'fresh tail' }],
      state: 'needs_input',
      cursor: 5,
      has_more: false,
      transcript: 'available',
    });
    await settle();
    expect(container.textContent).toContain('fresh tail');

    // A resolves last with stale data — it must be dropped, not applied.
    held[0]!({
      messages: [{ seq: 5, kind: 'text', role: 'assistant', text: 'stale tail' }],
      state: 'working',
      cursor: 5,
      has_more: false,
      transcript: 'available',
    });
    await settle();
    expect(container.textContent).not.toContain('stale tail');
    expect(container.textContent).toContain('fresh tail');
    // State stayed needs_input (the stale 'working' tail was dropped): the
    // needs-input status line is still in the stream, not reverted away.
    expect(container.querySelector('.chat-needs-input')).not.toBeNull();
    // One tail GET per refetch and nothing more: the light path never fetched
    // a latest window, and stale A stopped at its token check.
    expect(held).toHaveLength(2);
  });

  it('shows transcript-specific copy when the transcript is gone on a live run', async () => {
    h.messagesOnServer = {
      messages: [],
      state: 'ended',
      cursor: 0,
      has_more: false,
      transcript: 'gone',
    };
    await mountChat(); // h.runOnServer stays active

    const note = container.querySelector('.chat-composer-note');
    expect(note?.textContent).toContain('Transcript no longer available');
    expect(note?.textContent).not.toContain('This instance has ended');
    expect(container.querySelector('.chat-input')).toBeNull();
  });

  it('does not resurrect Load earlier after paging up hit the beginning', async () => {
    h.messagesOnServer = {
      messages: Array.from({ length: 70 }, (_, i) => ({
        seq: i + 1,
        kind: 'text' as const,
        role: 'assistant' as const,
        text: `m${i + 1} of history`,
      })),
      state: 'needs_input',
      cursor: 70,
      has_more: true,
      transcript: 'available',
    };
    await mountChat(); // latest window is seq 11..70
    expect(buttonByText('Load earlier')).not.toBeNull();

    buttonByText('Load earlier')!.click();
    await settle();
    expect(container.textContent).toContain('m1 of history');
    expect(buttonByText('Load earlier')).toBeNull(); // the beginning was reached

    // A refetch's latest window says has_more (it talks about ITS window) —
    // the button must stay gone.
    await emitMessagesChangedSettled();
    expect(buttonByText('Load earlier')).toBeNull();
  });

  it('invites the first message for an active idle run whose transcript is not yet located', async () => {
    // codex writes its transcript only at first-turn start (issue #96): an
    // active run composed idle with no messages and an unlocated transcript
    // trusts the adapter state (ADR-0038) over the transcript-derived
    // placeholder and shows ready-to-start copy — the first_message spawn path
    // means there is nothing to auto-send here.
    h.messagesOnServer = {
      messages: [],
      state: 'idle',
      cursor: 0,
      has_more: false,
      transcript: 'locating',
      transcript_id: '',
    };
    await mountChat();

    const empty = container.querySelector('.chat-stream .empty');
    expect(empty?.textContent).toBe('Ready — your first message starts the conversation.');
    expect(container.textContent).not.toContain('Waiting for the transcript');
    // The composer is usable from this state — a first send must work, never
    // wait on transcript availability.
    expect(container.querySelector('.chat-input')).not.toBeNull();
    expect(h.replyPosts).toHaveLength(0);
  });

  it('keeps the transcript-locating placeholder before the first state is composed', async () => {
    // state '' (no poll composed the adapter state yet) with a locating
    // transcript is NOT the ready-to-start case — the transcript-derived
    // placeholder still stands.
    h.messagesOnServer = {
      messages: [],
      state: '',
      cursor: 0,
      has_more: false,
      transcript: 'locating',
      transcript_id: '',
    };
    await mountChat();

    const empty = container.querySelector('.chat-stream .empty');
    expect(empty?.textContent).toBe('Waiting for the transcript…');
    expect(container.textContent).not.toContain('your first message starts');
  });

  it('does not show the ready-to-start copy for an ended run with the same idle chat state', async () => {
    h.runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    h.messagesOnServer = {
      messages: [],
      state: 'idle',
      cursor: 0,
      has_more: false,
      transcript: 'locating',
      transcript_id: '',
    };
    await mountChat();

    // ended() gates the ready-to-start Match off; the locating placeholder shows.
    expect(container.textContent).not.toContain('your first message starts');
    const empty = container.querySelector('.chat-stream .empty');
    expect(empty?.textContent).toBe('Waiting for the transcript…');
  });

  it('drives every agent-naming string from the provider display_name', async () => {
    h.providersOnServer[0]!.display_name = 'Agent Zed';
    withAssistantText('done'); // needs_input
    await mountChat();

    expect(container.querySelector('.chat-needs-input')?.textContent).toBe(
      'Agent Zed is waiting for your reply.',
    );
    expect(container.textContent).not.toContain('Claude');
  });

  it('falls back to "the agent" while provider metadata is missing', async () => {
    h.providersOnServer = []; // no metadata for the run's provider
    withAssistantText('done'); // needs_input
    await mountChat();

    expect(container.querySelector('.chat-needs-input')?.textContent).toBe(
      'The agent is waiting for your reply.',
    );
  });
});
