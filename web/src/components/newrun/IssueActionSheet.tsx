// The issue action sheet of the New run page (issue #66): tapping an issue
// (on the Issues card or in the issue picker) asks what the agent should do
// with it. A Picker without an anchor — a bottom sheet on a phone, a centered
// dialog on desktop — titled "Issue #47": the title, the triage chip with the
// issue's other labels and its age, then the actions (Triage, Implement,
// Discuss, and Land as a fourth row only when the issue has an open PR —
// issue #88) with their one-line descriptions, the one that fits marked
// "Suggested" (Land whenever it shows, else by the triage label). The Land
// row also shows the PR's head branch, and a one-line note when landing by
// hand could collide with autoland (its switch is on for the repo and the PR
// is not escalated; a lander/fix/escalate run is live on the PR). The notes
// never disable the row: the operator decides. Choosing an action hands it
// to the host, which attaches it to the composer; the action rides as the
// run's first_message (lib/newRun.ts composeFirstMessage, landNotes).

import { For, Show, type JSX } from 'solid-js';
import type { Instance, IssueSummary } from '../../api';
import {
  issueActions,
  issueAge,
  landNotes,
  suggestedAction,
  type IssueAction,
} from '../../lib/newRun';
import Icon, { type IconName } from '../Icon';
import Picker from '../Picker';
import { TriageChip } from './IssuePicker';

const ACTION_ICONS: Record<IssueAction, IconName> = {
  triage: 'ticket',
  implement: 'zap',
  discuss: 'pencil',
  land: 'git-merge',
};

/** The labels TriageChip already shows; every other label is a plain chip. */
const TRIAGE_LABELS = new Set(['ready-for-agent', 'needs-triage', 'needs-info']);

export default function IssueActionSheet(props: {
  issue: IssueSummary | null;
  /** The repo the issue belongs to: a live run elsewhere never notes Land. */
  repoID: string;
  /** The repo's autoland switch (Repo.autoland_enabled), for Land's note. */
  autoland: boolean;
  /** The loaded instance list, for Land's live-lander note. */
  runs: readonly Pick<Instance, 'repo_id' | 'kind' | 'live' | 'pull_number'>[];
  onClose: () => void;
  onChoose: (action: IssueAction) => void;
  now: () => number;
}): JSX.Element {
  return (
    <Picker
      open={props.issue !== null}
      onClose={() => props.onClose()}
      title={props.issue === null ? '' : `Issue #${props.issue.number}`}
      size="regular"
      class="issue-action-sheet"
    >
      <Show when={props.issue}>
        {(issue) => {
          const suggested = () => suggestedAction(issue().labels, issue().pull);
          const notes = () => {
            const pull = issue().pull;
            return pull === null
              ? []
              : landNotes({
                  repoID: props.repoID,
                  pull,
                  autoland: props.autoland,
                  runs: props.runs,
                });
          };
          const age = () => issueAge(issue().created_at, props.now());
          return (
            <>
              <div class="issue-action-head">
                <b class="issue-action-title">{issue().title}</b>
                <div class="issue-action-labels">
                  <TriageChip labels={issue().labels} />
                  <For each={issue().labels.filter((label) => !TRIAGE_LABELS.has(label))}>
                    {(label) => <span class="chip">{label}</span>}
                  </For>
                  <Show when={age() !== ''}>
                    <small class="muted">
                      opened {age() === 'now' ? 'just now' : `${age()} ago`}
                    </small>
                  </Show>
                </div>
              </div>
              <p class="muted issue-action-ask">What should the agent do with it?</p>
              <div class="issue-actions">
                <For each={issueActions(issue())}>
                  {(action) => (
                    <button
                      type="button"
                      classList={{ 'issue-action': true, suggested: suggested() === action.id }}
                      onClick={() => props.onChoose(action.id)}
                    >
                      <Icon name={ACTION_ICONS[action.id]} class="issue-action-icon" />
                      <span class="issue-action-text">
                        <b class="issue-action-name">
                          {action.label}
                          <Show when={suggested() === action.id}>
                            <span class="chip issue-action-suggested">Suggested</span>
                          </Show>
                        </b>
                        <small class="issue-action-desc">
                          {action.describe(issue().number, issue().pull)}
                        </small>
                        <Show when={action.id === 'land' && issue().pull}>
                          {(pull) => (
                            <>
                              <small class="issue-action-branch">
                                <code>{pull().head_branch}</code>
                              </small>
                              <For each={notes()}>
                                {(note) => <small class="issue-action-note">{note}</small>}
                              </For>
                            </>
                          )}
                        </Show>
                      </span>
                      <Icon name="chevron-right" size={16} class="issue-action-chevron" />
                    </button>
                  )}
                </For>
              </div>
            </>
          );
        }}
      </Show>
    </Picker>
  );
}
