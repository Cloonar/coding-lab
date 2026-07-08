// Issue detail (/repos/:id/issues/:number): title, state badge, pre-wrapped
// body (no markdown engine — no heavy deps), label chips, and the comments
// thread (author · time · body). On builtin repos the operator can comment,
// toggle open/closed and edit the label set (multi-select from the repo's
// labels); on forge-bound repos those controls disappear behind a short
// "managed on the forge" note while the read view keeps working.

import { A, useParams } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal, onCleanup } from 'solid-js';
import {
  createIssueComment,
  errorMessage,
  getIssue,
  getRepo,
  listLabels,
  setIssueLabels,
  updateIssue,
  type IssueDetail as Issue,
  type Label,
} from '../api';
import ErrorBanner from '../components/ErrorBanner';
import LabelChip from '../components/LabelChip';
import LabelPicker from '../components/LabelPicker';
import RequireAuth from '../components/RequireAuth';
import { useEvents } from '../events';
import { canMutateTracker, formatDateTime } from '../lib/issues';
import { sameLabelSet, toggleLabel } from '../lib/labels';
import { resourceValue } from '../lib/resource';

export default function IssueDetail() {
  return (
    <RequireAuth>
      <IssueDetailView />
    </RequireAuth>
  );
}

function IssueDetailView() {
  const params = useParams<{ id: string; number: string }>();
  const events = useEvents();
  const issueNumber = () => Number(params.number);

  const [repo] = createResource(
    () => params.id,
    (id) => getRepo(id),
  );
  const [issue, { refetch }] = createResource(
    () => `${params.id}\n${params.number}`,
    (key) => {
      const sep = key.indexOf('\n');
      return getIssue(key.slice(0, sep), Number(key.slice(sep + 1)));
    },
  );
  // Every read outside the guarded <Match> branches goes through these
  // non-throwing accessors: a failed getRepo/listLabels must degrade the page
  // (read-only view + error banner), not blank a successfully loaded issue.
  const repoData = () => resourceValue(repo);
  const [labels, { refetch: refetchLabels }] = createResource(
    () => (repoData()?.tracker_binding === 'builtin' ? params.id : null),
    (id) => listLabels(id),
  );
  const labelList = () => resourceValue(labels);
  onCleanup(
    events.subscribe('issue.changed', (event) => {
      if (event.repoID !== params.id) return;
      void refetch();
      if (repoData()?.tracker_binding === 'builtin') void refetchLabels();
    }),
  );

  const canMutate = () => {
    const r = repoData();
    return r !== undefined && canMutateTracker(r.tracker_binding);
  };
  const colorOf = (name: string): string | undefined =>
    labelList()?.find((label) => label.name === name)?.color;

  const [error, setError] = createSignal<string | null>(null);
  const [stateBusy, setStateBusy] = createSignal(false);

  const toggleState = async (current: Issue) => {
    setError(null);
    setStateBusy(true);
    try {
      await updateIssue(params.id, issueNumber(), {
        state: current.state === 'open' ? 'closed' : 'open',
      });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setStateBusy(false);
      void refetch();
    }
  };

  return (
    <main class="page">
      <p class="crumb">
        <A href={`/repos/${params.id}/issues`}>← Issues</A>
      </p>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={repo.error !== undefined}>
        {/* The repo lookup failing must not blank a loaded issue: the view
            degrades to read-only (binding unknown) with the failure visible. */}
        <div class="banner error" role="alert">
          <span class="banner-text">{errorMessage(repo.error)}</span>
        </div>
      </Show>
      <Switch>
        <Match when={issue.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(issue.error)}</span>
          </div>
        </Match>
        <Match when={issue()}>
          {(i) => (
            <>
              <div class="section-head">
                <h2>
                  <span class="mono muted issue-number">#{i().number}</span> {i().title}
                </h2>
                <span class={`chip state-${i().state}`}>{i().state}</span>
              </div>
              <p class="muted card-sub">
                opened {formatDateTime(i().created_at)} · updated {formatDateTime(i().updated_at)}
              </p>
              <Show when={repoData() !== undefined && !canMutate()}>
                <p class="muted forge-note">Managed on the forge — lab shows a read-only view.</p>
              </Show>

              <div class="stack">
                <section class="card">
                  <Show
                    when={i().body !== ''}
                    fallback={<p class="muted issue-body">No description.</p>}
                  >
                    <p class="issue-body">{i().body}</p>
                  </Show>
                  <div class="chip-row">
                    <For each={i().labels}>
                      {(name) => <LabelChip name={name} color={colorOf(name)} />}
                    </For>
                    <Show when={i().labels.length === 0}>
                      <span class="muted issue-meta">No labels</span>
                    </Show>
                  </div>
                  <Show when={canMutate()}>
                    <LabelEditor
                      repoID={params.id}
                      number={i().number}
                      current={i().labels}
                      labels={labelList() ?? []}
                      onError={setError}
                      onSaved={() => void refetch()}
                    />
                    <div class="card-actions">
                      <button
                        type="button"
                        class={i().state === 'open' ? 'danger' : ''}
                        disabled={stateBusy()}
                        onClick={() => void toggleState(i())}
                      >
                        {stateBusy()
                          ? 'Working…'
                          : i().state === 'open'
                            ? 'Close issue'
                            : 'Reopen issue'}
                      </button>
                    </div>
                  </Show>
                </section>

                <section class="card">
                  <h2>Comments ({i().comments.length})</h2>
                  <Show
                    when={i().comments.length > 0}
                    fallback={<p class="muted">No comments yet.</p>}
                  >
                    <ul class="comment-list">
                      <For each={i().comments}>
                        {(comment) => (
                          <li class="comment">
                            <p class="muted comment-meta">
                              <strong>{comment.author}</strong> ·{' '}
                              {formatDateTime(comment.created_at)}
                            </p>
                            <p class="comment-body">{comment.body}</p>
                          </li>
                        )}
                      </For>
                    </ul>
                  </Show>
                  <Show when={canMutate()}>
                    <CommentForm
                      repoID={params.id}
                      number={i().number}
                      onError={setError}
                      onPosted={() => void refetch()}
                    />
                  </Show>
                </section>
              </div>
            </>
          )}
        </Match>
      </Switch>
    </main>
  );
}

