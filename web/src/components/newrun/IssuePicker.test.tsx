// IssuePicker contract (issue #66): a wide Picker titled "Open issues ·
// <repo>" listing every open issue newest first, in the card's row look; the
// pinned search narrows by number (with or without `#`, as a prefix) or by
// words from the title, and Enter picks the first match; the state chips
// (All, needs-triage, ready-for-agent, needs-info, unlabeled) carry counts,
// hide at zero (All never does) and narrow the rows; "No matching issues."
// when nothing is left; tapping a row picks it.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { IssueSummary } from '../../api';
import IssuePicker from './IssuePicker';

const NOW = Date.parse('2026-10-07T12:00:00Z');
const DAY = 24 * 60 * 60 * 1000;

function issue(number: number, daysAgo: number, labels: string[], title: string): IssueSummary {
  const at = new Date(NOW - daysAgo * DAY).toISOString();
  return {
    number,
    title,
    body: '',
    state: 'open',
    labels,
    comments_count: 0,
    created_at: at,
    updated_at: at,
    pull: null,
  };
}

const ISSUES = [
  issue(5, 20, ['needs-triage'], 'Pin the kernel to the LTS channel'),
  issue(56, 3, ['ready-for-agent', 'enhancement'], 'Record the Runner a run was spawned with'),
  issue(47, 8, ['needs-triage'], 'Warpgate dashboard exposure'),
  issue(51, 4, ['bug'], 'Chat header context meter'),
  issue(12, 40, ['ready-for-agent'], 'Round invoice totals once'),
];

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

function mount(issues = ISSUES) {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onPick = vi.fn<(picked: IssueSummary) => void>();
  const onClose = vi.fn();
  dispose = render(
    () => (
      <IssuePicker
        open
        onClose={onClose}
        repoName="coding-lab"
        issues={issues}
        onPick={onPick}
        now={() => NOW}
      />
    ),
    container,
  );
  return { onPick, onClose };
}

const panel = () => document.querySelector<HTMLElement>('.issue-picker')!;
const numbers = () =>
  Array.from(panel().querySelectorAll('.issue-row-number')).map((el) => el.textContent);
const search = () => panel().querySelector<HTMLInputElement>('input[aria-label="Filter issues"]')!;
const chips = () =>
  Array.from(panel().querySelectorAll<HTMLButtonElement>('.picker-filters button')).map((b) =>
    b.textContent?.trim(),
  );
const chip = (label: string) =>
  Array.from(panel().querySelectorAll<HTMLButtonElement>('.picker-filters button')).find((b) =>
    b.textContent?.startsWith(label),
  )!;

function type(value: string): void {
  const input = search();
  input.value = value;
  input.dispatchEvent(new InputEvent('input', { bubbles: true }));
}

describe('IssuePicker', () => {
  it('is titled after the repo and lists every issue newest first', () => {
    mount();
    expect(panel().querySelector('.picker-title')?.textContent).toBe('Open issues · coding-lab');
    expect(search().placeholder).toBe('Number or words from the title');
    expect(numbers()).toEqual(['#56', '#51', '#47', '#5', '#12']);
  });

  it('filters by number, with or without #', () => {
    mount();
    type('#5');
    expect(numbers()).toEqual(['#56', '#51', '#5']);
    type('5');
    expect(numbers()).toEqual(['#56', '#51', '#5']);
    type('47');
    expect(numbers()).toEqual(['#47']);
  });

  it('filters by words from the title, in any order', () => {
    mount();
    type('channel kernel');
    expect(numbers()).toEqual(['#5']);
    type('nothing like this');
    expect(numbers()).toEqual([]);
    expect(panel().textContent).toContain('No matching issues.');
  });

  it('offers state chips with counts, hides the zero ones, and narrows by state', () => {
    mount(ISSUES.filter((i) => i.number !== 51)); // no unlabeled issue left
    expect(chips()).toEqual(['All 4', 'needs-triage 2', 'ready-for-agent 2']);

    chip('ready-for-agent').click();
    expect(chip('ready-for-agent').getAttribute('aria-pressed')).toBe('true');
    expect(numbers()).toEqual(['#56', '#12']);

    chip('All').click();
    expect(numbers()).toHaveLength(4);
  });

  it('shows the unlabeled chip when an issue has no triage label', () => {
    mount();
    expect(chips()).toEqual(['All 5', 'needs-triage 2', 'ready-for-agent 2', 'unlabeled 1']);
    chip('unlabeled').click();
    expect(numbers()).toEqual(['#51']);
  });

  it('Enter picks the first match; a tap picks its row', () => {
    const { onPick } = mount();
    type('warpgate');
    search().dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(onPick.mock.calls.map(([picked]) => picked.number)).toEqual([47]);

    type('');
    panel().querySelectorAll<HTMLButtonElement>('.issue-row')[1]!.click();
    expect(onPick.mock.calls.map(([picked]) => picked.number)).toEqual([47, 51]);
  });

  it('Enter with no match picks nothing', () => {
    const { onPick } = mount();
    type('zzz');
    search().dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(onPick).not.toHaveBeenCalled();
  });
});
