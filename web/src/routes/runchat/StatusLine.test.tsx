// Status line contract (issue #58 §2):
// - shown above the composer whenever the run is live, the transcript is
//   available and no dialog is pending — never for an ended run, a gone or
//   still-locating transcript, a pending dialog, or the degraded question
//   state;
// - the state is named in WORDS: working → "Working" + the running tool
//   call's title + elapsed since the turn's user message (ticking once a
//   second, only while working) + a one-tap Interrupt (POST /interrupt, no
//   confirm); needs_input → "Waiting for you"; idle / '' → "Idle" + how long
//   ago the last message arrived (slow refresh) + "Pull base · N behind" only
//   while behind, which sends `/pull-base` down the reply path;
// - the line never gates Send (Composer.test.tsx covers the always-Send
//   contract in every state).

import { render } from 'solid-js/web';
import { createSignal } from 'solid-js';
import { describe, expect, it, vi } from 'vitest';
import type { ChatMessage, ConversationState } from '../../api';
import {
  RUN_ID,
  baseRun,
  container,
  h,
  hashed,
  installChatHooks,
  mountChat,
  settle,
} from './harness';
import {
  StatusLine,
  formatAgo,
  formatElapsed,
  lastMessageTime,
  runningToolTitle,
  turnStartTime,
} from './StatusLine';

describe('status line helpers', () => {
  it('formats elapsed time as m:ss, then h:mm:ss', () => {
    expect(formatElapsed(0)).toBe('0:00');
    expect(formatElapsed(23_400)).toBe('0:23');
    expect(formatElapsed(12 * 60_000 + 5_000)).toBe('12:05');
    expect(formatElapsed(3_723_000)).toBe('1:02:03');
    expect(formatElapsed(-5_000)).toBe('0:00'); // clock skew never goes negative
  });

  it('formats idle time as just now, minutes, hours, days', () => {
    expect(formatAgo(30_000)).toBe('just now');
    expect(formatAgo(-60_000)).toBe('just now');
    expect(formatAgo(4 * 60_000 + 10_000)).toBe('4m ago');
    expect(formatAgo(3 * 3_600_000)).toBe('3h ago');
    expect(formatAgo(2 * 86_400_000 + 1)).toBe('2d ago');
  });

  const user = (seq: number, time?: string): ChatMessage => ({
    seq,
    kind: 'text',
    role: 'user',
    text: 'go',
    time,
  });
  const tool = (seq: number, title: string, status: 'running' | 'ok'): ChatMessage => ({
    seq,
    kind: 'tool',
    tool: { name: 'Bash', title, status },
  });

  it('names the LAST running tool call, else none', () => {
    expect(runningToolTitle([])).toBeNull();
    expect(runningToolTitle([tool(1, 'ls', 'ok')])).toBeNull();
    expect(
      runningToolTitle([tool(1, 'stale run', 'running'), tool(2, 'go build ./...', 'running')]),
    ).toBe('go build ./...');
    expect(
      runningToolTitle([
        { seq: 1, kind: 'tool', tool: { name: 'Grep', title: '', status: 'running' } },
      ]),
    ).toBe('Grep');
  });

  it('anchors the turn at the latest user message, and reads the newest timestamp', () => {
    const msgs: ChatMessage[] = [
      user(1, '2026-10-04T12:00:00.000Z'),
      { seq: 2, kind: 'text', role: 'assistant', text: 'ok', time: '2026-10-04T12:01:00.000Z' },
      user(3, '2026-10-04T12:05:00.000Z'),
      tool(4, 'go test', 'running'), // no time
    ];
    expect(turnStartTime(msgs)).toBe(Date.parse('2026-10-04T12:05:00.000Z'));
    expect(lastMessageTime(msgs)).toBe(Date.parse('2026-10-04T12:05:00.000Z'));
    // No usable stamp → no anchor (the line omits the time instead of guessing).
    expect(turnStartTime([user(1), tool(2, 'x', 'running')])).toBeNull();
    expect(turnStartTime([user(1, 'not a date')])).toBeNull();
    expect(lastMessageTime([tool(1, 'x', 'ok')])).toBeNull();
  });
});

