// Readiness (issue #61): how the SPA words and orders the server's readiness
// report, and where a failing check's fix lives. The report itself is the
// server's — the checks, their state and their one-sentence detail; nothing
// here re-evaluates a check. A check the server left out is not listed.
//
// Used by the repo home's Overview (the readiness block) and by the
// repositories list (the Needs you block), so both name checks, order them
// and jump to a fix the same way.

import type {
  Readiness,
  ReadinessCheck,
  ReadinessCheckID,
  ReadinessFix,
  ReadinessState,
} from '../api';

/** Each check's title, as the readiness block lists it. */
export const CHECK_TITLES: Record<ReadinessCheckID, string> = {
  clone: 'Clone',
  git_credential: 'Git credential',
  tracker: 'Tracker',
  agent_login: 'Agent',
  dev_image: 'Dev image',
  imports: 'Imports',
};

/**
 * A check's title. An id this client does not know yet (a newer server)
 * reads as its id with the underscores turned into spaces, capitalised.
 */
export function checkTitle(id: string): string {
  const known = (CHECK_TITLES as Record<string, string | undefined>)[id];
  if (known !== undefined) return known;
  const words = id.replaceAll('_', ' ').trim();
  return words === '' ? 'Check' : words.charAt(0).toUpperCase() + words.slice(1);
}

/**
 * A state as this client renders it. A state it does not know (a newer
 * server's) reads as pending: never as passing, and never a crash.
 */
export function checkState(state: unknown): ReadinessState {
  return state === 'passing' || state === 'failing' || state === 'pending' ? state : 'pending';
}

/**
 * A report's checks, tolerating what an older or newer server may send: a
 * missing report or check list reads as no checks, and an entry that is not
 * a check is dropped.
 */
export function checksOf(readiness: Readiness | null | undefined): ReadinessCheck[] {
  const checks: unknown = readiness?.checks;
  if (!Array.isArray(checks)) return [];
  return checks.filter(
    (check): check is ReadinessCheck => typeof check === 'object' && check !== null,
  );
}

/**
 * The report as the SPA renders it: the checks of checksOf(), each with a
 * known state (checkState). undefined for no report at all. Pure: a check
 * whose state is already known is passed through as is.
 */
export function normalizeReadiness(readiness: Readiness | null | undefined): Readiness | undefined {
  if (readiness === null || readiness === undefined || typeof readiness !== 'object') {
    return undefined;
  }
  return {
    // A missing roll-up says nothing; the checks decide (readinessState).
    state: readiness.state === undefined ? 'passing' : checkState(readiness.state),
    checks: checksOf(readiness).map((check) =>
      check.state === checkState(check.state) ? check : { ...check, state: 'pending' },
    ),
  };
}

const RANK: Record<ReadinessState, number> = { failing: 0, pending: 1, passing: 2 };

/**
 * Failing checks first, then pending, then passing; the server's canonical
 * order holds within each group. A state this client does not know ranks as
 * pending. Pure: returns a new array.
 */
export function orderChecks(checks: readonly ReadinessCheck[]): ReadinessCheck[] {
  return checks
    .map((check, index) => ({ check, index }))
    .sort(
      (a, b) =>
        RANK[checkState(a.check.state)] - RANK[checkState(b.check.state)] || a.index - b.index,
    )
    .map((entry) => entry.check);
}

/**
 * The roll-up the block shows: failing when any check fails, else pending
 * when any is pending, else passing. The server's own `state` counts too, so
 * a report that rolls up worse than its listed checks is never shown better.
 * An unknown state counts as pending; a missing roll-up leaves it to the
 * checks.
 */
export function readinessState(readiness: Readiness): ReadinessState {
  const states = checksOf(readiness).map((check) => checkState(check.state));
  if (readiness.state !== undefined) states.push(checkState(readiness.state));
  if (states.includes('failing')) return 'failing';
  if (states.includes('pending')) return 'pending';
  return 'passing';
}

export interface ReadinessHeadline {
  state: ReadinessState;
  /** "Ready to run" / "Getting ready" / "Not ready, 2 problems". */
  title: string;
  /** One sentence under the title. */
  detail: string;
}

/**
 * The block's one-line answer to "can a run start here right now?".
 * `cloning` says the repo's clone is in flight, so a pending block names the
 * clone as the thing to wait for even before the report lists it.
 */
export function readinessHeadline(readiness: Readiness, cloning = false): ReadinessHeadline {
  const checks = checksOf(readiness);
  const state =
    cloning && readinessState(readiness) === 'passing' ? 'pending' : readinessState(readiness);
  const failing = checks.filter((check) => check.state === 'failing').length;
  switch (state) {
    case 'failing':
      return {
        state,
        title: failing === 0 ? 'Not ready' : `Not ready, ${plural(failing, 'problem')}`,
        detail:
          failing > 1
            ? 'New runs are refused until these are fixed.'
            : 'New runs are refused until this is fixed.',
      };
    case 'pending': {
      const clonePending =
        cloning ||
        checks.some((check) => check.id === 'clone' && checkState(check.state) === 'pending');
      return {
        state,
        title: 'Getting ready',
        detail: clonePending
          ? 'Runs can start when the clone has finished.'
          : 'Runs can start when the pending checks have finished.',
      };
    }
    default: {
      const n = checks.length;
      return {
        state,
        title: 'Ready to run',
        detail:
          n === 0
            ? 'Nothing blocks a new run.'
            : n === 1
              ? 'The one check passes.'
              : `All ${n} checks pass.`,
      };
    }
  }
}

/**
 * Where a failing check's fix lives:
 * - `repo` → this repo's settings, scrolled to the section, with the field
 *   to focus as `?field=` (the settings page scrolls to it and focuses it);
 * - `global` → global Settings at its section;
 * - `credentials` → the Credentials page (the agent login cards live there).
 */
export function fixHref(repoID: string, fix: ReadinessFix): string {
  switch (fix.scope) {
    case 'repo': {
      let path = `/repos/${encodeURIComponent(repoID)}/settings`;
      if (fix.section !== undefined && fix.section !== '') {
        path += `/${encodeURIComponent(fix.section)}`;
      }
      if (fix.field !== undefined && fix.field !== '') {
        path += `?field=${encodeURIComponent(fix.field)}`;
      }
      return path;
    }
    case 'global':
      return fix.section !== undefined && fix.section !== ''
        ? `/settings/${encodeURIComponent(fix.section)}`
        : '/settings';
    case 'credentials':
      return '/credentials';
  }
}

/** Labels for the fix button, by the PATCH key of the field it opens. */
const FIELD_LABELS: Record<string, string> = {
  credential_id: 'Change credential',
  forge_credential_id: 'Change credential',
  tracker_binding: 'Change tracker',
  provider: 'Change agent',
  runner: 'Change Runner',
  image_ref: 'Change dev image',
};

/**
 * The fix button's label, named by its target: "Change credential" for a
 * credential field, "Change dev image" for the image, "Open Credentials" for
 * an agent login, and a plain "Open settings" when the target has no name of
 * its own.
 */
export function fixLabel(fix: ReadinessFix): string {
  if (fix.scope === 'credentials') return 'Open Credentials';
  const byField = fix.field !== undefined ? FIELD_LABELS[fix.field] : undefined;
  if (byField !== undefined) return byField;
  if (fix.field !== undefined && fix.field.endsWith('credential_id')) return 'Change credential';
  if (fix.scope === 'repo' && fix.section === 'imports') return 'Change imports';
  return fix.scope === 'global' ? 'Open global settings' : 'Open settings';
}

/** "1 problem" / "2 problems". */
export function plural(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? '' : 's'}`;
}
