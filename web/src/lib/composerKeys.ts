// Shared Enter-send gate (ADR-0031, issue #70): the two composers (chat reply,
// new-run spawn) must not drift on when Enter sends vs. inserts a newline.
// Bare Enter now sends on fine-pointer setups (mouse/trackpad — desktop and
// desktop-like hybrids), superseding the ADR-0022/0029 "bare Enter is always a
// newline" clauses; touch-only devices keep return-as-newline since there is
// no separate physical send affordance to fall back on. Shift+Enter and
// Alt+Enter stay an explicit newline everywhere (browser default inserts it —
// this function only decides whether to preventDefault + send). Cmd/Ctrl+Enter
// keeps sending in ALL environments, including touch + hardware-keyboard
// setups, since it was never gated on pointer type.

/**
 * True iff this keydown should trigger a composer send (caller still owns
 * `preventDefault()` and any empty/busy guard — this is a pure event→bool
 * predicate, no side effects).
 */
export function isComposerSend(e: KeyboardEvent): boolean {
  if (e.key !== 'Enter') return false;
  // IME guard: committing CJK/other composed input with Enter must never fire
  // a send. `isComposing` is the modern signal; `keyCode === 229` is the
  // legacy fallback some engines still report on the commit keydown.
  if (e.isComposing || e.keyCode === 229) return false;
  if (e.metaKey || e.ctrlKey) return true;
  if (e.shiftKey || e.altKey) return false;
  // Bare Enter: sends only on fine-pointer setups. Evaluated fresh on every
  // keydown (never snapshotted at module load or mount) so docking/undocking
  // a keyboard on a hybrid device takes effect immediately. `?.` is required —
  // jsdom has no matchMedia, and its absence must read as "not fine-pointer"
  // (false), which keeps phones/tablets safe by construction. Precedent:
  // RunChat.tsx's prefersReducedMotion check.
  return window.matchMedia?.('(hover: hover) and (pointer: fine)').matches === true;
}
