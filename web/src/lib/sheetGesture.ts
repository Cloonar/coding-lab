// Follow-finger bottom-sheet dismiss gesture state machine (issue #145): the
// tool-call detail panel is a modal bottom sheet on phones, and a downward drag
// starting ANYWHERE on the sheet drags it out with the finger toward off-screen;
// releasing past halfway (or on a downward flick) dismisses it. This is the
// vertical transposition of the horizontal drawer gesture (issue #140) — same
// house pattern, same feel constants, rotated 90 degrees onto the Y axis.
//
// Dismiss-only: the sheet is opened solely by tapping the triggering row (never
// by a drag), so while it is up the gesture's ONLY job is drag-to-dismiss. There
// is no `open` env flag and no reveal direction — the sheet always rests fully
// open at touch start, so the base offset is a constant 0 (contrast the drawer,
// whose base flips with drawerOpen). That is the one simplification dismiss-only
// buys us; everything else mirrors the drawer.
//
// Pure module (house pattern): consumes touch points plus environment answers
// resolved by the caller at touch start (sheet height? editable target?
// vertically-scrolled ancestor?) and emits decisions. It never touches the DOM —
// the panel component binds the listeners, answers the environment queries, and
// applies transforms / scrim opacity.
//
// Arbitration (who gets the touch):
//   - Vertical scrollers win unless fully at top: a touch starting inside an
//     ancestor that can scroll vertically AND has scrollTop > 0 is left entirely
//     to the page — the user is scrolling the sheet's inner content. At
//     scrollTop === 0 a downward pull has nothing left to scroll, so the sheet
//     may claim it (vertical transposition of #140's "scrollers win unless
//     scrollLeft 0"). The caller answers this via env.inScrolledScroller.
//   - Editables excluded: touches starting in textarea / input / select /
//     contenteditable never begin sheet tracking — vertical drags there are
//     cursor/selection.
//   - Axis dominance: after INTENT_PX of travel the dominant axis decides;
//     horizontal intent hands the touch to the page for good (ties go
//     horizontal, so vertical must strictly dominate to claim — the mirror of
//     the drawer, where ties went vertical).
//   - Downward-only claim: only a DOWNWARD vertical intent claims the touch. An
//     upward vertical intent hands off for good — an upward drag at scrollTop 0
//     is the user scrolling the inner content back up, not a sheet gesture.
//     (Mirrors the drawer's rightward-only claim when closed.)
//
// Commit rule on release: dismiss if dragged past COMMIT_FRACTION of the sheet
// height OR the release velocity is a downward flick (>= FLICK_VELOCITY_PX_MS); a
// flick wins over position in whichever direction it points, so a fast upward
// throw settles the sheet back open even from past halfway, and a small downward
// flick dismisses even from a shallow drag.

import {
  COMMIT_FRACTION,
  FLICK_VELOCITY_PX_MS,
  INTENT_PX,
  VELOCITY_WINDOW_MS,
} from './drawerGesture';
import type { GesturePoint } from './drawerGesture';

// Re-export so a consumer that imports the sheet gesture gets its point type
// from one place, even though it is physically shared with the drawer.
export type { GesturePoint } from './drawerGesture';

/** Environment answers resolved by the caller when the touch starts. */
export type SheetGestureEnv = {
  /** Rendered sheet height in px, measured at touch start. Fixes the clamp and
   * the commit threshold; a non-positive value ignores the touch. */
  sheetHeight: number;
  /** Touch target is inside input/textarea/select/contenteditable. */
  inEditable: boolean;
  /** Some ancestor of the target scrolls vertically and has scrollTop>0, so a
   * downward pull scrolls that content rather than dismissing the sheet. */
  inScrolledScroller: boolean;
};

export type SheetGestureDecision =
  /** Not our touch (or handed to the page for good) — do nothing. */
  | { kind: 'ignore' }
  /** Watching, intent undecided — do NOT preventDefault yet. */
  | { kind: 'track' }
  /** Claimed: preventDefault, translate the sheet to y px (0..height, 0 = fully
   * open) and set the scrim opacity to progress (1..0 open-ness). */
  | { kind: 'drag'; y: number; progress: number }
  /** Release commits dismissal: settle the sheet off-screen (caller closes the
   * panel). */
  | { kind: 'commit-dismiss' }
  /** Release settles back to fully open. */
  | { kind: 'settle-open' }
  /** Touch was stolen (OS gesture) mid-drag: animate back to fully open — the
   * caller's open state never changed. */
  | { kind: 'cancel-reset' };

export type SheetGesture = {
  /** Feed the initial touch point. Returns 'ignore' (editable target, scrolled
   * scroller, non-positive height) or 'track'. */
  start(point: GesturePoint, env: SheetGestureEnv): SheetGestureDecision;
  /** Feed a move of the tracked touch. Returns 'track' before intent, 'ignore'
   * once handed to the page, 'drag' once claimed. */
  move(point: GesturePoint): SheetGestureDecision;
  /** Feed the release. Returns 'ignore' if the touch was never claimed (taps
   * pass through), else 'commit-dismiss' / 'settle-open'. */
  end(point: GesturePoint): SheetGestureDecision;
  /** Feed a touchcancel. Returns 'cancel-reset' if a drag was in progress, else
   * 'ignore'. */
  cancel(): SheetGestureDecision;
};

