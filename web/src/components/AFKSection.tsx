// AFK controls on a repo card: 'Start AFK run (N ready)' with the claimable
// count (a hint — at 0 the button stays a real, enabled button, only visually
// greyed; the server re-checks claim/cap/auth authoritatively on every start
// and 409s a stale click), the auto toggle as a real button (v0: never a
// checkbox), and the three-strikes paused banner with the human Reset — the
// only un-pause there is.

import { Show, createResource, createSignal, onCleanup } from 'solid-js';
import {
  errorMessage,
  listClaimableIssues,
  resetAFK,
  setAFKAuto,
  startAFK,
  type Repo,
  type Run,
} from '../api';
import { useEvents } from '../events';
import { afkStartHint, isAFKPaused } from '../lib/afk';
import { resourceValue } from '../lib/resource';

export default function AFKSection(props: {
  repo: Repo;
  /** The repo row changed server-side (auto toggle / reset) — refetch repos. */
  onRepoChanged: () => void;
  /** An AFK run spawned — refetch instances, toast. */
  onStarted: (run: Run) => void;
  onError: (message: string) => void;
}) {
  const events = useEvents();
  const [claimable, { refetch }] = createResource(
    () => props.repo.id,
    (id) => listClaimableIssues(id),
  );
  // The claimable count follows issues (labels/state edits), claims (runs
  // starting/ending) and parked branches (discard frees a claim).
  // run.changed carries no repoID — refetch unconditionally.
  /* eslint-disable solid/reactivity -- handlers re-read props.repo.id fresh per SSE event */
  onCleanup(
    events.subscribe('issue.changed', (event) => {
      if (event.repoID === props.repo.id) void refetch();
    }),
  );
  onCleanup(events.subscribe('run.changed', () => void refetch()));
  onCleanup(
    events.subscribe('parked.changed', (event) => {
      if (event.repoID === props.repo.id) void refetch();
    }),
  );
  /* eslint-enable solid/reactivity */

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
    <div class="afk-section">
      <Show when={paused()}>
        <div class="banner error afk-paused" role="alert">
          <span class="banner-text">Paused after 3 failures</span>
          <button
            type="button"
            class="afk-reset"
            onClick={() => void reset()}
            disabled={busy() !== null}
          >
            {busy() === 'reset' ? 'Resetting…' : 'Reset'}
          </button>
        </div>
      </Show>
      <div class="afk-controls">
        <button
          type="button"
          classList={{ 'afk-start': true, greyed: hint().greyed }}
          onClick={() => void start()}
          disabled={busy() !== null}
        >
          {busy() === 'start' ? 'Starting…' : `Start AFK run${hint().suffix}`}
        </button>
        <button
          type="button"
          class="afk-auto"
          onClick={() => void toggleAuto()}
          disabled={busy() !== null}
          aria-pressed={props.repo.afk_auto_enabled}
        >
          Auto AFK runs: {props.repo.afk_auto_enabled ? 'On' : 'Off'}
        </button>
      </div>
    </div>
  );
}
