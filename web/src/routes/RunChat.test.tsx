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
import type { ChatMessage, MessagesResponse, Provider, Run, RunCommand } from '../api';
import App from '../App';
import { clearQueued, peekQueued, setQueued } from '../lib/queuedMessage';
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
let commandsOnServer: RunCommand[];
let replyPosts: { text: string }[];
// Reply POST status — 204 by default; a test flips it to 409 to exercise the
// queued-message auto-send failure path (draft seeding).
let replyStatus: number;
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
      // The run's slash-command catalog (issue #51 decision 5).
      if (url === `/api/v1/runs/${RUN_ID}/commands` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { commands: commandsOnServer }));
      }
      if (url === `/api/v1/runs/${RUN_ID}/reply` && method === 'POST') {
        replyPosts.push(JSON.parse(String(init?.body)) as { text: string });
        return Promise.resolve(
          replyStatus >= 400
            ? jsonResponse(replyStatus, { error: 'run is not accepting replies' })
            : jsonResponse(replyStatus, ''),
        );
      }
      if (url === `/api/v1/runs/${RUN_ID}/answer` && method === 'POST') {
        answerPosts.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return Promise.resolve(jsonResponse(204, ''));
      }
      if (url === `/api/v1/runs/${RUN_ID}/interrupt` && method === 'POST') {
        interruptPosts += 1;
        return Promise.resolve(jsonResponse(204, ''));
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
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

// The mobile `•••` overflow menu (issue #35 §1). Its trigger is always present;
// its items render only while the menu is open (like TopBar), so they never leak
// duplicate buttons into the default-DOM assertions above.
function moreButton(): HTMLButtonElement | null {
  return buttonByLabel('More actions');
}
function menuItem(text: string): HTMLButtonElement | undefined {
  return Array.from(container.querySelectorAll('.chat-menu button')).find(
    (b) => b.textContent?.trim() === text,
  ) as HTMLButtonElement | undefined;
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
      display_name: 'Claude Code',
      auth: { kind: 'oauth-code' },
      models: [],
      efforts: [],
      options: [],
      fallback_open: {
        url: 'https://claude.ai/code',
        title: "Opens the claude.ai session picker — the exact deep link wasn't captured",
      },
    },
  ];
  commandsOnServer = [
    {
      name: 'clear',
      description: 'Wipe conversation history and free up context',
      arg_hint: '',
      source: 'builtin',
      role: 'clear',
      chat_safe: true,
    },
    {
      name: 'compact',
      description: 'Compact the conversation',
      arg_hint: 'instructions',
      source: 'builtin',
      chat_safe: true,
    },
    {
      name: 'deploy',
      description: 'Ship it',
      arg_hint: 'env',
      source: 'project',
      chat_safe: true,
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
  replyStatus = 204;
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
  // The queued-message store is module-global — clear this run's entry so a
  // test that queues never leaks into the next.
  clearQueued(RUN_ID);
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

  it('surfaces a needs-input status line in the stream and keeps the composer usable', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'done' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: null,
    };
    await mountChat();

    // needs_input keeps the composer usable (just the input row) and surfaces
    // the waiting status as the last line of the stream, not a composer note (§3).
    expect(container.querySelector('.chat-input')).not.toBeNull();
    expect(container.textContent).toContain('Claude Code is waiting for your reply.');
    expect(container.querySelector('.chat-composer-note')).toBeNull(); // moved into the stream
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

  it('does not leak a stale high-seq tail when a rotation straddles a refetch', async () => {
    // The hard case: a rotation lands BETWEEN the two fetches of one refetch.
    // Accumulated stream on transcript A is [seq1, seq2]. A refetch fires
    // because A gained seq3; its after=2 tail GET still sees A (returns seq3),
    // then /clear rotates and the latest GET (and every later GET) sees the
    // fresh transcript B (seq1). Writing transcript_id resets the stream and
    // fires a superseding refetch mid-function; the stale outer refetch must not
    // merge its seq3 A-line back in — a seq that B (max seq 1) never overwrites
    // would otherwise persist forever (issue #34).
    messagesOnServer = {
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
      messages: [{ seq: 3, kind: 'text', role: 'assistant', text: 'A-stale-three' }],
      state: 'working',
      cursor: 3,
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
    // The after=2 tail GET sees A; the latest GET (after=0) and all later GETs
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
          return Promise.resolve(jsonResponse(200, { providers: providersOnServer }));
        if (url === `/api/v1/runs/${RUN_ID}` && method === 'GET')
          return Promise.resolve(jsonResponse(200, { ...runOnServer }));
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

    emitMessagesChanged();
    await settle();
    await settle();

    // Only the fresh transcript B shows; the stale seq-3 A tail is gone.
    expect(container.textContent).toContain('B-fresh-one');
    expect(container.textContent).not.toContain('A-stale-three');
    expect(container.textContent).not.toContain('A-two');
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
    providersOnServer = [
      {
        id: 'codex',
        display_name: 'Codex CLI',
        auth: { kind: 'external' },
        models: [],
        efforts: [],
        options: [],
      },
    ];
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

  // --- Needs-input status line relocated into the stream (issue #35 §3) ---

  it('renders the needs-input status line as the last stream item, not a composer note', async () => {
    withAssistantText('done'); // fixture state is needs_input
    await mountChat();

    const stream = container.querySelector('.chat-stream')!;
    const line = stream.querySelector('.chat-needs-input');
    expect(line?.textContent).toBe('Claude Code is waiting for your reply.');
    // §3: it is the LAST stream child (so it only shows at the bottom).
    expect(stream.lastElementChild).toBe(line);
    // A status line, not a chat bubble, and not the old composer note.
    expect(line?.closest('.chat-msg')).toBeNull();
    expect(container.querySelector('.chat-composer-note')).toBeNull();
    expect(container.querySelector('.chat-input')).not.toBeNull();
  });

  it('hides the needs-input line when the run resumes working', async () => {
    withAssistantText('done');
    await mountChat();
    expect(container.querySelector('.chat-needs-input')).not.toBeNull();

    messagesOnServer = { ...messagesOnServer, state: 'working' };
    emitMessagesChanged();
    await settle();

    // The line clears and the working hint takes over (reactive on state).
    expect(container.querySelector('.chat-needs-input')).toBeNull();
    expect(container.textContent).not.toContain('is waiting for your reply.');
    expect(container.querySelector('.chat-composer-hint')?.textContent).toContain(
      'tap to interrupt',
    );
  });

  it('drops the old claude.ai wording from the needs-input surface', async () => {
    withAssistantText('done');
    await mountChat();

    expect(container.textContent).not.toContain('reply below');
    expect(container.textContent).not.toContain('open it in claude.ai');
  });

  // --- One-line header + ••• dropdown (issue #35 §1) ---

  it('always renders the ••• menu trigger, even when the run is not live', async () => {
    runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    messagesOnServer = { ...messagesOnServer, state: 'ended' };
    await mountChat();

    expect(moreButton()).not.toBeNull();
    // Closed by default → no menu items leak into the DOM.
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
  });

  it('opens an anchored dropdown with Show thinking and Stop run for a live run', async () => {
    await mountChat(); // default fixture: live, needs_input

    expect(container.querySelector('.chat-menu-panel')).toBeNull();
    moreButton()!.click();
    await settle();

    expect(container.querySelector('.chat-menu-panel')).not.toBeNull();
    expect(menuItem('Show thinking')).toBeDefined();
    expect(menuItem('Stop run…')).toBeDefined();
  });

  it('omits Stop run from the dropdown when the run has ended', async () => {
    runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    messagesOnServer = { ...messagesOnServer, state: 'ended' };
    await mountChat();

    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).not.toBeNull();
    expect(menuItem('Show thinking')).toBeDefined();
    expect(menuItem('Stop run…')).toBeUndefined();
  });

  it('toggles thinking from the dropdown item', async () => {
    await mountChat();
    expect(container.textContent).not.toContain('secret reasoning');

    moreButton()!.click();
    await settle();
    menuItem('Show thinking')!.click();
    await settle();

    expect(container.textContent).toContain('secret reasoning');
  });

  it('closes the dropdown on Escape', async () => {
    await mountChat();
    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).not.toBeNull();

    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
  });

  it('renders a state dot carrying the convo class token for the live state', async () => {
    await mountChat(); // needs_input fixture
    // The mobile dot and the full chip both live in the DOM (CSS gates which
    // shows); the dot carries stateBadge('needs_input').cls, which the CSS maps
    // to the convo color (jsdom applies no stylesheet, so only the class is
    // assertable here — the color mapping is exercised by conversation.test.ts).
    expect(container.querySelector('.chat-state-dot.needs-input')).not.toBeNull();
  });

  // --- Quick-return header + jump-to-latest pill wiring (issue #35 §2 + §4) ---
  // jsdom has no layout/scroll engine, so the reducer logic is unit-tested in
  // chatStream.test.ts; here we lock the rendered DOM contract the component
  // wires up (so deleting the pill, no-op'ing onJump, or inverting a class fails).

  it('renders the header visible (no --hidden class) by default', async () => {
    await mountChat();
    const header = container.querySelector('.chat-header') as HTMLElement;
    expect(header).not.toBeNull();
    expect(header.classList.contains('chat-header--hidden')).toBe(false); // headerVisible starts true
  });

  it('mounts the jump pill hidden and reveals it (emphasized on needs_input) when scrolled up', async () => {
    withAssistantText('done'); // needs_input fixture
    await mountChat();

    const pillBtn = container.querySelector('.chat-jump') as HTMLButtonElement;
    expect(pillBtn).not.toBeNull(); // always mounted so it can fade OUT
    // At/near the bottom (jsdom metrics are 0) → hidden + inert.
    expect(pillBtn.classList.contains('hidden')).toBe(true);
    expect(pillBtn.getAttribute('aria-hidden')).toBe('true');
    expect(pillBtn.tabIndex).toBe(-1);

    // Fake a scrolled-up viewport and fire a user scroll → the pill reveals.
    const stream = container.querySelector('.chat-stream') as HTMLElement;
    Object.defineProperty(stream, 'scrollHeight', { value: 1000, configurable: true });
    Object.defineProperty(stream, 'clientHeight', { value: 100, configurable: true });
    stream.scrollTop = 0;
    stream.dispatchEvent(new Event('scroll'));
    await settle();

    expect(pillBtn.classList.contains('hidden')).toBe(false);
    // needs_input content below the fold → emphasized pill with the needs-you copy.
    expect(pillBtn.classList.contains('emphasis')).toBe(true);
    expect(pillBtn.getAttribute('aria-label')).toBe('Claude Code is waiting — jump to latest');
    expect(pillBtn.textContent).toContain('Claude Code needs you');

    // Tapping smooth-scrolls to the latest (jsdom's scrollTo is a no-op stub, so
    // spy it to prove onJump → jumpToLatest is wired).
    const scrollToSpy = vi.fn();
    stream.scrollTo = scrollToSpy as unknown as typeof stream.scrollTo;
    pillBtn.click();
    await settle();
    expect(scrollToSpy).toHaveBeenCalledWith(expect.objectContaining({ behavior: 'smooth' }));
  });

  it('shows the non-emphasized pill copy when the content below is not a needs-you signal', async () => {
    messagesOnServer = { ...messagesOnServer, state: 'working' }; // not needs_input
    await mountChat();

    const pillBtn = container.querySelector('.chat-jump') as HTMLButtonElement;
    const stream = container.querySelector('.chat-stream') as HTMLElement;
    Object.defineProperty(stream, 'scrollHeight', { value: 1000, configurable: true });
    Object.defineProperty(stream, 'clientHeight', { value: 100, configurable: true });
    stream.scrollTop = 0;
    stream.dispatchEvent(new Event('scroll'));
    await settle();

    expect(pillBtn.classList.contains('hidden')).toBe(false);
    expect(pillBtn.classList.contains('emphasis')).toBe(false);
    expect(pillBtn.getAttribute('aria-label')).toBe('Jump to latest');
    expect(pillBtn.textContent).toContain('Latest');
  });

  // --- Queued first message from the New-run composer (issue #41) ---

  it('renders the queued first message as a pending user bubble while it waits', async () => {
    // A still-locating transcript means the run is not ready — the auto-send
    // holds, so the pending bubble stays visible.
    messagesOnServer = {
      messages: [],
      state: 'idle',
      cursor: 0,
      has_more: false,
      transcript: 'locating',
      transcript_id: '',
    };
    setQueued(RUN_ID, 'kick things off');
    await mountChat();

    const bubble = container.querySelector('.chat-stream .chat-msg.role-user.pending');
    expect(bubble).not.toBeNull();
    expect(bubble?.textContent).toContain('kick things off');
    expect(bubble?.textContent).toContain('Sends when Claude Code is ready');
    // It is the LAST stream child — nearest the composer it collapses into.
    expect(container.querySelector('.chat-stream')!.lastElementChild).toBe(bubble);
    // Not sent yet (transcript still locating).
    expect(replyPosts).toHaveLength(0);
  });

  it('auto-sends the queued first message exactly once when the transcript is available', async () => {
    withAssistantText('ready'); // needs_input + transcript available on a live run
    setQueued(RUN_ID, 'queued opener');
    await mountChat();

    // Fired once with the queued text; the pending bubble cleared.
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('queued opener');
    expect(container.querySelector('.chat-msg.pending')).toBeNull();
    expect(peekQueued(RUN_ID)).toBeUndefined();

    // A later SSE refetch must NOT re-send — the entry was consumed before the POST.
    emitMessagesChanged();
    await settle();
    expect(replyPosts).toHaveLength(1);
  });

  it('seeds the composer draft and shows an error when the auto-send fails', async () => {
    withAssistantText('ready'); // live, needs_input → composer usable
    replyStatus = 409;
    setQueued(RUN_ID, 'seed me back');
    await mountChat();

    // The single attempt failed: the error surfaces, the entry is gone (taken
    // before the POST), and the text is handed to the composer as a draft.
    expect(replyPosts).toHaveLength(1);
    expect(peekQueued(RUN_ID)).toBeUndefined();
    expect(container.querySelector('.banner.error')?.textContent).toContain(
      'run is not accepting replies',
    );
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe(
      'seed me back',
    );
    expect(container.querySelector('.chat-msg.pending')).toBeNull();
  });

  // --- Provider-neutral copy (issue #51 decision 9) ---

  it('drives every agent-naming string from the provider display_name', async () => {
    providersOnServer[0]!.display_name = 'Agent Zed';
    withAssistantText('done'); // needs_input
    await mountChat();

    expect(container.querySelector('.chat-needs-input')?.textContent).toBe(
      'Agent Zed is waiting for your reply.',
    );
    expect(container.textContent).not.toContain('Claude');
  });

  it('falls back to "the agent" while provider metadata is missing', async () => {
    providersOnServer = []; // no metadata for the run's provider
    withAssistantText('done'); // needs_input
    await mountChat();

    expect(container.querySelector('.chat-needs-input')?.textContent).toBe(
      'The agent is waiting for your reply.',
    );
  });

  it('derives the answer-elsewhere hint from fallback_open in the locked question state', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    // fallback_open.url is https://claude.ai/code → the hint names its host.
    expect(container.querySelector('.chat-composer-note')?.textContent).toBe(
      'Claude Code needs input — open it at claude.ai to respond.',
    );
  });

  // --- Multi-question dialogs (issue #51 decision 3) ---

  const multiQuestionDialog = () => ({
    tool_id: 'toolu_mq',
    dialog_kind: 'question' as const,
    prompt: '2 questions',
    answerable: true,
    questions: [
      {
        header: 'Approach',
        text: 'Which approach?',
        options: [
          { label: 'Revert', description: 'Roll back the change' },
          { label: 'Patch forward' },
          { label: 'Other', is_other: true },
        ],
      },
      {
        header: 'Scope',
        text: 'Which areas?',
        multi_select: true,
        options: [{ label: 'Frontend' }, { label: 'Backend' }, { label: 'Other', is_other: true }],
      },
    ],
  });

  function questionEl(i: number): Element {
    const el = container.querySelectorAll('.dialog-question')[i];
    if (!el) throw new Error(`missing .dialog-question[${i}]`);
    return el;
  }
  function questionOption(qi: number, label: string): HTMLButtonElement {
    const btn = Array.from(
      questionEl(qi).querySelectorAll<HTMLButtonElement>('button.dialog-option'),
    ).find((b) => b.querySelector('.dialog-option-label')?.textContent === label);
    if (!btn) throw new Error(`missing option "${label}" in question ${qi}`);
    return btn;
  }

  it('renders a multi-question form and submits all answers atomically via answers[]', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: multiQuestionDialog(),
    };
    await mountChat();

    // Composer locked; the two questions render stacked, in order, each with
    // its header chip + question text; options show label + description.
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    const headers = Array.from(container.querySelectorAll('.dialog-question-header')).map(
      (el) => el.textContent,
    );
    expect(headers).toEqual(['Approach', 'Scope']);
    expect(questionEl(0).textContent).toContain('Which approach?');
    expect(questionEl(1).textContent).toContain('Which areas?');
    expect(questionOption(0, 'Revert').querySelector('.dialog-option-desc')?.textContent).toBe(
      'Roll back the change',
    );

    // The single-select question keeps its synthesized Other row, but the
    // multi-select question renders WITHOUT it: the adapter rejects Other in
    // a multi-select answer (normSelected policy — recipe unverified), so the
    // form never offers it.
    const optionLabels = (qi: number) =>
      Array.from(questionEl(qi).querySelectorAll('.dialog-option-label')).map(
        (el) => el.textContent,
      );
    expect(optionLabels(0)).toEqual(['Revert', 'Patch forward', 'Other']);
    expect(optionLabels(1)).toEqual(['Frontend', 'Backend']);

    // ONE submit for the whole form, disabled until every question is answered.
    const submit = () => buttonByText('Submit')!;
    expect(submit().disabled).toBe(true);

    // Answer question 0 (single select) — still incomplete.
    questionOption(0, 'Patch forward').click();
    await settle();
    expect(questionOption(0, 'Patch forward').getAttribute('aria-pressed')).toBe('true');
    expect(submit().disabled).toBe(true);

    // Question 1 is multi-select: toggle two options.
    questionOption(1, 'Frontend').click();
    questionOption(1, 'Backend').click();
    await settle();
    expect(submit().disabled).toBe(false);

    // Multi-select toggles OFF again too.
    questionOption(1, 'Backend').click();
    await settle();
    expect(questionOption(1, 'Backend').getAttribute('aria-pressed')).toBe('false');
    questionOption(1, 'Backend').click();
    await settle();

    submit().click();
    await settle();

    // One atomic POST, positionally aligned with the questions (no question
    // index on the wire): a single-select answer is `index` = the chosen
    // OPTION index (never `selected`); a multi-select answer is `selected` =
    // the toggled option indices ascending (no `index`). No flat fields.
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toEqual({
      tool_id: 'toolu_mq',
      answers: [{ index: 1 }, { selected: [0, 1] }],
    });
  });

  it('requires non-empty Other text for a single-select Other pick too', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: multiQuestionDialog(),
    };
    await mountChat();

    // Pick Other in question 0 and answer question 1 fully.
    questionOption(0, 'Other').click();
    questionOption(1, 'Frontend').click();
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(true); // Other text missing

    const other = questionEl(0).querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'ship a hotfix';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    buttonByText('Submit')!.click();
    await settle();
    // Single-select Other = index of the is_other row + the free text; the
    // multi-select answer stays selected-only.
    expect(answerPosts[0]).toEqual({
      tool_id: 'toolu_mq',
      answers: [{ index: 2, other_text: 'ship a hotfix' }, { selected: [0] }],
    });
  });

  // --- Plan approval + generic approval kind (issue #51 decision 3) ---

  it('renders a plan dialog as markdown with real approve/reject buttons answering flat', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'planned' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_plan',
        dialog_kind: 'plan',
        prompt: '# The plan\n\nDo **bold** things',
        answerable: true,
        options: [
          { label: 'Approve', description: 'Start implementing' },
          { label: 'Keep planning', description: 'Send feedback instead' },
        ],
      },
    };
    await mountChat();

    // The prompt IS the plan body — rendered markdown, not raw markup.
    const plan = container.querySelector('.chat-dialog-plan');
    expect(plan).not.toBeNull();
    expect(plan?.querySelector('.md-h')?.textContent).toBe('The plan');
    expect(plan?.querySelector('strong')?.textContent).toBe('bold');
    expect(plan?.textContent).not.toContain('# The plan');

    // Real option buttons with label + description; answering stays the flat
    // single-select shape (no answers[]).
    const approve = Array.from(
      container.querySelectorAll<HTMLButtonElement>('button.dialog-option'),
    ).find((b) => b.querySelector('.dialog-option-label')?.textContent === 'Approve');
    expect(approve?.querySelector('.dialog-option-desc')?.textContent).toBe('Start implementing');
    approve!.click();
    await settle();
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toEqual({ tool_id: 'toolu_plan', index: 0 });
  });

  it('renders the approval kind like a single-question dialog', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'may I?' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_ok',
        dialog_kind: 'approval',
        prompt: 'Allow the tool call?',
        answerable: true,
        options: [{ label: 'Allow' }, { label: 'Deny' }],
      },
    };
    await mountChat();

    expect(container.textContent).toContain('Allow the tool call?');
    buttonByText('Deny')!.click();
    await settle();
    expect(answerPosts[0]).toEqual({ tool_id: 'toolu_ok', index: 1 });
  });

  it('keeps the degraded note for a not-answerable dialog, hinting the provider surface', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'stuck' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_odd',
        dialog_kind: 'question',
        prompt: 'A shape lab cannot drive',
        answerable: false,
      },
    };
    await mountChat();

    expect(container.querySelector('.chat-composer-note')?.textContent).toBe(
      "This dialog can't be answered here — open it at claude.ai to respond.",
    );
    expect(container.querySelector('button.dialog-option')).toBeNull();
  });

  // --- Slash-command autocomplete (issue #51 decision 5) ---

  function setComposerText(value: string): void {
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = value;
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }
  function composerKey(key: string, init: KeyboardEventInit = {}): void {
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, ...init }));
  }
  function popRows(): HTMLButtonElement[] {
    return Array.from(container.querySelectorAll<HTMLButtonElement>('.chat-cmd-row'));
  }

  it('opens the command popover only for a leading slash and filters across name/desc/hint', async () => {
    await mountChat();

    // A mid-message slash never triggers it (prefix-only).
    setComposerText('deploy a/b please');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();

    // A leading slash lists the whole catalog with name, hint, description
    // and source badge.
    setComposerText('/');
    await settle();
    const rows = popRows();
    expect(rows).toHaveLength(3);
    expect(rows[0]?.querySelector('.chat-cmd-name')?.textContent).toBe('/clear');
    expect(rows[1]?.querySelector('.chat-cmd-hint')?.textContent).toBe('instructions');
    expect(rows[2]?.querySelector('.chat-cmd-desc')?.textContent).toBe('Ship it');
    expect(rows[2]?.querySelector('.chat-cmd-source')?.textContent).toBe('project');
    // The description clamps to one line in CSS; the row's tooltip carries it in full.
    expect(rows[2]?.title).toBe('Ship it');

    // Filtering matches the name…
    setComposerText('/cle');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/clear',
    ]);

    // …the description, and the arg hint.
    setComposerText('/ship');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/deploy',
    ]);
    setComposerText('/env');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/deploy',
    ]);

    // No match → closed.
    setComposerText('/nosuch');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('cycles with arrows, accepts with Enter (inserting "/name "), closes on Escape', async () => {
    await mountChat();
    setComposerText('/');
    await settle();

    // Down moves the active row; the listbox exposes it via aria-selected.
    composerKey('ArrowDown');
    await settle();
    expect(popRows()[1]?.getAttribute('aria-selected')).toBe('true');

    // Enter accepts the active command — no reply is sent, no newline typed.
    composerKey('Enter');
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/compact ');
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
    expect(replyPosts).toHaveLength(0);

    // Typing revives the popover; Escape dismisses it until the next keystroke.
    setComposerText('/cl');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).not.toBeNull();
    composerKey('Escape');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();

    // Tab accepts too.
    setComposerText('/dep');
    await settle();
    composerKey('Tab');
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/deploy ');
  });

  it('accepts a command on click and sends it through the normal reply path', async () => {
    await mountChat();
    setComposerText('/cle');
    await settle();

    popRows()[0]!.click();
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/clear ');

    // Sending is the ordinary reply POST — no special casing for commands.
    buttonByLabel('Send')!.click();
    await settle();
    expect(replyPosts).toEqual([{ text: '/clear' }]);
  });

  // --- New conversation (role=clear, issue #51 decision 2) ---

  it('binds New conversation to the clear-role command behind an inline confirm', async () => {
    await mountChat(); // default: active run, needs_input, catalog has role=clear

    const newConvo = buttonByText('New conversation');
    expect(newConvo).not.toBeNull();
    newConvo!.click();
    await settle();

    // Inline confirm — nothing sent yet; Cancel disarms.
    expect(replyPosts).toHaveLength(0);
    expect(buttonByText('Confirm clear')).not.toBeNull();
    buttonByText('Cancel')!.click();
    await settle();
    expect(buttonByText('Confirm clear')).toBeNull();

    // Confirming sends the command AS A REPLY ("/clear") — the normal path.
    buttonByText('New conversation')!.click();
    await settle();
    buttonByText('Confirm clear')!.click();
    await settle();
    expect(replyPosts).toEqual([{ text: '/clear' }]);
  });

  it('hides New conversation while a dialog locks the composer and on an ended run', async () => {
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
      ],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();
    expect(buttonByText('New conversation')).toBeNull();

    dispose?.();
    container.remove();
    runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    messagesOnServer = { ...messagesOnServer, messages: [], state: 'ended', pending_dialog: null };
    await mountChat();
    expect(buttonByText('New conversation')).toBeNull();
  });

  it('offers no New conversation when the catalog has no clear-role command', async () => {
    commandsOnServer = commandsOnServer.filter((c) => c.role !== 'clear');
    await mountChat();
    expect(buttonByText('New conversation')).toBeNull();
    // The remaining commands still autocomplete.
    setComposerText('/');
    await settle();
    expect(popRows()).toHaveLength(2);
  });

  it('hides New conversation while the agent is mid-turn (working)', async () => {
    // A clear bypasses the composer (calls replyRun directly), so it must honor
    // the same ADR-0022 mid-turn send-block the composer enforces — otherwise
    // clicking it pastes /clear into the live TUI in the middle of a turn.
    messagesOnServer = { ...messagesOnServer, state: 'working' };
    await mountChat();
    expect(buttonByText('New conversation')).toBeNull();
  });
});
