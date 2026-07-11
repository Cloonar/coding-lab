// Install-promotion truth table (issue #142). The point of the seam is that the
// Android `beforeinstallprompt` arrives LATE and ONCE: these tests drive it as a
// real dispatched event on an injected target and assert the sheet flips open
// reactively, the event is spent exactly once, and every eligibility gate flips
// independently. matchMedia is injected (jsdom has none) so the whole platform
// matrix — iOS, iPadOS-as-Mac, standalone, fine-pointer — is exercisable, and a
// missing matchMedia degrades to "no match", never a throw.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { createInstall, install, type InstallDeps, type InstallVariant } from './install';

const DISMISSED_KEY = 'lab.install-dismissed';

const ANDROID_UA =
  'Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Mobile Safari/537.36';
const IPHONE_UA =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1';
// iPadOS reports a desktop-Safari (Macintosh) UA; only a touch screen unmasks it.
const MAC_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15';

interface MemStore {
  map: Map<string, string>;
  getItem(k: string): string | null;
  setItem(k: string, v: string): void;
  removeItem(k: string): void;
}

function memStore(initial?: Record<string, string>): MemStore {
  const map = new Map<string, string>(Object.entries(initial ?? {}));
  return {
    map,
    getItem: (k) => map.get(k) ?? null,
    setItem: (k, v) => void map.set(k, v),
    removeItem: (k) => void map.delete(k),
  };
}

interface EnvOpts {
  ua?: string;
  maxTouchPoints?: number;
  standalone?: boolean; // navigator.standalone (iOS legacy installed flag)
  coarse?: boolean; // (pointer: coarse) matches
  displayStandalone?: boolean; // (display-mode: standalone) matches
  dismissed?: boolean; // pre-existing persisted dismissal
  matchMediaAbsent?: boolean; // omit matchMedia entirely (jsdom-like)
}

interface Env {
  target: EventTarget;
  store: MemStore;
  deps: InstallDeps;
}

function makeEnv(opts: EnvOpts = {}): Env {
  const media: Record<string, boolean> = {
    '(display-mode: standalone)': opts.displayStandalone ?? false,
    '(pointer: coarse)': opts.coarse ?? true, // phone default
  };
  const target = new EventTarget();
  const store = memStore(opts.dismissed ? { [DISMISSED_KEY]: '1' } : {});
  const deps: InstallDeps = {
    target,
    matchMedia: opts.matchMediaAbsent ? undefined : (q) => ({ matches: media[q] ?? false }),
    navigator: {
      userAgent: opts.ua ?? ANDROID_UA,
      maxTouchPoints: opts.maxTouchPoints ?? 5,
      standalone: opts.standalone,
    },
    storage: store,
  };
  return { target, store, deps };
}

// Track controllers so their listeners are torn down between tests.
const live: Array<ReturnType<typeof createInstall>> = [];
function create(env: Env): ReturnType<typeof createInstall> {
  const c = createInstall(env.deps);
  live.push(c);
  return c;
}
afterEach(() => {
  while (live.length) live.pop()?.dispose();
});

/** A cancelable beforeinstallprompt carrying a mocked one-shot prompt(). */
function makeBIP(prompt = vi.fn(() => Promise.resolve())) {
  const event = Object.assign(new Event('beforeinstallprompt', { cancelable: true }), { prompt });
  return { event: event as unknown as Event, prompt };
}

const flush = () => new Promise<void>((r) => setTimeout(r, 0));

describe('capture', () => {
  it('captures beforeinstallprompt: preventDefault fires, event() stashes it, eligible flips true', () => {
    const env = makeEnv(); // android, coarse, non-standalone
    const c = create(env);

    // Nothing installable yet: no event, so the Android sheet stays closed.
    expect(c.event()).toBeNull();
    expect(c.eligible()).toBe(false);
    expect(c.open()).toBe(false);

    const { event } = makeBIP();
    env.target.dispatchEvent(event);

    // preventDefault() suppressed Chrome's mini-infobar (observable as
    // defaultPrevented on the cancelable event we dispatched).
    expect(event.defaultPrevented).toBe(true);
    expect(c.event()).toBe(event);
    // The late event flips the auto-show on its own — the reactive point.
    expect(c.eligible()).toBe(true);
    expect(c.open()).toBe(true);
  });
});

