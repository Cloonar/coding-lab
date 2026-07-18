// RunChat behavioral contract (issue #7):
// - the stream renders user/assistant text, tool chips, and lifecycle/errors;
//   thinking is permanently dropped at paint — in the stream and in the tool
//   panel's list — with no toggle to reveal it (issue #68);
// - tool chips and group summaries are buttons whose click branches on the
//   1024px breakpoint (issue #154): on desktop they toggle a RICH inline
//   expansion in place (a lone chip its ToolViewBody, a group its member chips,
//   each independently expandable); on mobile they open the tool detail sheet
//   (issue #145) — a lone chip straight at its detail, a group at its list. On
//   desktop a FILE chip (diff/write/read) also carries an "open in sidebar"
//   affordance opening the flush-right, file-detail-only sidebar (§2);
//   command/fallback chips and group summary rows never get one, and mobile
//   gets none anywhere. Both the panel selection (seq-keyed, resolving live
//   across SSE refetches — decision 12; swaps in place on a retarget, closes via
//   ✕/Esc) and the desktop expansions (seq-keyed too) survive refetches and
//   clear on stream resets; the sheet flips to the in-flow sidebar when the
//   viewport crosses to >=1024px, and a non-file (group / command) selection
//   clears on that crossing since the desktop sidebar shows files only;
// - a run.messages.changed for THIS run coalesces on a 300ms trailing debounce
//   and then refetches LIGHT (issue #175): tail pages only, from
//   after=min(cursor, backpatchSeq-1), no latest-window request; other runs are
//   ignored; run.changed for other repos — or carrying a SIBLING runID — is
//   ignored too;
// - a refetch tails with after=<cursor> (paginating past the window limit) so
//   appends accumulate gap-free, and a stale in-flight response never applies
//   over a newer one (request-token guard); unchanged content (equal
//   content_hash) keeps message/DOM identity across refetches;
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
import type {
  ChatMessage,
  Dialog,
  MessagesResponse,
  Provider,
  Repo,
  Run,
  RunCommand,
} from '../api';
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
    title: null,
    model: 'opus[1m]',
    effort: 'max',
    remote: true,
    deep_link_url: 'https://claude.ai/code/session_1',
    started_at: '2026-07-06T15:00:00.000Z',
    budget_deadline: null,
    ended_at: null,
    outcome: 'active',
    failure_reason: null,
  };
}

// The run's repo (issue #132) — the header fetches getRepo(repo_id) for the
// forge git-icon link. A forgejo remote whose forgeWebUrl parses to a hosted
// web page; tests override forge_kind to exercise the hidden-icon path.
function baseRepo(): Repo {
  return {
    id: 'repo_1',
    name: 'proj',
    remote_url: 'https://git.cloonar.com/Cloonar/proj.git',
    credential_id: null,
    forge_credential_id: null,
    tracker_binding: 'forge',
    forge_kind: 'forgejo',
    default_branch: 'main',
    provider: null,
    incogni: false,
    model_default: null,
    effort_default: null,
    remote_default: null,
    afk_provider_default: null,
    afk_model_default: null,
    afk_effort_default: null,
    afk_remote_default: null,
    afk_options: null,
    afk_prompt: null,
    afk_prompt_effective: '',
    git_author_name: null,
    git_author_email: null,
    afk_branch_pattern: '',
    manual_branch_prefix: '',
    afk_auto_enabled: false,
    consecutive_failures: 0,
    budget_minutes: null,
    max_instances_override: null,
    clone_status: 'ready',
    clone_error: null,
    created_at: '2026-07-01T00:00:00.000Z',
    last_opened_at: null,
  };
}

let runOnServer: Run;
let repoOnServer: Repo;
let messagesOnServer: MessagesResponse;
let providersOnServer: Provider[];
let commandsOnServer: RunCommand[];
let replyPosts: { text: string }[];
// Reply POST status — 204 (success) by default. Kept as a knob so a reply-path
// test can flip it to a 4xx without re-plumbing the fetch stub.
let replyStatus: number;
// The 200 status's {notice} body (issue #149) — null leaves replyStatus:200
// untouched by any test that doesn't set it.
let replyNotice: string | null;
let answerPosts: Record<string, unknown>[];
let interruptPosts: number;
// Inline rename PATCHes (issue #111); the stub applies them to runOnServer so
// the onChanged refetch sees the new title like the real server round-trip.
let titlePatches: { title: string | null }[];
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
      // The header fetches the run's repo for the forge git-icon link (#132).
      if (url === `/api/v1/repos/${runOnServer.repo_id}` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...repoOnServer }));
      }
      if (url === `/api/v1/runs/${RUN_ID}` && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as { title: string | null };
        titlePatches.push(patch);
        runOnServer = { ...runOnServer, title: patch.title };
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
        if (replyStatus >= 400) {
          return Promise.resolve(
            jsonResponse(replyStatus, { error: 'run is not accepting replies' }),
          );
        }
        if (replyStatus === 200) {
          return Promise.resolve(jsonResponse(200, { notice: replyNotice }));
        }
        return Promise.resolve(jsonResponse(replyStatus, ''));
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

// The tool panel's desktop breakpoint (issue #145) — must byte-match the
// query RunChat builds from its DESKTOP_MIN_PX (the shell rail width).
const DESKTOP_QUERY = '(min-width: 1024px)';

// jsdom has no window.matchMedia — install a fake resolving the queries the
// app probes: isComposerSend's fine-pointer check (ADR-0031, issue #70) and
// the tool panel's desktop breakpoint (issue #145). Each query gets ONE
// persistent MediaQueryList whose `matches` flips via set() — dispatching
// 'change' to whatever listeners the component registered — so a test can
// cross the breakpoint live. Everything starts false (mobile, no fine
// pointer; RunChat's `(prefers-reduced-motion: reduce)` probe reads false
// too), keeping the untouched tests valid. vi.stubGlobal ties the mock's
// lifetime to vi.unstubAllGlobals() in afterEach; the memo below is cleared
// there alongside it.
let mediaStub: { set: (query: string, matches: boolean) => void } | undefined;

function stubMatchMedia(): { set: (query: string, matches: boolean) => void } {
  if (mediaStub !== undefined) return mediaStub;
  type Entry = { mql: { matches: boolean } & Record<string, unknown>; listeners: Set<() => void> };
  const entries = new Map<string, Entry>();
  const entry = (query: string): Entry => {
    let e = entries.get(query);
    if (e === undefined) {
      const listeners = new Set<() => void>();
      e = {
        listeners,
        mql: {
          matches: false,
          media: query,
          addEventListener: (type: string, listener: () => void) => {
            if (type === 'change') listeners.add(listener);
          },
          removeEventListener: (_type: string, listener: () => void) => {
            listeners.delete(listener);
          },
          addListener: () => {},
          removeListener: () => {},
          onchange: null,
          dispatchEvent: () => false,
        },
      };
      entries.set(query, e);
    }
    return e;
  };
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => entry(query).mql),
  );
  mediaStub = {
    set: (query, matches) => {
      const e = entry(query);
      e.mql.matches = matches;
      for (const listener of e.listeners) listener();
    },
  };
  return mediaStub;
}

/** The fine-pointer profile isComposerSend checks (ADR-0031, issue #70). */
function finePointer(matches: boolean): void {
  stubMatchMedia().set('(hover: hover) and (pointer: fine)', matches);
}

/** issue #175: the server-computed content_hash mimic — any author-controlled
 *  string works, as long as equal rendered content derives an equal hash and
 *  any mutation (status flip, output growth) derives a different one. Tests
 *  that mutate a message in place re-derive via `hashed` so the hash flips. */
