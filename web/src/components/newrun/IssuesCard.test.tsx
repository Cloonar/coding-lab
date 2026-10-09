// IssuesCard contract (issue #66):
// - it reads the open issues through GET /repos/{id}/issues?state=open (the
//   Issues tab's bounded read) and the instance list, and nothing else — no
//   ready-queue request;
// - the head shows the open-issue count and, unless AFK is paused or the
//   tracker check fails, the Auto switch (PUT afk/auto);
// - one AFK line: Auto off ends with Run one (POST afk/start, the run goes to
//   onStarted), paused reads "AFK paused after 3 failed runs" with Reset
//   (POST afk/reset); live AFK runs of this repo are counted;
// - at most four rows, newest first, with the triage chip in its tint and no
//   other labels; "All N open issues" only past four, opening the picker;
// - a failing tracker check shows its detail instead of rows; no open issues
//   reads "No open issues."; a failed read shows its error inside the card;
// - tapping a row (or picking in the picker) opens the action sheet with
//   "Suggested" on the fitting action; choosing calls onAction;
// - an issue with an open PR (issue #88) shows a "PR #88" chip in the run
//   tint after its triage chip, on the card and in the picker alike, and its
//   sheet offers Land with the repo's autoland switch and the live runs
//   feeding the collision notes.

import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Instance, IssuePull, IssueSummary, Repo, Run } from '../../api';
import { EventsProvider } from '../../events';
import type { IssueAction } from '../../lib/newRun';
import IssuesCard from './IssuesCard';

const REPO_ID = 'repo_1';
const NOW = Date.parse('2026-10-07T12:00:00Z');
const DAY = 24 * 60 * 60 * 1000;

class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

function repoFixture(overrides: Partial<Repo> = {}): Repo {
  return {
    id: REPO_ID,
    name: 'coding-lab',
    remote_url: 'git@h:o/r.git',
    credential_id: null,
    forge_credential_id: null,
    tracker_binding: 'builtin',
    forge_kind: 'none',
    default_branch: 'main',
    provider: 'claude-code',
    incogni: false,
    model_default: null,
    effort_default: null,
    remote_default: null,
    afk_provider_default: null,
    afk_model_default: null,
    afk_effort_default: null,
    afk_remote_default: null,
    afk_options: null,
    afk_prompt: null,
    afk_prompt_effective: 'Resolve issue #<N> on branch <BRANCH>, then open a PR.',
    git_author_name: null,
    git_author_email: null,
    afk_branch_pattern: 'afk/<N>',
    manual_branch_prefix: 'lab/',
    afk_auto_enabled: false,
    consecutive_failures: 0,
    budget_minutes: null,
    max_instances_override: null,
    clone_status: 'ready',
    clone_error: null,
    created_at: '2026-07-06T00:00:00.000Z',
    last_opened_at: null,
    summary: {
      claimable: 2,
      open_issues: 9,
      readiness: { state: 'passing', checks: [] },
    },
    autoland_enabled: false,
    max_fix_attempts: 2,
    auto_merge: true,
    lander_provider: null,
    lander_model: null,
    lander_effort: null,
    runner: 'host',
    container_memory: null,
    container_pids: null,
    container_nofile: null,
    image_ref: null,
    ...overrides,
  };
}

function issue(number: number, daysAgo: number, labels: string[], title = `Issue ${number}`) {
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
  } satisfies IssueSummary;
}

const PULL: IssuePull = {
  number: 88,
  head_branch: 'afk/47',
  url: 'https://github.com/o/r/pull/88',
  escalated: false,
};

/** The fixture issue with an open PR attached. */
function withPull(base: IssueSummary, pull: IssuePull = PULL): IssueSummary {
  return { ...base, pull };
}

function instance(overrides: Partial<Instance>): Partial<Instance> {
  return { id: 'run', repo_id: REPO_ID, kind: 'afk_auto', live: true, state: '', ...overrides };
}

let openIssues: IssueSummary[];
let issuesFail: boolean;
let instances: Partial<Instance>[];
let requests: { method: string; url: string; body?: unknown }[];
let started: Run[];
let actions: { action: IssueAction; issue: number }[];
let errors: string[];
let repoChanged: number;
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function jsonResponse(status: number, body?: unknown) {
  const text = body === undefined ? '' : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text === '' ? 'null' : text) as unknown),
    text: () => Promise.resolve(text),
  };
}