describe('StatusLine clock', () => {
  function mountLine(initial: ConversationState, messages: ChatMessage[]) {
    const el = document.createElement('div');
    document.body.appendChild(el);
    const [state, setState] = createSignal<ConversationState>(initial);
    const run = vi.fn(() => Promise.resolve());
    const dispose = render(
      () => (
        <StatusLine
          runID="run_x"
          state={state()}
          messages={messages}
          commitsBehind={0}
          interrupt={{ busy: () => false, run }}
          onError={() => {}}
          onNotice={() => {}}
          onPulled={() => {}}
        />
      ),
      el,
    );
    const text = () => el.querySelector('.chat-status-text')?.textContent ?? '';
    return {
      el,
      text,
      setState,
      run,
      cleanup: () => {
        dispose();
        el.remove();
      },
    };
  }

  const turn: ChatMessage[] = [
    { seq: 1, kind: 'text', role: 'user', text: 'build it', time: '2026-10-04T12:00:00.000Z' },
    {
      seq: 2,
      kind: 'tool',
      tool: { name: 'Bash', title: 'go build ./...', status: 'running' },
      time: '2026-10-04T12:00:05.000Z',
    },
  ];

  it('ticks the elapsed time once a second while working, and stops when the state leaves working', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-04T12:00:23.000Z'));
    const line = mountLine('working', turn);
    try {
      expect(line.text()).toBe('Working · go build ./... · 0:23');
      expect(line.el.querySelector('.chat-status-dot.working')).not.toBeNull();

      await vi.advanceTimersByTimeAsync(5_000);
      expect(line.text()).toBe('Working · go build ./... · 0:28');

      // The one-tap Interrupt fires the shared controller.
      const btn = line.el.querySelector<HTMLButtonElement>('.chat-status-interrupt')!;
      expect(btn.textContent?.trim()).toBe('Interrupt');
      btn.click();
      expect(line.run).toHaveBeenCalledTimes(1);

      // Waiting: no clock at all — the interval is cleared.
      line.setState('needs_input');
      expect(line.text()).toBe('Waiting for you');
      expect(line.el.querySelector('.chat-status-dot.waiting')).not.toBeNull();
      expect(line.el.querySelector('.chat-status-action')).toBeNull();
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      line.cleanup();
      vi.useRealTimers();
    }
  });

  it('refreshes the idle "ago" on a slow timer and clears it on unmount', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-04T12:00:30.000Z'));
    const line = mountLine('idle', turn);
    try {
      expect(line.text()).toBe('Idle · just now'); // last message 12:00:05
      expect(line.el.querySelector('.chat-status-dot.idle')).not.toBeNull();
      expect(vi.getTimerCount()).toBe(1);

      await vi.advanceTimersByTimeAsync(4 * 60_000);
      expect(line.text()).toBe('Idle · 4m ago');

      // '' (no state composed yet) reads as idle too.
      line.setState('');
      expect(line.text()).toBe('Idle · 4m ago');
    } finally {
      line.cleanup();
    }
    expect(vi.getTimerCount()).toBe(0);
    vi.useRealTimers();
  });

  it('omits the time when no message carries one, and the title when nothing runs', () => {
    const line = mountLine('working', [{ seq: 1, kind: 'text', role: 'user', text: 'go' }]);
    try {
      expect(line.text()).toBe('Working');
      line.setState('idle');
      expect(line.text()).toBe('Idle');
    } finally {
      line.cleanup();
    }
  });
});

