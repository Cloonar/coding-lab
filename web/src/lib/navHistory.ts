// "Is there an in-app entry to go back to?" (issue #76) — the chat's back
// arrow pops the in-app history when there is one and otherwise falls back to
// Runs (`/`): a chat opened cold from a deep link, a PWA shortcut or a
// notification click must not send Back out of the app (or nowhere at all).
//
// @solidjs/router already stamps every history entry with
// `history.state._depth` (dist/index.js saveCurrentDepth): a push records
// `history.length - 1`, a replace keeps the entry's depth, and back/forward
// land on the entry's own stamp. But the FIRST entry is stamped with
// `history.length - 1` too, so a deep link opened in a tab that already has
// history (another site, an earlier visit) starts at depth > 0 — depth alone
// cannot say "this entry is the app's own". So the depth of the app's first
// entry is recorded once at boot (initNavHistory, from index.tsx) and Back
// counts as possible only while the current entry sits above it.
//
// Known limit: the boot depth lives in memory, so after a reload deep in the
// app the reloaded entry becomes the new floor and Back falls back to `/`
// instead of popping. That errs toward staying in the app — persisting the
// floor (sessionStorage) could instead pop the user out to another site when
// the same tab later re-enters lab by a fresh navigation.

let bootDepth: number | null = null;

/** The router's depth stamp on the current entry, or null when absent. */
function currentDepth(): number | null {
  const state: unknown = window.history.state;
  if (state !== null && typeof state === 'object' && '_depth' in state) {
    const depth = (state as { _depth: unknown })._depth;
    if (typeof depth === 'number') return depth;
  }
  return null;
}

/**
 * Record the app's first entry as the floor. Idempotent — only the first call
 * counts. The router stamps the boot entry when its module loads, so by the
 * time index.tsx runs the stamp is there; the router's own formula
 * (`history.length - 1`) is the fallback should the stamp be missing.
 */
export function initNavHistory(): void {
  if (bootDepth !== null) return;
  bootDepth = currentDepth() ?? window.history.length - 1;
}

/** True while an in-app entry exists below the current one. */
export function canGoBack(): boolean {
  const depth = currentDepth();
  return bootDepth !== null && depth !== null && depth > bootDepth;
}

/** Test seam: forget the recorded floor. */
export function resetNavHistory(): void {
  bootDepth = null;
}
