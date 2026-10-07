// RepoPills contract (issue #66): the pills render in the order given, the
// selected one aria-pressed, each with its readiness dot; a pill pick reports
// the repo. "All N" shows the total and opens the repository picker (a dialog,
// not a navigation), toggling on a second click. The picker filters by name
// and by host as you type; Enter picks the first ENABLED match (a cloning row
// is skipped); cloning (with the live percent) and clone-failed rows are
// disabled and never pick; a ready repo whose tracker fails shows "tracker
// failing" and IS pickable; the Recent group precedes All repositories.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Repo } from '../../api';
import type { CloneProgress } from '../../stores/cloneProgress';
import RepoPills from './RepoPills';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  document.body.style.overflow = '';
});

// A minimal repo; RepoPills reads only id, name, remote_url, clone_status and
// summary.readiness, so the rest of the Repo shape is cast in.
function repoFixture(overrides: Partial<Repo> = {}): Repo {
  return {
    id: 'repo_1',
    name: 'coding-lab',
    remote_url: 'git@github.com:Cloonar/coding-lab.git',
    clone_status: 'ready',
    clone_error: null,
    summary: { claimable: null, open_issues: null, readiness: { state: 'passing', checks: [] } },
    ...overrides,
  } as unknown as Repo;
}

const failingTracker = {
  state: 'failing',
  checks: [{ id: 'tracker', state: 'failing', label: 'Tracker' }],
};

const lab = repoFixture({ id: 'r_lab', name: 'coding-lab' });
const pipeline = repoFixture({
  id: 'r_pipe',
  name: 'data-pipeline',
  remote_url: 'git@gitlab.example.org:data/pipeline.git',
  summary: { claimable: null, open_issues: null, readiness: { state: 'failing', checks: [] } },
});
const nixos = repoFixture({
  id: 'r_nix',
  name: 'cloonar-nixos',
  remote_url: 'git@github.com:Cloonar/nixos.git',
  clone_status: 'cloning',
});
const billing = repoFixture({
  id: 'r_bill',
  name: 'billing-api',
  remote_url: 'git@github.com:Cloonar/billing.git',
  clone_status: 'error',
});
const tracker = repoFixture({
  id: 'r_trk',
  name: 'tracker-less',
  remote_url: 'git@github.com:Cloonar/tracker.git',
  summary: {
    claimable: null,
    open_issues: null,
    readiness: failingTracker,
  } as unknown as Repo['summary'],
});
const docs = repoFixture({
  id: 'r_docs',
  name: 'docs-site',
  remote_url: 'git@git.example.org:o/docs.git',
});

const ALL = [lab, pipeline, nixos, billing, tracker, docs];

function mount(
  overrides: {
    repos?: Repo[];
    pills?: Repo[];
    recentIds?: string[];
    selectedId?: string | null;
    progress?: (id: string) => CloneProgress | null;
  } = {},
) {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onPick = vi.fn();
  dispose = render(
    () => (
      <RepoPills
        repos={overrides.repos ?? ALL}
        pills={overrides.pills ?? [lab, pipeline]}
        recentIds={overrides.recentIds ?? ['r_pipe', 'r_lab']}
        selectedId={'selectedId' in overrides ? (overrides.selectedId ?? null) : 'r_lab'}
        progress={overrides.progress ?? (() => null)}
        onPick={onPick}
      />
    ),
    container,
  );
  return { onPick };
}

const pills = () => [...container.querySelectorAll<HTMLButtonElement>('.repo-pill')];
const allPill = () => container.querySelector<HTMLButtonElement>('.repo-pill-all')!;
const panel = () => document.querySelector<HTMLElement>('.picker');
const rows = () => [...document.querySelectorAll<HTMLButtonElement>('.picker [role="option"]')];
const rowNames = () => rows().map((row) => row.querySelector('.picker-option-title')!.textContent);
const search = () => document.querySelector<HTMLInputElement>('.picker input.select-search')!;
const type = (value: string) => {
  const input = search();
  input.value = value;
  input.dispatchEvent(new InputEvent('input', { bubbles: true }));
};
const enter = () =>
  search().dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }),
  );

