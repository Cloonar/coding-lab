// IssueActionSheet contract (issue #66): open exactly while an issue is
// given; titled "Issue #47"; shows the title, the triage chip, the other
// labels as plain chips and "opened 8 d ago"; asks "What should the agent do
// with it?" and offers Triage, Implement and Discuss with their one-line
// descriptions, "Suggested" on the one the triage label calls for (triage for
// needs-triage, implement for ready-for-agent, discuss otherwise); choosing
// reports the action. An issue with an open PR (issue #88) adds Land as a
// fourth row, Suggested over every label, with the PR's head branch on a
// second line and the collision notes (Autoland on and the PR not escalated;
// a live lander/fix/escalate run on the PR) — which never disable it.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Instance, IssuePull, IssueSummary } from '../../api';
import type { IssueAction } from '../../lib/newRun';
import IssueActionSheet from './IssueActionSheet';

const NOW = Date.parse('2026-10-07T12:00:00Z');
const DAY = 24 * 60 * 60 * 1000;

const PULL: IssuePull = {
  number: 88,
  head_branch: 'afk/47',
  url: 'https://github.com/o/r/pull/88',
  escalated: false,
};

type SheetRun = Pick<Instance, 'repo_id' | 'kind' | 'live' | 'pull_number'>;

function issue(labels: string[], pull: IssuePull | null = null): IssueSummary {
  const at = new Date(NOW - 8 * DAY).toISOString();
  return {
    number: 47,
    title: 'Warpgate: dashboard exposure and per-run recordings deep link',
    body: '',
    state: 'open',
    labels,
    comments_count: 0,
    created_at: at,
    updated_at: at,
    pull,
  };
}

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

function mount(initial: IssueSummary | null, opts: { autoland?: boolean; runs?: SheetRun[] } = {}) {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onChoose = vi.fn<(action: IssueAction) => void>();
  const onClose = vi.fn();
  const [current, setCurrent] = createSignal<IssueSummary | null>(initial);
  dispose = render(
    () => (
      <IssueActionSheet
        issue={current()}
        repoID="repo_1"
        autoland={opts.autoland ?? false}
        runs={opts.runs ?? []}
        onClose={onClose}
        onChoose={onChoose}
        now={() => NOW}
      />
    ),
    container,
  );
  return { onChoose, onClose, setCurrent };
}

const sheet = () => document.querySelector<HTMLElement>('.issue-action-sheet');
const actionButtons = () =>
  Array.from(sheet()!.querySelectorAll<HTMLButtonElement>('.issue-action'));
const suggested = () => sheet()!.querySelector('.issue-action.suggested')?.textContent ?? '';

describe('IssueActionSheet', () => {
  it('is closed without an issue and opens with one', () => {
    const { setCurrent } = mount(null);
    expect(sheet()).toBeNull();
    setCurrent(issue([]));
    expect(sheet()).not.toBeNull();
  });

  it('shows the issue, its labels and age, and the three actions', () => {
    mount(issue(['needs-triage', 'enhancement']));
    const el = sheet()!;
    expect(el.querySelector('.picker-title')?.textContent).toBe('Issue #47');
    expect(el.querySelector('.issue-action-title')?.textContent).toBe(
      'Warpgate: dashboard exposure and per-run recordings deep link',
    );
    const labels = el.querySelector('.issue-action-labels')!;
    expect(labels.querySelector('.triage-chip')?.textContent).toBe('needs-triage');
    expect(Array.from(labels.querySelectorAll('.chip')).map((c) => c.textContent)).toEqual([
      'needs-triage',
      'enhancement',
    ]);
    expect(labels.textContent).toContain('opened 8 d ago');
    expect(el.textContent).toContain('What should the agent do with it?');

    const buttons = actionButtons();
    expect(buttons.map((b) => b.querySelector('.issue-action-name')?.textContent)).toEqual([
      'TriageSuggested',
      'Implement',
      'Discuss',
    ]);
    expect(buttons[0]!.querySelector('.issue-action-desc')?.textContent).toContain(
      'Runs /triage #47',
    );
  });

  it('suggests Triage for needs-triage, Implement for ready-for-agent, Discuss otherwise', () => {
    const { setCurrent } = mount(issue(['needs-triage']));
    expect(suggested()).toContain('Triage');
    setCurrent(issue(['ready-for-agent']));
    expect(suggested()).toContain('Implement');
    setCurrent(issue(['needs-info']));
    expect(suggested()).toContain('Discuss');
    setCurrent(issue(['bug']));
    expect(suggested()).toContain('Discuss');
    expect(sheet()!.querySelectorAll('.issue-action-suggested')).toHaveLength(1);
  });

  it('choosing an action reports it', () => {
    const { onChoose } = mount(issue(['ready-for-agent']));
    actionButtons()[2]!.click();
    expect(onChoose).toHaveBeenCalledWith('discuss');
  });

  it('shows three rows without an open PR, no branch line and no notes', () => {
    mount(issue(['needs-triage']), { autoland: true });
    expect(actionButtons()).toHaveLength(3);
    expect(sheet()!.textContent).not.toContain('Land');
    expect(sheet()!.querySelector('.issue-action-branch')).toBeNull();
    expect(sheet()!.querySelector('.issue-action-note')).toBeNull();
  });

  it('adds Land as a fourth, Suggested row with the head branch for an issue with a PR', () => {
    mount(issue(['needs-triage'], PULL));
    const buttons = actionButtons();
    expect(buttons.map((b) => b.querySelector('.issue-action-name')?.textContent)).toEqual([
      'Triage',
      'Implement',
      'Discuss',
      'LandSuggested',
    ]);
    const land = buttons[3]!;
    expect(land.querySelector('.issue-action-desc')?.textContent).toMatch(
      /validate and merge PR #88/i,
    );
    expect(land.querySelector('.issue-action-branch code')?.textContent).toBe('afk/47');
    expect(land.querySelector('.issue-action-note')).toBeNull();
    // The branch line is Land's alone.
    expect(sheet()!.querySelectorAll('.issue-action-branch')).toHaveLength(1);
  });

  it('notes Autoland and a live lander on the PR, and Land stays choosable', () => {
    const { onChoose } = mount(issue(['ready-for-agent'], PULL), {
      autoland: true,
      runs: [{ repo_id: 'repo_1', kind: 'fix', live: true, pull_number: 88 }],
    });
    const land = actionButtons()[3]!;
    expect(
      Array.from(land.querySelectorAll('.issue-action-note')).map((n) => n.textContent),
    ).toEqual(['Autoland is on for this repo', 'A lander run is live on this PR']);
    expect(land.disabled).toBe(false);
    land.click();
    expect(onChoose).toHaveBeenCalledWith('land');
  });

  it('leaves the Autoland note out for an escalated PR, and ignores runs on other PRs', () => {
    mount(issue([], { ...PULL, escalated: true }), {
      autoland: true,
      runs: [
        { repo_id: 'repo_1', kind: 'lander', live: true, pull_number: 89 },
        { repo_id: 'repo_1', kind: 'lander', live: false, pull_number: 88 },
      ],
    });
    expect(sheet()!.querySelector('.issue-action-note')).toBeNull();
  });
});