function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      requests.push({
        method,
        url,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      if (url === `/api/v1/repos/${REPO_ID}/issues?state=open` && method === 'GET') {
        if (issuesFail) return Promise.resolve(jsonResponse(502, { error: 'forge unreachable' }));
        return Promise.resolve(jsonResponse(200, { binding: 'builtin', issues: openIssues }));
      }
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/afk/start` && method === 'POST') {
        return Promise.resolve(jsonResponse(202, { run: { id: 'run_1', issue_number: 7 } }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/afk/auto` && method === 'PUT') {
        return Promise.resolve(jsonResponse(200, { id: REPO_ID, afk_auto_enabled: true }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/afk/reset` && method === 'POST') {
        return Promise.resolve(jsonResponse(200, { id: REPO_ID, consecutive_failures: 0 }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mount(repo: Repo): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(
    () => (
      <EventsProvider>
        <IssuesCard
          repo={repo}
          onAction={(action, picked) => actions.push({ action, issue: picked.number })}
          onRepoChanged={() => {
            repoChanged += 1;
          }}
          onStarted={(run) => started.push(run)}
          onError={(message) => errors.push(message)}
          now={() => NOW}
        />
      </EventsProvider>
    ),
    container,
  );
  await settle();
}

const card = () => container.querySelector<HTMLElement>('.issues-card')!;
const rowNumbers = (root: ParentNode = card()) =>
  Array.from(root.querySelectorAll('.issue-row-number')).map((el) => el.textContent);
const afkText = () => card().querySelector('.issues-card-afk-text')?.textContent ?? null;
const autoSwitch = () => card().querySelector<HTMLButtonElement>('button[role="switch"]');
const sheet = () => document.querySelector<HTMLElement>('.issue-action-sheet');
const issuePicker = () => document.querySelector<HTMLElement>('.issue-picker');

function button(text: string, root: ParentNode = card()): HTMLButtonElement {
  const el = Array.from(root.querySelectorAll('button')).find((b) => b.textContent?.includes(text));
  if (!el) throw new Error(`missing button containing ${JSON.stringify(text)}`);
  return el;
}

function requestsTo(url: string, method = 'GET') {
  return requests.filter((r) => r.url === url && r.method === method);
}

const SIX = () => [
  issue(40, 30, ['needs-info']),
  issue(56, 3, ['ready-for-agent', 'enhancement']),
  issue(47, 8, ['needs-triage', 'enhancement']),
  issue(45, 9, ['bug']),
  issue(52, 5, ['needs-triage']),
  issue(30, 60, []),
];

beforeEach(() => {
  openIssues = SIX();
  issuesFail = false;
  instances = [];
  requests = [];
  started = [];
  actions = [];
  errors = [];
  repoChanged = 0;
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('IssuesCard rows', () => {
  it('reads the open issues and the instances, and nothing else', async () => {
    await mount(repoFixture());
    expect(requestsTo(`/api/v1/repos/${REPO_ID}/issues?state=open`)).toHaveLength(1);
    expect(requests.every((r) => !r.url.includes('/ready'))).toBe(true);
    expect(requests.map((r) => r.url).sort()).toEqual(
      ['/api/v1/instances', `/api/v1/repos/${REPO_ID}/issues?state=open`].sort(),
    );
  });

  it('lists at most four, newest first, with the count, the triage chip by tint and no other labels', async () => {
    await mount(repoFixture());
    expect(card().querySelector('h2')?.textContent).toBe('Issues');
    expect(card().querySelector('.issues-card-count')?.textContent).toBe('6');
    expect(rowNumbers()).toEqual(['#56', '#52', '#47', '#45']);

    const rows = card().querySelectorAll('.issue-row');
    const chipOf = (i: number) => rows[i]!.querySelector('.triage-chip');
    expect(chipOf(0)?.textContent).toBe('ready-for-agent');
    expect(chipOf(0)?.classList.contains('in-use')).toBe(true);
    expect(chipOf(1)?.textContent).toBe('needs-triage');
    expect(chipOf(1)?.classList.contains('status-warn')).toBe(true);
    expect(chipOf(3)).toBeNull(); // only "bug": unlabeled for triage, no chip
    expect(card().textContent).not.toContain('enhancement');
    expect(card().textContent).not.toContain('bug');
    expect(rows[0]!.querySelector('.issue-row-age')?.textContent).toBe('3 d');
  });

  it('shows a "PR #88" chip in the run tint, after the triage chip, only for an issue with a PR', async () => {
    openIssues = [withPull(issue(47, 8, ['needs-triage'])), issue(52, 5, ['needs-triage'])];
    await mount(repoFixture());
    const rows = card().querySelectorAll('.issue-row');
    const chips = (i: number) =>
      Array.from(rows[i]!.querySelectorAll('.chip')).map((c) => c.textContent);
    expect(rowNumbers()).toEqual(['#52', '#47']);
    expect(chips(1)).toEqual(['needs-triage', 'PR #88']);
    expect(rows[1]!.querySelector('.pr-chip')?.classList.contains('in-use')).toBe(true);
    expect(chips(0)).toEqual(['needs-triage']);
    expect(card().querySelectorAll('.pr-chip')).toHaveLength(1);
  });

  it('shows the PR chip in the picker rows too', async () => {
    openIssues = [
      ...SIX().slice(1),
      withPull(issue(40, 30, ['needs-info']), { ...PULL, number: 91 }),
    ];
    await mount(repoFixture());
    button('All 6 open issues').click();
    await settle();
    const pickerChips = Array.from(issuePicker()!.querySelectorAll('.pr-chip')).map(
      (c) => c.textContent,
    );
    expect(pickerChips).toEqual(['PR #91']);
  });

  it('tints needs-info in the idle tint', async () => {
    openIssues = [issue(40, 1, ['needs-info'])];
    await mount(repoFixture());
    const chip = card().querySelector('.triage-chip');
    expect(chip?.textContent).toBe('needs-info');
    expect(chip?.classList.contains('idle')).toBe(true);
  });

  it('shows the summary count before the list loads', async () => {
    issuesFail = true;
    await mount(repoFixture());
    expect(card().querySelector('.issues-card-count')?.textContent).toBe('9');
  });

  it('offers "All N open issues" only past four, and it opens the picker', async () => {
    await mount(repoFixture());
    button('All 6 open issues').click();
    await settle();
    expect(issuePicker()).not.toBeNull();
    expect(issuePicker()?.textContent).toContain('Open issues · coding-lab');
    expect(rowNumbers(issuePicker()!)).toEqual(['#56', '#52', '#47', '#45', '#40', '#30']);
  });

  it('has no "All" button with four or fewer issues', async () => {
    openIssues = SIX().slice(0, 4);
    await mount(repoFixture());
    expect(rowNumbers()).toHaveLength(4);
    expect(card().textContent).not.toContain('open issues');
  });

  it('reads "No open issues." when there are none', async () => {
    openIssues = [];
    await mount(repoFixture());
    expect(card().textContent).toContain('No open issues.');
    expect(card().querySelector('.issues-card-count')?.textContent).toBe('0');
  });

  it('shows a failed read inside the card', async () => {
    issuesFail = true;
    await mount(repoFixture());
    expect(card().querySelector('.issues-card-note')?.textContent).toBe('forge unreachable');
    expect(document.querySelector('.banner')).toBeNull();
  });

  it('shows the failing tracker check instead of rows, without the Auto switch or the AFK line', async () => {
    await mount(
      repoFixture({
        summary: {
          claimable: 2,
          open_issues: 9,
          readiness: {
            state: 'failing',
            checks: [
              {
                id: 'tracker',
                state: 'failing',
                detail: 'The forge token was rejected, so issues cannot be read.',
              },
            ],
          },
        },
      }),
    );
    expect(card().textContent).toContain('The forge token was rejected, so issues cannot be read.');
    expect(rowNumbers()).toEqual([]);
    expect(autoSwitch()).toBeNull();
    expect(afkText()).toBeNull();
    expect(requestsTo(`/api/v1/repos/${REPO_ID}/issues?state=open`)).toHaveLength(0);
  });
});

describe('IssuesCard AFK line', () => {
  it('a repo that cannot start a run (cloning, clone failed) shows no AFK line and no Auto switch', async () => {
    await mount(repoFixture({ clone_status: 'cloning', afk_auto_enabled: false }));
    expect(afkText()).toBeNull();
    expect(autoSwitch()).toBeNull();
    expect(() => button('Run one')).toThrow();
  });

  it('Auto off: the line ends with Run one, which starts an AFK run and reports it', async () => {
    await mount(repoFixture({ afk_auto_enabled: false }));
    expect(afkText()).toBe('Auto off · 2 ready');
    button('Run one').click();
    await settle();
    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/start`, 'POST')).toHaveLength(1);
    expect(started.map((run) => run.issue_number)).toEqual([7]);
    expect(repoChanged).toBe(1);
    expect(errors).toEqual([]);
  });

  it('Auto on: counts this repo’s live AFK runs and offers no Run one', async () => {
    instances = [
      instance({ id: 'a', kind: 'afk_auto' }),
      instance({ id: 'b', kind: 'afk_manual' }),
      instance({ id: 'c', kind: 'manual' }),
      instance({ id: 'd', kind: 'afk_auto', live: false }),
      instance({ id: 'e', kind: 'afk_auto', repo_id: 'other' }),
    ];
    await mount(repoFixture({ afk_auto_enabled: true }));
    expect(afkText()).toBe('Auto on · 2 ready · 2 AFK runs live · next claim when a slot frees');
    expect(card().querySelector('.issues-card-afk-btn')).toBeNull();
  });

  it('the Auto switch applies at once', async () => {
    await mount(repoFixture({ afk_auto_enabled: false }));
    expect(autoSwitch()?.getAttribute('aria-checked')).toBe('false');
    autoSwitch()!.click();
    await settle();
    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/auto`, 'PUT')[0]?.body).toEqual({
      enabled: true,
    });
    expect(repoChanged).toBe(1);
  });

  it('paused: the line says so, Reset un-pauses, and the Auto switch is hidden', async () => {
    await mount(repoFixture({ consecutive_failures: 3 }));
    expect(afkText()).toBe('AFK paused after 3 failed runs · 2 ready');
    expect(autoSwitch()).toBeNull();
    button('Reset').click();
    await settle();
    expect(requestsTo(`/api/v1/repos/${REPO_ID}/afk/reset`, 'POST')).toHaveLength(1);
    expect(repoChanged).toBe(1);
  });
});

describe('IssuesCard issue action', () => {
  it('tapping a row opens the action sheet with Suggested on the fitting action; choosing reports it', async () => {
    await mount(repoFixture());
    button('Issue 47').click();
    await settle();
    expect(sheet()?.querySelector('.picker-title')?.textContent).toBe('Issue #47');
    const suggested = sheet()!.querySelector('.issue-action.suggested');
    expect(suggested?.textContent).toContain('Triage');
    expect(suggested?.textContent).toContain('Suggested');

    button('Implement', sheet()!).click();
    await settle();
    expect(actions).toEqual([{ action: 'implement', issue: 47 }]);
    expect(sheet()).toBeNull();
  });

  it('picking in the picker closes it and opens the action sheet for that issue', async () => {
    await mount(repoFixture());
    button('All 6 open issues').click();
    await settle();
    button('Issue 30', issuePicker()!).click();
    await settle();
    expect(issuePicker()).toBeNull();
    expect(sheet()?.querySelector('.picker-title')?.textContent).toBe('Issue #30');
    expect(sheet()!.querySelector('.issue-action.suggested')?.textContent).toContain('Discuss');

    button('Discuss', sheet()!).click();
    await settle();
    expect(actions).toEqual([{ action: 'discuss', issue: 30 }]);
  });

  it('an issue with a PR offers Land, noted from the autoland switch and the live runs', async () => {
    openIssues = [withPull(issue(47, 8, ['needs-triage']))];
    instances = [
      instance({ id: 'run_l', kind: 'lander', pull_number: 88 }),
      // Another repo's lander on a PR with the same number does not count.
      instance({ id: 'run_x', repo_id: 'repo_2', kind: 'lander', pull_number: 88 }),
    ];
    await mount(repoFixture({ autoland_enabled: true }));
    button('Issue 47').click();
    await settle();
    const suggested = sheet()!.querySelector('.issue-action.suggested');
    expect(suggested?.textContent).toContain('Land');
    expect(
      Array.from(sheet()!.querySelectorAll('.issue-action-note')).map((n) => n.textContent),
    ).toEqual(['Autoland is on for this repo', 'A lander run is live on this PR']);

    button('Land', sheet()!).click();
    await settle();
    expect(actions).toEqual([{ action: 'land', issue: 47 }]);
  });
});
