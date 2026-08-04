// Change-request detail (/repos/:id/crs/:number): title, state, branches,
// pre-wrapped body, closes chips linking to the built-in issues, and the
// phone-first diff view — monospace rows grouped per file, +/- line tinting,
// visually distinct sticky-ish file headers, horizontal scrolling INSIDE
// each file's container (the page never scrolls sideways), a truncation
// notice when the server bounded the diff and the server's note when the
// head branch is gone. Merge sits behind a confirm panel (ff vs merge-commit
// is the server's concern — the UI just confirms); a 409 body — push
// rejection stderr, non-open CR, missing head branch — is shown verbatim in
// the error banner because it carries the actionable git/hook message.
// cr.changed (scoped to this repo) refetches.

import { useParams } from '@solidjs/router';
import { For, Match, Show, Switch, createMemo, createResource, createSignal } from 'solid-js';
import { closeCR, errorMessage, getCR, getRepo, mergeCR, type CRDetail as CR } from '../api';
import ClosesChips from '../components/ClosesChips';
import Crumbs, { type Crumb } from '../components/Crumbs';
import ErrorBanner from '../components/ErrorBanner';
import RequireAuth from '../components/RequireAuth';
import SectionCard from '../components/SectionCard';
import SectionHead from '../components/SectionHead';
import { classifyDiff, groupDiffFiles, type DiffFileGroup } from '../lib/crs';
import { formatDateTime } from '../lib/issues';
import { createLiveResource } from '../lib/liveResource';
import { resourceValue } from '../lib/resource';

export default function CRDetail() {
  return (
    <RequireAuth>
      <CRDetailView />
    </RequireAuth>
  );
}

