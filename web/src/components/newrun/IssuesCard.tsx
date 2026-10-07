// The Issues card of the New run page (issue #66), for the selected repo:
// the heading with the open-issue count and the Auto switch, one AFK line
// (it replaces the AFK strip that sat under the composer), then the newest
// open issues — at most four — with their triage chip and age, and "All N
// open issues" for the rest (IssuePicker). Tapping an issue opens the action
// sheet (IssueActionSheet); the chosen action goes to the page, which
// attaches it to the composer.
//
// Reads: the repo's open issues through listIssues(repo, 'open') — the same
// bounded read the repo's Issues tab makes (routes/RepoIssues.tsx,
// ADR-0050), refetched on this repo's issue.changed — and the live instance
// list for the "N AFK runs live" count. Nothing else: the ready count is the
// repo summary's claimable count, so the card adds no forge request of its
// own. A failing tracker check skips the issue read entirely and shows the
// check's detail instead of rows (and no AFK line: nothing can be claimed).
//
// Run one, Auto and Reset go through AFKStrip's createAFKActions, so busy
// states, errors and the optimistic Auto match the repo home's AFK card. An
// AFK start is reported to the page (a toast), never a navigation.

import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createSignal,
  createUniqueId,
  on,
  type JSX,
} from 'solid-js';
import { errorMessage, listIssues, type IssueSummary, type Repo, type Run } from '../../api';
import { createLiveInstances } from '../../lib/liveInstances';
import { createLiveResource } from '../../lib/liveResource';
import { afkLine, newestIssues, type IssueAction } from '../../lib/newRun';
import { checkState, checksOf } from '../../lib/readiness';
import { resourceValue } from '../../lib/resource';
import { createAFKActions } from '../AFKStrip';
import Icon from '../Icon';
import ToggleSwitch from '../Switch';
import IssueActionSheet from './IssueActionSheet';
import IssuePicker, { IssueRow } from './IssuePicker';

/** How many issues the card lists before "All N open issues". */
const CARD_ROWS = 4;

export interface IssuesCardProps {
  repo: Repo;
  /** The operator chose an action for an issue in the action sheet. */
  onAction: (action: IssueAction, issue: IssueSummary) => void;
  /** Same meaning as AFKControlsProps in AFKStrip.tsx. */
  onRepoChanged: () => void | Promise<unknown>;
  onStarted: (run: Run) => void; // an AFK "Run one" started — the page toasts, never navigates
  onError: (message: string) => void;
  /** Clock for ages; default Date.now. */
  now?: () => number;
}