describe('eligible truth table', () => {
  const withEvent = (env: Env, c: ReturnType<typeof create>) => {
    env.target.dispatchEvent(makeBIP().event);
    return c;
  };

  it('android with event, coarse, non-standalone, not dismissed → eligible', () => {
    const env = makeEnv();
    const c = withEvent(env, create(env));
    expect(c.eligible()).toBe(true);
  });

  it('display-mode standalone gates it out', () => {
    const env = makeEnv({ displayStandalone: true });
    const c = withEvent(env, create(env));
    expect(c.eligible()).toBe(false);
  });

  it('navigator.standalone (iOS legacy) gates it out', () => {
    const env = makeEnv({ ua: IPHONE_UA, standalone: true });
    const c = create(env); // iOS needs no event
    expect(c.eligible()).toBe(false);
  });

  it('a fine pointer (not coarse) gates it out', () => {
    const env = makeEnv({ coarse: false });
    const c = withEvent(env, create(env));
    expect(c.eligible()).toBe(false);
  });

  it('a pre-existing dismissed flag gates it out', () => {
    const env = makeEnv({ dismissed: true });
    const c = withEvent(env, create(env));
    expect(c.eligible()).toBe(false);
  });

  it('android WITHOUT the event is not eligible (no dead Install button)', () => {
    const env = makeEnv();
    const c = create(env); // never fire beforeinstallprompt
    expect(c.eligible()).toBe(false);
  });

  it('iOS is eligible with NO event (Share-sheet install, no beforeinstallprompt)', () => {
    const env = makeEnv({ ua: IPHONE_UA });
    const c = create(env);
    expect(c.variant()).toBe('ios');
    expect(c.eligible()).toBe(true);
  });

  it('appinstalled gates it out even with the event still nominally in hand', () => {
    const env = makeEnv();
    const c = withEvent(env, create(env));
    expect(c.eligible()).toBe(true);
    env.target.dispatchEvent(new Event('appinstalled'));
    expect(c.eligible()).toBe(false);
  });
});

describe('variant', () => {
  const cases: Array<[string, EnvOpts, InstallVariant]> = [
    ['iPhone UA', { ua: IPHONE_UA }, 'ios'],
    ['iPadOS (Mac UA + touch)', { ua: MAC_UA, maxTouchPoints: 5 }, 'ios'],
    ['desktop Mac (Mac UA, no touch)', { ua: MAC_UA, maxTouchPoints: 0 }, 'android'],
    ['Android UA', { ua: ANDROID_UA }, 'android'],
  ];
  for (const [name, opts, expected] of cases) {
    it(`${name} → ${expected}`, () => {
      expect(create(makeEnv(opts)).variant()).toBe(expected);
    });
  }
});

describe('dismiss', () => {
  it('closes the sheet and persists the flag; a fresh controller stays hidden', () => {
    const env = makeEnv({ ua: IPHONE_UA }); // iOS: eligible with no event
    const c = create(env);
    expect(c.open()).toBe(true);

    c.dismiss();
    expect(c.open()).toBe(false);
    expect(env.store.map.get(DISMISSED_KEY)).toBe('1');

    // A controller booting against the same storage sees the flag → hidden.
    const fresh = create(env);
    expect(fresh.eligible()).toBe(false);
    expect(fresh.open()).toBe(false);
  });
});

describe('openFromSettings', () => {
  it('clears the dismissal and forces open even after a prior dismiss', () => {
    const env = makeEnv({ ua: IPHONE_UA });
    const c = create(env);
    c.dismiss();
    expect(c.open()).toBe(false);

    c.openFromSettings();
    expect(env.store.map.has(DISMISSED_KEY)).toBe(false);
    expect(c.open()).toBe(true);
  });

  it('opens even when an auto-show gate (coarse) fails — the manual re-entry', () => {
    const env = makeEnv({ ua: IPHONE_UA, coarse: false });
    const c = create(env);
    expect(c.eligible()).toBe(false); // coarse gate fails auto-show
    c.openFromSettings();
    expect(c.open()).toBe(true); // but Settings still forces it open
  });
});

