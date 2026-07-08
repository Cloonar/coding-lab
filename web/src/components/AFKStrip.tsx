// AFK strip under the New-run composer (issue #41), scoped to the SELECTED
// repo: a compact one-row port of the old repo-card AFKSection. Same behavior —
// 'Run one (N ready)' with the claimable-count hint (a hint only: at 0 the
// button stays a real, enabled button, just greyed, since the server re-checks
// claim/cap/auth authoritatively and 409s a stale click), the auto toggle as a
// real button (aria-pressed, never a checkbox), and the three-strikes paused
// banner with the human Reset (the only un-pause). AFK start success is a toast
// in the parent, never a navigation — the composer stays put.

import { Show, createSignal } from 'solid-js';
import {
  errorMessage,
  listClaimableIssues,
  resetAFK,
  setAFKAuto,
  startAFK,
  type Repo,
  type Run,
} from '../api';
import { afkStartHint, isAFKPaused } from '../lib/afk';
import { createLiveResource } from '../lib/liveResource';
import { resourceValue } from '../lib/resource';

export default function AFKStrip(props: {
  repo: Repo;
  /** The repo row changed server-side (auto toggle / reset) — refetch repos. */
  onRepoChanged: () => void;
  /** An AFK run spawned — toast it (NO navigation). */
  onStarted: (run: Run) => void;
  onError: (message: string) => void;
}) {
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
  const paused = () => isAFKPaused(props.repo.consecutive_failures);

  const [busy, setBusy] = createSignal<'start' | 'auto' | 'reset' | null>(null);

  const act = async (kind: 'start' | 'auto' | 'reset', action: () => Promise<void>) => {
    setBusy(kind);
    try {
      await action();
    } catch (err) {
      props.onError(errorMessage(err));
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
      void refetch(); // the spawned claim consumed one claimable issue
    });
  };

  const toggleAuto = () => {
    const repoID = props.repo.id;
    const next = !props.repo.afk_auto_enabled;
    const onRepoChanged = props.onRepoChanged;
    return act('auto', async () => {
      await setAFKAuto(repoID, next);
      onRepoChanged();
    });
  };

  const reset = () => {
    const repoID = props.repo.id;
    const onRepoChanged = props.onRepoChanged;
    return act('reset', async () => {
      await resetAFK(repoID);
      onRepoChanged();
    });
  };

  return (
    <div class="afk-strip">
      <Show when={paused()}>
        <div class="banner error afk-strip-paused" role="alert">
          <span class="banner-text">Paused after 3 failures</span>
          <button
            type="button"
            class="afk-strip-reset"
            onClick={() => void reset()}
            disabled={busy() !== null}
          >
            {busy() === 'reset' ? 'Resetting…' : 'Reset'}
          </button>
        </div>
      </Show>
      <div class="afk-strip-row">
        <button
          type="button"
          classList={{ 'afk-strip-start': true, greyed: hint().greyed }}
          onClick={() => void start()}
          disabled={busy() !== null}
        >
          {busy() === 'start' ? 'Starting…' : `Run one${hint().suffix}`}
        </button>
        <button
          type="button"
          class="afk-strip-auto"
          onClick={() => void toggleAuto()}
          disabled={busy() !== null}
          aria-pressed={props.repo.afk_auto_enabled}
        >
          Auto: {props.repo.afk_auto_enabled ? 'On' : 'Off'}
        </button>
      </div>
    </div>
  );
}
