import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, vi } from 'vitest';
import type { ChatMessage, MessagesResponse, Provider, Repo, Run, RunCommand } from '../../api';
import App from '../../App';
import RunChat from '../RunChat';

export const RUN_ID = 'run_1';

export class FakeEventSource {
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

export function baseRun(): Run {
  return {
    id: RUN_ID,
    repo_id: 'repo_1',
    kind: 'manual',
    provider: 'claude-code',
    issue_number: null,
    pull_number: null,
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
export function baseRepo(): Repo {
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
    autoland_enabled: false,
    max_fix_attempts: 2,
    auto_merge: true,
    lander_provider: null,
    lander_model: null,
    lander_effort: null,
    runner: 'host',
    container_memory: null,
    container_pids: null,
    container_nofile: null,
    image_ref: null,
  };
}

/**
 * Mutable server/response state the tests poke directly (e.g.
 * `h.messagesOnServer = {...}`, `h.replyStatus = 500`). Plain `export let`
 * bindings can't work here — ESM importers get a read-only view of an
 * imported binding, so a test file can never reassign one. Routing every
 * whole-variable reassignment through fields on a single exported object
 * sidesteps that: importers mutate `h.x`, never rebind `x` itself.
 */
export interface ChatHarnessState {
  runOnServer: Run;
  repoOnServer: Repo;
  messagesOnServer: MessagesResponse;
  providersOnServer: Provider[];
  commandsOnServer: RunCommand[];
  replyPosts: { text: string }[];
  // Reply POST status — 204 (success) by default. Kept as a knob so a reply-path
  // test can flip it to a 4xx without re-plumbing the fetch stub.
  replyStatus: number;
  // The 200 status's {notice} body (issue #149) — null leaves replyStatus:200
  // untouched by any test that doesn't set it.
  replyNotice: string | null;
  answerPosts: Record<string, unknown>[];
  interruptPosts: number;
  // Inline rename PATCHes (issue #111); the stub applies them to runOnServer so
  // the onChanged refetch sees the new title like the real server round-trip.
  titlePatches: { title: string | null }[];
}
export const h = {} as ChatHarnessState;

let dispose: (() => void) | undefined;
export let container: HTMLDivElement;

export function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

/** Mimics the server's windowMessages (chat.go): after= tails, before= pages up. */
export function messagesWindow(url: string): MessagesResponse {
  const q = new URL(url, 'http://lab').searchParams;
  const after = Number(q.get('after') ?? '0');
  const before = Number(q.get('before') ?? '0');
  const limit = Number(q.get('limit') ?? '60');
  const all = h.messagesOnServer.messages;
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
  return { ...h.messagesOnServer, messages: win, has_more: hasMore };
}

export function stubApi(): void {
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
        return Promise.resolve(jsonResponse(200, { providers: h.providersOnServer }));
      }
      if (url === `/api/v1/runs/${RUN_ID}` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...h.runOnServer }));
      }
      // The header fetches the run's repo for the forge git-icon link (#132).
      if (url === `/api/v1/repos/${h.runOnServer.repo_id}` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...h.repoOnServer }));
      }
      if (url === `/api/v1/runs/${RUN_ID}` && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as { title: string | null };
        h.titlePatches.push(patch);
        h.runOnServer = { ...h.runOnServer, title: patch.title };
        return Promise.resolve(jsonResponse(200, { ...h.runOnServer }));
      }
      if (url.startsWith(`/api/v1/runs/${RUN_ID}/messages`) && method === 'GET') {
        return Promise.resolve(jsonResponse(200, messagesWindow(url)));
      }
      // The run's slash-command catalog (issue #51 decision 5).
      if (url === `/api/v1/runs/${RUN_ID}/commands` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { commands: h.commandsOnServer }));
      }
      if (url === `/api/v1/runs/${RUN_ID}/reply` && method === 'POST') {
        h.replyPosts.push(JSON.parse(String(init?.body)) as { text: string });
        if (h.replyStatus >= 400) {
          return Promise.resolve(
            jsonResponse(h.replyStatus, { error: 'run is not accepting replies' }),
          );
        }
        if (h.replyStatus === 200) {
          return Promise.resolve(jsonResponse(200, { notice: h.replyNotice }));
        }
        return Promise.resolve(jsonResponse(h.replyStatus, ''));
      }
      if (url === `/api/v1/runs/${RUN_ID}/answer` && method === 'POST') {
        h.answerPosts.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return Promise.resolve(jsonResponse(204, ''));
      }
      if (url === `/api/v1/runs/${RUN_ID}/interrupt` && method === 'POST') {
        h.interruptPosts += 1;
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

export const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
export async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flush();
}

