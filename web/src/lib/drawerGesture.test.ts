// Follow-finger drawer gesture (issue #140). This suite pins the arbitration
// (who gets the touch: editables and scrolled scrollers never; axis dominance
// with ties going vertical; rightward-only claim when the drawer is closed),
// the per-move drag mapping (finger dx -> rail offset -px..0 and 0..1 open-ness,
// clamped on overshoot), and the release rule (a flick within the velocity
// window beats position in whichever direction it points; a stale/held release
// is never a flick; exactly COMMIT_FRACTION opens). Time only enters via the
// point timestamps — the module never reads a clock.

import { describe, expect, it } from 'vitest';
import {
  COMMIT_FRACTION,
  FLICK_VELOCITY_PX_MS,
  INTENT_PX,
  VELOCITY_WINDOW_MS,
  createDrawerGesture,
} from './drawerGesture';
import type { GestureDecision, GestureEnv, GesturePoint } from './drawerGesture';

// Test-local drawer width. Picked at 100 so a px offset reads directly as a
// percentage of open-ness; it is unrelated to VELOCITY_WINDOW_MS (also 100).
const WIDTH = 100;

const env = (over: Partial<GestureEnv> = {}): GestureEnv => ({
  drawerOpen: false,
  drawerWidth: WIDTH,
  inEditable: false,
  inScrolledScroller: false,
  ...over,
});

const pt = (x: number, y: number, t: number): GesturePoint => ({ x, y, t });

// Expected drag decision for a clamped rail offset. progress is derived with
// the same formula the module uses, so this pins the offset->open-ness mapping
// (a bug in either would break the exact-equality match) without hardcoding
// float literals.
const drag = (x: number): GestureDecision => ({
  kind: 'drag',
  x,
  progress: (x + WIDTH) / WIDTH,
});

describe('createDrawerGesture arbitration', () => {
  it('ignores a touch that starts in an editable, forever (composer never drags the drawer)', () => {
    const g = createDrawerGesture();
    expect(g.start(pt(0, 0, 0), env({ inEditable: true }))).toEqual({ kind: 'ignore' });
    // A clean rightward drag that would otherwise claim stays ignored.
    expect(g.move(pt(40, 0, 50))).toEqual({ kind: 'ignore' });
    expect(g.move(pt(80, 0, 100))).toEqual({ kind: 'ignore' });
    expect(g.end(pt(80, 0, 150))).toEqual({ kind: 'ignore' });
  });

  it('splits on scrollLeft: a scrolled scroller is ignored; the identical sequence otherwise claims', () => {
    const startAt = pt(0, 0, 0);
    const moveTo = pt(30, 0, 50);

    const scrolled = createDrawerGesture();
    expect(scrolled.start(startAt, env({ inScrolledScroller: true }))).toEqual({ kind: 'ignore' });
    expect(scrolled.move(moveTo)).toEqual({ kind: 'ignore' });

    const atLeft = createDrawerGesture();
    expect(atLeft.start(startAt, env({ inScrolledScroller: false }))).toEqual({ kind: 'track' });
    // dx=30 crosses INTENT_PX rightward while closed -> claimed on this move.
    expect(atLeft.move(moveTo)).toEqual(drag(-WIDTH + 30));
  });

  it('hands a vertical intent to the page for good, even if the finger later swings right', () => {
    const g = createDrawerGesture();
    expect(g.start(pt(0, 0, 0), env())).toEqual({ kind: 'track' });
    // |dy|=20 >= INTENT_PX and dominates |dx|=5 -> vertical -> ignore forever.
    expect(g.move(pt(5, 20, 50))).toEqual({ kind: 'ignore' });
    expect(g.move(pt(200, 20, 100))).toEqual({ kind: 'ignore' });
    expect(g.end(pt(200, 20, 150))).toEqual({ kind: 'ignore' });
  });

  it('hands off a leftward horizontal intent while closed (page keeps the drag)', () => {
    const g = createDrawerGesture();
    expect(g.start(pt(100, 0, 0), env())).toEqual({ kind: 'track' });
    // dx=-15 (horizontal, leftward) while closed -> nothing to reveal -> ignore.
    expect(g.move(pt(85, 2, 50))).toEqual({ kind: 'ignore' });
    expect(g.move(pt(50, 0, 100))).toEqual({ kind: 'ignore' });
    expect(g.end(pt(50, 0, 150))).toEqual({ kind: 'ignore' });
  });

  it('stays tracking (not claimed) until either axis reaches INTENT_PX', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    // Just shy of the threshold on both axes.
    expect(g.move(pt(INTENT_PX - 1, INTENT_PX - 1, 20))).toEqual({ kind: 'track' });
    // Reaching INTENT_PX horizontally (ties go vertical, so make dx strictly win).
    expect(g.move(pt(INTENT_PX, 0, 40))).toEqual(drag(-WIDTH + INTENT_PX));
  });

  it('ignores a start with non-positive drawer width', () => {
    const g = createDrawerGesture();
    expect(g.start(pt(0, 0, 0), env({ drawerWidth: 0 }))).toEqual({ kind: 'ignore' });
    expect(g.move(pt(40, 0, 50))).toEqual({ kind: 'ignore' });
  });
});

