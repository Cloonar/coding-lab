// The repositories list's rows (issue #61), in its two layouts:
// - RepoList, below 1024px: one row per repo stacking the name (Incogni as a
//   chip beside it), the remote as host plus path, and ONE status line — live
//   runs with the count waiting for the operator, the claimable count, the AFK
//   state;
// - RepoTable, from 1024px: the same data as a real table with the columns
//   Repository, Runs, Ready, AFK, Autoland and Last run.
// In both, the whole row opens the repo home: the name is a real link (the
// keyboard path, and Ctrl/Cmd/Shift/middle-click on it open a new tab or
// window as on any link), and a plain click anywhere else on the row follows
// it too. A modified or non-primary click on the row, or a click that ends a
// text selection made in the row, is left alone. Rows are keyed by the
// caller's stable row objects, so a refetch updates them in place.
// A cloning repo shows its progress ("Cloning 62%") and dashes for the AFK
// columns; a failed clone and a not-ready repo say so in words. An unknown
// claimable count is a dash, never 0.

import { A, useNavigate } from '@solidjs/router';
import { For, Show, type JSX } from 'solid-js';
import type { Repo } from '../../api';
import Icon from '../../components/Icon';
import { afkState, isNotReady, relativeTime, type RunCounts } from '../../lib/repoList';
import { remoteLabel } from '../../lib/repoName';
import type { CloneProgress } from '../../stores/cloneProgress';

export interface RepoRowData {
  repo: Repo;
  /** Live and waiting instances — read reactively (a getter in the list). */
  readonly counts: RunCounts;
  /** The running clone's latest progress, or null — read reactively. */
  readonly progress: CloneProgress | null;
}

/**
 * Follows the row's link on a plain primary click, unless the click landed
 * on a control of its own (the link itself, a button), carried a modifier
 * (Ctrl/Cmd/Shift/Alt mean "somewhere else" to the browser — the name link
 * does that natively), or ended a text selection made inside the row.
 */
function useRowClick(): (event: MouseEvent, repoID: string) => void {
  const navigate = useNavigate();
  return (event, repoID) => {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
    const target = event.target instanceof Element ? event.target : null;
    if (target !== null && target.closest('a, button, input, label') !== null) return;
    const row = event.currentTarget instanceof Node ? event.currentTarget : null;
    if (row !== null && selectingIn(row)) return;
    navigate(`/repos/${repoID}`);
  };
}

/** True when a non-empty text selection starts or ends inside `row`. */
function selectingIn(row: Node): boolean {
  const selection = window.getSelection?.();
  if (selection === null || selection === undefined || selection.isCollapsed) return false;
  if (selection.toString().trim() === '') return false;
  const inRow = (node: Node | null) => node !== null && row.contains(node);
  return inRow(selection.anchorNode) || inRow(selection.focusNode);
}

/** "Cloning 62%" / "Cloning" while the percent is unknown. */
function cloningLabel(progress: CloneProgress | null): string {
  const percent = progress?.percent ?? null;
  return percent === null ? 'Cloning' : `Cloning ${percent}%`;
}

function RepoName(props: { repo: Repo }) {
  return (
    <span class="repo-row-name">
      <A href={`/repos/${props.repo.id}`} class="repo-row-link">
        {props.repo.name}
      </A>
      <Show when={props.repo.incogni}>
        <span class="chip incogni">incogni</span>
      </Show>
    </span>
  );
}

function Dash(props: { label: string }) {
  return (
    <span class="muted">
      <span aria-hidden="true">–</span>
      <span class="visually-hidden">{props.label}</span>
    </span>
  );
}

function LiveCount(props: { counts: RunCounts; none: string }) {
  return (
    <Show when={props.counts.live > 0} fallback={<span class="muted">{props.none}</span>}>
      <span class="repo-live">
        <span class="live-dot-ok" aria-hidden="true" />
        {props.counts.live} live
      </span>
      <Show when={props.counts.waiting > 0}>
        {' '}
        <span class="chip waiting">{props.counts.waiting} waiting</span>
      </Show>
    </Show>
  );
}

function AFKLabel(props: { repo: Repo; short: boolean }) {
  return (
    <>
      <Show
        when={afkState(props.repo) === 'paused'}
        fallback={
          <span classList={{ muted: !props.repo.afk_auto_enabled }}>
            {props.repo.afk_auto_enabled ? 'Auto on' : props.short ? 'Off' : 'Auto off'}
          </span>
        }
      >
        <span class="chip status-cloning">{props.short ? 'Paused' : 'AFK paused'}</span>
      </Show>
      <Show when={isNotReady(props.repo)}>
        {' '}
        <span class="chip status-error">Not ready</span>
      </Show>
    </>
  );
}

