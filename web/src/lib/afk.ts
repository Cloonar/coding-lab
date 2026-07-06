// AFK UI decisions, ported from lab-v0 (afk-engine port spec §2.7/§2.8) plus
// the M5 additions: label parsing ('afk-<N>' / 'afk-auto-<N>'), the
// start-button ready hint, the three-strikes paused check and the budget
// countdown derived from runs.budget_deadline (persisted clock, D12b).

/** v0 afkPauseThreshold: consecutive_failures >= 3 pauses the auto loop. */
export const AFK_PAUSE_THRESHOLD = 3;

/** Three consecutive failures pause a repo's AUTO loop until a human Reset. */
export function isAFKPaused(consecutiveFailures: number): boolean {
  return consecutiveFailures >= AFK_PAUSE_THRESHOLD;
}

export interface AFKLabel {
  /** The claimed issue number (always >= 1). */
  issue: number;
  /** True for scheduler-launched runs ('afk-auto-<N>'). */
  auto: boolean;
}

/**
 * v0 parseAFKLabel: cut 'afk-', then an optional 'auto-' marker, then the
 * issue number — which must parse and be >= 1. Anything else (user labels
 * like 'afk-feature', bare 'afk-', zero/negative) is not an AFK label.
 * Mirrors Go's Atoi acceptance (optional sign, digits) so 'afk-007' still
 * reads as issue 7, exactly like v0.
 */
export function parseAFKLabel(label: string): AFKLabel | null {
  if (!label.startsWith('afk-')) return null;
  let rest = label.slice('afk-'.length);
  let auto = false;
  if (rest.startsWith('auto-')) {
    auto = true;
    rest = rest.slice('auto-'.length);
  }
  if (!/^[+-]?\d+$/.test(rest)) return null;
  const issue = parseInt(rest, 10);
  if (!Number.isSafeInteger(issue) || issue < 1) return null;
  return { issue, auto };
}

export interface AFKStartHint {
  /** '' or ' (N ready)' — leading space, v0 exact suffix. */
  suffix: string;
  /**
   * True when the claimable count is known to be 0 — a VISUAL hint only
   * (greyed styling). The button MUST stay a real, enabled submit: the count
   * is stale by design and the server re-checks claim/cap/auth authoritatively
   * on every start (port-spec §2.7/§3.9 — never HTML `disabled`).
   */
  greyed: boolean;
}

/**
 * The 'Start AFK run' button decoration from the CLAIMABLE count (ready
 * minus claimed branches). Unknown count (null: still loading, or the ready
 * endpoint failed) → plain button, no greying; the count is only ever a hint —
 * the server re-checks claim/cap/auth authoritatively on every start. At a
 * known 0 the button stays rendered and enabled (v0 morph lesson), only
 * visually greyed.
 */
export function afkStartHint(count: number | null): AFKStartHint {
  if (count === null) return { suffix: '', greyed: false };
  return { suffix: ` (${count} ready)`, greyed: count === 0 };
}

/**
 * Human countdown from runs.budget_deadline: '~1h 12m left', '~9m left',
 * '<1m left', or 'over budget' once the deadline passed (the reaper's next
 * tick will time the run out). null when there is no (parseable) deadline —
 * manual runs carry none.
 */
export function budgetRemaining(deadline: string | null, nowMs: number): string | null {
  if (deadline === null) return null;
  const end = new Date(deadline).getTime();
  if (Number.isNaN(end)) return null;
  const ms = end - nowMs;
  if (ms <= 0) return 'over budget';
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 1) return '<1m left';
  if (minutes < 60) return `~${minutes}m left`;
  return `~${Math.floor(minutes / 60)}h ${minutes % 60}m left`;
}
