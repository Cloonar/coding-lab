// Follow-finger bottom-sheet dismiss gesture (issue #145). This suite pins the
// arbitration (who gets the touch: editables and vertically-scrolled scrollers
// never; axis dominance with ties going horizontal; downward-only claim), the
// per-move drag mapping (finger dy -> translateY 0..height and 1..0 open-ness,
// clamped on overshoot and on reversal above the top), and the release rule (a
// flick within the velocity window beats position in whichever direction it
// points; a stale/held release is never a flick; a drag of exactly
// COMMIT_FRACTION dismisses). Time only enters via the point timestamps — the
// module never reads a clock. The tuning constants are imported from
// './drawerGesture' because the two gestures deliberately share feel.

import { describe, expect, it } from 'vitest';
import {
  COMMIT_FRACTION,
  FLICK_VELOCITY_PX_MS,
  INTENT_PX,
  VELOCITY_WINDOW_MS,
} from './drawerGesture';
import type { GesturePoint } from './drawerGesture';
import { createSheetGesture } from './sheetGesture';
import type { SheetGestureDecision, SheetGestureEnv } from './sheetGesture';

// Test-local sheet height. Picked at 100 so a translateY reads directly as a
// percentage of dismissed-ness; it is unrelated to VELOCITY_WINDOW_MS (also 100).
const HEIGHT = 100;

const env = (over: Partial<SheetGestureEnv> = {}): SheetGestureEnv => ({
  sheetHeight: HEIGHT,
  inEditable: false,
  inScrolledScroller: false,
  ...over,
});

const pt = (x: number, y: number, t: number): GesturePoint => ({ x, y, t });

// Expected drag decision for a clamped translateY. progress is derived with the
// same formula the module uses, so this pins the offset->open-ness mapping (a bug
// in either would break the exact-equality match) without hardcoding float
// literals.
const drag = (y: number): SheetGestureDecision => ({
  kind: 'drag',
  y,
  progress: (HEIGHT - y) / HEIGHT,
});

describe('createSheetGesture arbitration', () => {
  it('ignores a touch that starts in an editable, forever (composer never drags the sheet)', () => {
    const g = createSheetGesture();
    expect(g.start(pt(0, 0, 0), env({ inEditable: true }))).toEqual({ kind: 'ignore' });
    // A clean downward drag that would otherwise claim stays ignored.
    expect(g.move(pt(0, 40, 50))).toEqual({ kind: 'ignore' });
    expect(g.move(pt(0, 80, 100))).toEqual({ kind: 'ignore' });
    expect(g.end(pt(0, 80, 150))).toEqual({ kind: 'ignore' });
  });

  it('splits on scrollTop: a scrolled scroller is ignored; the identical sequence otherwise claims', () => {
    const startAt = pt(0, 0, 0);
    const moveTo = pt(0, 30, 50);

    const scrolled = createSheetGesture();
    expect(scrolled.start(startAt, env({ inScrolledScroller: true }))).toEqual({ kind: 'ignore' });
    expect(scrolled.move(moveTo)).toEqual({ kind: 'ignore' });

    const atTop = createSheetGesture();
    expect(atTop.start(startAt, env({ inScrolledScroller: false }))).toEqual({ kind: 'track' });
    // dy=30 crosses INTENT_PX downward -> claimed on this move.
    expect(atTop.move(moveTo)).toEqual(drag(30));
  });

  it('hands a horizontal intent to the page for good, even if the finger later swings down', () => {
    const g = createSheetGesture();
    expect(g.start(pt(0, 0, 0), env())).toEqual({ kind: 'track' });
    // |dx|=20 >= INTENT_PX and dominates |dy|=5 -> horizontal -> ignore forever.
    expect(g.move(pt(20, 5, 50))).toEqual({ kind: 'ignore' });
    expect(g.move(pt(20, 200, 100))).toEqual({ kind: 'ignore' });
    expect(g.end(pt(20, 200, 150))).toEqual({ kind: 'ignore' });
  });

  it('hands off an upward vertical intent (inner content scroll keeps the drag)', () => {
    const g = createSheetGesture();
    expect(g.start(pt(0, 100, 0), env())).toEqual({ kind: 'track' });
    // dy=-15 (vertical, upward) -> nothing to dismiss upward -> ignore.
    expect(g.move(pt(2, 85, 50))).toEqual({ kind: 'ignore' });
    expect(g.move(pt(0, 50, 100))).toEqual({ kind: 'ignore' });
    expect(g.end(pt(0, 50, 150))).toEqual({ kind: 'ignore' });
  });

  it('stays tracking (not claimed) until either axis reaches INTENT_PX', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    // Just shy of the threshold on both axes.
    expect(g.move(pt(INTENT_PX - 1, INTENT_PX - 1, 20))).toEqual({ kind: 'track' });
    // Reaching INTENT_PX vertically (ties go horizontal, so make dy strictly win).
    expect(g.move(pt(0, INTENT_PX, 40))).toEqual(drag(INTENT_PX));
  });

  it('ignores a start with non-positive sheet height', () => {
    const g = createSheetGesture();
    expect(g.start(pt(0, 0, 0), env({ sheetHeight: 0 }))).toEqual({ kind: 'ignore' });
    expect(g.move(pt(0, 40, 50))).toEqual({ kind: 'ignore' });
  });
});

