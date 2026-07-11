// Follow-finger drawer gesture state machine (issue #140): a rightward drag
// starting ANYWHERE on the mobile viewport drags the drawer out with the
// finger; while open, a leftward drag anywhere (drawer body or scrim) drags it
// back. This replaces the old 24px left edge-zone snap, which collided with
// the OS edge gestures (iOS back swipe, Android gesture nav) — the OS keeps
// the edge; we take the surface.
//
// Pure module (house pattern): consumes touch points plus environment answers
// resolved by the caller at touch start (drawer open? drawer width? editable
// target? horizontally-scrolled ancestor?) and emits decisions. It never
// touches the DOM — AppShell binds the listeners, answers the environment
// queries, and applies transforms/classes.
//
// Arbitration (who gets the touch):
//   - Horizontal scrollers win unless fully left: a touch starting inside an
//     ancestor that can scroll horizontally AND has scrollLeft > 0 is left
//     entirely to the page. At scrollLeft === 0 a rightward pull has nothing
//     to scroll, so the drawer may claim it (mirrors iOS back-swipe vs
//     carousel behavior). The caller answers this via env.inScrolledScroller.
//   - Editables excluded: touches starting in textarea / input / select /
//     contenteditable (the composer!) never begin drawer tracking —
//     horizontal drags there are cursor/selection.
//   - Axis dominance: after INTENT_PX of travel the dominant axis decides;
//     vertical intent hands the touch to the page for good (the chat log and
//     the drawer's nav list keep scrolling).
//   - Rightward-only claim when closed: with the drawer closed, only a
//     rightward horizontal intent claims the touch; leftward horizontal drags
//     always stay with the page. While open, both directions track (a
//     rightward drag just holds the drawer at fully open).
//   - No edge special case: edge starts are tracked like anywhere else. Where
//     the OS owns the edge it steals the touch and the page gets touchcancel
//     -> cancel-reset (animate back to the pre-drag state).
//
// Commit rule on release: open if dragged past COMMIT_FRACTION of the drawer
// width OR the release velocity is a flick (>= FLICK_VELOCITY_PX_MS); a flick
// wins over position in whichever direction it points, so a fast leftward
// throw closes even from past halfway. Mirror rule for closing.

/** Travel (px) before horizontal vs vertical intent is decided. */
export const INTENT_PX = 8;
/** Fraction of the drawer width dragged past which a release commits. */
export const COMMIT_FRACTION = 0.5;
/** Release speed (px/ms) that commits regardless of travel. Tuning knob, not
 * spec — validated by a real-device feel pass, not by the unit tests. */
export const FLICK_VELOCITY_PX_MS = 0.3;
/** Only finger samples this recent (ms) count toward the release velocity, so
 * a fast drag followed by a hold-still release is not a flick. */
export const VELOCITY_WINDOW_MS = 100;

/** A finger position: viewport client px plus the event timestamp in ms. */
export type GesturePoint = { x: number; y: number; t: number };

/** Environment answers resolved by the caller when the touch starts. */
export type GestureEnv = {
  /** Was the drawer open when the touch started? Fixes the drag's base
   * offset and the target of a cancel-reset. */
  drawerOpen: boolean;
  /** Rendered drawer width in px (260 or 85vw), measured at touch start. */
  drawerWidth: number;
  /** Touch target is inside input/textarea/select/contenteditable. */
  inEditable: boolean;
  /** Some ancestor of the target scrolls horizontally and has scrollLeft>0. */
  inScrolledScroller: boolean;
};

export type GestureDecision =
  /** Not our touch (or handed to the page for good) — do nothing. */
  | { kind: 'ignore' }
  /** Watching, intent undecided — do NOT preventDefault yet. */
  | { kind: 'track' }
  /** Claimed: preventDefault, translate the rail to x px (-width..0) and set
   * the scrim opacity to progress (0..1 open-ness). */
  | { kind: 'drag'; x: number; progress: number }
  /** Release commits open: settle the drawer open. */
  | { kind: 'commit-open' }
  /** Release commits closed: settle the drawer closed. */
  | { kind: 'commit-closed' }
  /** Touch was stolen (OS edge gesture) mid-drag: animate back to the
   * pre-drag state — the caller's open/closed signal never changed. */
  | { kind: 'cancel-reset' };