function hashed(m: ChatMessage): ChatMessage {
  return {
    ...m,
    content_hash: `h:${m.seq}:${m.text ?? ''}:${m.tool?.status ?? ''}:${m.tool?.output ?? ''}`,
  };
}

/** Set the server's messages to a single assistant text message. */
function withAssistantText(text: string): void {
  messagesOnServer = {
    messages: [hashed({ seq: 1, kind: 'text', role: 'assistant', text })],
    state: 'needs_input',
    cursor: 1,
    has_more: false,
    transcript: 'available',
  };
}

function emitMessagesChanged(
  runID: string = RUN_ID,
  extra: { state?: string; backpatchSeq?: number } = {},
): void {
  FakeEventSource.instances[0]?.emit('run.messages.changed', {
    type: 'run.messages.changed',
    repoID: 'repo_1',
    runID,
    ...extra,
  });
}

// Mirror of RunChat's MESSAGES_DEBOUNCE_MS (issue #175): run.messages.changed
// coalesces on this trailing edge before its light refetch fires.
const MESSAGES_DEBOUNCE_MS = 300;

/**
 * Emit run.messages.changed and ride out the trailing debounce (issue #175):
 * fake timers around the emit make the 300ms advance instant (the refetch's
 * promise chain flushes inside advanceTimersByTimeAsync — the settle()-style
 * setTimeout(0) hops advance under fake timers too), then a real-timer settle
 * drains whatever the refetch scheduled after the flush.
 */
async function emitMessagesChangedSettled(
  runID: string = RUN_ID,
  extra: { state?: string; backpatchSeq?: number } = {},
): Promise<void> {
  vi.useFakeTimers();
  emitMessagesChanged(runID, extra);
  await vi.advanceTimersByTimeAsync(MESSAGES_DEBOUNCE_MS + 100);
  vi.useRealTimers();
  await settle();
}

beforeEach(() => {
  runOnServer = baseRun();
  repoOnServer = baseRepo();
  providersOnServer = [
    {
      id: 'claude-code',
      display_name: 'Claude Code',
      supports_remote: true,
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
    ].map((m) => hashed(m as ChatMessage)),
    state: 'needs_input',
    cursor: 4,
    has_more: false,
    transcript: 'available',
  };
  replyPosts = [];
  replyStatus = 204;
  replyNotice = null;
  answerPosts = [];
  interruptPosts = 0;
  titlePatches = [];
  stubApi();
});

