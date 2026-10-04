// New issue form (/repos/:id/issues/new, builtin repos only): title, optional
// body and an optional label set picked from the repo's labels. A forge-bound
// repo gets the managed-on-the-forge note instead of the form — the server
// would 409 the create anyway. Success navigates to the fresh issue. It renders
// inside the repo home frame's Issues tab (issue #61): the frame owns the page,
// the repo heading and the repo fetch; this page keeps an "Issues / New issue"
// trail within the tab.

import { useNavigate, useParams } from '@solidjs/router';
import { Match, Show, Switch, createResource, createSignal } from 'solid-js';
import { createIssue, errorMessage, listLabels, type CreateIssueRequest } from '../api';
import Crumbs, { type Crumb } from '../components/Crumbs';
import FormCard from '../components/FormCard';
import LabelPicker from '../components/LabelPicker';
import SectionHead from '../components/SectionHead';
import { canMutateTracker } from '../lib/issues';
import { toggleLabel } from '../lib/labels';
import { resourceValue } from '../lib/resource';
import { useRepoHome } from './repo-home/context';

export default function NewIssue() {
  const params = useParams<{ id: string }>();
  const navigate = useNavigate();
  const home = useRepoHome();

  // The frame's repo (undefined while loading or after a failed getRepo — the
  // frame shows that banner). A failed listLabels keeps the form usable: the
  // label list goes through the non-throwing accessor.
  const repoData = () => home.repo();
  const builtin = () => {
    const r = repoData();
    return r !== undefined && canMutateTracker(r.tracker_binding);
  };
  const [labels] = createResource(
    () => (builtin() ? params.id : null),
    (id) => listLabels(id),
  );
  const labelList = () => resourceValue(labels);

  const crumbs = (): Crumb[] => [
    { label: 'Issues', href: `/repos/${params.id}/issues` },
    { label: 'New issue' },
  ];

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
    <>
      <Crumbs segments={crumbs()} />
      <SectionHead title="New issue" />
      <Switch>
        <Match when={repoData() !== undefined && !builtin()}>
          <p class="muted forge-note">Managed on the forge — create issues there.</p>
        </Match>
        <Match when={repoData()}>
          <FormCard
            error={error()}
            onDismissError={() => setError(null)}
            onSubmit={(e) => void submit(e)}
            busy={busy()}
            wide
            submitLabel="Create issue"
            busyLabel="Creating…"
          >
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
          </FormCard>
        </Match>
      </Switch>
    </>
  );
}
