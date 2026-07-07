// Chat-stream helper contracts (issue #7 / ADR-0016): merge-by-seq with
// later-window-wins (the after=-cursor refetch relies on it to back-patch
// tool status flips and answered dialogs), the append cursor, and the scroll
// math the view applies manually because jsdom can't do real scrolling.

import { describe, expect, it } from 'vitest';
import type { ChatMessage } from '../api';
import {
  BOTTOM_SLACK_PX,
  anchoredScrollTop,
  isNearBottom,
  maxSeq,
  mergeMessages,
  mergeRefetch,
} from './chatStream';

const msg = (seq: number, text: string): ChatMessage => ({
  seq,
  kind: 'text',
  role: 'assistant',
  text,
});

describe('mergeMessages', () => {
  it('unions windows sorted by seq', () => {
    const merged = mergeMessages([msg(3, 'c'), msg(1, 'a')], [msg(2, 'b'), msg(4, 'd')]);
    expect(merged.map((m) => m.seq)).toEqual([1, 2, 3, 4]);
  });

  it('lets the later window win per seq (in-place tool updates)', () => {
    const running: ChatMessage = {
      seq: 2,
      kind: 'tool',
      tool: { name: 'Bash', title: 'Running ls', status: 'running' },
    };
    const done: ChatMessage = {
      seq: 2,
      kind: 'tool',
      tool: { name: 'Bash', title: 'Ran ls', status: 'ok', output: 'a' },
    };
    const merged = mergeMessages([msg(1, 'a'), running], [done]);
    expect(merged).toHaveLength(2);
    expect(merged[1]?.tool?.status).toBe('ok');
  });
});

describe('mergeRefetch', () => {
  it('applies prev, then the tail batches, then the latest window — latest wins', () => {
    const pending: ChatMessage = {
      seq: 2,
      kind: 'dialog',
      dialog: { tool_id: 'toolu_1', dialog_kind: 'question', prompt: 'Pick', answerable: true },
    };
    const answered: ChatMessage = { seq: 2, kind: 'text', role: 'user', text: 'picked A' };
    const merged = mergeRefetch(
      [msg(1, 'a'), pending],
      [msg(3, 'tail'), msg(4, 'tail 2')],
      [answered, msg(3, 'tail'), msg(4, 'tail 2')],
    );
    expect(merged.map((m) => m.seq)).toEqual([1, 2, 3, 4]);
    expect(merged[1]?.kind).toBe('text'); // the latest window back-patched seq 2
  });

  it('keeps accumulated history the latest window no longer covers', () => {
    const merged = mergeRefetch([msg(1, 'old')], [msg(5, 'new')], [msg(5, 'new')]);
    expect(merged.map((m) => m.seq)).toEqual([1, 5]);
  });
});

describe('maxSeq', () => {
  it('is 0 for an empty stream', () => {
    expect(maxSeq([])).toBe(0);
  });

  it('finds the highest seq regardless of order', () => {
    expect(maxSeq([msg(4, 'd'), msg(9, 'i'), msg(2, 'b')])).toBe(9);
  });
});

describe('isNearBottom', () => {
  it('is true exactly up to the slack threshold', () => {
    const at = { scrollTop: 560, scrollHeight: 1000, clientHeight: 400 };
    const within = { scrollTop: 600 - BOTTOM_SLACK_PX, scrollHeight: 1000, clientHeight: 400 };
    const beyond = { scrollTop: 600 - BOTTOM_SLACK_PX - 1, scrollHeight: 1000, clientHeight: 400 };
    expect(isNearBottom(at)).toBe(true);
    expect(isNearBottom(within)).toBe(true);
    expect(isNearBottom(beyond)).toBe(false);
  });
});

describe('anchoredScrollTop', () => {
  it('offsets by the prepended height so the view does not jump', () => {
    expect(anchoredScrollTop({ scrollTop: 0, scrollHeight: 1000 }, 1600)).toBe(600);
    expect(anchoredScrollTop({ scrollTop: 120, scrollHeight: 1000 }, 1600)).toBe(720);
  });

  it('never anchors backwards when nothing was prepended', () => {
    expect(anchoredScrollTop({ scrollTop: 120, scrollHeight: 1000 }, 1000)).toBe(120);
  });
});
