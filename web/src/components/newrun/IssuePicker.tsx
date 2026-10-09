// The issue picker of the New run page (issue #66): "All N open issues" on
// the Issues card opens every open issue in place — a bottom sheet on a
// phone, a wide popover anchored to that button on desktop. A search field
// (an issue number, with or without `#`, or words from the title) and the
// triage-state chips with their counts stay pinned while the rows scroll;
// Enter picks the first match. Picking a row hands the issue to the card,
// which closes this picker and opens the action sheet.
//
// The rows are the card's own (IssueRow, exported here with TriageChip so
// the card, this picker and the action sheet draw an issue one way); an
// issue with an open PR adds a "PR #88" chip in the run tint after the
// triage chip (issue #88). The filtering and counting rules live in
// lib/newRun.ts.

import { For, Show, createEffect, createSignal, createUniqueId, on, type JSX } from 'solid-js';
import type { IssueSummary } from '../../api';
import {
  ISSUE_FILTERS,
  filterIssues,
  issueAge,
  issueFilterCounts,
  newestIssues,
  triageState,
  triageTint,
  type IssueFilter,
} from '../../lib/newRun';
import Picker, { PickerFilters, PickerSearch, type PickerFilter } from '../Picker';

/** The app's existing chip tints, by triage tint: run (green), notice (amber), idle. */
const TINT_CLASS = { run: 'in-use', notice: 'status-warn', idle: 'idle' } as const;

/** The issue's triage label as a tinted chip; nothing for an unlabeled issue. */
export function TriageChip(props: { labels: readonly string[] }): JSX.Element {
  const state = () => triageState(props.labels);
  return (
    <Show when={triageTint(state())}>
      {(tint) => <span class={`chip triage-chip ${TINT_CLASS[tint()]}`}>{state()}</span>}
    </Show>
  );
}

/**
 * One issue as a full-width button (>=44px): the number in mono, the title
 * clamped to two lines, the triage chip, the open PR's chip (`PR #88`, run
 * tint) when the issue has one, the age. Other labels are not shown. The
 * chips share one wrapping group, so on a phone the two stack rather than
 * squeeze the title down to a word.
 */
export function IssueRow(props: {
  issue: IssueSummary;
  now: () => number;
  onSelect: () => void;
}): JSX.Element {
  return (
    <button type="button" class="issue-row" aria-haspopup="dialog" onClick={() => props.onSelect()}>
      <span class="mono muted issue-row-number">#{props.issue.number}</span>
      <span class="issue-row-title">{props.issue.title}</span>
      <span class="issue-row-chips">
        <TriageChip labels={props.issue.labels} />
        <Show when={props.issue.pull}>
          {(pull) => <span class={`chip pr-chip ${TINT_CLASS.run}`}>PR #{pull().number}</span>}
        </Show>
      </span>
      <small class="muted issue-row-age">{issueAge(props.issue.created_at, props.now())}</small>
    </button>
  );
}

/** The chip label of a state filter. */
function filterLabel(filter: IssueFilter): string {
  return filter === 'all' ? 'All' : filter;
}

export default function IssuePicker(props: {
  open: boolean;
  onClose: () => void;
  repoName: string;
  issues: IssueSummary[];
  anchor?: () => HTMLElement | undefined;
  onPick: (issue: IssueSummary) => void;
  now: () => number;
}): JSX.Element {
  const listId = `issue-picker-${createUniqueId()}`;
  let search: HTMLInputElement | undefined;
  const [query, setQuery] = createSignal('');
  const [chosen, setChosen] = createSignal<IssueFilter>('all');

  // Every opening starts from the whole list.
  createEffect(
    on(
      () => props.open,
      (open) => {
        if (!open) return;
        setQuery('');
        setChosen('all');
      },
    ),
  );

  const counts = () => issueFilterCounts(props.issues);
  // A pressed chip whose count fell to zero (a live update) is hidden, so
  // the rows fall back to All rather than to an invisible filter.
  const state = (): IssueFilter => {
    const value = chosen();
    return value === 'all' || counts()[value] > 0 ? value : 'all';
  };
  const filters = (): PickerFilter[] =>
    ISSUE_FILTERS.filter((filter) => filter === 'all' || counts()[filter] > 0).map((filter) => ({
      value: filter,
      label: filterLabel(filter),
      count: counts()[filter],
    }));
  const rows = () =>
    filterIssues(newestIssues(props.issues, props.issues.length), query(), state());

  const pickFirst = () => {
    const first = rows()[0];
    if (first !== undefined) props.onPick(first);
  };

  return (
    <Picker
      open={props.open}
      onClose={() => props.onClose()}
      title={`Open issues · ${props.repoName}`}
      size="wide"
      anchor={props.anchor}
      initialFocus={() => search}
      class="issue-picker"
      header={
        <>
          <PickerSearch
            ref={(el) => (search = el)}
            value={query()}
            onInput={setQuery}
            placeholder="Number or words from the title"
            aria-label="Filter issues"
            aria-controls={listId}
            onEnter={pickFirst}
          />
          <PickerFilters
            items={filters()}
            value={state()}
            onChange={(value) => setChosen(value as IssueFilter)}
            aria-label="Filter by state"
          />
        </>
      }
    >
      <div class="issue-list" id={listId}>
        <For each={rows()} fallback={<p class="muted issue-list-empty">No matching issues.</p>}>
          {(issue) => (
            <IssueRow issue={issue} now={props.now} onSelect={() => props.onPick(issue)} />
          )}
        </For>
      </div>
    </Picker>
  );
}
