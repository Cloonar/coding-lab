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

/**
 * Quick-return header (issue #35 §2): committed upward scroll (px) accumulated
 * before the header slides out of the way. Reveal is hair-trigger (no threshold).
 */
export const HEADER_HIDE_THRESHOLD_PX = 10;

export interface QuickReturnState {
  /** The previous tick's scrollTop — the direction baseline. */
  lastScrollTop: number;
  /** Committed reading-onward distance since the last reversal. */
  hideAccum: number;
  /** Is the header currently shown? */
  visible: boolean;
}

/**
 * One user scroll tick → the next quick-return header state (issue #35 §2).
 * Pure, so the swipe-direction / 10px-commit / past-header gate is unit-testable
 * without jsdom (which has no scrolling — the same reason isNearBottom lives here).
 *
 * - Within the header's own band, or at the very top / iOS rubber-band overscroll
 *   (scrollTop ≤ headerHeight, including negatives): pinned visible, streak reset.
 * - Backtracking toward the top (delta < 0): hair-trigger reveal, streak reset.
 * - Reading onward (delta > 0): hide only once the *accumulated* commit crosses
 *   the threshold — a single touch tick is only a few px, so summing until a
 *   reversal matches "~10px committed" and kills flicker without demanding a
 *   single >10px event that real touch scrolling rarely produces.
 *
 * Programmatic scrolls (streaming autoscroll, follow-to-bottom, load-earlier
 * anchoring, tap-to-latest) must NOT be fed here — the caller suppresses them so
 * only user-initiated scroll ever toggles the header.
 */
export function quickReturnReduce(
  s: QuickReturnState,
  scrollTop: number,
  headerHeight: number,
): QuickReturnState {
  const delta = scrollTop - s.lastScrollTop;
  if (scrollTop <= headerHeight) return { lastScrollTop: scrollTop, hideAccum: 0, visible: true };
  if (delta < 0) return { lastScrollTop: scrollTop, hideAccum: 0, visible: true };
  const hideAccum = s.hideAccum + delta; // delta === 0 → unchanged
  const visible = hideAccum >= HEADER_HIDE_THRESHOLD_PX ? false : s.visible;
  return { lastScrollTop: scrollTop, hideAccum, visible };
}

/**
 * Jump-to-latest pill state (issue #35 §4). The stream is append-only, so
 * "scrolled up (not near bottom)" already means "newer content exists below" —
 * no separate unseen-message flag is needed for v1. The pill is emphasized
 * (accent-tinted) only when it is visible AND the content below is a needs-you
 * signal, so a hidden pill never claims emphasis.
 */
export function pillState(
  nearBottom: boolean,
  needsInput: boolean,
): { visible: boolean; emphasized: boolean } {
  const visible = !nearBottom;
  return { visible, emphasized: visible && needsInput };
}