describe('createDrawerGesture dragging (closed drawer)', () => {
  it('emits a rising rail offset and open-ness, clamped on overshoot past the width', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    // base = -WIDTH; x = clamp(-WIDTH + dx, -WIDTH, 0).
    expect(g.move(pt(10, 0, 50))).toEqual(drag(-90)); // progress 0.1
    expect(g.move(pt(50, 0, 100))).toEqual(drag(-50)); // progress 0.5
    // dx=150 overshoots the width: x clamps to 0, progress clamps to 1.
    expect(g.move(pt(150, 0, 150))).toEqual({ kind: 'drag', x: 0, progress: 1 });
  });
});

describe('createDrawerGesture release (closed drawer)', () => {
  it('commits open on a slow release past half', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(60, 0, 100));
    g.move(pt(65, 0, 180)); // prunes the seed sample (t=0 < 80)
    // Oldest in-window sample at t=200 is (60,100): v=(66-60)/(200-100)=0.06 < FLICK.
    // Position: dx=66 -> progress 0.66 >= COMMIT_FRACTION -> open.
    expect(g.end(pt(66, 0, 200))).toEqual({ kind: 'commit-open' });
  });

  it('commits closed on a slow release below half', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(30, 0, 100));
    g.move(pt(35, 0, 180));
    // v=(36-30)/(200-100)=0.06 < FLICK; progress 0.36 < COMMIT_FRACTION -> closed.
    expect(g.end(pt(36, 0, 200))).toEqual({ kind: 'commit-closed' });
  });

  it('commits open on a rightward flick even though travel stayed below half', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(20, 0, 50));
    g.move(pt(40, 0, 90));
    // At t=100 the window reaches back to t=0, oldest sample (0,0):
    // v=(40-0)/(100-0)=0.4 >= FLICK_VELOCITY_PX_MS, rightward -> open,
    // despite progress 0.4 < COMMIT_FRACTION.
    expect(FLICK_VELOCITY_PX_MS).toBeLessThanOrEqual(0.4);
    expect(g.end(pt(40, 0, 100))).toEqual({ kind: 'commit-open' });
  });

  it('lets flick direction beat position: past half, then a fast leftward throw closes', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    expect(g.move(pt(90, 0, 100))).toEqual(drag(-10)); // dragged well past half (progress 0.9)
    g.move(pt(55, 0, 190)); // throwing back left; prunes the seed (t=0 < 90)
    // At t=200 the window starts at t=100, oldest in-window sample (90,100):
    // v=(55-90)/(200-100)=-0.35 -> |v| >= FLICK, leftward -> closed,
    // even though end progress 0.55 >= COMMIT_FRACTION.
    expect(g.end(pt(55, 0, 200))).toEqual({ kind: 'commit-closed' });
  });

  it('treats a quick drag then a long hold as no flick (stale samples -> position decides)', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(60, 0, 10)); // fast rightward: 6 px/ms instantaneously
    g.move(pt(30, 0, 20)); // finger settles back to 30
    // Release comes VELOCITY_WINDOW_MS+ after the last move, so every sample is
    // stale (t < 200-100) -> no in-window sample -> v=0, not a flick. Position:
    // dx=30 -> progress 0.3 < COMMIT_FRACTION -> closed (momentum is not carried).
    const late = 20 + VELOCITY_WINDOW_MS + 80;
    expect(g.end(pt(30, 0, late))).toEqual({ kind: 'commit-closed' });
  });

  it('opens on a release whose progress is exactly COMMIT_FRACTION (>= boundary)', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    const half = WIDTH * COMMIT_FRACTION; // dx that lands progress exactly at the fraction
    expect(g.move(pt(half, 0, 100))).toEqual(drag(-WIDTH + half));
    // Release far later so no sample is in-window -> v=0 -> pure position rule.
    expect(g.end(pt(half, 0, 100 + VELOCITY_WINDOW_MS + 50))).toEqual({ kind: 'commit-open' });
  });
});

