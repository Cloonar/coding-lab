// The AFK controls — Run one, the Auto toggle and the three-strikes Reset —
// in two layouts that share one core (createAFKActions below):
//
// - AFKStrip (default export): the strip under the New-run composer (issue
//   #41), scoped to the SELECTED repo: a compact one-row port of the old
//   repo-card AFKSection. 'Run one (N ready)' carries the claimable-count hint,
//   read live from the ready queue (GET /ready?claimable=1), the auto toggle is
//   a real button (aria-pressed, never a checkbox), and the paused banner holds
//   the human Reset (the only un-pause).
// - AFKCard: the AFK block of the repo home's Overview (issue #61), laid out as
//   a card: "N issues ready for an agent.", Run one, Auto as a switch, the
//   paused banner with Reset. Its count is the repo summary's claimable count —
//   the card never reads the ready queue itself, so opening a repo home makes
//   no request to a forge.
//
// Shared behavior: the count is a hint only — at a known 0 the button stays a
// real, enabled button, just greyed, and an unknown count shows no number —
// since the server re-checks claim/cap/auth authoritatively and 409s a stale
// click. AFK start success is reported to the parent (a toast), never a
// navigation. Auto applies at once.

import { Show, createSignal, createUniqueId } from 'solid-js';
import {
  errorMessage,
  listClaimableIssues,
  resetAFK,
  setAFKAuto,
  startAFK,
  type Repo,
  type Run,
} from '../api';
import Banner from './Banner';
import Icon from './Icon';
import ToggleSwitch from './Switch';
import { AFK_PAUSE_THRESHOLD, afkStartHint, claimableSentence, isAFKPaused } from '../lib/afk';
import { createLiveResource } from '../lib/liveResource';
import { resourceValue } from '../lib/resource';

export interface AFKControlsProps {
  repo: Repo;
  /**
   * The repo row changed server-side (auto toggle / reset) — refetch it. A
   * returned promise is awaited, so the controls stay busy until it settles.
   */
  onRepoChanged: () => void | Promise<unknown>;
  /**
   * The repo as the Auto or Reset request answered it, before onRepoChanged:
   * lets the parent show the change at once, even when its refetch fails.
   */
  onRepo?: (repo: Repo) => void;
  /** An AFK run spawned — toast it (NO navigation). */
  onStarted: (run: Run) => void;
  onError: (message: string) => void;
  /** Auto was switched (after the repo refetch) — e.g. a toast. */
  onAutoChanged?: (enabled: boolean) => void;
  /** The three-strikes pause was reset (after the repo refetch) — e.g. a toast. */
  onReset?: () => void;
}

type AFKBusy = 'start' | 'auto' | 'reset' | null;

/**
 * The behavior both layouts share: Run one → startAFK, Auto → setAFKAuto with
 * the flipped flag, Reset → resetAFK; one action at a time, failures to
 * `onError`. `afterStart` runs after a successful start (the strip refetches
 * its count). The returned `auto` override holds the requested Auto state
 * while the request and the parent's refetch are in flight.
 */
