// Instance identity rendering, ported from lab-v0 instance.go (sessions-spawn
// port spec §2.7). A session name is `<repoName>~<label>` — the sanitizers
// only ever emit [A-Za-z0-9_-], so the FIRST `~` is always the boundary.
// Manual labels are `<user>-YYYYMMDD-HHMM` or a bare stamp; the row title
// renders as 'label · 15:30' / '15:30' exactly like v0. AFK labels
// ('afk-<N>' / 'afk-auto-<N>') render as 'AFK #N' (v0 badge string, both
// kinds); any other non-timestamp label renders verbatim.

import { parseAFKLabel } from './afk';

/**
 * The label part of a session name: everything after the first `~`.
 * No separator → "" (hand-made or pre-ADR-0017 legacy session).
 */
export function sessionLabel(sessionName: string): string {
  const idx = sessionName.indexOf('~');
  return idx === -1 ? '' : sessionName.slice(idx + 1);
}

/**
 * The repo part of a session name: everything before the first `~`.
 * No separator → "" (hand-made or pre-ADR-0017 legacy session).
 */
export function sessionRepo(sessionName: string): string {
  const idx = sessionName.indexOf('~');
  return idx === -1 ? '' : sessionName.slice(0, idx);
}

/** v0 manualTimestampRe: leading group greedy so dashed labels keep dashes. */
const manualTimestampRe = /^(?:(.+)-)?(\d{8})-(\d{2})(\d{2})$/;

export interface ManualLabelParts {
  user: string;
  /** "HH:MM", or "" when the label carries no timestamp (hand-made/legacy). */
  hhmm: string;
}

/**
 * Splits a manual label into user + rendered clock time. No timestamp match →
 * the whole label is the user portion with hhmm "".
 */
export function parseManualLabel(label: string): ManualLabelParts {
  const m = manualTimestampRe.exec(label);
  if (!m) return { user: label, hhmm: '' };
  return { user: m[1] ?? '', hhmm: `${m[3] ?? ''}:${m[4] ?? ''}` };
}

/** Row title: 'AFK #7', 'debug · 15:30', bare '15:30', or the label verbatim. */
export function instanceTitle(label: string): string {
  const afk = parseAFKLabel(label);
  if (afk !== null) return `AFK #${afk.issue}`;
  const { user, hhmm } = parseManualLabel(label);
  if (hhmm === '') return label;
  return user === '' ? hhmm : `${user} · ${hhmm}`;
}
