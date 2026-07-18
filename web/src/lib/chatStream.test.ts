// Chat-stream helper contracts (issue #7 / ADR-0016): merge-by-seq with
// later-window-wins (the after=-cursor refetch relies on it to back-patch
// tool status flips and answered dialogs) — identity-stable when equal
// content_hashes prove a message unchanged (issue #175) — the append cursor,
// and the scroll math the view applies manually because jsdom can't do real
// scrolling.

import { describe, expect, it } from 'vitest';
import type { ChatMessage } from '../api';
import {
  BOTTOM_SLACK_PX,
  HEADER_HIDE_THRESHOLD_PX,
  anchoredScrollTop,
  isNearBottom,
  maxSeq,
  mergeMessages,
  mergeRefetch,
  pillState,
  quickReturnReduce,
  type QuickReturnState,
} from './chatStream';

const msg = (seq: number, text: string): ChatMessage => ({
  seq,
  kind: 'text',
  role: 'assistant',
  text,
});

/** A hashed message (issue #175): equal content ⇒ equal (derived) hash. */
const hmsg = (seq: number, text: string, hash = `h:${seq}:${text}`): ChatMessage => ({
  ...msg(seq, text),
  content_hash: hash,
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

  // --- Identity-stable merge (issue #175) ---

  it('returns the prev ARRAY itself when an identical window merges again', () => {
    const prev = [hmsg(1, 'a'), hmsg(2, 'b')];
    // Fresh parses of the same content — new objects, same hashes.
    expect(mergeMessages(prev, [hmsg(1, 'a'), hmsg(2, 'b')])).toBe(prev);
  });

  it('keeps the PREV element on an equal-hash collision', () => {
    const prev = [hmsg(1, 'a'), hmsg(2, 'b')];
    const merged = mergeMessages(prev, [hmsg(2, 'b'), hmsg(3, 'c')]);
    expect(merged).not.toBe(prev); // seq 3 appended
    expect(merged.map((m) => m.seq)).toEqual([1, 2, 3]);
    expect(merged[1]).toBe(prev[1]); // same object, not just equal content
  });

  it('replaces the message when the hash differs', () => {
    const prev = [hmsg(2, 'running…', 'h-old')];
    const incoming = [hmsg(2, 'done', 'h-new')];
    expect(mergeMessages(prev, incoming)[0]).toBe(incoming[0]);
  });

  it('falls back to later-wins when either side lacks a hash (older servers)', () => {
    // Neither hashed.
    const bare = [msg(2, 'old')];
    const bareIncoming = [msg(2, 'new')];
    expect(mergeMessages(bare, bareIncoming)[0]).toBe(bareIncoming[0]);
    // Prev hashed, incoming not — still later-wins, never a stale keep.
    const hashedPrev = [hmsg(2, 'old')];
    const unhashedIncoming = [msg(2, 'new')];
    expect(mergeMessages(hashedPrev, unhashedIncoming)[0]).toBe(unhashedIncoming[0]);
  });

  it('mixed window: new array, unchanged elements keep identity, the changed one is replaced', () => {
    const prev = [hmsg(1, 'a'), hmsg(2, 'b', 'h-b1'), hmsg(3, 'c')];
    const incoming = [hmsg(1, 'a'), hmsg(2, 'b2', 'h-b2'), hmsg(3, 'c')];
    const merged = mergeMessages(prev, incoming);
    expect(merged).not.toBe(prev);
    expect(merged[0]).toBe(prev[0]);
    expect(merged[1]).toBe(incoming[1]);
    expect(merged[2]).toBe(prev[2]);
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

  it('inherits identity stability from mergeMessages by composition (issue #175)', () => {
    const prev = [hmsg(1, 'a'), hmsg(2, 'b')];
    // A no-op refetch (tail re-delivers, latest matches) → the prev ARRAY itself.
    expect(mergeRefetch(prev, [hmsg(2, 'b')], [hmsg(1, 'a'), hmsg(2, 'b')])).toBe(prev);
    // A changed latest replaces exactly that element; the rest keep identity.
    const latest = [hmsg(1, 'a'), hmsg(2, 'B!', 'h-b-new')];
    const merged = mergeRefetch(prev, [], latest);
    expect(merged).not.toBe(prev);
    expect(merged[0]).toBe(prev[0]);
    expect(merged[1]).toBe(latest[1]);
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

describe('quickReturnReduce (issue #35 §2)', () => {
  const start = (over: Partial<QuickReturnState> = {}): QuickReturnState => ({
    lastScrollTop: 500,
    hideAccum: 0,
    visible: true,
    ...over,
  });
  const H = 48; // header height gate

  it('pins the header inside the header band and at the very top', () => {
    expect(quickReturnReduce(start({ visible: false }), 0, H).visible).toBe(true);
    expect(quickReturnReduce(start({ visible: false }), H, H).visible).toBe(true);
    // iOS rubber-band overscroll (negative scrollTop) also pins it.
    expect(quickReturnReduce(start({ visible: false }), -30, H).visible).toBe(true);
  });

  it('reveals on any backtrack toward the top (hair-trigger) and resets the streak', () => {
    const next = quickReturnReduce(start({ visible: false, hideAccum: 99 }), 497, H);
    expect(next.visible).toBe(true);
    expect(next.hideAccum).toBe(0);
  });

  it('hides only after committed reading-onward scroll crosses the threshold', () => {
    let st = start(); // 500, visible
    st = quickReturnReduce(st, 505, H); // +5, below threshold
    expect(st.visible).toBe(true);
    st = quickReturnReduce(st, 512, H); // +7 → 12 committed ≥ 10
    expect(st.visible).toBe(false);
    expect(HEADER_HIDE_THRESHOLD_PX).toBe(10);
  });

  it('a reversal resets the streak so a tiny wobble never hides it', () => {
    let st = start();
    st = quickReturnReduce(st, 506, H); // +6
    st = quickReturnReduce(st, 503, H); // reversal → reset + reveal
    st = quickReturnReduce(st, 509, H); // +6 again, streak only 6
    expect(st.visible).toBe(true);
  });

  it('is a no-op on a zero delta', () => {
    const st = start({ hideAccum: 4 });
    expect(quickReturnReduce(st, 500, H)).toEqual(st);
  });
});

describe('pillState (issue #35 §4)', () => {
  it('is hidden at/near the bottom', () => {
    expect(pillState(true, false)).toEqual({ visible: false, emphasized: false });
  });

  it('is visible (append-only ⇒ newer content below) when scrolled up', () => {
    expect(pillState(false, false)).toEqual({ visible: true, emphasized: false });
  });

  it('is emphasized when the content below is a needs-you signal', () => {
    expect(pillState(false, true)).toEqual({ visible: true, emphasized: true });
  });

  it('never claims emphasis while hidden', () => {
    expect(pillState(true, true)).toEqual({ visible: false, emphasized: false });
  });
});
