// Chat-stream accumulation and scroll math for RunChat (issue #7 / ADR-0016).
// Pure helpers: merging message windows by seq (later window wins, so tool
// status flips and answered dialogs back-patch in place), the append cursor
// for after=<seq> tail fetches, and the scroll bookkeeping jsdom can't
// exercise — follow-bottom slack and manual prepend anchoring (iOS Safari has
// no overflow-anchor, so "Load earlier" restores the position by hand).

import type { ChatMessage } from '../api';

/** Merge windows by seq (later window wins per seq → in-place tool updates). */
export function mergeMessages(prev: ChatMessage[], incoming: ChatMessage[]): ChatMessage[] {
  const bySeq = new Map<number, ChatMessage>();
  for (const m of prev) bySeq.set(m.seq, m);
  for (const m of incoming) bySeq.set(m.seq, m);
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

/**
 * One refetch's application: the accumulated stream, then the after=<cursor>
 * tail batches (gap-free appends), then the latest window (back-patched
 * mutations near the tail) — later response wins per seq.
 */
export function mergeRefetch(
  prev: ChatMessage[],
  tail: ChatMessage[],
  latest: ChatMessage[],
): ChatMessage[] {
  return mergeMessages(mergeMessages(prev, tail), latest);
}

/** The append cursor: the highest seq seen so far, 0 for an empty stream. */
export function maxSeq(messages: ChatMessage[]): number {
  let max = 0;
  for (const m of messages) if (m.seq > max) max = m.seq;
  return max;
}

/** Within this many px of the end still counts as "at the bottom". */
export const BOTTOM_SLACK_PX = 40;

export interface ScrollMetrics {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

/**
 * True when the viewport sits at/near the stream's end — follow-bottom may
 * engage on appends without yanking a reader who scrolled up.
 */
export function isNearBottom(m: ScrollMetrics): boolean {
  return m.scrollHeight - m.scrollTop - m.clientHeight <= BOTTOM_SLACK_PX;
}

/**
 * The scrollTop that keeps the viewport visually anchored after older
 * messages were prepended: the content above grew by the scrollHeight delta.
 */
export function anchoredScrollTop(
  before: { scrollTop: number; scrollHeight: number },
  newScrollHeight: number,
): number {
  return before.scrollTop + Math.max(0, newScrollHeight - before.scrollHeight);
}