/** Phone rows: name, remote, one status line. */
export function RepoList(props: { rows: RepoRowData[]; labelledBy: string; empty?: JSX.Element }) {
  const rowClick = useRowClick();
  return (
    <Show when={props.rows.length > 0} fallback={props.empty}>
      <ul class="repo-list" aria-labelledby={props.labelledBy}>
        <For each={props.rows}>
          {(row) => (
            <li class="repo-row" onClick={(event) => rowClick(event, row.repo.id)}>
              <span class="repo-row-main">
                <RepoName repo={row.repo} />
                <span class="mono repo-row-remote">{remoteLabel(row.repo.remote_url)}</span>
                <span class="repo-row-status">
                  <StatusLine row={row} />
                </span>
              </span>
              <Icon name="chevron-right" size={20} class="repo-row-chevron" />
            </li>
          )}
        </For>
      </ul>
    </Show>
  );
}

function StatusLine(props: { row: RepoRowData }) {
  const repo = () => props.row.repo;
  const claimable = () => repo().summary?.claimable ?? null;
  return (
    <>
      <Show when={repo().clone_status === 'cloning'}>
        <span class="chip status-cloning">{cloningLabel(props.row.progress)}</span>
      </Show>
      <Show when={repo().clone_status === 'error'}>
        <span class="chip status-error">Clone failed</span>
      </Show>
      <Show when={repo().clone_status === 'ready'}>
        <span class="repo-status-part">
          <LiveCount counts={props.row.counts} none="No live runs" />
        </span>
        <span class="repo-status-part">
          <Show
            when={claimable() !== null}
            fallback={
              <span class="muted">
                <span aria-hidden="true">–</span> ready
                <span class="visually-hidden"> count not known</span>
              </span>
            }
          >
            {claimable()} ready
          </Show>
        </span>
        <span class="repo-status-part">
          <AFKLabel repo={repo()} short={false} />
        </span>
      </Show>
    </>
  );
}

/** Desktop: a semantic table, one row per repo. */
export function RepoTable(props: {
  rows: RepoRowData[];
  caption: string;
  now: number;
  empty?: JSX.Element;
}) {
  const rowClick = useRowClick();
  return (
    <Show when={props.rows.length > 0} fallback={props.empty}>
      <table class="repo-table">
        <caption class="visually-hidden">{props.caption}</caption>
        <colgroup>
          <col class="col-repo" />
          <col class="col-runs" />
          <col class="col-ready" />
          <col class="col-afk" />
          <col class="col-autoland" />
          <col class="col-last" />
        </colgroup>
        <thead>
          <tr>
            <th scope="col">Repository</th>
            <th scope="col">Runs</th>
            <th scope="col">Ready</th>
            <th scope="col">AFK</th>
            <th scope="col">Autoland</th>
            <th scope="col">Last run</th>
          </tr>
        </thead>
        <tbody>
          <For each={props.rows}>
            {(row) => (
              <TableRow
                row={row}
                now={props.now}
                onClick={(event) => rowClick(event, row.repo.id)}
              />
            )}
          </For>
        </tbody>
      </table>
    </Show>
  );
}

function TableRow(props: { row: RepoRowData; now: number; onClick: (event: MouseEvent) => void }) {
  const repo = () => props.row.repo;
  const ready = () => repo().clone_status === 'ready';
  const claimable = () => repo().summary?.claimable ?? null;
  const lastRun = () => relativeTime(repo().last_opened_at, props.now);
  return (
    <tr class="repo-row" onClick={(event) => props.onClick(event)}>
      <th scope="row">
        <span class="repo-row-main">
          <RepoName repo={repo()} />
          <span class="mono repo-row-remote">{remoteLabel(repo().remote_url)}</span>
        </span>
      </th>
      <td>
        <span class="repo-cell">
          <Show when={repo().clone_status === 'cloning'}>
            <span class="chip status-cloning">{cloningLabel(props.row.progress)}</span>
          </Show>
          <Show when={repo().clone_status === 'error'}>
            <span class="chip status-error">Clone failed</span>
          </Show>
          <Show when={ready()}>
            <LiveCount counts={props.row.counts} none="None" />
          </Show>
        </span>
      </td>
      <td>
        <Show when={ready() && claimable() !== null} fallback={<Dash label="Not known" />}>
          <span classList={{ muted: claimable() === 0 }}>{claimable()}</span>
        </Show>
      </td>
      <td>
        <span class="repo-cell">
          <Show when={ready()} fallback={<Dash label="Not available" />}>
            <AFKLabel repo={repo()} short={true} />
          </Show>
        </span>
      </td>
      <td>
        <Show when={ready()} fallback={<Dash label="Not available" />}>
          <span classList={{ muted: !repo().autoland_enabled }}>
            {repo().autoland_enabled ? 'On' : 'Off'}
          </span>
        </Show>
      </td>
      <td>
        <span class="repo-cell repo-last">
          <span class="muted">{lastRun()}</span>
          <Icon name="chevron-right" size={18} class="repo-row-chevron" />
        </span>
      </td>
    </tr>
  );
}