describe('RepoPills pills', () => {
  it('renders one pill per entry in order, then "All N"', () => {
    mount();
    expect(pills().map((p) => p.textContent?.trim())).toEqual([
      'coding-lab',
      'data-pipeline',
      `All ${ALL.length}`,
    ]);
    expect(allPill().textContent).toContain(`All ${ALL.length}`);
  });

  it('marks the selected pill aria-pressed', () => {
    mount();
    const [first, second] = pills();
    expect(first!.getAttribute('aria-pressed')).toBe('true');
    expect(second!.getAttribute('aria-pressed')).toBe('false');
  });

  it('presses no pill when the selected repo is not among them', () => {
    mount({ selectedId: null });
    expect(
      pills()
        .slice(0, 2)
        .map((p) => p.getAttribute('aria-pressed')),
    ).toEqual(['false', 'false']);
  });

  it('draws the readiness dot: ok, err and pending', () => {
    mount({ pills: [lab, pipeline, nixos] });
    const dots = pills()
      .slice(0, 3)
      .map((p) => p.querySelector('.repo-dot')!.className);
    expect(dots[0]).toContain('ok');
    expect(dots[1]).toContain('err');
    expect(dots[2]).toContain('pending');
  });

  it('reports a pill pick', () => {
    const { onPick } = mount();
    pills()[1]!.click();
    expect(onPick).toHaveBeenCalledTimes(1);
    expect(onPick).toHaveBeenCalledWith(pipeline);
    expect(panel()).toBeNull();
  });

  it('has no link to the repositories page', () => {
    mount();
    expect(container.querySelector('a')).toBeNull();
  });
});