describe('createDrawerGesture open drawer (symmetric close)', () => {
  const open = env({ drawerOpen: true });

  it('claims a rightward drag, clamps the rail at 0 / progress 1, commits open', () => {
    const g = createDrawerGesture();
    g.start(pt(100, 0, 0), open);
    // base=0; rightward dx has nothing further to open -> x clamps to 0.
    expect(g.move(pt(130, 0, 100))).toEqual({ kind: 'drag', x: 0, progress: 1 });
    expect(g.end(pt(130, 0, 100 + VELOCITY_WINDOW_MS + 50))).toEqual({ kind: 'commit-open' });
  });

  it('closes on a leftward drag past half', () => {
    const g = createDrawerGesture();
    g.start(pt(200, 0, 0), open);
    expect(g.move(pt(140, 0, 100))).toEqual(drag(-60)); // progress 0.4, past the closing half
    // Stale release -> v=0 -> position 0.4 < COMMIT_FRACTION -> closed.
    expect(g.end(pt(140, 0, 100 + VELOCITY_WINDOW_MS + 50))).toEqual({ kind: 'commit-closed' });
  });

  it('stays open on a small slow leftward drag', () => {
    const g = createDrawerGesture();
    g.start(pt(200, 0, 0), open);
    expect(g.move(pt(185, 0, 100))).toEqual(drag(-15)); // progress 0.85
    expect(g.end(pt(185, 0, 100 + VELOCITY_WINDOW_MS + 50))).toEqual({ kind: 'commit-open' });
  });

  it('closes on a short leftward flick even when released past the open half', () => {
    const g = createDrawerGesture();
    g.start(pt(200, 0, 0), open);
    g.move(pt(190, 0, 100));
    g.move(pt(160, 0, 190)); // prunes the seed (t=0 < 90)
    // Oldest in-window sample at t=200 is (190,100):
    // v=(155-190)/(200-100)=-0.35 -> |v| >= FLICK, leftward -> closed,
    // although end progress 0.55 >= COMMIT_FRACTION would keep it open.
    expect(g.end(pt(155, 0, 200))).toEqual({ kind: 'commit-closed' });
  });
});

describe('createDrawerGesture cancel and taps', () => {
  it('cancel-resets a drag in progress', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    g.move(pt(40, 0, 50)); // claimed
    expect(g.cancel()).toEqual({ kind: 'cancel-reset' });
    // State reset: a fresh touch tracks again.
    expect(g.start(pt(0, 0, 0), env())).toEqual({ kind: 'track' });
  });

  it('ignores cancel before a claim (still tracking, and before any start)', () => {
    const tracking = createDrawerGesture();
    tracking.start(pt(0, 0, 0), env());
    tracking.move(pt(2, 0, 10)); // below INTENT_PX -> still tracking, not claimed
    expect(tracking.cancel()).toEqual({ kind: 'ignore' });

    const fresh = createDrawerGesture();
    expect(fresh.cancel()).toEqual({ kind: 'ignore' });
  });

  it('ignores a tap that never crosses INTENT_PX (end without a claim passes through)', () => {
    const g = createDrawerGesture();
    g.start(pt(0, 0, 0), env());
    expect(g.move(pt(2, 1, 10))).toEqual({ kind: 'track' });
    expect(g.end(pt(3, 1, 20))).toEqual({ kind: 'ignore' });
  });

  it('ignores move/end when never started', () => {
    const g = createDrawerGesture();
    expect(g.move(pt(40, 0, 0))).toEqual({ kind: 'ignore' });
    expect(g.end(pt(40, 0, 10))).toEqual({ kind: 'ignore' });
  });
});
