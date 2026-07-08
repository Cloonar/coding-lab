// New issue form (/repos/:id/issues/new, builtin repos only): title, optional
// body and an optional label set picked from the repo's labels. A forge-bound
// repo gets the managed-on-the-forge note instead of the form — the server
// would 409 the create anyway. Success navigates to the fresh issue.

import { A, useNavigate, useParams } from '@solidjs/router';
import { Match, Show, Switch, createResource, createSignal } from 'solid-js';
import { createIssue, errorMessage, getRepo, listLabels, type CreateIssueRequest } from '../api';
import ErrorBanner from '../components/ErrorBanner';
import LabelPicker from '../components/LabelPicker';
import RequireAuth from '../components/RequireAuth';
import { canMutateTracker } from '../lib/issues';
import { toggleLabel } from '../lib/labels';
import { resourceValue } from '../lib/resource';

export default function NewIssue() {
  return (
    <RequireAuth>
      <NewIssueView />
    </RequireAuth>
  );
}

function NewIssueView() {
  const params = useParams<{ id: string }>();
  const navigate = useNavigate();

  const [repo] = createResource(
    () => params.id,
    (id) => getRepo(id),
  );
  // Reads outside the guarded <Match> branches (the h2, builtin(), the label
  // picker) go through the non-throwing accessors so a failed getRepo renders
  // the error banner — and a failed listLabels keeps the form usable —
  // instead of blanking the page.
  const repoData = () => resourceValue(repo);
  const builtin = () => {
    const r = repoData();
    return r !== undefined && canMutateTracker(r.tracker_binding);
  };
  const [labels] = createResource(
    () => (builtin() ? params.id : null),
    (id) => listLabels(id),
  );
  const labelList = () => resourceValue(labels);

  const [title, setTitle] = createSignal('');
  const [body, setBody] = createSignal('');
  const [selected, setSelected] = createSignal<string[]>([]);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    const trimmed = title().trim();
    if (trimmed === '') {
      setError('Title must not be empty.');
      return;
    }
    setBusy(true);
    setError(null);
    const req: CreateIssueRequest = { title: trimmed };
    if (body() !== '') req.body = body();
    if (selected().length > 0) req.labels = selected();
    try {
      const created = await createIssue(params.id, req);
      navigate(`/repos/${params.id}/issues/${created.number}`);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <main class="page">
      <p class="crumb">
        <A href={`/repos/${params.id}/issues`}>← Issues</A>
      </p>
      <div class="section-head">
        <h2>{repoData()?.name ?? 'Repository'} · New issue</h2>
      </div>
      <Switch>
        <Match when={repo.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(repo.error)}</span>
          </div>
        </Match>
        <Match when={repo() !== undefined && !builtin()}>
          <p class="muted forge-note">Managed on the forge — create issues there.</p>
        </Match>
        <Match when={repo()}>
          <form class="card form-card" onSubmit={(e) => void submit(e)}>
            <ErrorBanner message={error()} onDismiss={() => setError(null)} />
            <label class="field">
              <span>Title</span>
              <input
                type="text"
                name="title"
                required
                autocomplete="off"
                value={title()}
                onInput={(e) => setTitle(e.currentTarget.value)}
              />
            </label>
            <label class="field">
              <span>Body</span>
              <textarea
                name="body"
                rows="6"
                value={body()}
                onInput={(e) => setBody(e.currentTarget.value)}
              />
              <small class="hint">Optional — plain text or markdown, rendered as text.</small>
            </label>
            <Show when={(labelList()?.length ?? 0) > 0}>
              <div class="field">
                <span>Labels</span>
                <LabelPicker
                  labels={labelList() ?? []}
                  selected={selected()}
                  onToggle={(name) => setSelected(toggleLabel(selected(), name))}
                />
              </div>
            </Show>
            <button type="submit" class="primary wide" disabled={busy()}>
              {busy() ? 'Creating…' : 'Create issue'}
            </button>
          </form>
        </Match>
      </Switch>
    </main>
  );
}
