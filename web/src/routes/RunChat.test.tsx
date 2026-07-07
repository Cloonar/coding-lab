// RunChat behavioral contract (issue #7):
// - the stream renders user/assistant text, tool chips, and lifecycle/errors;
//   thinking is hidden until the toggle is pressed;
// - a run.messages.changed for THIS run refetches; other runs are ignored;
//   run.changed for other repos is ignored too;
// - a refetch tails with after=<cursor> (paginating past the window limit) so
//   appends accumulate gap-free, and a stale in-flight response never applies
//   over a newer one (request-token guard);
// - the composer replies (POST /reply) and clears; Cmd/Ctrl+Enter sends, bare
//   Enter does not; while working the Send button morphs into a one-tap
//   Interrupt (ADR-0022) — no Send, no "queued" copy, textarea stays editable;
// - a pending dialog locks the composer and renders native option buttons that
//   POST /answer with the option index; the panel is gated on state==='question'
//   and its selections reset when the dialog identity (tool_id) changes;
// - an ended run is read-only (no composer, no reply POST); a gone transcript
//   on a live run gets transcript-specific copy;
// - interrupt POSTs /interrupt with one tap (no confirm), from the working
//   composer and from the locked question-state escape hatch;
// - "Load earlier" never resurrects once paging-up hit the beginning.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ChatMessage, MessagesResponse, Provider, Run } from '../api';
import App from '../App';
import RunChat from './RunChat';

const RUN_ID = 'run_1';

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((event: { data: string }) => void)[]>();
  constructor() {
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, listener: (event: { data: string }) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }
  close(): void {}
  emit(type: string, payload: Record<string, unknown>): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data: JSON.stringify(payload) });
    }
  }
}

function baseRun(): Run {
  return {
    id: RUN_ID,
    repo_id: 'repo_1',
    kind: 'manual',
    provider: 'claude-code',
    issue_number: null,
    branch: 'lab/x',
    worktree_path: '/wt/x',
    session_name: 'proj~dom-20260706-1500',
    model: 'opus[1m]',
    effort: 'max',
    deep_link_url: 'https://claude.ai/code/session_1',
    started_at: '2026-07-06T15:00:00.000Z',
    budget_deadline: null,
    ended_at: null,
    outcome: 'active',
    failure_reason: null,
  };
}

let runOnServer: Run;
let messagesOnServer: MessagesResponse;
let providersOnServer: Provider[];
let replyPosts: { text: string }[];
let answerPosts: Record<string, unknown>[];
let interruptPosts: number;
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

/** Mimics the server's windowMessages (chat.go): after= tails, before= pages up. */
function messagesWindow(url: string): MessagesResponse {
  const q = new URL(url, 'http://lab').searchParams;
  const after = Number(q.get('after') ?? '0');
  const before = Number(q.get('before') ?? '0');
  const limit = Number(q.get('limit') ?? '60');
  const all = messagesOnServer.messages;
  let win: ChatMessage[];
  let hasMore: boolean;
  if (after > 0) {
    win = all.filter((m) => m.seq > after).slice(0, limit);
    hasMore = all.some((m) => m.seq <= after);
  } else if (before > 0) {
    const older = all.filter((m) => m.seq < before);
    hasMore = older.length > limit;
    win = older.slice(-limit);
  } else {
    hasMore = all.length > limit;
    win = all.slice(-limit);
  }
  return { ...messagesOnServer, messages: win, has_more: hasMore };
}

function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === '/api/v1/auth/state' && method === 'GET') {
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
        );
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: providersOnServer }));
      }
      if (url === `/api/v1/runs/${RUN_ID}` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...runOnServer }));
      }
      if (url.startsWith(`/api/v1/runs/${RUN_ID}/messages`) && method === 'GET') {
        return Promise.resolve(jsonResponse(200, messagesWindow(url)));
      }
      if (url === `/api/v1/runs/${RUN_ID}/reply` && method === 'POST') {
        replyPosts.push(JSON.parse(String(init?.body)) as { text: string });
        return Promise.resolve(jsonResponse(204, ''));
      }
      if (url === `/api/v1/runs/${RUN_ID}/answer` && method === 'POST') {
        answerPosts.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return Promise.resolve(jsonResponse(204, ''));
      }
      if (url === `/api/v1/runs/${RUN_ID}/interrupt` && method === 'POST') {
        interruptPosts += 1;
        return Promise.resolve(jsonResponse(204, ''));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flush();
}

