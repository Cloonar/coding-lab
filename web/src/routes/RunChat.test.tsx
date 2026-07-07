// RunChat behavioral contract (issue #7):
// - the stream renders user/assistant text, tool chips, and lifecycle/errors;
//   thinking is hidden until the toggle is pressed;
// - a run.messages.changed for THIS run refetches; other runs are ignored;
//   run.changed for other repos is ignored too;
// - a refetch tails with after=<cursor> (paginating past the window limit) so
//   appends accumulate gap-free, and a stale in-flight response never applies
//   over a newer one (request-token guard);
// - the composer replies (POST /reply) and clears; while working it shows the
//   queued hint;
// - a pending dialog locks the composer and renders native option buttons that
//   POST /answer with the option index; the panel is gated on state==='question'
//   and its selections reset when the dialog identity (tool_id) changes;
// - an ended run is read-only (no composer, no reply POST); a gone transcript
//   on a live run gets transcript-specific copy;
// - interrupt POSTs /interrupt behind a confirm tap;
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

    buttonByText('Send')!.click();
    await settle();

    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('keep going');
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
  });

  it('shows the queued hint while the agent is working', async () => {
    messagesOnServer = { ...messagesOnServer, state: 'working' };
    await mountChat();
    expect(container.querySelector('.chat-composer-hint')?.textContent).toContain('queued');
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

  it('is read-only for an ended run', async () => {
    runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    messagesOnServer = { ...messagesOnServer, state: 'ended' };
    await mountChat();

    expect(container.querySelector('.chat-input')).toBeNull();
    expect(container.querySelector('.chat-composer-note')?.textContent).toContain('read-only');
  });

  it('interrupts behind a confirm tap', async () => {
    await mountChat();
    buttonByText('Interrupt')!.click();
    await settle();
    expect(interruptPosts).toBe(0); // nothing until confirmed
    buttonByText('Confirm')!.click();
    await settle();
    expect(interruptPosts).toBe(1);
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
      a.textContent?.includes('Open ↗'),
    );
    expect(link?.getAttribute('href')).toBe('https://claude.ai/code');
    expect(link?.getAttribute('title')).toContain('claude.ai session picker');
  });

  it('shows a copyable tmux-attach for a link-less provider (no web fallback)', async () => {
    runOnServer = { ...baseRun(), provider: 'codex', deep_link_url: null };
    providersOnServer = [{ id: 'codex', models: [], efforts: [] }];
    await mountChat();

    expect(
      Array.from(container.querySelectorAll('a')).some((a) => a.textContent?.includes('Open ↗')),
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
});
