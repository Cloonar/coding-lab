// Deep-link / connecting-state logic for instance rows (claude-integration
// port spec §3.1): while the background registry poll runs the row shows a
// "connecting…" pulse; a captured deep link wins over everything; a finished
// capture with no link falls back to the generic claude.ai session picker —
// a miss never hides the Open affordance.

export const GENERIC_DEEP_LINK = 'https://claude.ai/code';

/** v0 anchor title on the generic-link fallback, kept verbatim. */
export const GENERIC_LINK_TITLE =
  "Opens the claude.ai session picker — the exact deep link wasn't captured";

export type OpenState =
  | { kind: 'connecting' }
  | { kind: 'link'; url: string; exact: true }
  | { kind: 'link'; url: string; exact: false; title: string };

export function openState(instance: {
  connecting: boolean;
  deep_link_url: string | null;
}): OpenState {
  // A captured URL beats a still-set connecting flag: the capture wrote the
  // link the moment it landed, the flag clears a beat later.
  if (instance.deep_link_url !== null && instance.deep_link_url !== '') {
    return { kind: 'link', url: instance.deep_link_url, exact: true };
  }
  if (instance.connecting) return { kind: 'connecting' };
  return { kind: 'link', url: GENERIC_DEEP_LINK, exact: false, title: GENERIC_LINK_TITLE };
}
