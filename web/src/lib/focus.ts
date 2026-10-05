// Focus rescue (issue #61): an in-place action can remove the very control it
// was started from — a Needs you entry that is fixed, the last parked entry
// discarded, Stop all once nothing is live, a failing check that turns
// pending. The browser then drops keyboard focus to <body>, and a keyboard or
// screen reader user is thrown back to the top of the page. rescueFocus()
// moves focus to a stable target nearby instead — but only when focus really
// was lost: it never takes focus away from where the operator moved it
// meanwhile.

/** True when keyboard focus is nowhere useful: on <body>, on nothing, or on a removed element. */
export function focusLost(): boolean {
  const active = document.activeElement;
  return (
    active === null ||
    active === document.body ||
    active === document.documentElement ||
    !active.isConnected
  );
}

/**
 * When focus was lost (focusLost), focuses the first target that is on the
 * page. Pass the closest sensible target first (the next item), then a
 * stable one (the section heading, with tabindex="-1"). Returns whether it
 * moved focus.
 */
export function rescueFocus(...targets: Array<HTMLElement | null | undefined>): boolean {
  if (!focusLost()) return false;
  const target = targets.find((el) => el !== null && el !== undefined && el.isConnected);
  if (target === undefined || target === null) return false;
  target.focus();
  return true;
}
