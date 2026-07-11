// Install-promotion logic for the phone PWA install sheet (issue #142).
//
// Two things make this headless module worth its own tested seam. First,
// Chromium fires `beforeinstallprompt` ONCE, seconds after load, and only when
// the site is actually installable — so the decision to show an Install
// affordance is inherently reactive and time-shifted, not a synchronous read at
// mount. Second, the event is a scarce, one-shot resource: it must be captured
// the instant it fires (calling preventDefault() to suppress Chrome's own
// mini-infobar, so our sheet is the only prompt the user sees) and then spent
// exactly once. The module-scope `install` singleton exists precisely so that
// capture is wired up at app-module init — before any component mounts — since
// the browser will not re-fire the event if we were listening too late.
//
// iOS has no such event: Safari never fires `beforeinstallprompt` and installs
// only via the manual Share -> Add to Home Screen flow, so the iOS variant is
// eligible purely on the platform/standalone gates with no event in hand.
//
// Follows the lib/ tested-logic convention: detections and storage are injected
// (default: the real window) so the whole truth table is exercisable under
// jsdom, and derivations are plain functions over signals — no createMemo /
// createEffect, which warn and leak when created outside a root.

import { createSignal } from 'solid-js';

/** The Chromium install event. Only `.prompt()` is load-bearing here. */
export interface BeforeInstallPromptEvent extends Event {
  prompt(): Promise<void>;
}

export type InstallVariant = 'ios' | 'android';

export interface InstallController {
  /** 'ios' on iOS/iPadOS (Share-sheet install), else 'android' (event-driven). */
  variant(): InstallVariant;
  /** The stashed, un-spent `beforeinstallprompt`, reactive; null until it fires. */
  event(): BeforeInstallPromptEvent | null;
  /** Sheet visibility: an auto-eligible show OR a manual open from Settings. */
  open(): boolean;
  /** The auto-show truth table (see below); the reactive driver of `open()`. */
  eligible(): boolean;
  /** Whether the Settings "Install app" row should render. */
  settingsRowVisible(): boolean;
  /** Re-entry from Settings: clears the dismissed flag and forces the sheet open. */
  openFromSettings(): void;
  /** Close the sheet and persist a permanent dismissal. */
  dismiss(): void;
  /** Fire the stashed event's native prompt, one-shot; then drop it and close. */
  promptInstall(): void;
  /** Remove the capture listeners (tests / teardown). */
  dispose(): void;
}

/** localStorage key for the permanent "user dismissed the sheet" flag. */
const DISMISSED_KEY = 'lab.install-dismissed';

/** iPadOS reports a desktop-Safari (Macintosh) UA, unmasked only by touch. */
const IPADOS_TOUCH_POINTS = 1;

/** Navigator surface we read — `standalone` is the iOS-legacy installed flag. */
interface InstallNavigator {
  userAgent: string;
  maxTouchPoints: number;
  standalone?: boolean;
}

/**
 * Everything createInstall touches in the environment, injected so the truth
 * table is testable and so importing the module never throws under jsdom
 * (matchMedia is unimplemented there). Detections are read through here lazily,
 * on each derivation call — never snapshotted at construction.
 */
export interface InstallDeps {
  /** Event target for beforeinstallprompt / appinstalled (default: window). */
  target: EventTarget;
  /**
   * Optional on purpose: jsdom (the vitest environment) does not implement
   * matchMedia AT ALL, so its absence must read as "no media match" (false) —
   * never a throw. The singleton's derivations are called during jsdom renders
   * of AppShell/Settings in other test files, so they must degrade to sensible
   * falses (standalone=false, coarse=false) when it is missing.
   */
  matchMedia?: (query: string) => { matches: boolean };
  navigator: InstallNavigator;
  storage: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;
}

/** In-memory storage fallback for a no-window host (keeps defaults total). */
function memoryStorage(): Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> {
  const map = new Map<string, string>();
  return {
    getItem: (k) => map.get(k) ?? null,
    setItem: (k, v) => void map.set(k, v),
    removeItem: (k) => void map.delete(k),
  };
}

/**
 * Real-environment deps. matchMedia falls back to a never-matches stub when the
 * host lacks it (jsdom) — the same "absence reads as false" convention as
 * composerKeys, which keeps every gate safe by construction. Guarded on
 * `window` so the module-scope singleton is import-safe in any environment.
 */
function defaultDeps(): InstallDeps {
  const win = typeof window !== 'undefined' ? window : undefined;
  const nav = win?.navigator as (Navigator & { standalone?: boolean }) | undefined;
  return {
    target: win ?? new EventTarget(),
    // Left undefined when the host lacks matchMedia (jsdom); the guard in
    // createInstall turns that into matches=false.
    matchMedia: win?.matchMedia?.bind(win),
    navigator: {
      userAgent: nav?.userAgent ?? '',
      maxTouchPoints: nav?.maxTouchPoints ?? 0,
      standalone: nav?.standalone,
    },
    storage: win?.localStorage ?? memoryStorage(),
  };
}