export type DrawerGesture = {
  /** Feed the initial touch point. Returns 'ignore' (editable target,
   * scrolled scroller, non-positive width) or 'track'. */
  start(point: GesturePoint, env: GestureEnv): GestureDecision;
  /** Feed a move of the tracked touch. Returns 'track' before intent,
   * 'ignore' once handed to the page, 'drag' once claimed. */
  move(point: GesturePoint): GestureDecision;
  /** Feed the release. Returns 'ignore' if the touch was never claimed
   * (taps pass through), else 'commit-open' / 'commit-closed'. */
  end(point: GesturePoint): GestureDecision;
  /** Feed a touchcancel. Returns 'cancel-reset' if a drag was in progress,
   * else 'ignore'. */
  cancel(): GestureDecision;
};

/** One reusable tracker; each start() begins a fresh touch. */
export function createDrawerGesture(): DrawerGesture {
  // 'idle' collapses the three "not our touch" situations — never started,
  // permanently ignored (editable / scrolled scroller / zero width), and handed
  // to the page (vertical or leftward-closed intent) — because move/end/cancel
  // treat all three identically: ignore, and reset on the terminal events. Only
  // 'tracking' (intent undecided) and 'claimed' (dragging) do real work.
  let phase: 'idle' | 'tracking' | 'claimed' = 'idle';
  let startPoint: GesturePoint | null = null;
  let env: GestureEnv | null = null;
  // Finger samples (client x + timestamp) kept only to measure the release
  // velocity, in ascending-time order so the oldest sample still inside the
  // velocity window is simply the first one at or past the window threshold.
  let samples: { x: number; t: number }[] = [];

  const reset = () => {
    phase = 'idle';
    startPoint = null;
    env = null;
    samples = [];
  };

  const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));

  // Rail offset (-width..0) and open-ness (0..1) for the CURRENT finger dx,
  // recomputed per event. `base` is where the rail rests at touch start: 0 when
  // already open, -width when closed, so the finger drags from that offset.
  const dragAt = (point: GesturePoint): { x: number; progress: number } => {
    const e = env as GestureEnv;
    const base = e.drawerOpen ? 0 : -e.drawerWidth;
    const x = clamp(base + (point.x - (startPoint as GesturePoint).x), -e.drawerWidth, 0);
    return { x, progress: (x + e.drawerWidth) / e.drawerWidth };
  };

  const record = (point: GesturePoint) => {
    samples.push({ x: point.x, t: point.t });
    // Prune relative to THIS sample's time, so a finger that keeps moving keeps
    // the window fresh — and one that stops flushes stale samples on the way to
    // the (velocity-less) release.
    const cutoff = point.t - VELOCITY_WINDOW_MS;
    samples = samples.filter((s) => s.t >= cutoff);
  };

  return {
    start(point, e) {
      reset();
      // Editable target (the composer), a horizontally-scrolled ancestor, or a
      // non-positive drawer width: never ours. Stay 'idle' so every later event
      // on this touch ignores.
      if (e.inEditable || e.inScrolledScroller || e.drawerWidth <= 0) {
        return { kind: 'ignore' };
      }
      startPoint = point;
      env = e;
      samples = [{ x: point.x, t: point.t }];
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
      // Dominant axis decides; ties go vertical (mirrors the old inline
      // `Math.abs(dx) > Math.abs(dy) ? 'horizontal' : 'vertical'`). Vertical
      // intent belongs to the scrolling content — hand it off for good.
      if (Math.abs(dx) <= Math.abs(dy)) {
        phase = 'idle';
        return { kind: 'ignore' };
      }
      // Horizontal. Closed, only a rightward pull can reveal the drawer, so a
      // leftward drag stays with the page; open, both directions are ours (a
      // rightward drag just holds it fully open).
      if (!(env as GestureEnv).drawerOpen && dx < 0) {
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
      const v = oldest && point.t - oldest.t > 0 ? (point.x - oldest.x) / (point.t - oldest.t) : 0;

      let decision: GestureDecision;
      if (Math.abs(v) >= FLICK_VELOCITY_PX_MS) {
        // A flick commits in the direction it points regardless of travel: a
        // fast leftward throw closes even from past halfway, and vice versa.
        decision = v > 0 ? { kind: 'commit-open' } : { kind: 'commit-closed' };
      } else {
        // No flick: position decides, `>=` so exactly COMMIT_FRACTION opens.
        decision =
          dragAt(point).progress >= COMMIT_FRACTION
            ? { kind: 'commit-open' }
            : { kind: 'commit-closed' };
      }
      reset();
      return decision;
    },

    cancel() {
      // A drag in progress needs the caller to animate back to the untouched
      // open/closed state (cancel-reset); anything else is a plain ignore.
      const claimed = phase === 'claimed';
      reset();
      return claimed ? { kind: 'cancel-reset' } : { kind: 'ignore' };
    },
  };
}