export default function IssuesCard(props: IssuesCardProps): JSX.Element {
  const headingId = `issues-card-${createUniqueId()}`;
  const now = () => (props.now ?? Date.now)();

  /** The repo's failing tracker check, if its readiness report has one. */
  const trackerCheck = () =>
    checksOf(props.repo.summary?.readiness).find(
      (check) => check.id === 'tracker' && checkState(check.state) === 'failing',
    );
  const trackerFailing = () => trackerCheck() !== undefined;

  // Keyed on the repo id; a failing tracker leaves the resource idle (the
  // read could only fail the same way).
  const [page] = createLiveResource(
    () => (trackerFailing() ? null : props.repo.id),
    (id) => listIssues(id, 'open'),
    [{ type: 'issue.changed', match: (event) => event.repoID === props.repo.id }],
  );
  const issues = (): IssueSummary[] | undefined => resourceValue(page)?.issues;

  const { instances } = createLiveInstances();
  const liveAFK = () =>
    (resourceValue(instances) ?? []).filter(
      (run) =>
        run.repo_id === props.repo.id &&
        run.live &&
        (run.kind === 'afk_manual' || run.kind === 'afk_auto'),
    ).length;

  // After a start the page's repo refetches, so the summary's ready count
  // follows the claim (the AFKCard policy).
  const afk = createAFKActions(props, () => props.onRepoChanged());
  const line = () =>
    afkLine({
      auto: afk.auto(),
      paused: afk.paused(),
      ready: props.repo.summary?.claimable ?? null,
      liveAFK: liveAFK(),
    });

  const count = (): number | null => issues()?.length ?? props.repo.summary?.open_issues ?? null;
  const rows = () => newestIssues(issues() ?? [], CARD_ROWS);

  let allButton: HTMLButtonElement | undefined;
  const [pickerOpen, setPickerOpen] = createSignal(false);
  const [sheetIssue, setSheetIssue] = createSignal<IssueSummary | null>(null);

  // Another repo: whatever was open belonged to the old one.
  createEffect(
    on(
      () => props.repo.id,
      () => {
        setPickerOpen(false);
        setSheetIssue(null);
      },
      { defer: true },
    ),
  );

  // From the picker: close it first (its cleanup hands focus back), then
  // open the sheet.
  const pick = (issue: IssueSummary) => {
    setPickerOpen(false);
    setSheetIssue(issue);
  };
  const choose = (action: IssueAction) => {
    const issue = sheetIssue();
    setSheetIssue(null);
    if (issue !== null) props.onAction(action, issue);
  };

  return (
    <section class="card issues-card" aria-labelledby={headingId}>
      <div class="issues-card-head">
        <h2 id={headingId}>Issues</h2>
        <Show when={count() !== null}>
          <span class="chip issues-card-count">{count()}</span>
        </Show>
        <span class="spacer" />
        <Show when={!afk.paused() && !trackerFailing()}>
          <ToggleSwitch
            label="Auto"
            name="afk_auto_enabled"
            checked={afk.auto()}
            disabled={afk.busy() !== null}
            onChange={(next) => void afk.toggleAuto(next)}
            class="issues-card-auto"
          />
        </Show>
      </div>

      <Show when={!trackerFailing()}>
        <div class="issues-card-afk">
          <span class="issues-card-afk-text">{line().text}</span>
          <Switch>
            <Match when={line().action === 'run-one'}>
              <button
                type="button"
                classList={{
                  'issues-card-afk-btn': true,
                  greyed: props.repo.summary?.claimable === 0,
                }}
                onClick={() => void afk.start()}
                disabled={afk.busy() !== null}
              >
                <Icon name="play" size={16} />
                {afk.busy() === 'start' ? 'Starting…' : 'Run one'}
              </button>
            </Match>
            <Match when={line().action === 'reset'}>
              <button
                type="button"
                class="issues-card-afk-btn"
                onClick={() => void afk.reset()}
                disabled={afk.busy() !== null}
              >
                {afk.busy() === 'reset' ? 'Resetting…' : 'Reset'}
              </button>
            </Match>
          </Switch>
        </div>
      </Show>

      <Switch>
        <Match when={trackerCheck()}>
          {(check) => (
            <p class="muted issues-card-note">
              {check().detail.trim() !== '' ? check().detail : 'The tracker cannot be read.'}
            </p>
          )}
        </Match>
        <Match when={page.error !== undefined}>
          <p class="muted issues-card-note">{errorMessage(page.error)}</p>
        </Match>
        <Match when={issues() === undefined}>
          <p class="muted issues-card-note issues-card-loading">Loading issues…</p>
        </Match>
        <Match when={rows().length === 0}>
          <p class="muted issues-card-note">No open issues.</p>
        </Match>
        <Match when={true}>
          <div class="issue-list">
            <For each={rows()}>
              {(issue) => (
                <IssueRow issue={issue} now={now} onSelect={() => setSheetIssue(issue)} />
              )}
            </For>
          </div>
          <Show when={(issues()?.length ?? 0) > CARD_ROWS}>
            <button
              type="button"
              class="issues-card-all"
              ref={allButton}
              aria-haspopup="dialog"
              aria-expanded={pickerOpen()}
              onClick={() => setPickerOpen((open) => !open)}
            >
              All {issues()?.length} open issues
              <Icon name="chevron-down" size={16} />
            </button>
          </Show>
        </Match>
      </Switch>

      <IssuePicker
        open={pickerOpen()}
        onClose={() => setPickerOpen(false)}
        repoName={props.repo.name}
        issues={issues() ?? []}
        anchor={() => allButton}
        onPick={pick}
        now={now}
      />
      <IssueActionSheet
        issue={sheetIssue()}
        onClose={() => setSheetIssue(null)}
        onChoose={choose}
        now={now}
      />
    </section>
  );
}
