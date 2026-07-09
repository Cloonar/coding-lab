// RunChat behavioral contract (issue #7):
// - the stream renders user/assistant text, tool chips, and lifecycle/errors;
//   thinking is permanently dropped at paint — in the stream and inside
//   expanded tool groups — with no toggle to reveal it (issue #68);
// - a run.messages.changed for THIS run refetches; other runs are ignored;
//   run.changed for other repos is ignored too;
// - a refetch tails with after=<cursor> (paginating past the window limit) so
//   appends accumulate gap-free, and a stale in-flight response never applies
//   over a newer one (request-token guard);
// - the composer replies (POST /reply) and clears; Cmd/Ctrl+Enter sends, bare
//   Enter does not; Send is ALWAYS present in the unlocked states and enabled
//   with text even while working (ADR-0029, issue #61), POSTing /reply
//   immediately — no morph, no queue copy, no working hint; Cmd/Ctrl+Enter
//   sends while working too;
// - a pending dialog renders as an interactive card INSIDE the chat stream
//   (issue #56) — deduped by tool_id against a transcript dialog message, never
//   twice — with native option buttons that POST /answer with the option index;
//   the composer collapses to a waiting note + Interrupt (no textarea) until it
//   resolves; the messages-scan fallback is gated on state==='question' and
//   selections reset when the dialog identity (tool_id) changes; a newly
//   arriving card scrolls its top into view only while following the tail;
// - an ANSWERED dialog message (outcome present — issue #56 decision 3)
//   renders as a compact inert Q→A summary: no buttons, no interactive card,
//   no raw tool chip; outcome PRESENCE alone is the answered signal;
// - dialog options are house-style cards (issue #56 decision 7): full-width,
//   descriptions always visible — the flat multi-select path is toggle-card
//   buttons (aria-pressed) instead of checkboxes, same Submit payload;
// - an ended run is read-only (no composer, no reply POST); a gone transcript
//   on a live run gets transcript-specific copy;
// - one-tap interrupt (POST /interrupt, no confirm) lives in the live-gated
//   header — inline on desktop and a `•••` menu item above Stop, a `pause`
//   glyph distinct from the two-step danger `square` Stop — plus the two
//   locked-state escape hatches;
// - the run's spawn-time model · effort rides a read-only chip beside the
//   state chip on desktop, and a non-interactive info row (role=none, not a
//   menuitem) pinned atop the `•••` panel on mobile — catalog pretty labels
//   with the raw id as fallback, hidden entirely for a legacy row with no
//   model (issue #68);
// - "Load earlier" never resurrects once paging-up hit the beginning.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ChatMessage, Dialog, MessagesResponse, Provider, Run, RunCommand } from '../api';
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
  it('renders text and tool chips; thinking never renders (issue #68)', async () => {
    await mountChat();

    expect(container.textContent).toContain('do the thing');
    expect(container.textContent).toContain('all done');
    expect(container.querySelector('.chat-tool')?.textContent).toContain('Ran ls');
    // Thinking is permanently dropped at paint — there is no toggle to reveal it.
    expect(container.textContent).not.toContain('secret reasoning');
    expect(buttonByText('Show thinking')).toBeNull();
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

  it('keeps Send available and sending while the agent is working', async () => {
    // ADR-0029 (issue #61): Send no longer morphs — it stays in the composer
    // through `working`, enabled once the box has text, and POSTs /reply
    // immediately (a genuinely mid-turn reply is queued by the agent's own TUI,
    // with no queue UI here). No working hint, no "tap to interrupt" copy.
    messagesOnServer = { ...messagesOnServer, state: 'working' };
    await mountChat();

    const row = container.querySelector('.chat-composer-row');
    expect(row).not.toBeNull();
    const send = row!.querySelector<HTMLButtonElement>('button[aria-label="Send"]');
    expect(send).not.toBeNull();
    // Disabled while the box is empty, even though the agent is working.
    expect(send!.disabled).toBe(true);
    // The composer carries no Interrupt of its own now (the header holds the
    // one-tap turn Interrupt); scope so the live header button isn't counted.
    expect(container.querySelector('.chat-composer button[aria-label="Interrupt"]')).toBeNull();
    // The deleted working hint / "tap to interrupt" / queue copy are all gone.
    expect(container.querySelector('.chat-composer-hint')).toBeNull();
    expect(container.textContent).not.toContain('tap to interrupt');
    expect(container.textContent).not.toContain('queued');

    // Typing enables Send; the textarea is editable throughout.
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    expect(input.disabled).toBe(false);
    input.value = 'mid-turn thought';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(send!.disabled).toBe(false);

    // Clicking POSTs /reply immediately with the typed text.
    send!.click();
    await settle();
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('mid-turn thought');
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

  it('preserves a compose-ahead draft across a working→idle state flip', async () => {
    messagesOnServer = { ...messagesOnServer, state: 'working' };
    await mountChat();

    // Type a draft while working — Send is already present and enabled (ADR-0029).
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'draft thought';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByLabel('Send')!.disabled).toBe(false);

    // The agent returns to needs_input: the draft survives the state flip and
    // sending it posts the retained text.
    messagesOnServer = { ...messagesOnServer, state: 'needs_input' };
    emitMessagesChanged();
    await settle();

    const preserved = container.querySelector('.chat-input') as HTMLTextAreaElement;
    expect(preserved.value).toBe('draft thought');
    const send = buttonByLabel('Send');
    expect(send).not.toBeNull();
    expect(send!.disabled).toBe(false);

    send!.click();
    await settle();
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('draft thought');
  });

  it('sends on Cmd/Ctrl+Enter even while the agent is working (ADR-0029)', async () => {
    messagesOnServer = { ...messagesOnServer, state: 'working' };
    await mountChat();

    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ship it now';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    // The shortcut no longer gates on `working` — it sends in every unlocked state.
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('ship it now');
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

    // The free-text reply composer collapses while the dialog is pending.
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    // The interactive card renders in the STREAM at the dialog message's
    // position (issue #56) — the message does not double as an inert prompt.
    const card = container.querySelector('.chat-stream .chat-dialog-card');
    expect(card).not.toBeNull();
    expect(card?.textContent).toContain('Which fix?');
    expect(container.querySelector('.chat-dialog-inline')).toBeNull();

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

    // Composer is locked; the card renders the field's prompt + options,
    // APPENDED as the last stream item (nothing in the transcript anchors it).
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    const card = container.querySelector('.chat-stream .chat-dialog-card');
    expect(card).not.toBeNull();
    expect(card?.textContent).toContain('Pick a flavor?');
    expect(container.querySelector('.chat-stream')!.lastElementChild).toBe(card);

    buttonByText('Option B')!.click();
    await settle();
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toMatchObject({ tool_id: 'toolu_field', index: 1 });
  });

  // --- Inline dialog cards in the stream (issue #56) ---

  const singleSelectDialog = (toolID = 'toolu_card') => ({
    tool_id: toolID,
    dialog_kind: 'question' as const,
    prompt: 'Pick a flavor?',
    answerable: true,
    options: [{ label: 'Option A' }, { label: 'Option B' }],
  });

  it('collapses the composer to a waiting note + Interrupt while the card is pending', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectDialog(),
    };
    await mountChat();

    // The interactive single-select renders inside the scrollable stream.
    const card = container.querySelector('.chat-stream .chat-dialog-card');
    expect(card).not.toBeNull();
    expect(card?.querySelector('.chat-dialog-prompt')?.textContent).toBe('Pick a flavor?');

    // The composer: one-line waiting note pointing up at the card, plus the
    // Interrupt escape hatch — no textarea, no Send (decision 2).
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    expect(container.querySelector('.chat-input')).toBeNull();
    expect(buttonByLabel('Send')).toBeNull();
    expect(container.querySelector('.chat-composer .chat-composer-note')?.textContent).toBe(
      'Claude Code is waiting on your answer — see the question above.',
    );
    // Exactly ONE escape-hatch `.chat-interrupt`, in the composer — always
    // visible however far the stream card scrolls, and the card carries none.
    // (The live header's turn Interrupt is class `chat-turn-interrupt`, which
    // does not match `.chat-interrupt`, so it is intentionally not counted here.)
    const hatch = container.querySelector<HTMLButtonElement>('.chat-composer .chat-interrupt');
    expect(hatch).not.toBeNull();
    expect(container.querySelectorAll('.chat-interrupt')).toHaveLength(1);
    expect(card?.querySelector('.chat-interrupt')).toBeNull();
    // Scope the click to the escape hatch: buttonByLabel('Interrupt') would hit
    // the header turn Interrupt first in DOM order on this live run.
    hatch!.click();
    await settle();
    expect(interruptPosts).toBe(1); // one tap, no confirm
  });

  it('renders the dialog exactly once when a stream message and pending_dialog share a tool_id', async () => {
    // The transcript flushed a dialog message AND the spool serves the same
    // tool_id: ONE interactive card, at the stream message's position, fed by
    // the richer field data.
    messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'dialog',
          dialog: {
            tool_id: 'toolu_dup',
            dialog_kind: 'question',
            prompt: 'Which fix?',
            answerable: true,
            options: [{ label: 'Revert' }, { label: 'Patch forward' }],
          },
        },
        { seq: 2, kind: 'text', role: 'assistant', text: 'context after' },
      ],
      state: 'question',
      cursor: 2,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_dup',
        dialog_kind: 'question',
        prompt: 'Which fix?',
        answerable: true,
        options: [
          { label: 'Revert', description: 'Roll back the change' },
          { label: 'Patch forward' },
        ],
      },
    };
    await mountChat();

    // Exactly one card, the interactive options render once, and no inert
    // duplicate of the same prompt remains.
    expect(container.querySelectorAll('.chat-dialog-card')).toHaveLength(1);
    expect(
      Array.from(container.querySelectorAll('button.dialog-option')).filter(
        (b) => b.querySelector('.dialog-option-label')?.textContent === 'Revert',
      ),
    ).toHaveLength(1);
    expect(container.querySelector('.chat-dialog-inline')).toBeNull();

    // At the stream MESSAGE's position — the later text message follows it —
    // and carrying the spool field's data (the transcript copy lacks the
    // description).
    const card = container.querySelector('.chat-stream .chat-dialog-card')!;
    expect(card.nextElementSibling?.textContent).toContain('context after');
    expect(card.querySelector('.dialog-option-desc')?.textContent).toBe('Roll back the change');

    buttonByText('Patch forward')!.click();
    await settle();
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toMatchObject({ tool_id: 'toolu_dup', index: 1 });
  });

  it('removes the card and returns the textarea once the dialog resolves', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectDialog(),
    };
    await mountChat();
    expect(container.querySelector('.chat-dialog-card')).not.toBeNull();
    expect(container.querySelector('.chat-input')).toBeNull();

    // The dialog resolves (answered on another surface) and the agent works on.
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'working',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: null,
    };
    emitMessagesChanged();
    await settle();

    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    expect(container.querySelector('.chat-composer-row')).not.toBeNull();
    expect(container.querySelector('.chat-input')).not.toBeNull();
  });

  // --- Arrival auto-scroll (issue #56 decision 5) ---
  // jsdom has no Element.prototype.scrollIntoView — install a recording stub
  // (capturing `this`) so the card scroll is observable and attributable.

  function stubScrollIntoView(): {
    calls: { target: Element; arg: unknown }[];
    restore: () => void;
  } {
    const calls: { target: Element; arg: unknown }[] = [];
    const proto = Element.prototype as unknown as { scrollIntoView?: (arg?: unknown) => void };
    proto.scrollIntoView = vi.fn(function (this: Element, arg?: unknown) {
      calls.push({ target: this, arg });
    });
    return {
      calls,
      restore: () => {
        delete proto.scrollIntoView;
      },
    };
  }

  it('scrolls the arriving dialog card top into view while following the tail', async () => {
    const scrolls = stubScrollIntoView();
    try {
      await mountChat(); // no dialog yet; jsdom geometry reads as at-bottom
      expect(scrolls.calls).toHaveLength(0);

      // A refetch brings a NEW pending dialog: follow is on, so the CARD's
      // top comes into view — not the stream bottom.
      messagesOnServer = {
        ...messagesOnServer,
        state: 'question',
        pending_dialog: singleSelectDialog('toolu_scroll'),
      };
      emitMessagesChanged();
      await settle();

      const card = container.querySelector('.chat-dialog-card');
      expect(card).not.toBeNull();
      expect(scrolls.calls).toHaveLength(1);
      expect(scrolls.calls[0]?.target).toBe(card);
      expect(scrolls.calls[0]?.arg).toEqual({ block: 'start' });

      // The SAME dialog on the next tick is not new — no re-yank.
      emitMessagesChanged();
      await settle();
      expect(scrolls.calls).toHaveLength(1);
    } finally {
      scrolls.restore();
    }
  });

  it('leaves the viewport alone when a dialog arrives while scrolled up', async () => {
    const scrolls = stubScrollIntoView();
    try {
      await mountChat(); // no dialog yet
      // Fake a scrolled-up viewport (the jump-pill test's geometry recipe):
      // far from the bottom, so follow is off.
      const stream = container.querySelector('.chat-stream') as HTMLElement;
      Object.defineProperty(stream, 'scrollHeight', { value: 1000, configurable: true });
      Object.defineProperty(stream, 'clientHeight', { value: 100, configurable: true });
      stream.scrollTop = 0;

      messagesOnServer = {
        ...messagesOnServer,
        state: 'question',
        pending_dialog: singleSelectDialog('toolu_up'),
      };
      emitMessagesChanged();
      await settle();

      // The card rendered, but the position was preserved — the jump pill is
      // the scrolled-up reader's affordance (untouched by issue #56).
      expect(container.querySelector('.chat-dialog-card')).not.toBeNull();
      expect(scrolls.calls).toHaveLength(0);
      expect(stream.scrollTop).toBe(0); // no bottom-follow either
    } finally {
      scrolls.restore();
    }
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
    // one-tap Interrupt escape hatch remains (decision 5) — no confirm step.
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.chat-composer-row')).toBeNull(); // locked
    // Scope to the composer: on this live run the header also renders a turn
    // Interrupt (class `chat-turn-interrupt`), which buttonByLabel would hit
    // first in DOM order.
    const hatch = container.querySelector<HTMLButtonElement>('.chat-composer .chat-interrupt');
    expect(hatch).not.toBeNull();
    // The hatch wears the `pause` glyph now (two rects) — the morph's pulse cue
    // is gone; two rects also read distinct from the danger `square` Stop's one.
    expect(hatch!.querySelectorAll('svg rect')).toHaveLength(2);
    hatch!.click();
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
    // State stayed needs_input (the stale 'working' tail was dropped): the
    // needs-input status line is still in the stream, not reverted away.
    expect(container.querySelector('.chat-needs-input')).not.toBeNull();
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

    // Flat multi-select options are toggle cards (issue #56 decision 7).
    buttonByText('One')!.click();
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

  it('drops folded-in thinking inside an opened tool group, permanently (issue #68)', async () => {
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

    // Thinking folds in (keeps the run together) but is never counted.
    expect(container.querySelector('.tool-group-count')?.textContent).toBe('2 tool calls');
    expect(container.querySelector('.tool-group-body')?.textContent).not.toContain(
      'secret group reasoning',
    );

    // Opening the group (a user tap) still never reveals it — there is no
    // toggle left to flip.
    const details = container.querySelector('.chat-tool-group') as HTMLDetailsElement;
    details.open = true;
    details.dispatchEvent(new Event('toggle'));
    await settle();

    expect(container.querySelector('.tool-group-body')?.textContent).not.toContain(
      'secret group reasoning',
    );
    expect(buttonByText('Show thinking')).toBeNull();
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

    // The needs-input line clears. The composer no longer reacts to `working`
    // (ADR-0029): no working hint element exists, and Send stays present.
    expect(container.querySelector('.chat-needs-input')).toBeNull();
    expect(container.textContent).not.toContain('is waiting for your reply.');
    expect(container.querySelector('.chat-composer-hint')).toBeNull();
    expect(buttonByLabel('Send')).not.toBeNull();
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

  it('opens an anchored dropdown with the model info row, open affordance, Interrupt and Stop run for a live run', async () => {
    await mountChat(); // default fixture: live, needs_input

    expect(container.querySelector('.chat-menu-panel')).toBeNull();
    moreButton()!.click();
    await settle();

    const panel = container.querySelector('.chat-menu-panel');
    expect(panel).not.toBeNull();
    expect(container.querySelector('.chat-menu-open')).not.toBeNull(); // the open affordance
    expect(menuItem('Interrupt')).toBeDefined(); // live turn Interrupt (ADR-0029)
    expect(menuItem('Stop run…')).toBeDefined();
    expect(menuItem('Show thinking')).toBeUndefined();
    expect(menuItem('New conversation')).toBeUndefined();

    // The spawn-time model info row (issue #68): a plain, non-focusable div —
    // NOT a menuitem — pinned as the panel's FIRST child, above every item.
    const info = panel!.firstElementChild as HTMLElement;
    expect(info.classList.contains('chat-menu-info')).toBe(true);
    expect(info.tagName).toBe('DIV');
    expect(info.getAttribute('role')).toBe('none');
    expect(info.textContent).toBe('opus[1m] · max');

    // The model string never leaks into an actual menu item.
    const menuItemTexts = Array.from(document.querySelectorAll('[role=menuitem]')).map(
      (el) => el.textContent,
    );
    expect(menuItemTexts.some((t) => t?.includes('opus[1m]'))).toBe(false);
  });

  it('omits Stop run from the dropdown when the run has ended', async () => {
    runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    messagesOnServer = { ...messagesOnServer, state: 'ended' };
    await mountChat();

    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).not.toBeNull();
    // The model info row shows regardless of liveness (it's spawn-time, not run state).
    expect(container.querySelector('.chat-menu-info')?.textContent).toBe('opus[1m] · max');
    // Both the turn Interrupt and Stop are live-gated — gone on an ended run.
    expect(menuItem('Interrupt')).toBeUndefined();
    expect(menuItem('Stop run…')).toBeUndefined();
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

  // --- Header turn Interrupt (ADR-0029, issue #61) ---
  // The one-tap turn Interrupt relocated from the composer to the header, gated
  // on the LIVE run outcome (not the derived `working` state). jsdom loads no
  // CSS, so both the desktop actions and the `•••` menu are in the DOM at once.

  it('renders a one-tap desktop header Interrupt that posts /interrupt, live-gated', async () => {
    await mountChat(); // default fixture: live (outcome active)

    const interrupt = container.querySelector<HTMLButtonElement>(
      '.chat-desktop-actions button[aria-label="Interrupt"]',
    );
    expect(interrupt).not.toBeNull();
    expect(interrupt!.classList.contains('chat-turn-interrupt')).toBe(true);
    expect(interrupt!.title).toBe('Interrupt the current turn (keeps the session)');
    // The `pause` glyph (two rects) reads distinct from the danger two-step
    // `square` Stop (one rect) that renders immediately after it.
    expect(interrupt!.querySelectorAll('svg rect')).toHaveLength(2);
    const stop = container.querySelector<HTMLButtonElement>('.chat-desktop-actions .chat-stop');
    expect(stop!.querySelectorAll('svg rect')).toHaveLength(1);

    // One click fires interrupt with no confirm step.
    interrupt!.click();
    await settle();
    expect(interruptPosts).toBe(1);
  });

  it('omits the header Interrupt for an ended run (live-gated)', async () => {
    runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    messagesOnServer = { ...messagesOnServer, state: 'ended' };
    await mountChat();

    expect(
      container.querySelector('.chat-desktop-actions button[aria-label="Interrupt"]'),
    ).toBeNull();
    // None anywhere: not live (no header/menu turn Interrupt) and the ended
    // composer is read-only (no escape hatch).
    expect(buttonByLabel('Interrupt')).toBeNull();
  });

  it('offers a menu Interrupt above Stop that fires and closes the menu', async () => {
    await mountChat(); // default fixture: live
    moreButton()!.click();
    await settle();

    const panel = container.querySelector('.chat-menu-panel')!;
    const interrupt = menuItem('Interrupt');
    const stop = menuItem('Stop run…');
    expect(interrupt).toBeDefined();
    expect(stop).toBeDefined();
    expect(interrupt!.title).toBe('Interrupt the current turn (keeps the session)');
    // Listed ABOVE the danger Stop item among the panel's buttons.
    const buttons = Array.from(panel.querySelectorAll('button'));
    expect(buttons.indexOf(interrupt!)).toBeLessThan(buttons.indexOf(stop!));

    // Clicking fires interrupt AND closes the menu (one tap, no confirm).
    interrupt!.click();
    await settle();
    expect(interruptPosts).toBe(1);
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
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

    // Composer locked; the form lives in the stream card with NO inner
    // scrollbox — nothing in the card carries an inline max-height (the CSS
    // caps are gone; jsdom applies no stylesheet, so inline style is the
    // assertable surface). The chat pane stays the only scrollbar.
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    const mqCard = container.querySelector('.chat-stream .chat-dialog-card');
    expect(mqCard).not.toBeNull();
    expect(mqCard?.querySelector('.dialog-questions')).not.toBeNull();
    expect(
      Array.from(mqCard!.querySelectorAll<HTMLElement>('*')).filter(
        (el) => el.style.maxHeight !== '',
      ),
    ).toEqual([]);

    // The two questions render stacked, in order, each with its header chip +
    // question text; options show label + description.
    const headers = Array.from(container.querySelectorAll('.dialog-question-header')).map(
      (el) => el.textContent,
    );
    expect(headers).toEqual(['Approach', 'Scope']);
    expect(questionEl(0).textContent).toContain('Which approach?');
    expect(questionEl(1).textContent).toContain('Which areas?');
    expect(questionOption(0, 'Revert').querySelector('.dialog-option-desc')?.textContent).toBe(
      'Roll back the change',
    );

    // BOTH questions keep the synthesized Other row — the multi-select one
    // included (the adapter fills the TUI's free-text row from other_text;
    // compat §7, captured live 2026-07-09).
    const optionLabels = (qi: number) =>
      Array.from(questionEl(qi).querySelectorAll('.dialog-option-label')).map(
        (el) => el.textContent,
      );
    expect(optionLabels(0)).toEqual(['Revert', 'Patch forward', 'Other']);
    expect(optionLabels(1)).toEqual(['Frontend', 'Backend', 'Other']);

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

  it('multi-select Other in a form: toggling it gates on text and rides other_text, never selected', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: multiQuestionDialog(),
    };
    await mountChat();

    questionOption(0, 'Revert').click();
    questionOption(1, 'Backend').click();
    questionOption(1, 'Other').click();
    await settle();
    // Other toggled but empty → the form is incomplete.
    expect(questionOption(1, 'Other').getAttribute('aria-pressed')).toBe('true');
    expect(buttonByText('Submit')!.disabled).toBe(true);

    const other = questionEl(1).querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'the build scripts';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    buttonByText('Submit')!.click();
    await settle();
    // The Other row's INDEX (2) stays out of selected — its text IS its
    // toggle (the adapter pastes it onto the TUI's free-text row, compat §7).
    expect(answerPosts[0]).toEqual({
      tool_id: 'toolu_mq',
      answers: [{ index: 0 }, { selected: [1], other_text: 'the build scripts' }],
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

    // The prompt IS the plan body — rendered markdown, not raw markup, inside
    // the stream card (issue #56 decision 4: full height, no inner scrollbox,
    // approve/reject after the whole plan).
    const plan = container.querySelector('.chat-stream .chat-dialog-card .chat-dialog-plan');
    expect(plan).not.toBeNull();
    expect(plan?.querySelector('.md-h')?.textContent).toBe('The plan');
    expect(plan?.querySelector('strong')?.textContent).toBe('bold');
    expect(plan?.textContent).not.toContain('# The plan');
    expect((plan as HTMLElement).style.maxHeight).toBe('');

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

    // The degraded note renders inside the stream card (the composer shows its
    // own waiting note alongside — hence the scoped query).
    expect(container.querySelector('.chat-dialog-card .chat-composer-note')?.textContent).toBe(
      "This dialog can't be answered here — open it at claude.ai to respond.",
    );
    expect(container.querySelector('button.dialog-option')).toBeNull();
  });

  // --- Answered Q→A summaries in the stream (issue #56 decision 3) ---
  // A dialog message WITH an outcome is history: a compact inert summary —
  // never the interactive card, never a raw tool chip, no buttons. Outcome
  // PRESENCE alone is the answered signal (an all-omitempty rejection
  // serializes as {}), so nothing keys on the inner fields.

  /** Serve one answered dialog message followed by later assistant text. */
  function withAnsweredDialog(dialog: Dialog): void {
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'dialog', dialog },
        { seq: 2, kind: 'text', role: 'assistant', text: 'moving on' },
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
      pending_dialog: null,
    };
  }

  it('renders an answered single-select dialog as an inert Q→A summary', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_done',
      dialog_kind: 'question',
      prompt: 'Which fix?',
      answerable: true,
      options: [{ label: 'Revert' }, { label: 'Patch forward' }],
      outcome: { results: [{ question: 'Which fix?', chosen: ['Patch forward'] }] },
    });
    await mountChat();

    const summary = container.querySelector('.chat-stream .chat-dialog-answered');
    expect(summary).not.toBeNull();
    // The question (muted line) above the chosen label.
    expect(summary?.querySelector('.dialog-qa-question')?.textContent).toBe('Which fix?');
    expect(summary?.querySelector('.dialog-qa-chosen')?.textContent).toBe('Patch forward');
    // Compact history: the unchosen option is NOT rendered.
    expect(summary?.textContent).not.toContain('Revert');
    // Inert: no buttons, no interactive card, no raw tool chip.
    expect(summary?.querySelector('button')).toBeNull();
    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    expect(container.querySelector('.chat-tool')).toBeNull();
  });

  it('renders one Q→A pair per outcome result, in dialog order', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_mq_done',
      dialog_kind: 'question',
      prompt: '2 questions',
      answerable: true,
      outcome: {
        results: [
          { question: 'Which approach?', chosen: ['Patch forward'] },
          { question: 'Which areas?', chosen: ['Frontend', 'Backend'] },
        ],
      },
    });
    await mountChat();

    const pairs = Array.from(container.querySelectorAll('.dialog-qa'));
    expect(pairs).toHaveLength(2);
    expect(pairs[0]?.querySelector('.dialog-qa-question')?.textContent).toBe('Which approach?');
    expect(pairs[0]?.querySelector('.dialog-qa-chosen')?.textContent).toBe('Patch forward');
    expect(pairs[1]?.querySelector('.dialog-qa-question')?.textContent).toBe('Which areas?');
    // Multi-select labels join with ", " in recorded toggle order.
    expect(pairs[1]?.querySelector('.dialog-qa-chosen')?.textContent).toBe('Frontend, Backend');
  });

  it('renders an other-text answer as the operator’s quoted words, distinct from labels', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_other',
      dialog_kind: 'question',
      prompt: 'Which fix?',
      answerable: true,
      outcome: {
        // A multi-select result can carry BOTH chosen labels and typed text.
        results: [
          { question: 'Which fix?', chosen: ['Patch forward'], other_text: 'and add a test' },
        ],
      },
    });
    await mountChat();

    const answer = container.querySelector('.dialog-qa-answer');
    expect(answer?.querySelector('.dialog-qa-chosen')?.textContent).toBe('Patch forward');
    // The typed text renders quoted — a different text node than a label.
    expect(answer?.querySelector('.dialog-qa-other')?.textContent).toBe('“and add a test”');
  });

  it('marks a result with neither chosen nor other_text as unanswered', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_blank',
      dialog_kind: 'question',
      prompt: 'Which fix?',
      answerable: true,
      outcome: { results: [{ question: 'Which fix?' }] },
    });
    await mountChat();

    expect(container.querySelector('.dialog-qa-question')?.textContent).toBe('Which fix?');
    expect(container.querySelector('.dialog-qa-none')?.textContent).toBe('No answer recorded');
  });

  it('renders a dismissed dialog as the question text plus one dismissed marker', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_dismissed',
      dialog_kind: 'question',
      prompt: 'Which fix?',
      answerable: true,
      options: [{ label: 'Revert' }],
      outcome: { dismissed: true, results: [{ question: 'Which fix?' }] },
    });
    await mountChat();

    const summary = container.querySelector('.chat-dialog-answered')!;
    expect(summary.querySelector('.dialog-qa-question')?.textContent).toBe('Which fix?');
    const markers = summary.querySelectorAll('.chat-dialog-outcome');
    expect(markers).toHaveLength(1);
    expect(markers[0]?.textContent).toBe('Dismissed');
    expect(summary.querySelector('button')).toBeNull();
    expect(container.querySelector('.chat-dialog-card')).toBeNull();
  });

  it('renders an approved plan as the full markdown plus the approved marker, inert', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_plan_ok',
      dialog_kind: 'plan',
      prompt: '# The plan\n\nDo **bold** things',
      answerable: true,
      options: [{ label: 'Approve' }, { label: 'Keep planning' }],
      outcome: { approved: true },
    });
    await mountChat();

    const summary = container.querySelector('.chat-dialog-answered')!;
    // The FULL plan, rendered markdown (same .chat-dialog-plan face as live).
    expect(summary.querySelector('.chat-dialog-plan .md-h')?.textContent).toBe('The plan');
    expect(summary.querySelector('.chat-dialog-plan strong')?.textContent).toBe('bold');
    expect(summary.querySelector('.chat-dialog-outcome')?.textContent).toBe('Plan approved');
    // No approve/reject buttons — this is history.
    expect(summary.querySelector('button')).toBeNull();
    expect(buttonByText('Approve')).toBeNull();
    expect(container.querySelector('.chat-dialog-card')).toBeNull();
  });

  it('renders a rejected plan with the typed feedback as an operator quote', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_plan_no',
      dialog_kind: 'plan',
      prompt: '# The plan\n\nSteps',
      answerable: true,
      outcome: { feedback: 'tighten the tests first' },
    });
    await mountChat();

    const summary = container.querySelector('.chat-dialog-answered')!;
    expect(summary.querySelector('.chat-dialog-outcome')?.textContent).toBe('Plan rejected');
    expect(summary.querySelector('.dialog-qa-other')?.textContent).toBe(
      '“tighten the tests first”',
    );
  });

  it('treats an EMPTY outcome object as answered (rejection without feedback)', async () => {
    // The critical wire semantics: all outcome fields are omitempty, so a plan
    // rejected without typed feedback arrives as {} — still answered.
    withAnsweredDialog({
      tool_id: 'toolu_plan_bare',
      dialog_kind: 'plan',
      prompt: '# The plan\n\nSteps',
      answerable: true,
      outcome: {},
    });
    await mountChat();

    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    expect(container.querySelector('.chat-dialog-answered .chat-dialog-outcome')?.textContent).toBe(
      'Plan rejected',
    );
    expect(container.querySelector('.chat-dialog-answered button')).toBeNull();
  });

  it('renders a plan dismissal marker', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_plan_gone',
      dialog_kind: 'plan',
      prompt: '# The plan\n\nSteps',
      answerable: true,
      outcome: { dismissed: true },
    });
    await mountChat();

    expect(container.querySelector('.chat-dialog-answered .chat-dialog-outcome')?.textContent).toBe(
      'Plan dismissed',
    );
  });

  // --- House-style option cards (issue #56 decision 7) ---

  function optionCard(label: string): HTMLButtonElement {
    const btn = Array.from(
      container.querySelectorAll<HTMLButtonElement>('button.dialog-option'),
    ).find((b) => b.querySelector('.dialog-option-label')?.textContent === label);
    if (!btn) throw new Error(`missing option card "${label}"`);
    return btn;
  }

  /** The REAL wire shape: the adapter appends the free-text Other row
   *  (is_other) to EVERY question, multi-select included (chat_dialog.go). */
  const flatMultiDialog = () => ({
    tool_id: 'toolu_flat_multi',
    dialog_kind: 'question' as const,
    prompt: 'Which areas?',
    answerable: true,
    multi: true,
    options: [
      { label: 'Frontend', description: 'The SPA under web/' },
      { label: 'Backend', description: 'The Go API' },
      { label: 'Other', is_other: true },
    ],
  });

  it('renders flat multi-select options as toggle cards with visible descriptions', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'pick some' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatMultiDialog(),
    };
    await mountChat();

    // Toggle-card buttons, not checkboxes — and the descriptions show now.
    expect(container.querySelector('.dialog-check')).toBeNull();
    expect(container.querySelector('input[type="checkbox"]')).toBeNull();
    expect(optionCard('Frontend').querySelector('.dialog-option-desc')?.textContent).toBe(
      'The SPA under web/',
    );
    // Card, not pill: the seg class is gone from dialog options.
    expect(optionCard('Frontend').classList.contains('seg')).toBe(false);
    // Completeness gating: nothing selected yet → Submit disabled.
    expect(buttonByText('Submit')!.disabled).toBe(true);

    // Toggling carries the selected state on the card itself.
    expect(optionCard('Frontend').getAttribute('aria-pressed')).toBe('false');
    optionCard('Frontend').click();
    await settle();
    expect(optionCard('Frontend').getAttribute('aria-pressed')).toBe('true');
    expect(optionCard('Frontend').classList.contains('selected')).toBe(true);
    expect(optionCard('Frontend').querySelector('.dialog-option-check')).not.toBeNull();

    // Toggling OFF works too, then re-select both for the submit.
    optionCard('Frontend').click();
    await settle();
    expect(optionCard('Frontend').getAttribute('aria-pressed')).toBe('false');
    expect(optionCard('Frontend').querySelector('.dialog-option-check')).toBeNull();
    optionCard('Frontend').click();
    optionCard('Backend').click();
    await settle();

    // The Submit flow and payload are byte-for-byte the pre-card contract
    // (an untouched Other row adds nothing to the wire).
    buttonByText('Submit')!.click();
    await settle();
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toEqual({ tool_id: 'toolu_flat_multi', selected: [0, 1] });
  });

  it('flat multi-select Other: toggling opens the input, gates Submit on text, rides other_text', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'pick some' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatMultiDialog(),
    };
    await mountChat();

    // No stray input while Other is untoggled.
    expect(container.querySelector('.dialog-other-input')).toBeNull();

    optionCard('Backend').click();
    optionCard('Other').click();
    await settle();
    // Other toggled but empty → Submit stays disabled even with a real pick.
    expect(optionCard('Other').getAttribute('aria-pressed')).toBe('true');
    expect(buttonByText('Submit')!.disabled).toBe(true);

    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;
    expect(other).not.toBeNull();
    other.value = 'the CI pipeline';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    buttonByText('Submit')!.click();
    await settle();
    // The Other row's INDEX (2) never enters selected — its text IS its
    // toggle (the adapter pastes it onto the TUI's "Type something" row,
    // which fills AND checks it — compat §7, live 2026-07-09).
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toEqual({
      tool_id: 'toolu_flat_multi',
      selected: [1],
      other_text: 'the CI pipeline',
    });
  });

  it('flat multi-select accepts an Other-only answer (nothing else toggled)', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'pick some' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatMultiDialog(),
    };
    await mountChat();

    optionCard('Other').click();
    await settle();
    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'docs only';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    buttonByText('Submit')!.click();
    await settle();
    expect(answerPosts[0]).toEqual({
      tool_id: 'toolu_flat_multi',
      selected: [],
      other_text: 'docs only',
    });
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

  // --- No New conversation control anywhere (removed, issue #68) ---

  it('has no New conversation button or menu item; the clear command still autocompletes', async () => {
    await mountChat(); // default: active run, catalog has a role=clear command

    expect(buttonByText('New conversation')).toBeNull();
    moreButton()!.click();
    await settle();
    expect(menuItem('New conversation')).toBeUndefined();
    expect(buttonByText('Confirm clear')).toBeNull();

    // Composer autocomplete for /clear is untouched — only the dedicated
    // button + two-step confirm are gone.
    setComposerText('/');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toContain(
      '/clear',
    );
  });

  // --- Spawn-time model chip (issue #68) ---

  it('renders the desktop model chip with raw ids when the catalog has no match', async () => {
    await mountChat(); // default mocks: providersOnServer[0].models/efforts are both []

    expect(container.querySelector('.chat-model-chip')?.textContent).toBe('opus[1m] · max');
  });

  it('renders the desktop model chip with catalog pretty labels when they match', async () => {
    providersOnServer[0]!.models = [{ value: 'opus[1m]', label: 'Opus 4.6 [1m]' }];
    providersOnServer[0]!.efforts = [{ value: 'max', label: 'Max' }];
    await mountChat();

    expect(container.querySelector('.chat-model-chip')?.textContent).toBe('Opus 4.6 [1m] · Max');
  });

  it('hides the model chip and the menu info row entirely for a legacy run with no model', async () => {
    runOnServer = { ...baseRun(), model: '' };
    await mountChat();

    expect(container.querySelector('.chat-model-chip')).toBeNull();
    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-info')).toBeNull();
  });

  it('renders the model alone, with no separator, when effort is empty', async () => {
    runOnServer = { ...baseRun(), effort: '' };
    await mountChat();

    expect(container.querySelector('.chat-model-chip')?.textContent).toBe('opus[1m]');
  });
});