export function createInstall(deps: InstallDeps = defaultDeps()): InstallController {
  // --- storage-backed dismissal (mirrored in a signal) ----------------------
  // All storage access is wrapped in try/catch: private-mode Safari throws on
  // access, and the in-memory signal must still govern the sheet either way
  // (same convention as AppShell's readCollapsed).
  const readDismissed = (): boolean => {
    try {
      return deps.storage.getItem(DISMISSED_KEY) === '1';
    } catch {
      return false;
    }
  };
  const writeDismissed = (next: boolean): void => {
    setDismissed(next);
    try {
      if (next) deps.storage.setItem(DISMISSED_KEY, '1');
      else deps.storage.removeItem(DISMISSED_KEY);
    } catch {
      // Storage disabled — the signal above still drives the UI.
    }
  };

  const [event, setEvent] = createSignal<BeforeInstallPromptEvent | null>(null);
  const [installed, setInstalled] = createSignal(false);
  const [manualOpen, setManualOpen] = createSignal(false);
  const [dismissed, setDismissed] = createSignal(readDismissed());

  // --- capture (the reason the singleton is created at module init) ---------
  const onBeforeInstallPrompt = (e: Event): void => {
    // preventDefault suppresses Chrome's mini-infobar so our sheet is the sole
    // install prompt; stashing the event is what makes an Android install
    // possible at all — it is the only handle to .prompt().
    e.preventDefault();
    setEvent(e as BeforeInstallPromptEvent);
  };
  const onAppInstalled = (): void => {
    // Installed: the event is spent, the sheet and the Settings row have no
    // further purpose — collapse all three at once.
    setInstalled(true);
    setEvent(null);
    setManualOpen(false);
  };
  deps.target.addEventListener('beforeinstallprompt', onBeforeInstallPrompt);
  deps.target.addEventListener('appinstalled', onAppInstalled);

  // --- detections (lazy: evaluated per derivation call, never at construct) --
  // Absence-safe matchMedia read: a missing implementation (jsdom, or a deps
  // object that omits it) means the query does not match, never a throw.
  const media = (query: string): boolean =>
    typeof deps.matchMedia === 'function' ? deps.matchMedia(query).matches : false;

  const standalone = (): boolean =>
    media('(display-mode: standalone)') ||
    // navigator.standalone is the iOS-legacy "launched from Home Screen" flag.
    Boolean(deps.navigator.standalone);

  const coarse = (): boolean => media('(pointer: coarse)');

  const ios = (): boolean => {
    const ua = deps.navigator.userAgent;
    // iPadOS masquerades as desktop Safari (Macintosh UA); its only tell is a
    // touch screen, so a Mac UA with >1 touch point is really an iPad.
    return (
      /iPad|iPhone|iPod/.test(ua) ||
      (/Macintosh/.test(ua) && deps.navigator.maxTouchPoints > IPADOS_TOUCH_POINTS)
    );
  };

  const variant = (): InstallVariant => (ios() ? 'ios' : 'android');

  // --- derivations ----------------------------------------------------------
  // Auto-show truth table. The event() clause is what makes this time-shifted:
  // on Android the event lands seconds after mount and flips eligible() true,
  // so the sheet appears on its own; if it never lands, nothing shows — no dead
  // Install button. iOS has no event, so it clears this clause on platform.
  const eligible = (): boolean =>
    !standalone() &&
    coarse() &&
    !dismissed() &&
    !installed() &&
    (variant() === 'ios' || event() !== null);

  const open = (): boolean => manualOpen() || eligible();

  // The Settings row rule is enumerated exhaustively in the issue and is
  // DELIBERATELY missing the coarse gate: Settings is a manual re-entry point,
  // so a fine-pointer device that fails the auto-show still gets the row (iOS
  // always; Android only once the event is in hand — no dead row otherwise).
  const settingsRowVisible = (): boolean =>
    !standalone() && !installed() && (variant() === 'ios' ? true : event() !== null);

  // --- actions --------------------------------------------------------------
  const openFromSettings = (): void => {
    // Clear the dismissal (storage + signal) and force the sheet open even on a
    // device that fails an auto-show gate like coarse — this is the sole
    // re-entry after a permanent dismiss.
    writeDismissed(false);
    setManualOpen(true);
  };

  const dismiss = (): void => {
    // Permanent: the Settings row is the only way back. Close now, persist so a
    // fresh controller on the next load stays hidden.
    setManualOpen(false);
    writeDismissed(true);
  };

  const promptInstall = (): void => {
    const e = event();
    if (!e) return;
    // The Chromium prompt is one-shot and takes over the screen: fire it, then
    // drop the stash and clear the manual open so no dead Install button lingers
    // behind the native UI. A rejected promise is swallowed (logged) rather than
    // left as an unhandled rejection.
    void e.prompt().catch((err: unknown) => {
      console.warn('install prompt failed', err);
    });
    setEvent(null);
    setManualOpen(false);
  };

  const dispose = (): void => {
    deps.target.removeEventListener('beforeinstallprompt', onBeforeInstallPrompt);
    deps.target.removeEventListener('appinstalled', onAppInstalled);
  };

  return {
    variant,
    event,
    open,
    eligible,
    settingsRowVisible,
    openFromSettings,
    dismiss,
    promptInstall,
    dispose,
  };
}

/**
 * App singleton, created at module scope so `beforeinstallprompt` capture is
 * armed at import time — the browser fires it once and will not replay it for a
 * late listener. Import-safe under jsdom via defaultDeps' window guard.
 */
export const install: InstallController = createInstall();