describe('RepoPills picker', () => {
  it('opens the repository picker from the All pill and toggles it shut', () => {
    mount();
    expect(panel()).toBeNull();
    allPill().click();
    const p = panel()!;
    expect(p.getAttribute('role')).toBe('dialog');
    expect(document.getElementById(p.getAttribute('aria-labelledby')!)!.textContent).toBe(
      'Repository',
    );
    expect(allPill().getAttribute('aria-expanded')).toBe('true');
    allPill().click();
    expect(panel()).toBeNull();
  });

  it('puts a filter first, focused, with the mockup placeholder', () => {
    mount();
    allPill().click();
    expect(search().placeholder).toBe('Type a repository name');
    expect(search().getAttribute('aria-label')).toBe('Filter repositories');
    expect(document.activeElement).toBe(search());
    // pinned in the header, not scrolling with the rows
    expect(document.querySelector('.picker-header')!.contains(search())).toBe(true);
  });

  it('lists Recent (stored order) before All repositories · N (the rest)', () => {
    mount();
    allPill().click();
    const groups = [...document.querySelectorAll('.picker-group')].map((g) => g.textContent);
    expect(groups).toEqual(['Recent', `All repositories · ${ALL.length}`]);
    expect(rowNames()).toEqual([
      'data-pipeline',
      'coding-lab',
      'cloonar-nixos',
      'billing-api',
      'tracker-less',
      'docs-site',
    ]);
  });

  it('skips stored recent ids that no longer exist and omits an empty Recent group', () => {
    mount({ recentIds: ['gone'] });
    allPill().click();
    expect([...document.querySelectorAll('.picker-group')].map((g) => g.textContent)).toEqual([
      `All repositories · ${ALL.length}`,
    ]);
    expect(rows()).toHaveLength(ALL.length);
  });

  it('shows the host as description, a leading dot and the selected check', () => {
    mount();
    allPill().click();
    const labRow = rows().find((r) => r.textContent?.includes('coding-lab'))!;
    expect(labRow.querySelector('.picker-option-desc')!.textContent).toContain(
      'github.com/Cloonar/coding-lab',
    );
    expect(labRow.querySelector('.repo-dot.ok')).not.toBeNull();
    expect(labRow.getAttribute('aria-selected')).toBe('true');
    expect(labRow.querySelector('.picker-option-check')).not.toBeNull();
    const other = rows().find((r) => r.textContent?.includes('data-pipeline'))!;
    expect(other.getAttribute('aria-selected')).toBe('false');
  });

  it('filters by name as you type, as one flat list', () => {
    mount();
    allPill().click();
    type('pipe');
    expect(rowNames()).toEqual(['data-pipeline']);
    expect(document.querySelector('.picker-group')).toBeNull();
    type('');
    expect(rows()).toHaveLength(ALL.length);
    expect(document.querySelector('.picker-group')).not.toBeNull();
  });

  it('filters by host', () => {
    mount();
    allPill().click();
    type('gitlab.example');
    expect(rowNames()).toEqual(['data-pipeline']);
    type('GIT.EXAMPLE.ORG');
    expect(rowNames()).toEqual(['docs-site']);
  });

  it('says so when nothing matches, and Enter picks nothing', () => {
    const { onPick } = mount();
    allPill().click();
    type('zzz');
    expect(rows()).toHaveLength(0);
    expect(document.querySelector('.repo-picker-empty')!.textContent).toBe(
      'No repository matches.',
    );
    enter();
    expect(onPick).not.toHaveBeenCalled();
    expect(panel()).not.toBeNull();
  });

  it('Enter picks the first enabled match and closes', () => {
    const { onPick } = mount();
    allPill().click();
    type('cloonar');
    // Matches by host too (all Cloonar-hosted); the list keeps repo order.
    expect(rowNames()[0]).toBe('coding-lab');
    expect(rowNames()).toContain('cloonar-nixos');
    enter();
    expect(onPick).toHaveBeenCalledTimes(1);
    expect(onPick.mock.calls[0]![0]).toBe(lab);
    expect(panel()).toBeNull();
  });

  it('Enter skips a leading cloning row', () => {
    const { onPick } = mount({ repos: [nixos, lab], recentIds: [] });
    allPill().click();
    type('o');
    expect(rowNames()).toEqual(['cloonar-nixos', 'coding-lab']);
    enter();
    expect(onPick).toHaveBeenCalledWith(lab);
  });

  it('Enter with an empty query picks the first enabled row as displayed', () => {
    const { onPick } = mount({ recentIds: ['r_nix', 'r_pipe'] });
    allPill().click();
    enter();
    expect(onPick).toHaveBeenCalledWith(pipeline);
  });

  it('shows cloning rows disabled with the live percent, and does not pick them', () => {
    const { onPick } = mount({
      progress: (id) =>
        id === 'r_nix' ? { phase: 'Receiving objects', percent: 62, line: '' } : null,
    });
    allPill().click();
    const row = rows().find((r) => r.textContent?.includes('cloonar-nixos'))!;
    expect(row.disabled).toBe(true);
    expect(row.querySelector('.picker-option-status')!.textContent).toBe('cloning 62%');
    row.click();
    expect(onPick).not.toHaveBeenCalled();
    expect(panel()).not.toBeNull();
  });

  it('updates the cloning percent as progress changes', () => {
    container = document.createElement('div');
    document.body.appendChild(container);
    const [percent, setPercent] = createSignal<number | null>(null);
    dispose = render(
      () => (
        <RepoPills
          repos={ALL}
          pills={[lab]}
          recentIds={[]}
          selectedId="r_lab"
          progress={(id) => (id === 'r_nix' ? { phase: 'x', percent: percent(), line: '' } : null)}
          onPick={() => {}}
        />
      ),
      container,
    );
    allPill().click();
    const status = () =>
      rows()
        .find((r) => r.textContent?.includes('cloonar-nixos'))!
        .querySelector('.picker-option-status')!.textContent;
    expect(status()).toBe('cloning…');
    setPercent(40);
    expect(status()).toBe('cloning 40%');
  });

  it('shows a failed clone disabled with "clone failed"', () => {
    const { onPick } = mount();
    allPill().click();
    const row = rows().find((r) => r.textContent?.includes('billing-api'))!;
    expect(row.disabled).toBe(true);
    expect(row.querySelector('.picker-option-status')!.textContent).toBe('clone failed');
    row.click();
    expect(onPick).not.toHaveBeenCalled();
  });

  it('shows "tracker failing" on a ready repo and still picks it', () => {
    const { onPick } = mount();
    allPill().click();
    const row = rows().find((r) => r.textContent?.includes('tracker-less'))!;
    expect(row.disabled).toBe(false);
    expect(row.querySelector('.picker-option-status')!.textContent).toBe('tracker failing');
    row.click();
    expect(onPick).toHaveBeenCalledWith(tracker);
    expect(panel()).toBeNull();
  });

  it('picking a row reports the repo and closes the picker', () => {
    const { onPick } = mount();
    allPill().click();
    rows()
      .find((r) => r.textContent?.includes('docs-site'))!
      .click();
    expect(onPick).toHaveBeenCalledTimes(1);
    expect(onPick).toHaveBeenCalledWith(docs);
    expect(panel()).toBeNull();
  });

  it('starts from an empty filter when reopened', () => {
    mount();
    allPill().click();
    type('pipe');
    allPill().click();
    expect(panel()).toBeNull();
    allPill().click();
    expect(search().value).toBe('');
    expect(rows()).toHaveLength(ALL.length);
  });

  it('closes on Escape without picking', () => {
    const { onPick } = mount();
    allPill().click();
    document.activeElement!.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    );
    expect(panel()).toBeNull();
    expect(onPick).not.toHaveBeenCalled();
  });
});