describe('createSheetGesture dragging', () => {
  it('emits a rising translateY and falling open-ness, clamped on overshoot past the height', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    // y = clamp(dy, 0, HEIGHT).
    expect(g.move(pt(0, 10, 50))).toEqual(drag(10)); // progress 0.9
    expect(g.move(pt(0, 50, 100))).toEqual(drag(50)); // progress 0.5
    // dy=150 overshoots the height: y clamps to HEIGHT, progress clamps to 0.
    expect(g.move(pt(0, 150, 150))).toEqual({ kind: 'drag', y: HEIGHT, progress: 0 });
  });

  it('clamps translateY to 0 when the finger reverses above the sheet top', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    expect(g.move(pt(0, 40, 50))).toEqual(drag(40)); // claimed downward
    // Finger swings back up past the start: dy negative -> y clamps at 0, fully
    // open again (progress 1), still a drag because the touch is already claimed.
    expect(g.move(pt(0, -30, 100))).toEqual({ kind: 'drag', y: 0, progress: 1 });
  });
});

describe('createSheetGesture release', () => {
  it('commits dismiss on a slow release past half', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(0, 60, 100));
    g.move(pt(0, 65, 180)); // prunes the seed sample (t=0 < 80)
    // Oldest in-window sample at t=200 is (60,100): v=(66-60)/(200-100)=0.06 < FLICK.
    // Position: y=66 >= COMMIT_FRACTION*HEIGHT (50) -> dismiss.
    expect(g.end(pt(0, 66, 200))).toEqual({ kind: 'commit-dismiss' });
  });

  it('settles open on a slow release below half', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(0, 30, 100));
    g.move(pt(0, 35, 180));
    // v=(36-30)/(200-100)=0.06 < FLICK; y=36 < 50 -> settle-open.
    expect(g.end(pt(0, 36, 200))).toEqual({ kind: 'settle-open' });
  });

  it('commits dismiss on a downward flick even though travel stayed below half', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(0, 20, 50));
    g.move(pt(0, 40, 90));
    // At t=100 the window reaches back to t=0, oldest sample (0,0):
    // v=(40-0)/(100-0)=0.4 >= FLICK_VELOCITY_PX_MS, downward -> dismiss,
    // despite y 40 < 50.
    expect(FLICK_VELOCITY_PX_MS).toBeLessThanOrEqual(0.4);
    expect(g.end(pt(0, 40, 100))).toEqual({ kind: 'commit-dismiss' });
  });

  it('lets flick direction beat position: past half, then a fast upward throw settles open', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    expect(g.move(pt(0, 90, 100))).toEqual(drag(90)); // dragged well past half (progress 0.1)
    g.move(pt(0, 55, 190)); // throwing back up; prunes the seed (t=0 < 90)
    // At t=200 the window starts at t=100, oldest in-window sample (90,100):
    // v=(55-90)/(200-100)=-0.35 -> |v| >= FLICK, upward -> settle-open,
    // even though end y 55 >= 50.
    expect(g.end(pt(0, 55, 200))).toEqual({ kind: 'settle-open' });
  });

  it('treats a quick drag then a long hold as no flick (stale samples -> position decides)', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(0, 60, 10)); // fast downward: 6 px/ms instantaneously
    g.move(pt(0, 30, 20)); // finger settles back to 30
    // Release comes VELOCITY_WINDOW_MS+ after the last move, so every sample is
    // stale (t < 200-100) -> no in-window sample -> v=0, not a flick. Position:
    // y=30 < 50 -> settle-open (momentum is not carried).
    const late = 20 + VELOCITY_WINDOW_MS + 80;
    expect(g.end(pt(0, 30, late))).toEqual({ kind: 'settle-open' });
  });

  it('dismisses on a release whose drag is exactly COMMIT_FRACTION of the height (>= boundary)', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    const half = HEIGHT * COMMIT_FRACTION; // dy that lands the drag exactly at the fraction
    expect(g.move(pt(0, half, 100))).toEqual(drag(half));
    // Release far later so no sample is in-window -> v=0 -> pure position rule.
    expect(g.end(pt(0, half, 100 + VELOCITY_WINDOW_MS + 50))).toEqual({ kind: 'commit-dismiss' });
  });
});

describe('createSheetGesture cancel and taps', () => {
  it('cancel-resets a drag in progress', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(0, 40, 50)); // claimed
    expect(g.cancel()).toEqual({ kind: 'cancel-reset' });
    // State reset: a fresh touch tracks again.
    expect(g.start(pt(0, 0, 0), env())).toEqual({ kind: 'track' });
  });

  it('ignores cancel before a claim (still tracking, and before any start)', () => {
    const tracking = createSheetGesture();
    tracking.start(pt(0, 0, 0), env());
    tracking.move(pt(0, 2, 10)); // below INTENT_PX -> still tracking, not claimed
    expect(tracking.cancel()).toEqual({ kind: 'ignore' });

    const fresh = createSheetGesture();
    expect(fresh.cancel()).toEqual({ kind: 'ignore' });
  });

  it('ignores a tap that never crosses INTENT_PX (end without a claim passes through)', () => {
    const g = createSheetGesture();
    g.start(pt(0, 0, 0), env());
    expect(g.move(pt(1, 2, 10))).toEqual({ kind: 'track' });
    expect(g.end(pt(1, 3, 20))).toEqual({ kind: 'ignore' });
  });

  it('ignores move/end when never started', () => {
    const g = createSheetGesture();
    expect(g.move(pt(0, 40, 0))).toEqual({ kind: 'ignore' });
    expect(g.end(pt(0, 40, 10))).toEqual({ kind: 'ignore' });
  });
});
