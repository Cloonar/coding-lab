// Issue detail (/repos/:id/issues/:number): title, state badge, pre-wrapped
// body (no markdown engine — no heavy deps), label chips, and the comments
// thread (author · time · body). Title and body are editable on both bindings
// — the server routes the patch through the tracker seam. State, labels and
// comments stay builtin-only: their controls disappear on forge-bound repos
// behind a short managed-on-the-forge note while the read view keeps working.

import { useParams } from '@solidjs/router';
import { For, Match, Show, Switch, createResource, createSignal } from 'solid-js';
import {
  createIssueComment,
  errorMessage,
  getIssue,
  getRepo,
  listLabels,
  setIssueLabels,
  updateIssue,
  type IssueDetail as Issue,
  type IssuePatch,
  type Label,
} from '../api';
import Crumbs, { type Crumb } from '../components/Crumbs';
import Banner from '../components/Banner';
import LabelChip from '../components/LabelChip';
import LabelPicker from '../components/LabelPicker';
import RequireAuth from '../components/RequireAuth';
import SectionCard from '../components/SectionCard';
import SectionHead from '../components/SectionHead';
import { canMutateTracker, formatDateTime } from '../lib/issues';
import { sameLabelSet, toggleLabel } from '../lib/labels';
import { createLiveResource } from '../lib/liveResource';
import { forgeWebUrl } from '../lib/repoName';
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
  const issueNumber = () => Number(params.number);

  const [repo] = createResource(
    () => params.id,
    (id) => getRepo(id),
  );
  const [issue, { refetch }] = createLiveResource(
    () => `${params.id}\n${params.number}`,
    (key) => {
      const sep = key.indexOf('\n');
      return getIssue(key.slice(0, sep), Number(key.slice(sep + 1)));
    },
    [{ type: 'issue.changed', match: (event) => event.repoID === params.id }],
  );
  // Every read outside the guarded <Match> branches goes through these
  // non-throwing accessors: a failed getRepo/listLabels must degrade the page
  // (read-only view + error banner), not blank a successfully loaded issue.
  const repoData = () => resourceValue(repo);
  const [labels] = createLiveResource(
    () => (repoData()?.tracker_binding === 'builtin' ? params.id : null),
    (id) => listLabels(id),
    [
      {
        type: 'issue.changed',
        match: (event) => event.repoID === params.id && repoData()?.tracker_binding === 'builtin',
      },
    ],
  );
  const labelList = () => resourceValue(labels);

  const canMutate = () => {
    const r = repoData();
    return r !== undefined && canMutateTracker(r.tracker_binding);
  };
  const colorOf = (name: string): string | undefined =>
    labelList()?.find((label) => label.name === name)?.color;

  const crumbs = (): Crumb[] => [
    { label: 'Repos', href: '/repos' },
    { label: repoData()?.name ?? 'Repository', href: `/repos/${params.id}/issues` },
    { label: 'Issues', href: `/repos/${params.id}/issues` },
    { label: `#${issueNumber()}` },
  ];

  // The forge note deep-links to this exact issue when the remote parses; a
  // remote that doesn't (or a missing repo) leaves the sentence plain text.
  const forgeIssueUrl = (number: number): string | null => {
    const r = repoData();
    if (r === undefined) return null;
    const base = forgeWebUrl(r.remote_url, r.forge_kind);
    return base === null ? null : `${base}/issues/${number}`;
  };

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
      <Crumbs segments={crumbs()} />
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Show when={repo.error !== undefined}>
        {/* The repo lookup failing must not blank a loaded issue: the view
            degrades to read-only (binding unknown) with the failure visible. */}
        <Banner message={errorMessage(repo.error)} />
      </Show>
      <Switch>
        <Match when={issue.error !== undefined}>
          <Banner message={errorMessage(issue.error)} />
        </Match>
        <Match when={issue()}>
          {(i) => (
            <>
              <SectionHead
                title={
                  <>
                    <span class="mono muted issue-number">#{i().number}</span> {i().title}
                  </>
                }
                action={<span class={`chip state-${i().state}`}>{i().state}</span>}
              />
              <p class="muted card-sub">
                opened {formatDateTime(i().created_at)} · updated {formatDateTime(i().updated_at)}
              </p>
              <Show when={repoData() !== undefined && !canMutate()}>
                <p class="muted forge-note">
                  <Show
                    when={forgeIssueUrl(i().number)}
                    fallback="State, labels and comments are managed on the forge; title and body are editable here."
                  >
                    {(href) => (
                      <a href={href()} target="_blank" rel="noreferrer">
                        State, labels and comments are managed on the forge; title and body are
                        editable here.
                      </a>
                    )}
                  </Show>
                </p>
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
                  <Show when={repoData() !== undefined}>
                    <TitleBodyEditor
                      repoID={params.id}
                      number={i().number}
                      title={i().title}
                      body={i().body}
                      onError={setError}
                      onSaved={() => void refetch()}
                    />
                  </Show>
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

                <SectionCard title={`Comments (${i().comments.length})`}>
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
                </SectionCard>
              </div>
            </>
          )}
        </Match>
      </Switch>
    </main>
  );
}

/**
 * Title/body editor: seeds both fields from the issue on open and sends a
 * single PATCH carrying only the fields that actually changed ({title} and/or
 * {body}, never state). Available on every binding — the server routes the
 * patch through the tracker seam. An empty body is legal (clears it); Save
 * stays disabled while the trimmed title is empty or nothing differs.
 */
function TitleBodyEditor(props: {
  repoID: string;
  number: number;
  title: string;
  body: string;
  onError: (message: string) => void;
  onSaved: () => void;
}) {
  const [editing, setEditing] = createSignal(false);
  const [title, setTitle] = createSignal('');
  const [body, setBody] = createSignal('');
  const [busy, setBusy] = createSignal(false);

  const open = () => {
    setTitle(props.title);
    setBody(props.body);
    setEditing(true);
  };
  const titleChanged = () => title().trim() !== props.title;
  const bodyChanged = () => body() !== props.body;
  const dirty = () => titleChanged() || bodyChanged();

  const save = async () => {
    const patch: IssuePatch = {};
    if (titleChanged()) patch.title = title().trim();
    if (bodyChanged()) patch.body = body();
    setBusy(true);
    try {
      await updateIssue(props.repoID, props.number, patch);
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
            Edit
          </button>
        </div>
      }
    >
      <label class="field">
        <span>Title</span>
        <input
          type="text"
          name="issue-title"
          value={title()}
          onInput={(e) => setTitle(e.currentTarget.value)}
        />
      </label>
      <label class="field">
        <span>Description</span>
        <textarea
          name="issue-body"
          rows="6"
          value={body()}
          onInput={(e) => setBody(e.currentTarget.value)}
        />
      </label>
      <div class="card-actions">
        <button
          type="button"
          class="primary"
          disabled={busy() || title().trim() === '' || !dirty()}
          onClick={() => void save()}
        >
          {busy() ? 'Saving…' : 'Save'}
        </button>
        <button type="button" onClick={() => setEditing(false)} disabled={busy()}>
          Cancel
        </button>
      </div>
    </Show>
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