function createAFKActions(props: AFKControlsProps, afterStart: () => unknown) {
  const [busy, setBusy] = createSignal<AFKBusy>(null);
  const [autoOverride, setAutoOverride] = createSignal<boolean | null>(null);

  const act = async (kind: Exclude<AFKBusy, null>, action: () => Promise<void>) => {
    const onError = props.onError; // read before the await, like every prop below
    setBusy(kind);
    try {
      await action();
    } catch (err) {
      onError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  // Reactive reads happen in the handlers themselves (tracked as event
  // handlers); the closures passed to act() only see captured plain values.
  const start = () => {
    const repoID = props.repo.id;
    const onStarted = props.onStarted;
    return act('start', async () => {
      const run = await startAFK(repoID);
      onStarted(run);
      await afterStart();
    });
  };

  const toggleAuto = (next = !props.repo.afk_auto_enabled) => {
    const repoID = props.repo.id;
    const onRepoChanged = props.onRepoChanged;
    const onRepo = props.onRepo;
    const onAutoChanged = props.onAutoChanged;
    setAutoOverride(next);
    return act('auto', async () => {
      try {
        const updated = await setAFKAuto(repoID, next);
        onRepo?.(updated);
        await onRepoChanged();
        onAutoChanged?.(next);
      } finally {
        setAutoOverride(null);
      }
    });
  };

  const reset = () => {
    const repoID = props.repo.id;
    const onRepoChanged = props.onRepoChanged;
    const onRepo = props.onRepo;
    const onReset = props.onReset;
    return act('reset', async () => {
      const updated = await resetAFK(repoID);
      onRepo?.(updated);
      await onRepoChanged();
      onReset?.();
    });
  };

  return {
    busy,
    paused: () => isAFKPaused(props.repo.consecutive_failures),
    auto: () => autoOverride() ?? props.repo.afk_auto_enabled,
    start,
    toggleAuto,
    reset,
  };
}

export default function AFKStrip(props: AFKControlsProps) {
  // The claimable count follows issues (labels/state edits), claims (runs
  // starting/ending) and parked branches (discard frees a claim).
  // run.changed carries no repoID — refetch unconditionally.
  const [claimable, { refetch }] = createLiveResource(
    () => props.repo.id,
    (id) => listClaimableIssues(id),
    [
      { type: 'issue.changed', match: (event) => event.repoID === props.repo.id },
      { type: 'run.changed' },
      { type: 'parked.changed', match: (event) => event.repoID === props.repo.id },
    ],
  );

  // Unknown count (still loading / ready endpoint failed) → null → plain
  // enabled button: the hint must never block the authoritative click.
  const count = (): number | null => {
    const issues = resourceValue(claimable);
    return issues === undefined ? null : issues.length;
  };
  const hint = () => afkStartHint(count());
  // The spawned claim consumed one claimable issue: re-read the count.
  const afk = createAFKActions(props, () => void refetch());

  return (
    <div class="afk-strip">
      <Show when={afk.paused()}>
        <Banner
          message="Paused after 3 failures"
          class="afk-strip-paused"
          action={
            <button
              type="button"
              class="afk-strip-reset"
              onClick={() => void afk.reset()}
              disabled={afk.busy() !== null}
            >
              {afk.busy() === 'reset' ? 'Resetting…' : 'Reset'}
            </button>
          }
        />
      </Show>
      <div class="afk-strip-row">
        <button
          type="button"
          classList={{ 'afk-strip-start': true, greyed: hint().greyed }}
          onClick={() => void afk.start()}
          disabled={afk.busy() !== null}
        >
          {afk.busy() === 'start' ? 'Starting…' : `Run one${hint().suffix}`}
        </button>
        <button
          type="button"
          class="afk-strip-auto"
          onClick={() => void afk.toggleAuto()}
          disabled={afk.busy() !== null}
          aria-pressed={props.repo.afk_auto_enabled}
        >
          Auto: {props.repo.afk_auto_enabled ? 'On' : 'Off'}
        </button>
      </div>
    </div>
  );
}

/**
 * The repo home's AFK card. The count is `repo.summary.claimable` (null = not
 * known yet: no number). After a start the parent's repo refetches too, so the
 * summary's count follows the claim.
 */
export function AFKCard(props: AFKControlsProps) {
  const count = (): number | null => props.repo.summary?.claimable ?? null;
  const afk = createAFKActions(props, () => props.onRepoChanged());
  const headingId = `afk-card-${createUniqueId()}`;

  return (
    <section class="overview-card afk-card" aria-labelledby={headingId}>
      <div class="overview-card-head">
        <h2 id={headingId}>AFK</h2>
      </div>
      <Show when={afk.paused()}>
        <Banner
          variant="notice"
          message={`Paused after ${AFK_PAUSE_THRESHOLD} failed runs.`}
          class="afk-card-paused"
          action={
            <button
              type="button"
              class="afk-card-reset"
              onClick={() => void afk.reset()}
              disabled={afk.busy() !== null}
            >
              {afk.busy() === 'reset' ? 'Resetting…' : 'Reset'}
            </button>
          }
        />
      </Show>
      <p class="afk-card-count">{claimableSentence(count())}</p>
      <div class="afk-card-actions">
        <button
          type="button"
          classList={{ 'afk-card-start': true, greyed: count() === 0 }}
          onClick={() => void afk.start()}
          disabled={afk.busy() !== null}
        >
          <Icon name="play" size={18} />
          {afk.busy() === 'start' ? 'Starting…' : 'Run one'}
        </button>
        <span class="spacer" />
        <ToggleSwitch
          label="Auto"
          name="afk_auto_enabled"
          checked={afk.auto()}
          disabled={afk.busy() !== null}
          onChange={(next) => void afk.toggleAuto(next)}
          class="afk-card-auto"
        />
      </div>
    </section>
  );
}
