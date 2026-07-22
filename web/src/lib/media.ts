// Reactive matchMedia primitive (issue #198): a boolean accessor tracking a
// media query live. SettingsLayout gates its mobile-index / desktop
// master-detail split on it, so the split must FLIP on viewport resize — a
// one-shot `.matches` read would strand the layout on the mount-time width.
// jsdom ships no window.matchMedia at all; its absence reads as a static
// "no match" (the composerKeys.ts convention), never a crash.

import { createSignal, onCleanup } from 'solid-js';
import type { Accessor } from 'solid-js';

export function createMediaQuery(query: string): Accessor<boolean> {
  const list = window.matchMedia?.(query);
  if (list === undefined) return () => false;
  const [matches, setMatches] = createSignal(list.matches);
  // Re-read list.matches instead of trusting the event payload: test fakes
  // (the harness stubMatchMedia idiom) fire bare change notifications.
  const onChange = () => setMatches(list.matches);
  list.addEventListener('change', onChange); // addListener is deprecated
  onCleanup(() => list.removeEventListener('change', onChange));
  return matches;
}