describe('appinstalled', () => {
  it('closes the sheet, drops the event, and hides the Settings row', () => {
    const env = makeEnv();
    const c = create(env);
    env.target.dispatchEvent(makeBIP().event);
    expect(c.open()).toBe(true);
    expect(c.settingsRowVisible()).toBe(true);

    env.target.dispatchEvent(new Event('appinstalled'));
    expect(c.open()).toBe(false);
    expect(c.event()).toBeNull();
    expect(c.settingsRowVisible()).toBe(false);
  });
});

describe('settingsRowVisible', () => {
  it('is hidden in standalone (already installed)', () => {
    const env = makeEnv({ ua: IPHONE_UA, displayStandalone: true });
    expect(create(env).settingsRowVisible()).toBe(false);
  });

  it('is shown on iOS otherwise (no event needed, no coarse gate)', () => {
    const env = makeEnv({ ua: IPHONE_UA, coarse: false });
    expect(create(env).settingsRowVisible()).toBe(true);
  });

  it('is hidden on Android without the event, shown once it arrives', () => {
    const env = makeEnv();
    const c = create(env);
    expect(c.settingsRowVisible()).toBe(false);
    env.target.dispatchEvent(makeBIP().event);
    expect(c.settingsRowVisible()).toBe(true);
  });
});

describe('promptInstall', () => {
  it('fires the native prompt once, drops the stash, and closes the sheet', () => {
    const env = makeEnv();
    const c = create(env);
    const { event, prompt } = makeBIP();
    env.target.dispatchEvent(event);
    expect(c.open()).toBe(true);

    c.promptInstall();
    expect(prompt).toHaveBeenCalledOnce();
    expect(c.event()).toBeNull(); // one-shot: spent
    expect(c.open()).toBe(false); // native UI takes over; no dead button

    c.promptInstall(); // idempotent no-op once spent
    expect(prompt).toHaveBeenCalledOnce();
  });

  it('swallows a rejected prompt() without an unhandled rejection', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const env = makeEnv();
    const c = create(env);
    const { event } = makeBIP(vi.fn(() => Promise.reject(new Error('user declined'))));
    env.target.dispatchEvent(event);

    expect(() => c.promptInstall()).not.toThrow();
    expect(c.event()).toBeNull();
    await flush();
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
  });
});

describe('matchMedia absent (jsdom)', () => {
  it('degrades detections to false without throwing, keeping the logic functional', () => {
    const env = makeEnv({ ua: IPHONE_UA, matchMediaAbsent: true });
    const c = create(env);

    // standalone/coarse both read false (no media), so nothing throws.
    expect(() => c.eligible()).not.toThrow();
    // coarse is false → the auto-show is gated out even on iOS...
    expect(c.eligible()).toBe(false);
    // ...but the Settings row (no coarse gate) still shows, and manual open works.
    expect(c.settingsRowVisible()).toBe(true);
    c.openFromSettings();
    expect(c.open()).toBe(true);
  });

  it('android with matchMedia absent: row appears on the event, auto-show does not', () => {
    const env = makeEnv({ matchMediaAbsent: true });
    const c = create(env);
    env.target.dispatchEvent(makeBIP().event);
    expect(c.settingsRowVisible()).toBe(true);
    expect(c.eligible()).toBe(false); // coarse unknown → false
  });
});

describe('module singleton', () => {
  it('imports safely under jsdom and returns sane detections (no throw)', () => {
    expect(() => install.eligible()).not.toThrow();
    expect(() => install.open()).not.toThrow();
    expect(() => install.settingsRowVisible()).not.toThrow();
    expect(['ios', 'android']).toContain(install.variant());
  });
});