/** One reusable tracker; each start() begins a fresh touch. */
export function createSheetGesture(): SheetGesture {
  // 'idle' collapses the three "not our touch" situations — never started,
  // permanently ignored (editable / scrolled scroller / zero height), and handed
  // to the page (horizontal or upward-vertical intent) — because move/end/cancel
  // treat all three identically: ignore, and reset on the terminal events. Only
  // 'tracking' (intent undecided) and 'claimed' (dragging) do real work.
  let phase: 'idle' | 'tracking' | 'claimed' = 'idle';
  let startPoint: GesturePoint | null = null;
  let env: SheetGestureEnv | null = null;
  // Finger samples (client y + timestamp) kept only to measure the release
  // velocity, in ascending-time order so the oldest sample still inside the
  // velocity window is simply the first one at or past the window threshold.
  let samples: { y: number; t: number }[] = [];

  const reset = () => {
    phase = 'idle';
    startPoint = null;
    env = null;
    samples = [];
  };

  const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));

  // TranslateY (0..height) and open-ness (1..0) for the CURRENT finger dy,
  // recomputed per event. The sheet always rests fully open at touch start, so
  // the base offset is a constant 0; the clamp saturates both overshoot past
  // off-screen and any reversal above the sheet top (dy < 0 -> y = 0).
  const dragAt = (point: GesturePoint): { y: number; progress: number } => {
    const e = env as SheetGestureEnv;
    const y = clamp(point.y - (startPoint as GesturePoint).y, 0, e.sheetHeight);
    return { y, progress: (e.sheetHeight - y) / e.sheetHeight };
  };

  const record = (point: GesturePoint) => {
    samples.push({ y: point.y, t: point.t });
    // Prune relative to THIS sample's time, so a finger that keeps moving keeps
    // the window fresh — and one that stops flushes stale samples on the way to
    // the (velocity-less) release.
    const cutoff = point.t - VELOCITY_WINDOW_MS;
    samples = samples.filter((s) => s.t >= cutoff);
  };

  return {
    start(point, e) {
      reset();
      // Editable target, a vertically-scrolled ancestor, or a non-positive sheet
      // height: never ours. Stay 'idle' so every later event on this touch
      // ignores.
      if (e.inEditable || e.inScrolledScroller || e.sheetHeight <= 0) {
        return { kind: 'ignore' };
      }
      startPoint = point;
      env = e;
      samples = [{ y: point.y, t: point.t }];
      phase = 'tracking';
      return { kind: 'track' };
    },

    move(point) {
      if (phase === 'idle') return { kind: 'ignore' };
      // Every processed move (tracking or dragging) feeds the velocity buffer.
      record(point);
      if (phase === 'claimed') return { kind: 'drag', ...dragAt(point) };

      // 'tracking': intent still undecided until either axis travels INTENT_PX.
      const dx = point.x - (startPoint as GesturePoint).x;
      const dy = point.y - (startPoint as GesturePoint).y;
      if (Math.abs(dx) < INTENT_PX && Math.abs(dy) < INTENT_PX) {
        return { kind: 'track' };
      }
      // Dominant axis decides; ties go horizontal (the mirror of the drawer's
      // vertical tiebreak). Horizontal intent belongs to the page — hand it off
      // for good.
      if (Math.abs(dy) <= Math.abs(dx)) {
        phase = 'idle';
        return { kind: 'ignore' };
      }
      // Vertical. Only a downward pull dismisses; an upward drag is the inner
      // content scrolling back up at scrollTop 0, so it stays with the page.
      if (dy < 0) {
        phase = 'idle';
        return { kind: 'ignore' };
      }
      // Claimed on this very move — emit the drag it already implies, not track.
      phase = 'claimed';
      return { kind: 'drag', ...dragAt(point) };
    },

    end(point) {
      if (phase !== 'claimed') {
        // Taps and handed-off touches pass through untouched.
        reset();
        return { kind: 'ignore' };
      }
      // Release velocity runs from the oldest sample still inside the window to
      // the release point itself. A fast drag that ends in a hold emits no new
      // samples, so all of them age out of the window -> no in-window sample ->
      // v = 0: holding still is never a flick.
      const cutoff = point.t - VELOCITY_WINDOW_MS;
      const oldest = samples.find((s) => s.t >= cutoff);
      const v = oldest && point.t - oldest.t > 0 ? (point.y - oldest.y) / (point.t - oldest.t) : 0;

      let decision: SheetGestureDecision;
      if (Math.abs(v) >= FLICK_VELOCITY_PX_MS) {
        // A flick commits in the direction it points regardless of travel: a
        // fast downward throw dismisses even from a shallow drag, and a fast
        // upward throw settles back open even from past halfway.
        decision = v > 0 ? { kind: 'commit-dismiss' } : { kind: 'settle-open' };
      } else {
        // No flick: position decides. `>=` so a drag of exactly COMMIT_FRACTION
        // of the height dismisses.
        decision =
          dragAt(point).y >= COMMIT_FRACTION * (env as SheetGestureEnv).sheetHeight
            ? { kind: 'commit-dismiss' }
            : { kind: 'settle-open' };
      }
      reset();
      return decision;
    },

    cancel() {
      // A drag in progress needs the caller to animate back to the untouched
      // fully-open state (cancel-reset); anything else is a plain ignore.
      const claimed = phase === 'claimed';
      reset();
      return claimed ? { kind: 'cancel-reset' } : { kind: 'ignore' };
    },
  };
}
