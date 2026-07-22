// Danger zone section, moved verbatim from the RepoSettings monolith (issue
// #198). Immediate by design — the delete round-trips on click (confirm
// first), so there is no useSettingsForm and no unsaved-changes guard here.

import { useNavigate } from '@solidjs/router';
import { Show, createSignal } from 'solid-js';
import type { Accessor } from 'solid-js';
import { ApiError, deleteRepo, errorMessage, type Repo } from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';

export default function DangerZone(props: { repo: Accessor<Repo> }) {
  const navigate = useNavigate();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [needsForce, setNeedsForce] = createSignal(false);
  const [force, setForce] = createSignal(false);

  const remove = async () => {
    const target = props.repo();
    if (!window.confirm(`Delete repository "${target.name}"? This removes lab's clone.`)) return;
    setBusy(true);
    setError(null);
    try {
      await deleteRepo(target.id, force());
      navigate('/');
    } catch (err) {
      // 409 while the clone runs — reveal the force checkbox and let the
      // operator escalate deliberately.
      if (err instanceof ApiError && err.status === 409) setNeedsForce(true);
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <section class="card danger-zone">
      <h2>Danger zone</h2>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={needsForce()}>
        <label class="check">
          <input
            type="checkbox"
            name="force"
            checked={force()}
            onChange={(e) => setForce(e.currentTarget.checked)}
          />
          <span>Force delete (abandon the running clone)</span>
        </label>
      </Show>
      <div class="card-actions">
        <button type="button" class="danger" onClick={() => void remove()} disabled={busy()}>
          {busy() ? 'Deleting…' : 'Delete repository'}
        </button>
      </div>
    </section>
  );
}