describe('StatusLine in the chat', () => {
  installChatHooks();

  const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
  const statusText = () => container.querySelector('.chat-status')?.textContent ?? '';
  const statusButton = (text: string) =>
    Array.from(container.querySelectorAll<HTMLButtonElement>('.chat-status button')).find(
      (b) => b.textContent?.trim() === text,
    );

  it('working: names the state and the running tool, shows elapsed time, and interrupts on one tap', async () => {
    h.messagesOnServer = {
      messages: [
        hashed({ seq: 1, kind: 'text', role: 'user', text: 'build it', time: ago(23_000) }),
        hashed({
          seq: 2,
          kind: 'tool',
          tool: { name: 'Bash', title: 'go build ./...', status: 'running' },
        }),
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    // Inside the composer, above the input row.
    const line = container.querySelector('.chat-composer .chat-status');
    expect(line).not.toBeNull();
    expect(line!.nextElementSibling?.classList.contains('chat-composer-row')).toBe(true);
    expect(statusText()).toContain('Working');
    expect(statusText()).toContain('go build ./...');
    expect(statusText()).toMatch(/· 0:2\d/);

    const interrupt = statusButton('Interrupt');
    expect(interrupt).toBeDefined();
    interrupt!.click();
    await settle();
    expect(h.interruptPosts).toBe(1); // one tap, no confirm step
    // Send is still right there (the line never gates it).
    expect(container.querySelector('.chat-composer-row button[aria-label="Send"]')).not.toBeNull();
  });

  it('needs_input: "Waiting for you", no action, and the in-stream waiting line stays', async () => {
    await mountChat(); // default fixture: needs_input
    expect(statusText()).toBe('Waiting for you');
    expect(container.querySelector('.chat-status button')).toBeNull();
    expect(container.querySelector('.chat-needs-input')?.textContent).toBe(
      'Claude Code is waiting for your reply.',
    );
  });

  it('idle: names the state and how long ago the last message arrived; no Pull base when not behind', async () => {
    h.messagesOnServer = {
      messages: [
        hashed({ seq: 1, kind: 'text', role: 'assistant', text: 'done', time: ago(4 * 60_000) }),
      ],
      state: 'idle',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(statusText()).toBe('Idle · 4m ago');
    expect(container.querySelector('.chat-status button')).toBeNull();
  });

  it('idle and behind: Pull base sends /pull-base down the reply path and refetches the run', async () => {
    h.runOnServer = { ...baseRun(), commits_behind: 3 };
    h.replyStatus = 200;
    h.replyNotice = 'already up to date with origin/main';
    h.messagesOnServer = { ...h.messagesOnServer, state: 'idle' };
    await mountChat();
    const runGets = () =>
      (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.filter(
        (c) => String(c[0]) === `/api/v1/runs/${RUN_ID}` && (c[1]?.method ?? 'GET') === 'GET',
      ).length;
    const before = runGets();

    const pull = statusButton('Pull base · 3 behind');
    expect(pull).toBeDefined();
    pull!.click();
    await settle();

    expect(h.replyPosts).toEqual([{ text: '/pull-base' }]);
    expect(container.querySelector('.banner.notice')?.textContent).toContain(
      'already up to date with origin/main',
    );
    expect(runGets()).toBeGreaterThan(before);
  });

  it('offers Pull base only while idle', async () => {
    h.runOnServer = { ...baseRun(), commits_behind: 2 };
    h.messagesOnServer = { ...h.messagesOnServer, state: 'working' };
    await mountChat();
    expect(statusText()).toContain('Working');
    expect(statusButton('Pull base · 2 behind')).toBeUndefined();
  });

  it('reads "Idle" before any state is composed', async () => {
    h.messagesOnServer = { ...h.messagesOnServer, state: '' };
    await mountChat();
    expect(statusText()).toBe('Idle');
  });

  it('is absent for an ended run', async () => {
    h.runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    await mountChat();
    expect(container.querySelector('.chat-status')).toBeNull();
  });

  it('is absent while the transcript is gone', async () => {
    h.messagesOnServer = { ...h.messagesOnServer, transcript: 'gone' };
    await mountChat();
    expect(container.querySelector('.chat-status')).toBeNull();
  });

  it('is absent while the transcript is still locating (the composer itself stays usable)', async () => {
    h.messagesOnServer = {
      messages: [],
      state: 'idle',
      cursor: 0,
      has_more: false,
      transcript: 'locating',
      transcript_id: '',
    };
    await mountChat();
    expect(container.querySelector('.chat-status')).toBeNull();
    expect(container.querySelector('.chat-composer-row .chat-input')).not.toBeNull();
  });

  it('is absent in the degraded question state (no structured dialog)', async () => {
    h.messagesOnServer = { ...h.messagesOnServer, state: 'question' };
    await mountChat();
    expect(container.querySelector('.chat-status')).toBeNull();
    expect(container.querySelector('.chat-composer .chat-interrupt')).not.toBeNull();
  });
});