afterEach(() => {
  vi.useRealTimers(); // a debounce test that failed mid-fake must not leak fake timers
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
  mediaStub = undefined; // the stub it memoized is gone with unstubAllGlobals
  Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
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

  it('shows a 200 reply notice as an informational banner, never the error banner (issue #149)', async () => {
    replyStatus = 200;
    replyNotice = 'already up to date with origin/main';
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ping';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    buttonByLabel('Send')!.click();
    await settle();

    const notice = container.querySelector('.banner.notice');
    expect(notice?.textContent).toContain('already up to date with origin/main');
    expect(notice?.getAttribute('role')).toBe('status');
    expect(container.querySelector('.banner.error')).toBeNull();
    // The composer still clears on a 200, same as a 204.
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
  });

  it('keeps a reply error on the error banner, never the notice banner', async () => {
    replyStatus = 409;
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ping';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    buttonByLabel('Send')!.click();
    await settle();

    expect(container.querySelector('.banner.error')?.textContent).toContain(
      'run is not accepting replies',
    );
    expect(container.querySelector('.banner.notice')).toBeNull();
  });

  it('bare Enter sends on fine-pointer; Shift+Enter stays a newline; Cmd/Ctrl+Enter always sends', async () => {
    finePointer(true);
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ship it';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    // Bare Enter sends on a fine-pointer (mouse/trackpad) setup and clears the box.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('ship it');
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');

    // Shift+Enter never sends — the browser-default newline is left alone
    // (the handler must not preventDefault it).
    input.value = 'more text';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    const shiftEnter = new KeyboardEvent('keydown', {
      key: 'Enter',
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    input.dispatchEvent(shiftEnter);
    await settle();
    expect(replyPosts).toHaveLength(1); // unchanged
    expect(shiftEnter.defaultPrevented).toBe(false);

    // Cmd/Ctrl+Enter still sends and clears.
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(replyPosts).toHaveLength(2);
    expect(replyPosts[1]?.text).toBe('more text');
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
  });

  it('bare Enter never sends without a fine pointer (no matchMedia, or a touch profile)', async () => {
    // Default jsdom: no window.matchMedia at all — reads as "not fine-pointer".
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'no matchMedia here';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(replyPosts).toHaveLength(0);

    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('no matchMedia here');
  });

  it('bare Enter never sends on a touch profile (matchMedia present but not fine-pointer)', async () => {
    finePointer(false);
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'tap city';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(replyPosts).toHaveLength(0);

    // Cmd/Ctrl+Enter sends regardless of pointer type.
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(replyPosts).toHaveLength(1);
    expect(replyPosts[0]?.text).toBe('tap city');
  });

  it('ignores Enter fired mid-IME-composition even on a fine-pointer setup', async () => {
    finePointer(true);
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'still composing';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    input.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'Enter',
        isComposing: true,
        bubbles: true,
        cancelable: true,
      }),
    );
    await settle();
    expect(replyPosts).toHaveLength(0);
  });

  it('does not send bare Enter on an empty box, and preventDefaults it (no stray newline)', async () => {
    finePointer(true);
    await mountChat(); // default needs_input, empty box
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    const evt = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    input.dispatchEvent(evt);
    await settle();
    expect(replyPosts).toHaveLength(0);
    expect(evt.defaultPrevented).toBe(true);
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
    await emitMessagesChangedSettled();

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

    // Bare Enter on a fine-pointer setup sends too, mid-turn (issue #70): the
    // always-send contract extends bare Enter the same way it already covers
    // Cmd/Ctrl+Enter.
    finePointer(true);
    input.value = 'another mid-turn thought';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(replyPosts).toHaveLength(2);
    expect(replyPosts[1]?.text).toBe('another mid-turn thought');
  });

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

  // --- Debounced light refetch (issue #175) ---
  // While an agent streams, run.messages.changed fires ~1/s; the chat now
  // coalesces a burst on a 300ms trailing edge and refetches LIGHT: tail pages
  // only, from after=min(cursor, backpatchSeq-1), never the latest window —
  // and unchanged content (equal content_hash) keeps message AND DOM identity.

  /** This run's /messages request URLs, in call order. */
  function messagesUrls(): string[] {
    return (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls
      .map((c) => String(c[0]))
      .filter((u) => u.includes(`/runs/${RUN_ID}/messages`));
  }

  it('coalesces a burst into ONE tail request and never fetches a latest window (issue #175)', async () => {
    await mountChat(); // seq 1..4 accumulated, cursor 4
    const before = messagesUrls().length;

    // The server gains an append mid-burst.
    messagesOnServer = {
      ...messagesOnServer,
      messages: [
        ...messagesOnServer.messages,
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
    messagesOnServer = {
      ...messagesOnServer,
      messages: messagesOnServer.messages.map((m) =>
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

  it('collapses the composer to a bare waiting note, with no composer Interrupt, while the card is pending', async () => {
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

    // The composer: one-line waiting note pointing up at the card — no
    // textarea, no Send (decision 2).
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    expect(container.querySelector('.chat-input')).toBeNull();
    expect(buttonByLabel('Send')).toBeNull();
    expect(container.querySelector('.chat-composer .chat-composer-note')?.textContent).toBe(
      'Claude Code is waiting on your answer — see the question above.',
    );
    // No composer Interrupt in this branch anymore (issue #165 item 3): an
    // accent square in Send's slot, right next to the live interactive card
    // above, drew muscle-memory "send" taps that declined the focused picker.
    // Neither the composer nor the card carries a `.chat-interrupt`.
    expect(container.querySelectorAll('.chat-interrupt')).toHaveLength(0);
    // The escape hatch survives elsewhere: the sticky header's turn Interrupt
    // (class `chat-turn-interrupt`) is gated on `live()`, which is true while
    // a dialog pends on this live run.
    expect(container.querySelector('.chat-turn-interrupt')).not.toBeNull();
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
    await emitMessagesChangedSettled();

    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    expect(container.querySelector('.chat-composer-row')).not.toBeNull();
    expect(container.querySelector('.chat-input')).not.toBeNull();
  });

  // --- Enter-to-submit on dialog "Other" free-text inputs (issue #165) ---
  // Enter must be exactly equivalent to clicking the adjacent Send/Submit
  // button: same disabled guard, same payload, never a shortcut around it.

  const singleSelectOtherDialog = (toolID = 'toolu_solo_other') => ({
    tool_id: toolID,
    dialog_kind: 'question' as const,
    prompt: 'Which fix?',
    answerable: true,
    options: [{ label: 'Revert' }, { label: 'Other', is_other: true }],
  });

  it('single-select Other: Enter submits exactly like clicking Send', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectOtherDialog(),
    };
    await mountChat();

    const other = container.querySelector('.dialog-other input') as HTMLInputElement;
    other.value = 'roll it back manually';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Send')!.disabled).toBe(false);

    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toEqual({
      tool_id: 'toolu_solo_other',
      index: 1,
      other_text: 'roll it back manually',
    });
  });

  it('single-select Other: Enter no-ops on empty or whitespace-only text, matching disabled Send', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectOtherDialog(),
    };
    await mountChat();

    const other = container.querySelector('.dialog-other input') as HTMLInputElement;
    expect(buttonByText('Send')!.disabled).toBe(true);
    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(answerPosts).toHaveLength(0);

    other.value = '   ';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Send')!.disabled).toBe(true);
    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(answerPosts).toHaveLength(0);
  });

  it('single-select Other: ignores Enter fired mid-IME-composition even with valid text', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectOtherDialog(),
    };
    await mountChat();

    const other = container.querySelector('.dialog-other input') as HTMLInputElement;
    other.value = 'still composing';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Send')!.disabled).toBe(false);

    other.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'Enter',
        isComposing: true,
        bubbles: true,
        cancelable: true,
      }),
    );
    await settle();
    expect(answerPosts).toHaveLength(0);
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
      await emitMessagesChangedSettled();

      const card = container.querySelector('.chat-dialog-card');
      expect(card).not.toBeNull();
      expect(scrolls.calls).toHaveLength(1);
      expect(scrolls.calls[0]?.target).toBe(card);
      expect(scrolls.calls[0]?.arg).toEqual({ block: 'start' });

      // The SAME dialog on the next tick is not new — no re-yank.
      await emitMessagesChangedSettled();
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
      await emitMessagesChangedSettled();

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
          return Promise.resolve(jsonResponse(200, { providers: providersOnServer }));
        if (url === `/api/v1/runs/${RUN_ID}` && method === 'GET')
          return Promise.resolve(jsonResponse(200, { ...runOnServer }));
        if (url === `/api/v1/repos/${runOnServer.repo_id}` && method === 'GET')
          return Promise.resolve(jsonResponse(200, { ...repoOnServer }));
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
    await emitMessagesChangedSettled();

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
    await emitMessagesChangedSettled();

    expect(container.textContent).toContain('Second question?');
    expect(buttonByText('Submit')!.disabled).toBe(true); // stale picks dropped
  });

  it('keeps in-progress picks, Other text and the input element across a refetch of the SAME dialog', async () => {
    // Every response is a fresh JSON parse — the same pending dialog arrives
    // as a new object each refetch. Neither the operator's picks, nor the
    // half-typed Other text, nor the input ELEMENT (focus!) may churn on an
    // SSE tick that changed nothing.
    messagesOnServer = {
      messages: [],
      state: 'question',
      cursor: 0,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_same',
        dialog_kind: 'question',
        prompt: 'Pick or type?',
        answerable: true,
        multi: true,
        options: [{ label: 'One' }, { label: 'Two' }, { label: 'Other', is_other: true }],
      },
    };
    await mountChat();

    buttonByText('One')!.click();
    buttonByText('Other')!.click();
    await settle();
    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'half-typed answer';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    await emitMessagesChangedSettled();

    const after = container.querySelector('.dialog-other-input') as HTMLInputElement;
    expect(after).toBe(other); // same element — focus survives
    expect(after.value).toBe('half-typed answer');
    expect(buttonByText('One')!.getAttribute('aria-pressed')).toBe('true');
    expect(buttonByText('Submit')!.disabled).toBe(false);
  });

  it('keeps multi-question form answers across a refetch of the SAME dialog', async () => {
    messagesOnServer = {
      messages: [],
      state: 'question',
      cursor: 0,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_form',
        dialog_kind: 'question',
        prompt: '2 questions',
        answerable: true,
        questions: [
          {
            text: 'Pick a flavor?',
            header: 'Flavor',
            options: [{ label: 'Sweet' }, { label: 'Sour' }, { label: 'Other', is_other: true }],
          },
          {
            text: 'Pick a size?',
            header: 'Size',
            options: [{ label: 'Small' }, { label: 'Large' }],
          },
        ],
      },
    };
    await mountChat();

    buttonByText('Other')!.click();
    await settle();
    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'umami';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    buttonByText('Large')!.click();
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    await emitMessagesChangedSettled();

    const after = container.querySelector('.dialog-other-input') as HTMLInputElement;
    expect(after).toBe(other);
    expect(after.value).toBe('umami');
    expect(buttonByText('Large')!.getAttribute('aria-pressed')).toBe('true');
    expect(buttonByText('Submit')!.disabled).toBe(false);
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

  it('titles the chat with the generated label and falls back to the provider web link', async () => {
    runOnServer = { ...baseRun(), deep_link_url: null };
    await mountChat();

    expect(container.querySelector('.chat-title-text')?.textContent).toBe('dom · 15:00');
    expect(container.querySelector('.chat-title-project')?.textContent).toBe('proj');
    // ADR-0017: the fallback URL + tooltip come from the providers API, not a
    // hardcoded constant. Scope to the OpenAffordance link (a.card-link) so the
    // header's forge git-icon link (issue #132) isn't mistaken for it.
    const link = container.querySelector<HTMLAnchorElement>('a.card-link');
    expect(link?.getAttribute('href')).toBe('https://claude.ai/code');
    expect(link?.getAttribute('title')).toContain('claude.ai session picker');
  });

  // --- Inline rename (issue #111) ---

  it('shows a set title verbatim with the project name as secondary text + session tooltip', async () => {
    runOnServer = { ...baseRun(), title: 'Fix the flaky login test' };
    await mountChat();

    const btn = container.querySelector('button.chat-title')!;
    expect(btn.querySelector('.chat-title-text')?.textContent).toBe('Fix the flaky login test');
    // The project name (not the old repeated label · session string) rides
    // beside the title — a SIBLING of the button now (issue #132), not inside
    // it; the full session name — the branch/worktree/tmux correlation — is the
    // button's tooltip.
    expect(btn.querySelector('.chat-title-project')).toBeNull();
    expect(container.querySelector('.chat-title-project')?.textContent).toBe('proj');
    expect(btn.getAttribute('title')).toBe('proj~dom-20260706-1500');
  });

  it('renames inline: click the title, submit → PATCH {title}, refetch shows the new name', async () => {
    await mountChat();
    // No title set: the project name still rides the generated title (issue #120).
    expect(container.querySelector('.chat-title-project')?.textContent).toBe('proj');

    (container.querySelector('button.chat-title') as HTMLButtonElement).click();
    await settle();
    const input = container.querySelector('.chat-title-input') as HTMLInputElement;
    expect(input).not.toBeNull();
    expect(input.value).toBe(''); // seeded with run.title ?? ''
    expect(input.placeholder).toBe('dom · 15:00'); // the generated title, repo-less

    input.value = '  Ship it  ';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    buttonByLabel('Save title')!.click();
    await settle();

    expect(titlePatches).toEqual([{ title: 'Ship it' }]); // trimmed
    // Back in view mode, the refetched run's title renders.
    expect(container.querySelector('.chat-title-input')).toBeNull();
    expect(container.querySelector('.chat-title-text')?.textContent).toBe('Ship it');
  });

  it('clears the override on empty submit, and Escape/Cancel exit without saving', async () => {
    runOnServer = { ...baseRun(), title: 'Old name' };
    await mountChat();

    // Escape exits edit mode without a PATCH.
    (container.querySelector('button.chat-title') as HTMLButtonElement).click();
    await settle();
    const input = container.querySelector('.chat-title-input') as HTMLInputElement;
    expect(input.value).toBe('Old name');
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await settle();
    expect(container.querySelector('.chat-title-input')).toBeNull();
    expect(titlePatches).toHaveLength(0);

    // Cancel exits without saving too.
    (container.querySelector('button.chat-title') as HTMLButtonElement).click();
    await settle();
    buttonByLabel('Cancel rename')!.click();
    await settle();
    expect(container.querySelector('.chat-title-input')).toBeNull();
    expect(titlePatches).toHaveLength(0);

    // Saving empty clears (PATCH {title: null}) — that IS the reset path —
    // and the header falls back to the generated title.
    (container.querySelector('button.chat-title') as HTMLButtonElement).click();
    await settle();
    const again = container.querySelector('.chat-title-input') as HTMLInputElement;
    again.value = '   ';
    again.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    buttonByLabel('Save title')!.click();
    await settle();
    expect(titlePatches).toEqual([{ title: null }]);
    expect(container.querySelector('.chat-title-text')?.textContent).toBe('dom · 15:00');
    expect(container.querySelector('.chat-title-project')?.textContent).toBe('proj');
  });

  it('falls back to the branch with no project span for a legacy no-~ session name', async () => {
    runOnServer = { ...baseRun(), session_name: 'legacy-session', title: null };
    await mountChat();

    const btn = container.querySelector('button.chat-title')!;
    expect(btn.querySelector('.chat-title-text')?.textContent).toBe(runOnServer.branch);
    // No `~` in the session name → no project name at all (issue #132): neither
    // the muted text/link nor the forge icon renders.
    expect(container.querySelector('.chat-title-project')).toBeNull();
    expect(container.querySelector('.chat-title-forge')).toBeNull();
    expect(btn.getAttribute('title')).toBe('legacy-session');
  });

  // --- Repo links in the header (issue #132) ---

  it('renders the project name as a link to the repo issues page', async () => {
    await mountChat();

    const link = container.querySelector<HTMLAnchorElement>('a.chat-title-project');
    expect(link).not.toBeNull();
    expect(link!.textContent).toBe('proj');
    // The issues page is the de-facto repo landing (no /repos/:id route).
    expect(link!.getAttribute('href')).toBe('/repos/repo_1/issues');
  });

  it('renders the git-icon forge link (new tab) for a forgejo repo with a parseable remote', async () => {
    await mountChat();

    const forge = container.querySelector<HTMLAnchorElement>('a.chat-title-forge');
    expect(forge).not.toBeNull();
    // forgeWebUrl strips the .git suffix off the clone URL's path.
    expect(forge!.getAttribute('href')).toBe('https://git.cloonar.com/Cloonar/proj');
    expect(forge!.getAttribute('target')).toBe('_blank');
    expect(forge!.getAttribute('rel')).toBe('noreferrer');
    expect(forge!.getAttribute('aria-label')).toBe('Open on forge');
    // The git-branch glyph (two circles), a deliberate choice over the
    // external-link icon (three paths, no circles).
    expect(forge!.querySelectorAll('svg circle')).toHaveLength(2);
  });

  it('hides the git-icon forge link when the repo has no forge (forge_kind none)', async () => {
    repoOnServer = { ...baseRepo(), forge_kind: 'none' };
    await mountChat();

    // The forge icon is hidden entirely — not greyed — while the project name
    // link still renders (it depends on repo_id, not the forge URL).
    expect(container.querySelector('.chat-title-forge')).toBeNull();
    expect(container.querySelector('a.chat-title-project')?.getAttribute('href')).toBe(
      '/repos/repo_1/issues',
    );
  });

  it('shows a copyable tmux-attach for a link-less provider (no web fallback)', async () => {
    // A provider with no remote-control knob is ALWAYS remote:false (the server
    // clamps it) — its attach affordance must survive the remote gate untouched
    // (issue #163), which a bare `if (!run.remote)` check would have killed.
    runOnServer = { ...baseRun(), provider: 'codex', remote: false, deep_link_url: null };
    providersOnServer = [
      {
        id: 'codex',
        display_name: 'Codex CLI',
        supports_remote: false,
        auth: { kind: 'external' },
        models: [],
        efforts: [],
        options: [],
      },
    ];
    await mountChat();

    // No OpenAffordance web link (a.card-link) for a link-less provider; the
    // header's forge git-icon link (issue #132) is a separate anchor and does
    // not count here.
    expect(container.querySelector('a.card-link')).toBeNull();
    const attach = container.querySelector('button.attach-copy');
    expect(attach?.textContent).toContain('Copy attach');
    expect(attach?.getAttribute('title')).toContain('tmux attach -t proj~dom-20260706-1500');
  });

  it('hides the Open affordance entirely for a remote-capable run spawned without remote control', async () => {
    // Remote control off = no session was registered with the provider's web
    // app, so the deep link AND its fallback picker link would both point at
    // nothing (issue #163): render nothing at all, not even the connecting pulse.
    runOnServer = { ...baseRun(), remote: false, deep_link_url: null };
    await mountChat();

    expect(container.querySelector('a.card-link')).toBeNull();
    expect(container.querySelector('button.attach-copy')).toBeNull();
    expect(container.querySelector('.chip.connecting')).toBeNull();
    // The rest of the header is unaffected — this is not an error state.
    expect(container.querySelector('.chat-title-text')?.textContent).toBe('dom · 15:00');
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
    await emitMessagesChangedSettled();
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

  it('drops folded-in thinking from the group and its panel list, permanently (issue #68)', async () => {
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
    expect(container.textContent).not.toContain('secret group reasoning');

    // Opening the panel list (a user tap) still never reveals it — one row
    // per TOOL, and there is no toggle left to flip.
    (container.querySelector('button.tool-group-summary') as HTMLButtonElement).click();
    await settle();

    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(2);
    expect(container.textContent).not.toContain('secret group reasoning');
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

  it('keeps the open panel live across an SSE refetch (decision 12)', async () => {
    // The group is re-derived on every refetch; the panel selection is keyed
    // by the first tool's immutable seq, so an SSE tick must neither close an
    // open list nor reset a pushed detail — it only grows them.
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok', output: 'one' } },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'running' } },
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    (container.querySelector('button.tool-group-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(2);

    // An SSE tick appends another tool and re-derives the group: the panel
    // stays open and the list shows the new row.
    messagesOnServer = {
      ...messagesOnServer,
      messages: [
        ...messagesOnServer.messages,
        { seq: 3, kind: 'tool', tool: { name: 'Bash', title: 'c', status: 'running' } },
      ],
      cursor: 3,
    };
    await emitMessagesChangedSettled();
    expect(container.querySelector('.tool-panel')).not.toBeNull();
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(3);
    expect(container.querySelector('.tool-group-count')?.textContent).toBe('3 tool calls');

    // Push into a detail, then grow ITS output on the next tick: the shown
    // text grows in place (live resolution, never a captured message) and the
    // page does not reset back to the list.
    panelRow('a').click();
    await settle();
    expect(container.querySelector('.tool-panel .tool-body.tool-output')?.textContent).toBe('one');

    messagesOnServer = {
      ...messagesOnServer,
      messages: messagesOnServer.messages.map((m) =>
        m.seq === 1 ? { ...m, tool: { ...m.tool!, output: 'one\ntwo' } } : m,
      ),
    };
    // A back-patch below the cursor rides the event's backpatchSeq (issue
    // #175) so the light refetch reaches back to it.
    await emitMessagesChangedSettled(RUN_ID, { backpatchSeq: 1 });
    expect(container.querySelector('.tool-panel .tool-body.tool-output')?.textContent).toBe(
      'one\ntwo',
    );
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(0); // still on detail
    expect(buttonByLabel('Back to list')).not.toBeNull();
  });

  // --- Tool detail panel: mobile sheet (issue #145) ---
  // On mobile (the default all-false matchMedia stub, or none at all) a chip /
  // group click opens the modal bottom sheet; crossing to DESKTOP_QUERY flips an
  // already-open panel to the in-flow sidebar. A desktop click itself expands
  // inline instead (issue #154, covered below), never opening the panel.

  function panelRow(title: string): HTMLButtonElement {
    const row = Array.from(container.querySelectorAll<HTMLButtonElement>('.tool-panel-row')).find(
      (r) => r.querySelector('.tool-title')?.textContent === title,
    );
    if (!row) throw new Error(`missing panel row "${title}"`);
    return row;
  }

  /** A coalesced run (ls/read/grep — group key = seq 2) AND a separate lone
   *  chip after a text break: both panel entry points side by side. */
  function withToolRunAndLoneChip(): void {
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'text', role: 'assistant', text: 'on it' },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'ls', status: 'ok', output: 'a' } },
        { seq: 3, kind: 'text', role: 'assistant', thinking: true, text: 'hmm' },
        { seq: 4, kind: 'tool', tool: { name: 'Read', title: 'read', status: 'error' } },
        { seq: 5, kind: 'tool', tool: { name: 'Bash', title: 'grep', status: 'ok' } },
        { seq: 6, kind: 'text', role: 'assistant', text: 'between' },
        {
          seq: 7,
          kind: 'tool',
          tool: { name: 'Bash', title: 'lone', status: 'ok', output: 'solo out' },
        },
      ],
      state: 'needs_input',
      cursor: 7,
      has_more: false,
      transcript: 'available',
    };
  }

  it('opens the panel straight at detail for a lone chip tap (mobile: modal sheet)', async () => {
    await mountChat(); // default fixture: one lone tool, 'Ran ls', output 'a\nb'
    expect(container.querySelector('.tool-panel')).toBeNull();

    const chip = container.querySelector('button.tool-summary') as HTMLButtonElement;
    chip.click();
    await settle();

    // Mobile: a modal bottom sheet.
    const panel = container.querySelector('aside.tool-panel');
    expect(panel).not.toBeNull();
    expect(panel!.getAttribute('role')).toBe('dialog');
    expect(panel!.classList.contains('tool-panel-sheet')).toBe(true);
    // Straight at detail — the tool's output, and NO back affordance (a lone
    // chip has no list to return to).
    expect(panel!.querySelector('.tool-body.tool-output')?.textContent).toBe('a\nb');
    expect(buttonByLabel('Back to list')).toBeNull();
    // The chip wears the selected state…
    expect(chip.classList.contains('selected')).toBe(true);
    expect(chip.getAttribute('aria-pressed')).toBe('true');
    // …and nothing expanded inline (issue #154 is desktop-only): the stream
    // carries no I/O pane and no inline body.
    expect(container.querySelector('.chat-stream .tool-body')).toBeNull();
    expect(container.querySelector('.tool-inline-body')).toBeNull();
  });

  it('opens the panel at the list page for a group tap, one row per tool', async () => {
    withToolRunAndLoneChip();
    await mountChat();

    const group = container.querySelector('button.tool-group-summary') as HTMLButtonElement;
    group.click();
    await settle();

    // The list page: the count heading and one row per TOOL (folded-in
    // thinking is excluded).
    expect(container.querySelector('.tool-panel-title')?.textContent).toBe('3 tool calls');
    const rows = Array.from(container.querySelectorAll('.tool-panel-row .tool-title')).map(
      (el) => el.textContent,
    );
    expect(rows).toEqual(['ls', 'read', 'grep']);
    // The group line is marked as the panel's source.
    expect(group.classList.contains('selected')).toBe(true);
    expect(group.getAttribute('aria-pressed')).toBe('true');
    // Nothing expanded inline on mobile (issue #154 is desktop-only).
    expect(container.querySelector('.tool-group-body')).toBeNull();
  });

  it('pushes a list row to its detail and returns via back', async () => {
    withToolRunAndLoneChip();
    await mountChat();
    (container.querySelector('button.tool-group-summary') as HTMLButtonElement).click();
    await settle();

    panelRow('read').click();
    await settle();
    // Detail: the tool's title in the head, plus the back affordance (a group
    // target entered at its list).
    expect(container.querySelector('.tool-panel-title .tool-title')?.textContent).toBe('read');
    const back = buttonByLabel('Back to list');
    expect(back).not.toBeNull();

    back!.click();
    await settle();
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(3);
    expect(buttonByLabel('Back to list')).toBeNull();
  });

  it('swaps the panel in place when a different chip is tapped while open', async () => {
    withToolRunAndLoneChip();
    await mountChat();

    const group = container.querySelector('button.tool-group-summary') as HTMLButtonElement;
    group.click();
    await settle();
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(3);

    // Tap the separate lone chip: ONE panel swaps to its detail (no
    // close/reopen churn) and the selected highlight moves with it.
    const chip = container.querySelector('button.tool-summary') as HTMLButtonElement;
    chip.click();
    await settle();
    expect(container.querySelectorAll('.tool-panel')).toHaveLength(1);
    expect(container.querySelector('.tool-panel .tool-body.tool-output')?.textContent).toBe(
      'solo out',
    );
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(0);
    expect(chip.classList.contains('selected')).toBe(true);
    expect(group.classList.contains('selected')).toBe(false);
  });

  it('closes the panel and clears the selected highlight via ✕', async () => {
    await mountChat();
    const chip = container.querySelector('button.tool-summary') as HTMLButtonElement;
    chip.click();
    await settle();
    expect(container.querySelector('.tool-panel')).not.toBeNull();

    // The panel's own ✕ (the error banner's dismiss is labeled "Dismiss").
    container.querySelector<HTMLButtonElement>('.tool-panel button[aria-label="Close"]')!.click();
    await settle();
    expect(container.querySelector('.tool-panel')).toBeNull();
    expect(container.querySelector('.chat-tool.selected')).toBeNull();
    expect(chip.getAttribute('aria-pressed')).toBe('false');
  });

  it('resets the panel when the stream resets (transcript rotation)', async () => {
    messagesOnServer = { ...messagesOnServer, transcript_id: 'sess-A' };
    await mountChat();
    (container.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('.tool-panel')).not.toBeNull();

    // /clear rotates: the accumulated stream — and the seq-keyed panel
    // selection with it — drops before the fresh transcript merges in (the
    // same resetStream that clears on a route-param change).
    messagesOnServer = {
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

    expect(container.textContent).toContain('fresh start');
    expect(container.querySelector('.tool-panel')).toBeNull();
    expect(container.querySelector('.selected')).toBeNull();
  });

  it('keeps a FILE panel open across the 1024px breakpoint, switching containers', async () => {
    const media = stubMatchMedia(); // DESKTOP_QUERY reads false → mobile
    // A file tool: only a FILE selection survives the crossing to desktop (the
    // sidebar is file-only — a command/group selection clears, tested below).
    messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit foo.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'foo.ts', text: '@@ -1 +1 @@\n+x' },
          },
        },
      ],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();
    (container.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('aside.tool-panel')?.getAttribute('role')).toBe('dialog');
    expect(container.querySelector('.tool-scrim')).not.toBeNull();

    // Cross to desktop: shared state, pure render switch — the panel stays
    // open with the same file, now the in-flow complementary sidebar (no
    // dialog role, no scrim).
    media.set(DESKTOP_QUERY, true);
    await settle();
    const side = container.querySelector('aside.tool-panel');
    expect(side).not.toBeNull();
    expect(side!.getAttribute('role')).toBeNull();
    expect(side!.classList.contains('tool-panel-side')).toBe(true);
    expect(container.querySelector('.tool-scrim')).toBeNull();
    expect(side!.querySelector('.tool-view-path-text')?.textContent).toBe('foo.ts');
  });

  // (A desktop chip-BODY click no longer opens the panel — it expands inline
  // (below). The desktop panel is now reached by a file chip's "open in sidebar"
  // affordance instead, and its Escape-close is covered by the desktop file
  // sidebar tests further below (issue #154 §2).)

  // --- Desktop inline expansion (issue #154) ---
  // At >=1024px (DESKTOP_QUERY true) a chip / group click toggles a rich inline
  // body in place instead of opening the panel. The mobile sheet flow above is
  // pinned unchanged.

  it('expands a lone chip inline on desktop; a second click collapses it (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    await mountChat(); // default fixture: one lone tool, 'Ran ls', output 'a\nb'
    const chip = container.querySelector('button.tool-summary') as HTMLButtonElement;
    expect(chip.getAttribute('aria-expanded')).toBe('false');
    expect(container.querySelector('.tool-inline-body')).toBeNull();

    chip.click();
    await settle();
    // The rich body renders inline under the stream — no sidebar / sheet panel.
    const body = container.querySelector('.chat-stream .tool-inline-body');
    expect(body).not.toBeNull();
    expect(body!.querySelector('.tool-body.tool-output')?.textContent).toBe('a\nb');
    expect(container.querySelector('aside.tool-panel')).toBeNull();
    expect(container.querySelector('.tool-panel-side')).toBeNull();
    expect(chip.getAttribute('aria-expanded')).toBe('true');
    // Desktop expansion never touches the sheet-selection state.
    expect(chip.getAttribute('aria-pressed')).toBe('false');
    expect(container.querySelector('.chat-tool.selected')).toBeNull();

    chip.click();
    await settle();
    expect(container.querySelector('.tool-inline-body')).toBeNull();
    expect(chip.getAttribute('aria-expanded')).toBe('false');
  });

  it('expands a group inline on desktop, each member chip independently (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withToolRunAndLoneChip();
    await mountChat();
    const group = container.querySelector('button.tool-group-summary') as HTMLButtonElement;
    expect(group.getAttribute('aria-expanded')).toBe('false');
    expect(container.querySelector('.tool-group-body')).toBeNull();

    group.click();
    await settle();
    const body = container.querySelector('.tool-group-body');
    expect(body).not.toBeNull();
    // One chip per TOOL, folded-in thinking excluded: ls, read, grep.
    const titles = Array.from(body!.querySelectorAll('.chat-tool .tool-title')).map(
      (el) => el.textContent,
    );
    expect(titles).toEqual(['ls', 'read', 'grep']);
    expect(group.getAttribute('aria-expanded')).toBe('true');
    expect(container.querySelector('aside.tool-panel')).toBeNull();

    // A member chip expands its OWN inline body, independent of the group.
    const lsChip = Array.from(
      body!.querySelectorAll<HTMLButtonElement>('button.tool-summary'),
    ).find((b) => b.querySelector('.tool-title')?.textContent === 'ls')!;
    lsChip.click();
    await settle();
    expect(lsChip.getAttribute('aria-expanded')).toBe('true');
    // The summary sits inside a .tool-summary-row now (issue #154 split it so a
    // sidebar affordance can be its sibling), so reach the body via the frame.
    const inline = lsChip.closest('.chat-tool')!.querySelector('.tool-inline-body');
    expect(inline).not.toBeNull();
    expect(inline!.querySelector('.tool-body.tool-output')?.textContent).toBe('a');
  });

  it('keeps an inline expansion open across an SSE refetch, keyed by seq (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    await mountChat(); // lone tool at seq 3, output 'a\nb'
    (container.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('.tool-inline-body .tool-body.tool-output')?.textContent).toBe(
      'a\nb',
    );

    // An SSE tick grows the output at seq 3 — a back-patch below the cursor,
    // so its content_hash flips (re-derived) and the event names backpatchSeq
    // (issue #175). The seq-keyed expansion neither closes (native <details>
    // would) nor freezes (a captured message would) — it grows in place.
    messagesOnServer = {
      ...messagesOnServer,
      messages: messagesOnServer.messages.map((m) =>
        m.seq === 3 ? hashed({ ...m, tool: { ...m.tool!, output: 'a\nb\nc' } }) : m,
      ),
    };
    await emitMessagesChangedSettled(RUN_ID, { backpatchSeq: 3 });
    const body = container.querySelector('.tool-inline-body');
    expect(body).not.toBeNull();
    expect(body!.querySelector('.tool-body.tool-output')?.textContent).toBe('a\nb\nc');
    // The freshly re-rendered summary button still reads expanded.
    expect(
      (container.querySelector('button.tool-summary') as HTMLButtonElement).getAttribute(
        'aria-expanded',
      ),
    ).toBe('true');
  });

  // --- Desktop file sidebar affordance (issue #154 §1/§2) ---
  // On desktop a FILE chip (diff/write/read) gets an "open in sidebar" icon
  // button; its click opens the flush-right file sidebar. Command/fallback chips
  // and group summary rows never get one, and mobile gets none anywhere.

  /** Two lone FILE chips (diffs on foo.ts / bar.ts, split by text so they don't
   *  coalesce) plus a lone COMMAND chip — only the two files get the affordance. */
  function withFileChips(): void {
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'text', role: 'assistant', text: 'editing' },
        {
          seq: 2,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit foo.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'foo.ts', text: '@@ -1 +1 @@\n-old\n+new-foo' },
          },
        },
        { seq: 3, kind: 'text', role: 'assistant', text: 'and' },
        {
          seq: 4,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit bar.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'bar.ts', text: '@@ -1 +1 @@\n+new-bar' },
          },
        },
        { seq: 5, kind: 'text', role: 'assistant', text: 'then' },
        {
          seq: 6,
          kind: 'tool',
          tool: {
            name: 'Bash',
            title: 'run tests',
            status: 'ok',
            view: { kind: 'command', command: 'npm test' },
            output: 'passed',
          },
        },
      ],
      state: 'needs_input',
      cursor: 6,
      has_more: false,
      transcript: 'available',
    };
  }

  /** The .chat-tool frame of the chip whose summary title matches. */
  function chipFrame(title: string): HTMLElement {
    const summary = Array.from(
      container.querySelectorAll<HTMLButtonElement>('button.tool-summary'),
    ).find((b) => b.querySelector('.tool-title')?.textContent === title);
    if (!summary) throw new Error(`missing chip "${title}"`);
    return summary.closest('.chat-tool') as HTMLElement;
  }
  /** The affordance in a chip's COLLAPSED row (not the expanded body's copy). */
  function rowAffordance(frame: HTMLElement): HTMLButtonElement | null {
    return frame.querySelector<HTMLButtonElement>(
      ':scope > .tool-summary-row button[aria-label="Open in sidebar"]',
    );
  }

  it('shows the open-in-sidebar affordance on desktop file chips only (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withFileChips();
    await mountChat();

    // The two file chips carry the affordance in their collapsed row…
    expect(rowAffordance(chipFrame('edit foo.ts'))).not.toBeNull();
    expect(rowAffordance(chipFrame('edit bar.ts'))).not.toBeNull();
    // …the command chip never does.
    expect(rowAffordance(chipFrame('run tests'))).toBeNull();
  });

  it('never shows the affordance on a group summary row, only its member chips (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    // A run of file edits coalesces into a group.
    messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit a.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'a.ts', text: '@@ -1 +1 @@\n+a' },
          },
        },
        {
          seq: 2,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit b.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'b.ts', text: '@@ -1 +1 @@\n+b' },
          },
        },
      ],
      state: 'needs_input',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    const groupFrame = container.querySelector('.chat-tool-group') as HTMLElement;
    const groupSummary = groupFrame.querySelector('button.tool-group-summary') as HTMLButtonElement;
    // Collapsed group: no affordance anywhere in the frame (the summary row is
    // not a file chip).
    expect(groupFrame.querySelector('button[aria-label="Open in sidebar"]')).toBeNull();

    // Expanded: each member chip carries its own affordance (via the recursion).
    groupSummary.click();
    await settle();
    expect(
      groupFrame.querySelectorAll('.tool-group-body button[aria-label="Open in sidebar"]'),
    ).toHaveLength(2);
  });

  it('opens the file sidebar from a chip affordance, replaces on a second file, Escape closes (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withFileChips();
    await mountChat();

    const fooFrame = chipFrame('edit foo.ts');
    rowAffordance(fooFrame)!.click();
    await settle();

    // The in-flow desktop sidebar shows foo.ts, and the chip is selected/pressed.
    const side = container.querySelector('aside.tool-panel.tool-panel-side');
    expect(side).not.toBeNull();
    expect(side!.querySelector('.tool-view-path-text')?.textContent).toBe('foo.ts');
    expect(fooFrame.classList.contains('selected')).toBe(true);
    expect(
      (fooFrame.querySelector('button.tool-summary') as HTMLButtonElement).getAttribute(
        'aria-pressed',
      ),
    ).toBe('true');

    // Opening a second file replaces the content in place — one panel, new path.
    rowAffordance(chipFrame('edit bar.ts'))!.click();
    await settle();
    expect(container.querySelectorAll('aside.tool-panel')).toHaveLength(1);
    const side2 = container.querySelector('aside.tool-panel-side')!;
    expect(side2.querySelector('.tool-view-path-text')?.textContent).toBe('bar.ts');
    expect(side2.textContent).not.toContain('foo.ts');
    expect(fooFrame.classList.contains('selected')).toBe(false);
    expect(chipFrame('edit bar.ts').classList.contains('selected')).toBe(true);

    // Escape closes the sidebar and clears the highlight.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    expect(container.querySelector('aside.tool-panel')).toBeNull();
    expect(container.querySelector('.chat-tool.selected')).toBeNull();
  });

  it('carries the affordance in the expanded inline path header too (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withFileChips();
    await mountChat();

    const fooFrame = chipFrame('edit foo.ts');
    // A body (summary) click toggles inline expansion — not the sidebar.
    (fooFrame.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();

    const inline = fooFrame.querySelector('.tool-inline-body');
    expect(inline).not.toBeNull();
    expect(
      inline!.querySelector('.tool-view-path button[aria-label="Open in sidebar"]'),
    ).not.toBeNull();
    // The body click opened no sidebar/sheet.
    expect(container.querySelector('aside.tool-panel')).toBeNull();
  });

  it('shows no open-in-sidebar affordance anywhere on mobile (issue #154)', async () => {
    withFileChips();
    await mountChat(); // no matchMedia stub → mobile

    expect(container.querySelector('button[aria-label="Open in sidebar"]')).toBeNull();

    // Opening a file chip's sheet adds none either (the sheet has no affordance).
    (chipFrame('edit foo.ts').querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('aside.tool-panel')).not.toBeNull();
    expect(container.querySelector('button[aria-label="Open in sidebar"]')).toBeNull();
  });

  it('closes an open group sheet when the viewport crosses to desktop (file-only sidebar) (issue #154)', async () => {
    const media = stubMatchMedia(); // mobile
    messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok' } },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'ok' } },
      ],
      state: 'needs_input',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    (container.querySelector('button.tool-group-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('aside.tool-panel')).not.toBeNull();

    // Cross to desktop: the sidebar shows file details only, so a group
    // selection clears — the panel closes rather than showing a list.
    media.set(DESKTOP_QUERY, true);
    await settle();
    expect(container.querySelector('aside.tool-panel')).toBeNull();
  });

  it('closes an open command-tool sheet when crossing to desktop (issue #154)', async () => {
    const media = stubMatchMedia(); // mobile
    await mountChat(); // default fixture: lone Bash 'Ran ls', no file view

    (container.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('aside.tool-panel')).not.toBeNull();

    // A non-file tool selection clears when the sidebar (file-only) takes over.
    media.set(DESKTOP_QUERY, true);
    await settle();
    expect(container.querySelector('aside.tool-panel')).toBeNull();
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
    await emitMessagesChangedSettled();

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

  // --- Secret exposure warning badge (issue #108) ---

  it('omits the exposure badge for a run that has exposed nothing', async () => {
    await mountChat(); // baseRun() carries no exposed_secrets
    expect(container.querySelector('.chat-state-dot.exposed')).toBeNull();
    expect(container.querySelector('.chip.exposed')).toBeNull();
  });

  it('renders a singular exposure badge naming the one exposed secret', async () => {
    runOnServer = { ...baseRun(), exposed_secrets: ['API_KEY'] };
    await mountChat();
    expect(container.querySelector('.chat-state-dot.exposed')).not.toBeNull();
    const chip = container.querySelector('.chip.exposed');
    expect(chip?.textContent).toBe('API_KEY exposed');
    expect(chip?.getAttribute('title')).toContain('API_KEY');
  });

  it('renders a plural exposure badge with a tooltip listing every exposed secret', async () => {
    runOnServer = { ...baseRun(), exposed_secrets: ['ALPHA_KEY', 'ZEBRA_KEY'] };
    await mountChat();
    const chip = container.querySelector('.chip.exposed');
    expect(chip?.textContent).toBe('2 secrets exposed');
    expect(chip?.getAttribute('title')).toContain('ALPHA_KEY');
    expect(chip?.getAttribute('title')).toContain('ZEBRA_KEY');
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

  // --- Ready-to-start placeholder for an idle active run (issue #96) ---

  it('invites the first message for an active idle run whose transcript is not yet located', async () => {
    // codex writes its transcript only at first-turn start (issue #96): an
    // active run composed idle with no messages and an unlocated transcript
    // trusts the adapter state (ADR-0038) over the transcript-derived
    // placeholder and shows ready-to-start copy — the first_message spawn path
    // means there is nothing to auto-send here.
    messagesOnServer = {
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
    expect(replyPosts).toHaveLength(0);
  });

  it('keeps the transcript-locating placeholder before the first state is composed', async () => {
    // state '' (no poll composed the adapter state yet) with a locating
    // transcript is NOT the ready-to-start case — the transcript-derived
    // placeholder still stands.
    messagesOnServer = {
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
    runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    messagesOnServer = {
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

  it('does not name the web host in the locked-question hint for a remote-off run', async () => {
    // Issue #163: with remote control off no claude.ai session was ever
    // registered, so naming that host sends the operator to a page that cannot
    // exist — the same reason the Open affordance is hidden. The hint must fall
    // back to the generic wording, and this branch is exactly where a non-remote
    // run lands (claude without --remote-control flushes its pending tool_use,
    // so the dialog arrives by transcript scan rather than the spool).
    runOnServer = { ...baseRun(), remote: false, deep_link_url: null };
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    const note = container.querySelector('.chat-composer-note')?.textContent;
    expect(note).toBe('Claude Code needs input — open the session to respond.');
    expect(note).not.toContain('claude.ai');
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

  it('multi-question form Other: Enter no-ops while incomplete, then fires the atomic submit once complete (issue #165)', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: multiQuestionDialog(),
    };
    await mountChat();

    questionOption(0, 'Other').click();
    await settle();
    const other = questionEl(0).querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'ship a hotfix';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    // Question 1 (Scope) is still unanswered — the form is incomplete, so
    // Enter in question 0's Other input must no-op exactly like the disabled
    // Submit button.
    expect(buttonByText('Submit')!.disabled).toBe(true);
    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(answerPosts).toHaveLength(0);

    questionOption(1, 'Frontend').click();
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    // Same atomic payload clicking Submit would send.
    expect(answerPosts).toHaveLength(1);
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

  it('does not name the web host in the unanswerable-dialog note for a remote-off run', async () => {
    // The other half of the same gate (issue #163): an unanswerable shape on a
    // remote-off run must not point at a claude.ai session that was never
    // registered — the operator's real recourse is Interrupt.
    runOnServer = { ...baseRun(), remote: false, deep_link_url: null };
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
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

    const note = container.querySelector('.chat-dialog-card .chat-composer-note')?.textContent;
    expect(note).toBe("This dialog can't be answered here — open the session to respond.");
    expect(note).not.toContain('claude.ai');
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

  it('flat multi-select Other: Enter no-ops until ready, then submits exactly like clicking Submit (issue #165)', async () => {
    messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'pick some' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatMultiDialog(),
    };
    await mountChat();

    optionCard('Backend').click();
    optionCard('Other').click();
    await settle();
    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;

    // Other toggled but still empty → not ready, Enter no-ops like the
    // disabled Submit.
    expect(buttonByText('Submit')!.disabled).toBe(true);
    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(answerPosts).toHaveLength(0);

    other.value = 'the CI pipeline';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(answerPosts).toHaveLength(1);
    expect(answerPosts[0]).toEqual({
      tool_id: 'toolu_flat_multi',
      selected: [1],
      other_text: 'the CI pipeline',
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

  it('cycles with arrows, completes with Tab (inserting "/name "), closes on Escape', async () => {
    await mountChat();
    setComposerText('/');
    await settle();

    // Down moves the active row; the listbox exposes it via aria-selected.
    composerKey('ArrowDown');
    await settle();
    expect(popRows()[1]?.getAttribute('aria-selected')).toBe('true');

    // Tab completes the active command — no reply is sent (issue #122: Tab is
    // the ONLY completion gesture; Enter never accepts the highlight).
    composerKey('Tab');
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

    // Tab also completes a single filtered match.
    setComposerText('/dep');
    await settle();
    composerKey('Tab');
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/deploy ');
  });

  it('ranks name matches above description matches (issue #122 tiered ranking)', async () => {
    // The original bug: 'setup-matt-pocock-skills' sits before 'triage' in
    // catalog order and its DESCRIPTION mentions triage, so the flat filter
    // listed it first and the highlight (index 0) picked the wrong skill.
    // Name tiers (exact, prefix, substring) now beat the description/arg-hint
    // tier at every query length; discovery via description still works, it
    // just never outranks a name match.
    commandsOnServer = [
      {
        name: 'setup-matt-pocock-skills',
        description: 'Vendor skills for planning and triage',
        arg_hint: '',
        source: 'project',
        chat_safe: true,
      },
      {
        name: 'triage',
        description: 'Triage issues',
        arg_hint: '',
        source: 'project',
        chat_safe: true,
      },
    ];
    await mountChat();

    // Exact-name tier wins over the earlier catalog entry's description match.
    setComposerText('/triage');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/triage',
      '/setup-matt-pocock-skills',
    ]);
    expect(popRows()[0]?.getAttribute('aria-selected')).toBe('true');

    // Name-prefix tier wins the same way mid-typing.
    setComposerText('/tri');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/triage',
      '/setup-matt-pocock-skills',
    ]);
  });

  it('sends the raw input on Enter while the popover is open — never the highlight (issue #122)', async () => {
    // Reverses the issue #70 popover-precedence rule (ADR-0041): Enter no
    // longer accepts the highlighted row; it falls through to the ordinary
    // fine-pointer send gate and posts the box exactly as typed — partial
    // text included (Tab first to complete is the user's responsibility).
    finePointer(true);
    await mountChat();
    setComposerText('/cle');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).not.toBeNull();

    composerKey('Enter');
    await settle();
    expect(replyPosts).toEqual([{ text: '/cle' }]);
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('sends a Tab-completed command as typed on Enter (fine-pointer)', async () => {
    finePointer(true);
    await mountChat();
    setComposerText('/cle');
    await settle();
    composerKey('Tab');
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/clear ');

    composerKey('Enter');
    await settle();
    expect(replyPosts).toEqual([{ text: '/clear' }]); // trimmed by the send path
  });

  it('bare Enter with the popover open neither sends nor completes without a fine pointer', async () => {
    // jsdom has no matchMedia → not fine-pointer, so bare Enter stays the
    // browser-default newline; with Enter no longer captured by the popover
    // there is nothing else it may do.
    await mountChat();
    setComposerText('/cle');
    await settle();

    composerKey('Enter');
    await settle();
    expect(replyPosts).toHaveLength(0);
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/cle');
  });

  it('Cmd/Ctrl+Enter still sends the raw text over an open popover', async () => {
    await mountChat();
    setComposerText('/cle');
    await settle();

    composerKey('Enter', { ctrlKey: true });
    await settle();
    expect(replyPosts).toEqual([{ text: '/cle' }]);
  });

  it('sends a no-argument command immediately on click (issue #122)', async () => {
    await mountChat();
    setComposerText('/cle');
    await settle();

    // /clear declares no arg_hint → the click IS the send: the ordinary reply
    // POST fires, the box clears, and the popover goes with it.
    popRows()[0]!.click();
    await settle();
    expect(replyPosts).toEqual([{ text: '/clear' }]);
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('completes a hinted command on click instead of sending (issue #122)', async () => {
    await mountChat();
    setComposerText('/comp');
    await settle();

    // /compact declares arg_hint 'instructions' → the click completes to
    // "/name " and waits for the argument; nothing is posted.
    popRows()[0]!.click();
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/compact ');
    expect(replyPosts).toHaveLength(0);
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
    providersOnServer[0]!.models = [{ value: 'opus[1m]', label: 'Opus 4.6 [1m]', efforts: [] }];
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

  // --- "N behind" chip (issue #149) ---

  it('renders the "N behind" chip when commits_behind is positive', async () => {
    runOnServer = { ...baseRun(), commits_behind: 3 };
    await mountChat();

    const chip = container.querySelector('.chat-behind-chip');
    expect(chip?.textContent).toBe('3 behind');
    expect(chip?.getAttribute('title')).toBe('3 commits behind the base branch');
  });

  it('hides the "N behind" chip when commits_behind is 0', async () => {
    runOnServer = { ...baseRun(), commits_behind: 0 };
    await mountChat();

    expect(container.querySelector('.chat-behind-chip')).toBeNull();
  });

  it('hides the "N behind" chip when commits_behind is absent', async () => {
    await mountChat(); // baseRun() carries no commits_behind

    expect(container.querySelector('.chat-behind-chip')).toBeNull();
  });
});