function CRDetailView() {
  const params = useParams<{ id: string; number: string }>();
  const crNumber = () => Number(params.number);

  const [cr, { refetch }] = createLiveResource(
    () => `${params.id}\n${params.number}`,
    (key) => {
      const sep = key.indexOf('\n');
      return getCR(key.slice(0, sep), Number(key.slice(sep + 1)));
    },
    [{ type: 'cr.changed', match: (event) => event.repoID === params.id }],
  );
  // Non-throwing accessor for reads outside the guarded <Match> branches.
  const crData = () => resourceValue(cr);
  // The repo feeds the breadcrumb name only; a failed lookup must degrade to
  // the placeholder, never blank the CR view — so it goes through the same
  // non-throwing accessor.
  const [repo] = createResource(
    () => params.id,
    (id) => getRepo(id),
  );
  const repoData = () => resourceValue(repo);

  const crumbs = (): Crumb[] => [
    { label: 'Repos', href: '/repos' },
    { label: repoData()?.name ?? 'Repository', href: `/repos/${params.id}/issues` },
    { label: 'Change requests', href: `/repos/${params.id}/crs` },
    { label: `#${crNumber()}` },
  ];

  // Memoized: the classifier walks the (up to ~1MiB) diff text once per
  // fetched detail, not once per render.
  const files = createMemo(() => {
    const diff = crData()?.diff;
    return diff === undefined ? null : groupDiffFiles(classifyDiff(diff));
  });

  const [error, setError] = createSignal<string | null>(null);
  const [confirming, setConfirming] = createSignal(false);
  const [busy, setBusy] = createSignal(false);

  const merge = async () => {
    setError(null);
    setBusy(true);
    try {
      await mergeCR(params.id, crNumber());
      setConfirming(false);
    } catch (err) {
      // 409 body verbatim: push-rejection stderr is the actionable message.
      setError(errorMessage(err));
    } finally {
      // Stay busy until the refetch settles: cr() holds the stale 'open'
      // state until then, and re-enabled buttons against it invite a mis-tap
      // ("Close without merging" on a just-merged CR).
      await refetch();
      setBusy(false);
    }
  };

  const close = async () => {
    setError(null);
    setBusy(true);
    try {
      await closeCR(params.id, crNumber());
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      await refetch();
      setBusy(false);
    }
  };

  return (
    <main class="page">
      <Crumbs segments={crumbs()} />
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Switch>
        <Match when={cr.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(cr.error)}</span>
          </div>
        </Match>
        <Match when={cr()}>
          {(c) => (
            <>
              <SectionHead
                title={
                  <>
                    <span class="mono muted issue-number">CR #{c().number}</span> {c().title}
                  </>
                }
                action={<span class={`chip state-${c().state}`}>{c().state}</span>}
              />
              <p class="muted card-sub mono cr-branches">
                {c().head_branch} → {c().base_branch}
              </p>
              <p class="muted card-sub">
                opened {formatDateTime(c().created_at)}
                <Show when={c().merged_at !== null}>
                  {' '}
                  · merged {formatDateTime(c().merged_at!)}
                </Show>
                <Show when={c().merge_commit !== null}>
                  {' '}
                  · <span class="mono">{c().merge_commit!.slice(0, 10)}</span>
                </Show>
              </p>

              <div class="stack">
                <section class="card">
                  <Show
                    when={c().body !== ''}
                    fallback={<p class="muted issue-body">No description.</p>}
                  >
                    <p class="issue-body">{c().body}</p>
                  </Show>
                  <div class="chip-row">
                    <ClosesChips repoID={params.id} closes={c().closes} />
                    <Show when={c().closes.length === 0}>
                      <span class="muted issue-meta">Closes no issues</span>
                    </Show>
                  </div>
                  <Show when={c().state === 'open'}>
                    <Show
                      when={confirming()}
                      fallback={
                        <div class="card-actions">
                          <button
                            type="button"
                            class="primary"
                            disabled={busy()}
                            onClick={() => setConfirming(true)}
                          >
                            Merge
                          </button>
                          <button
                            type="button"
                            class="danger"
                            disabled={busy()}
                            onClick={() => void close()}
                          >
                            {busy() ? 'Working…' : 'Close without merging'}
                          </button>
                        </div>
                      }
                    >
                      <MergeConfirm
                        cr={c()}
                        busy={busy()}
                        onConfirm={() => void merge()}
                        onCancel={() => setConfirming(false)}
                      />
                    </Show>
                  </Show>
                </section>

                <SectionCard title="Diff" class="diff-card">
                  <Show when={c().diff_truncated === true}>
                    <p class="diff-truncated">
                      Diff truncated — the change is larger than the view limit.
                    </p>
                  </Show>
                  <Show
                    when={files() !== null}
                    fallback={<p class="muted diff-note">{c().note ?? 'No diff available.'}</p>}
                  >
                    <Show
                      when={files()!.length > 0}
                      fallback={<p class="muted diff-note">No changes between the branches.</p>}
                    >
                      <For each={files()}>{(file) => <DiffFileSection file={file} />}</For>
                    </Show>
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
 * Rows rendered per file before the rest goes behind an explicit "Show
 * remaining…" tap. The server bounds the diff at ~1MiB precisely so it can
 * reach a phone — but eagerly materializing tens of thousands of DOM rows
 * (one div per line, laid out at intrinsic width) freezes exactly that
 * phone. 400 rows keeps every ordinary file fully visible.
 */
export const DIFF_ROWS_COLLAPSED = 400;

function DiffFileSection(props: { file: DiffFileGroup }) {
  const [expanded, setExpanded] = createSignal(false);
  const rows = () => (expanded() ? props.file.rows : props.file.rows.slice(0, DIFF_ROWS_COLLAPSED));
  const hidden = () => props.file.rows.length - DIFF_ROWS_COLLAPSED;
  return (
    <section class="diff-file-section">
      <Show when={props.file.name !== ''}>
        <div class="diff-file-head mono">{props.file.name}</div>
      </Show>
      <div class="diff-scroll">
        <For each={rows()}>
          {(row) => <div class={`diff-row diff-${row.kind}`}>{row.text}</div>}
        </For>
      </div>
      <Show when={!expanded() && hidden() > 0}>
        <button type="button" class="diff-expand" onClick={() => setExpanded(true)}>
          Show remaining {hidden()} lines
        </button>
      </Show>
    </section>
  );
}

/**
 * Merge confirm gate: nothing is POSTed until the operator confirms here.
 * Whether the merge fast-forwards or builds a merge commit is decided
 * server-side, so the panel only states what is observable: the branches
 * and the built-in issues the merge will close.
 */
function MergeConfirm(props: {
  cr: CR;
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <div class="merge-confirm" role="alertdialog" aria-label={`Merge CR #${props.cr.number}`}>
      <p class="merge-question">
        Merge <span class="mono">{props.cr.head_branch}</span> into{' '}
        <span class="mono">{props.cr.base_branch}</span> and push to the remote?
        <Show when={props.cr.closes.length > 0}>
          {' '}
          This closes issue{props.cr.closes.length === 1 ? '' : 's'}{' '}
          {props.cr.closes.map((n) => `#${n}`).join(', ')}.
        </Show>
      </p>
      <div class="card-actions">
        <button
          type="button"
          class="primary"
          disabled={props.busy}
          onClick={() => props.onConfirm()}
        >
          {props.busy ? 'Merging…' : 'Confirm merge'}
        </button>
        <button type="button" disabled={props.busy} onClick={() => props.onCancel()}>
          Cancel
        </button>
      </div>
    </div>
  );
}