export async function mountChat(): Promise<void> {
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

export function buttonByText(text: string): HTMLButtonElement | null {
  return (
    Array.from(container.querySelectorAll('button')).find((b) => b.textContent?.trim() === text) ??
    null
  );
}

export function buttonByLabel(label: string): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
}

// The mobile `•••` overflow menu (issue #35 §1). Its trigger is always present;
// its items render only while the menu is open (like TopBar), so they never leak
// duplicate buttons into the default-DOM assertions above.
export function moreButton(): HTMLButtonElement | null {
  return buttonByLabel('More actions');
}
export function menuItem(text: string): HTMLButtonElement | undefined {
  return Array.from(container.querySelectorAll('.chat-menu button')).find(
    (b) => b.textContent?.trim() === text,
  ) as HTMLButtonElement | undefined;
}

// jsdom has no clipboard — install a spy so copy buttons are exercisable.
export function stubClipboard(): ReturnType<typeof vi.fn> {
  const writeText = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
  return writeText;
}

// The tool panel's desktop breakpoint (issue #145) — must byte-match the
// query RunChat builds from its DESKTOP_MIN_PX (the shell rail width).
export const DESKTOP_QUERY = '(min-width: 1024px)';

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

export function stubMatchMedia(): { set: (query: string, matches: boolean) => void } {
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
export function finePointer(matches: boolean): void {
  stubMatchMedia().set('(hover: hover) and (pointer: fine)', matches);
}

/** issue #175: the server-computed content_hash mimic — any author-controlled
 *  string works, as long as equal rendered content derives an equal hash and
 *  any mutation (status flip, output growth) derives a different one. Tests
 *  that mutate a message in place re-derive via `hashed` so the hash flips. */
export function hashed(m: ChatMessage): ChatMessage {
  return {
    ...m,
    content_hash: `h:${m.seq}:${m.text ?? ''}:${m.tool?.status ?? ''}:${m.tool?.output ?? ''}`,
  };
}

/** Set the server's messages to a single assistant text message. */
export function withAssistantText(text: string): void {
  h.messagesOnServer = {
    messages: [hashed({ seq: 1, kind: 'text', role: 'assistant', text })],
    state: 'needs_input',
    cursor: 1,
    has_more: false,
    transcript: 'available',
  };
}

export function emitMessagesChanged(
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
export const MESSAGES_DEBOUNCE_MS = 300;

/**
 * Emit run.messages.changed and ride out the trailing debounce (issue #175):
 * fake timers around the emit make the 300ms advance instant (the refetch's
 * promise chain flushes inside advanceTimersByTimeAsync — the settle()-style
 * setTimeout(0) hops advance under fake timers too), then a real-timer settle
 * drains whatever the refetch scheduled after the flush.
 */
export async function emitMessagesChangedSettled(
  runID: string = RUN_ID,
  extra: { state?: string; backpatchSeq?: number } = {},
): Promise<void> {
  vi.useFakeTimers();
  emitMessagesChanged(runID, extra);
  await vi.advanceTimersByTimeAsync(MESSAGES_DEBOUNCE_MS + 100);
  vi.useRealTimers();
  await settle();
}

export function installChatHooks(): void {
  beforeEach(() => {
    h.runOnServer = baseRun();
    h.repoOnServer = baseRepo();
    h.providersOnServer = [
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
    h.commandsOnServer = [
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
    h.messagesOnServer = {
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
    h.replyPosts = [];
    h.replyStatus = 204;
    h.replyNotice = null;
    h.answerPosts = [];
    h.interruptPosts = 0;
    h.titlePatches = [];
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
}