/**
 * Multi-select label editor: seeds the selection from the issue's current
 * labels on open; Save stays disabled until the set actually differs.
 */
function LabelEditor(props: {
  repoID: string;
  number: number;
  current: string[];
  labels: Label[];
  onError: (message: string) => void;
  onSaved: () => void;
}) {
  const [editing, setEditing] = createSignal(false);
  const [selected, setSelected] = createSignal<string[]>([]);
  const [busy, setBusy] = createSignal(false);

  const open = () => {
    setSelected(props.current);
    setEditing(true);
  };
  const dirty = () => !sameLabelSet(selected(), props.current);

  const save = async () => {
    setBusy(true);
    try {
      await setIssueLabels(props.repoID, props.number, selected());
      setEditing(false);
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
      props.onSaved();
    }
  };

  return (
    <Show
      when={editing()}
      fallback={
        <div class="card-actions">
          <button type="button" class="small" onClick={open}>
            Edit labels
          </button>
        </div>
      }
    >
      <LabelPicker
        labels={props.labels}
        selected={selected()}
        onToggle={(name) => setSelected(toggleLabel(selected(), name))}
      />
      <div class="card-actions">
        <button
          type="button"
          class="primary"
          disabled={!dirty() || busy()}
          onClick={() => void save()}
        >
          {busy() ? 'Saving…' : 'Save labels'}
        </button>
        <button type="button" onClick={() => setEditing(false)} disabled={busy()}>
          Cancel
        </button>
      </div>
    </Show>
  );
}

function CommentForm(props: {
  repoID: string;
  number: number;
  onError: (message: string) => void;
  onPosted: () => void;
}) {
  const [draft, setDraft] = createSignal('');
  const [busy, setBusy] = createSignal(false);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    const body = draft().trim();
    if (body === '') return;
    setBusy(true);
    try {
      await createIssueComment(props.repoID, props.number, body);
      setDraft('');
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
      props.onPosted();
    }
  };

  return (
    <form class="comment-form" onSubmit={(e) => void submit(e)}>
      <label class="field">
        <span>Add a comment</span>
        <textarea
          name="comment"
          rows="3"
          value={draft()}
          onInput={(e) => setDraft(e.currentTarget.value)}
        />
      </label>
      <button type="submit" class="primary" disabled={busy() || draft().trim() === ''}>
        {busy() ? 'Posting…' : 'Comment'}
      </button>
    </form>
  );
}