async function mountChat(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/runs/${RUN_ID}` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/runs/:id" component={RunChat} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function buttonByText(text: string): HTMLButtonElement | null {
  return (
    Array.from(container.querySelectorAll('button')).find((b) => b.textContent?.trim() === text) ??
    null
  );
}

function buttonByLabel(label: string): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
}

// jsdom has no clipboard — install a spy so copy buttons are exercisable.
function stubClipboard(): ReturnType<typeof vi.fn> {
  const writeText = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
  return writeText;
}

/** Set the server's messages to a single assistant text message. */
function withAssistantText(text: string): void {
  messagesOnServer = {
    messages: [{ seq: 1, kind: 'text', role: 'assistant', text }],
    state: 'needs_input',
    cursor: 1,
    has_more: false,
    transcript: 'available',
  };
}

function emitMessagesChanged(runID: string = RUN_ID): void {
  FakeEventSource.instances[0]?.emit('run.messages.changed', {
    type: 'run.messages.changed',
    repoID: 'repo_1',
    runID,
  });
}

beforeEach(() => {
  runOnServer = baseRun();
  providersOnServer = [
    {
      id: 'claude-code',
      models: [],
      efforts: [],
      options: [],
      fallback_open: {
        url: 'https://claude.ai/code',
        title: "Opens the claude.ai session picker — the exact deep link wasn't captured",
      },
    },
  ];
  messagesOnServer = {
    messages: [
      { seq: 1, kind: 'text', role: 'user', text: 'do the thing' },
      { seq: 2, kind: 'text', role: 'assistant', thinking: true, text: 'secret reasoning' },
      {
        seq: 3,
        kind: 'tool',
        tool: { name: 'Bash', title: 'Ran ls', status: 'ok', output: 'a\nb' },
      },
      { seq: 4, kind: 'text', role: 'assistant', text: 'all done' },
    ],
    state: 'needs_input',
    cursor: 4,
    has_more: false,
    transcript: 'available',
  };
  replyPosts = [];
  answerPosts = [];
  interruptPosts = 0;
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
  Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
});

describe('RunChat', () => {
  it('renders text and tool chips, hides thinking until toggled', async () => {
    await mountChat();

    expect(container.textContent).toContain('do the thing');
    expect(container.textContent).toContain('all done');
    expect(container.querySelector('.chat-tool')?.textContent).toContain('Ran ls');
    // Thinking hidden by default.
    expect(container.textContent).not.toContain('secret reasoning');

    buttonByText('Show thinking')!.click();
    await settle();
    expect(container.textContent).toContain('secret reasoning');
  });

  it('replies through the composer and clears the input', async () => {
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'keep going';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    buttonByLabel('Send')!.click();
    await settle();

    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('keep going');
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
  });

  it('sends on Cmd/Ctrl+Enter but never on bare Enter', async () => {
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ship it';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    // Bare Enter must not send — a phone's return key inserts a newline.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(replyPosts).toHaveLength(0);

    // Cmd/Ctrl+Enter sends and clears.
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('ship it');
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
  });

  it('morphs Send into a one-tap Interrupt while the agent is working', async () => {
    messagesOnServer = { ...messagesOnServer, state: 'working' };
    await mountChat();

    // No Send affordance and no "queued" copy anywhere while working.
    expect(buttonByLabel('Send')).toBeNull();
    expect(container.textContent).not.toContain('queued');
    expect(container.querySelector('.chat-composer-hint')?.textContent).toContain(
      'tap to interrupt',
    );

    // A single square Interrupt, carrying the reduced-motion-gated pulse cue
    // (decision 9c); the textarea stays editable (compose-ahead).
    const interrupt = buttonByLabel('Interrupt');
    expect(interrupt).not.toBeNull();
    expect(interrupt!.classList.contains('pulse')).toBe(true);
    expect(interrupt!.classList.contains('chat-interrupt')).toBe(true); // accent-square hook
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    expect(input).not.toBeNull();
    expect(input.disabled).toBe(false);

    // One tap posts /interrupt — no confirm step.
    interrupt!.click();
    await settle();
    expect(interruptPosts).toBe(1);
  });

  it('disables Send while the composer is empty', async () => {
    await mountChat(); // default needs_input, empty box
    const send = buttonByLabel('Send');
    expect(send).not.toBeNull();
    expect(send!.classList.contains('chat-send')).toBe(true); // accent-square hook
    expect(send!.disabled).toBe(true);

    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'x';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByLabel('Send')!.disabled).toBe(false);

    // Whitespace-only is still empty.
    input.value = '   ';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByLabel('Send')!.disabled).toBe(true);
  });

  it('preserves a compose-ahead draft and morphs Send back when the agent idles', async () => {
    messagesOnServer = { ...messagesOnServer, state: 'working' };
    await mountChat();

    // Type a draft while working — no Send, only the square Interrupt.
    expect(buttonByLabel('Send')).toBeNull();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'queued thought';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    // The agent returns to needs_input: Send morphs back, enabled, and the draft
    // survives — sending it posts the retained text.
    messagesOnServer = { ...messagesOnServer, state: 'needs_input' };
    emitMessagesChanged();
    await settle();

    const preserved = container.querySelector('.chat-input') as HTMLTextAreaElement;
    expect(preserved.value).toBe('queued thought');
    const send = buttonByLabel('Send');
    expect(send).not.toBeNull();
    expect(send!.disabled).toBe(false);
    expect(buttonByLabel('Interrupt')).toBeNull(); // the working square is gone

    send!.click();
    await settle();
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('queued thought');
  });

  it('refetches only for this run on run.messages.changed', async () => {
    await mountChat();
    const before = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length;

    FakeEventSource.instances[0]?.emit('run.messages.changed', {
      type: 'run.messages.changed',
      repoID: 'repo_1',
      runID: 'run_other',
    });
    await settle();
    expect((globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length).toBe(before);

    FakeEventSource.instances[0]?.emit('run.messages.changed', {
      type: 'run.messages.changed',
      repoID: 'repo_1',
      runID: RUN_ID,
    });
    await settle();
    expect((globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(
      before,
    );
  });

  it('locks the composer and answers a pending dialog by option index', async () => {
    messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'dialog',
          dialog: {
            tool_id: 'toolu_1',
            dialog_kind: 'question',
            prompt: 'Which fix?',
            answerable: true,
            options: [
              { label: 'Revert' },
              { label: 'Patch forward' },
              { label: 'Other', is_other: true },
            ],
          },
        },
      ],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    // The free-text reply composer is replaced by the dialog panel.
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    expect(container.textContent).toContain('Which fix?');

    buttonByText('Patch forward')!.click();
    await settle();
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toMatchObject({ tool_id: 'toolu_1', index: 1 });
  });

  it('renders and answers a pending dialog from the pending_dialog field (spool)', async () => {
    // The transcript carries no dialog message (Claude Code never flushes a
    // pending tool_use); the dialog arrives only via the top-level field.
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_field',
        dialog_kind: 'question',
        prompt: 'Pick a flavor?',
        answerable: true,
        options: [{ label: 'Option A' }, { label: 'Option B' }, { label: 'Other', is_other: true }],
      },
    };
    await mountChat();

    // Composer is locked; the dialog panel renders the field's prompt + options.
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    expect(container.textContent).toContain('Pick a flavor?');

    buttonByText('Option B')!.click();
    await settle();
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toMatchObject({ tool_id: 'toolu_field', index: 1 });
  });

  it('shows a generic needs-input card when blocked with no structured dialog', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'done' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: null,
    };
    await mountChat();

    // needs_input keeps the composer usable, with a generic "needs input" hint.
    expect(container.querySelector('.chat-input')).not.toBeNull();
    expect(container.textContent).toContain('Claude needs input');
  });

  it('resets the stream and unlocks the composer when the transcript rotates', async () => {
    // Pre-clear: two accumulated messages (seq 1–2) and a pending dialog that
    // locks the composer.
    messagesOnServer = {
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
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'user', text: 'fresh start' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      transcript_id: 'sess-B',
      pending_dialog: null,
    };
    emitMessagesChanged();
    await settle();
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

  it('does not reset the stream on the first transcript locate (locating -> available)', async () => {
    // The server always sends transcript_id (no omitempty): "" while locating.
    // The locating -> available transition ("" -> hash) is NOT a rotation and
    // must not trigger a spurious reset + refetch.
    messagesOnServer = {
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
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'located content' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      transcript_id: 'sess-A',
    };
    emitMessagesChanged();
    await settle();

    expect(container.textContent).toContain('located content');
    // Exactly one refetch cycle (the SSE-driven one); a spurious rotation reset
    // would fire a second refetch (cursor 0 → one more /messages GET).
    expect(messagesGets() - before).toBe(1);
  });

  it('is read-only for an ended run', async () => {
    runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    messagesOnServer = { ...messagesOnServer, state: 'ended' };
    await mountChat();

    expect(container.querySelector('.chat-input')).toBeNull();
    expect(container.querySelector('.chat-composer-note')?.textContent).toContain('read-only');
  });

  it('offers a one-tap Interrupt escape hatch in the locked question state', async () => {
    // state 'question' with no structured dialog: the composer is locked, but a
    // one-tap Interrupt square remains (decision 5) — no confirm step.
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.chat-composer-row')).toBeNull(); // locked
    const interrupt = buttonByLabel('Interrupt');
    expect(interrupt).not.toBeNull();
    // The escape-hatch square is inert — no pulse (decision 5); the pulse is the
    // working-state cue only.
    expect(interrupt!.classList.contains('pulse')).toBe(false);
    interrupt!.click();
    await settle();
    expect(interruptPosts).toBe(1); // one tap, no confirm gate
  });

  it('ignores run.changed for other repos', async () => {
    await mountChat();
    const fetchMock = globalThis.fetch as ReturnType<typeof vi.fn>;
    const before = fetchMock.mock.calls.length;

    FakeEventSource.instances[0]?.emit('run.changed', {
      type: 'run.changed',
      repoID: 'repo_other',
    });
    await settle();
    expect(fetchMock.mock.calls.length).toBe(before);

    FakeEventSource.instances[0]?.emit('run.changed', { type: 'run.changed', repoID: 'repo_1' });
    await settle();
    expect(fetchMock.mock.calls.length).toBeGreaterThan(before);
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
    messagesOnServer = {
      ...messagesOnServer,
      messages: [...messagesOnServer.messages, ...appended],
      cursor: 65,
    };

    emitMessagesChanged();
    await settle();

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

    emitMessagesChanged(); // refetch A — its after= request is held[0]
    await settle();
    emitMessagesChanged(); // refetch B — its after= request is held[1]
    await settle();
    expect(held).toHaveLength(2);

    // B finishes first: empty tail, then a latest window with the fresh state.
    held[1]!({
      messages: [],
      state: 'needs_input',
      cursor: 4,
      has_more: false,
      transcript: 'available',
    });
    await settle();
    expect(held).toHaveLength(3);
    held[2]!({
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
    expect(container.querySelector('.chat-composer-hint')).toBeNull(); // state not reverted
    expect(held).toHaveLength(3); // A never proceeded to its latest-window fetch
  });

  it('keeps the composer unlocked when a dialog message exists but state is not question', async () => {
    messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'dialog',
          dialog: {
            tool_id: 'toolu_1',
            dialog_kind: 'question',
            prompt: 'Which fix?',
            answerable: true,
            options: [{ label: 'Revert' }],
          },
        },
        { seq: 2, kind: 'text', role: 'assistant', text: 'answered elsewhere' },
      ],
      state: 'needs_input', // answered externally — the tailer moved on
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.chat-dialog')).toBeNull();
    expect(container.querySelector('.chat-composer-row')).not.toBeNull();
  });

  it('resets dialog selections when the pending dialog identity changes', async () => {
    const dialogMessage = (seq: number, toolID: string, prompt: string): ChatMessage => ({
      seq,
      kind: 'dialog',
      dialog: {
        tool_id: toolID,
        dialog_kind: 'question',
        prompt,
        answerable: true,
        multi: true,
        options: [{ label: 'One' }, { label: 'Two' }],
      },
    });
    messagesOnServer = {
      messages: [dialogMessage(1, 'toolu_a', 'First question?')],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    (container.querySelector('.dialog-check input') as HTMLInputElement).click();
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    // The pending dialog changes identity while the panel stays mounted.
    messagesOnServer = {
      ...messagesOnServer,
      messages: [
        dialogMessage(1, 'toolu_a', 'First question?'),
        dialogMessage(2, 'toolu_b', 'Second question?'),
      ],
      cursor: 2,
    };
    emitMessagesChanged();
    await settle();

    expect(container.textContent).toContain('Second question?');
    expect(buttonByText('Submit')!.disabled).toBe(true); // stale picks dropped
  });

  it('shows transcript-specific copy when the transcript is gone on a live run', async () => {
    messagesOnServer = {
      messages: [],
      state: 'ended',
      cursor: 0,
      has_more: false,
      transcript: 'gone',
    };
    await mountChat(); // runOnServer stays active

    const note = container.querySelector('.chat-composer-note');
    expect(note?.textContent).toContain('Transcript no longer available');
    expect(note?.textContent).not.toContain('This instance has ended');
    expect(container.querySelector('.chat-input')).toBeNull();
  });

  it('titles the chat with the repo half and falls back to the provider web link', async () => {
    runOnServer = { ...baseRun(), deep_link_url: null };
    await mountChat();

    expect(container.querySelector('.chat-title')?.textContent).toBe('proj · dom · 15:00');
    // ADR-0017: the fallback URL + tooltip come from the providers API, not a
    // hardcoded constant.
    const link = Array.from(container.querySelectorAll('a')).find((a) =>
      a.getAttribute('aria-label')?.includes('Open'),
    );
    expect(link?.getAttribute('href')).toBe('https://claude.ai/code');
    expect(link?.getAttribute('title')).toContain('claude.ai session picker');
  });

  it('shows a copyable tmux-attach for a link-less provider (no web fallback)', async () => {
    runOnServer = { ...baseRun(), provider: 'codex', deep_link_url: null };
    providersOnServer = [{ id: 'codex', models: [], efforts: [], options: [] }];
    await mountChat();

    expect(
      Array.from(container.querySelectorAll('a')).some((a) =>
        a.getAttribute('aria-label')?.includes('Open'),
      ),
    ).toBe(false);
    const attach = container.querySelector('button.attach-copy');
    expect(attach?.textContent).toContain('Copy attach');
    expect(attach?.getAttribute('title')).toContain('tmux attach -t proj~dom-20260706-1500');
  });

  it('does not resurrect Load earlier after paging up hit the beginning', async () => {
    messagesOnServer = {
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
    emitMessagesChanged();
    await settle();
    expect(buttonByText('Load earlier')).toBeNull();
  });

  // --- Markdown rendering (issue #13) ---

  it('renders assistant markdown: heading, bold, inline code, and a code block', async () => {
    withAssistantText('# Title\n\nsome **bold** and `inline`\n\n```js\nconst x = 1;\n```');
    await mountChat();

    expect(container.querySelector('.md .md-h')?.textContent).toBe('Title');
    expect(container.querySelector('.md strong')?.textContent).toBe('bold');
    expect(container.querySelector('.md-code')?.textContent).toBe('inline');
    expect(container.querySelector('.md-codeblock-lang')?.textContent).toBe('js');
    expect(container.querySelector('.md-pre')?.textContent).toContain('const x = 1;');
    // No literal asterisks leaked through — markdown was parsed, not shown raw.
    expect(container.querySelector('.md strong')?.textContent).not.toContain('*');
  });

  it('renders only allowed-scheme links and never emits a javascript: href', async () => {
    withAssistantText('[ok](https://ex.com) and [bad](javascript:alert(1))');
    await mountChat();

    const links = Array.from(container.querySelectorAll('.md a')) as HTMLAnchorElement[];
    expect(links).toHaveLength(1);
    expect(links[0]?.getAttribute('href')).toBe('https://ex.com');
    expect(links[0]?.getAttribute('rel')).toBe('noopener noreferrer');
    expect(links[0]?.getAttribute('target')).toBe('_blank');
    expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
    // the rejected link degrades to visible text, not a dropped node
    expect(container.textContent).toContain('javascript:alert(1)');
  });

  it('renders markdown for user messages but shows no whole-message copy button', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'user', text: 'do **this**' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.role-user .md strong')?.textContent).toBe('this');
    expect(container.querySelector('.role-user .chat-msg-actions')).toBeNull();
  });

  // --- Tool-call grouping (issue #13) ---

  it('coalesces a run of 2+ tool calls into one disclosure, rolling up errors', async () => {
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'text', role: 'assistant', text: 'on it' },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'ls', status: 'ok', output: 'a' } },
        { seq: 3, kind: 'text', role: 'assistant', thinking: true, text: 'hmm' },
        { seq: 4, kind: 'tool', tool: { name: 'Read', title: 'read', status: 'error' } },
        { seq: 5, kind: 'tool', tool: { name: 'Bash', title: 'grep', status: 'ok' } },
      ],
      state: 'needs_input',
      cursor: 5,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.chat-tool-group')).not.toBeNull();
    // Count is tools only — the folded-in thinking is not counted.
    expect(container.querySelector('.tool-group-count')?.textContent).toBe('3 tool calls');
    expect(container.querySelector('.tool-group-failed')?.textContent).toContain('1 failed');
    expect(container.querySelector('.tool-group-summary')?.classList.contains('has-error')).toBe(
      true,
    );
  });

  it('leaves a lone tool call as a plain chip (threshold is 2+)', async () => {
    // The default fixture has a single tool at seq 3.
    await mountChat();
    expect(container.querySelector('.chat-tool')).not.toBeNull();
    expect(container.querySelector('.chat-tool-group')).toBeNull();
  });

  it('hides folded-in thinking inside a group until the thinking toggle is on (decision 9)', async () => {
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok' } },
        { seq: 2, kind: 'text', role: 'assistant', thinking: true, text: 'secret group reasoning' },
        { seq: 3, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'ok' } },
      ],
      state: 'needs_input',
      cursor: 3,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    // Thinking folds in (keeps the run together) but is hidden in the body and
    // never counted while the toggle is off.
    expect(container.querySelector('.tool-group-body')?.textContent).not.toContain(
      'secret group reasoning',
    );
    expect(container.querySelector('.tool-group-count')?.textContent).toBe('2 tool calls');

    buttonByText('Show thinking')!.click();
    await settle();
    expect(container.querySelector('.tool-group-body')?.textContent).toContain(
      'secret group reasoning',
    );
  });

  it('shows a live "running…" summary while a trailing run is still in flight', async () => {
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok' } },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'running' } },
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.tool-group-count')?.textContent).toBe('2 tool calls');
    expect(container.querySelector('.tool-group-running')?.textContent).toContain('running');
  });

  it('keeps an expanded tool group open across an SSE refetch (decision 12)', async () => {
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok' } },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'running' } },
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    const details = container.querySelector('.chat-tool-group') as HTMLDetailsElement;
    expect(details.open).toBe(false);
    // Expand it as a user tap would.
    details.open = true;
    details.dispatchEvent(new Event('toggle'));
    await settle();

    // An SSE tick appends another tool and re-derives the group.
    messagesOnServer = {
      ...messagesOnServer,
      messages: [
        ...messagesOnServer.messages,
        { seq: 3, kind: 'tool', tool: { name: 'Bash', title: 'c', status: 'running' } },
      ],
      cursor: 3,
    };
    emitMessagesChanged();
    await settle();

    const after = container.querySelector('.chat-tool-group') as HTMLDetailsElement;
    expect(after.open).toBe(true); // survived the recompute — keyed by first tool seq
    expect(container.querySelector('.tool-group-count')?.textContent).toBe('3 tool calls');
  });

  // --- Copy-raw (issue #13) ---

  it('copies the raw markdown of an assistant message to the clipboard', async () => {
    const writeText = stubClipboard();
    withAssistantText('# Title\n\n**raw** markdown');
    await mountChat();

    const btn = container.querySelector('.chat-msg-actions .copy-btn') as HTMLButtonElement;
    expect(btn).not.toBeNull();
    btn.click();
    await settle();
    expect(writeText).toHaveBeenCalledWith('# Title\n\n**raw** markdown');
    // Feedback swaps to the copied state.
    expect(container.querySelector('.copy-btn.copied')).not.toBeNull();
  });

  it('copies the raw source of a fenced code block from its header bar', async () => {
    const writeText = stubClipboard();
    withAssistantText('```py\nprint(1)\nprint(2)\n```');
    await mountChat();

    const btn = container.querySelector('.md-codeblock-bar .copy-btn') as HTMLButtonElement;
    expect(btn).not.toBeNull();
    btn.click();
    await settle();
    expect(writeText).toHaveBeenCalledWith('print(1)\nprint(2)');
  });
});
