// Repo home frame contract (issue #61):
// - /repos/:id renders the frame: back link to the list, the repo name as the
//   page heading, the tracker binding chip, incogni, the clone state while not
//   ready (in words), the remote as host plus path;
// - tabs are links with their own URLs; Issues carries the open count only
//   when the summary knows it; CRs only for builtin-bound repos;
//   aria-current follows the URL (Issues also on labels);
// - every existing deep link renders its page inside the frame, /repos/new
//   still resolves to Add repository, a failed repo load shows the banner;
// - the frame's one live resource refetches on repo.changed for this repo;
// - a route notice arriving with the navigation shows once in a toast.

import { Route, useNavigate } from '@solidjs/router';
import { describe, expect, it } from 'vitest';
import { noticeState } from '../../lib/routeNotice';
import {
  REPO_ID,
  baseRepo,
  container,
  emit,
  h,
  installRepoHomeHooks,
  jsonResponse,
  mountRepoHome,
  routerHistory,
  settle,
  waitFor,
} from './harness';
import { SUMMARY_REFRESH_MS } from '../../lib/repoList';
import { activeRepoTab } from './index';

installRepoHomeHooks();

const BASE = `/repos/${REPO_ID}`;

const tabs = (): HTMLAnchorElement[] =>
  Array.from(container.querySelectorAll<HTMLAnchorElement>('nav.repo-tabs a'));
const tabLabels = (): string[] =>
  tabs().map((a) => a.firstChild?.textContent?.trim() ?? a.textContent?.trim() ?? '');
const currentTab = (): string[] =>
  tabs()
    .filter((a) => a.getAttribute('aria-current') === 'page')
    .map((a) => a.getAttribute('href') ?? '');
const repoGets = () => h.requests.filter((r) => r === `GET /api/v1/repos/${REPO_ID}`).length;

describe('repo home header', () => {
  it('names the repo, its binding and remote, with a back link to the list', async () => {
    await mountRepoHome();
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');

    const back = container.querySelector<HTMLAnchorElement>('a.back-link');
    expect(back?.getAttribute('href')).toBe('/repos');
    expect(back?.textContent?.trim()).toBe('Repositories');
    // The repo name is the page heading — the only h1.
    expect(Array.from(container.querySelectorAll('h1')).map((el) => el.textContent)).toEqual([
      'coding-lab',
    ]);
    const chips = Array.from(container.querySelectorAll('.repo-head .chip')).map(
      (el) => el.textContent,
    );
    expect(chips).toEqual(['forge · github']);
    expect(container.querySelector('.repo-head-remote')?.textContent).toBe(
      'github.com/Cloonar/coding-lab',
    );
  });

  it('shows the builtin binding and the incogni chip', async () => {
    h.repo = baseRepo({ tracker_binding: 'builtin', forge_kind: 'none', incogni: true });
    await mountRepoHome();
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');

    const chips = Array.from(container.querySelectorAll('.repo-head .chip')).map(
      (el) => el.textContent,
    );
    expect(chips).toEqual(['builtin tracker', 'incogni']);
  });

  it('names the clone state in words while the repo is not ready', async () => {
    h.repo = baseRepo({ clone_status: 'cloning' });
    await mountRepoHome();
    expect(
      (await waitFor(() => container.querySelector('.repo-head .chip.status-cloning'), 'chip'))
        .textContent,
    ).toBe('cloning');

    h.repo = baseRepo({ clone_status: 'error', clone_error: 'auth failed' });
    emit('repo.changed', { repoID: REPO_ID });
    expect(
      (await waitFor(() => container.querySelector('.repo-head .chip.status-error'), 'chip'))
        .textContent,
    ).toBe('clone failed');
    expect(container.querySelector('.repo-head .chip.status-cloning')).toBeNull();
  });

  it('shows no clone chip once the repo is ready', async () => {
    await mountRepoHome();
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');
    expect(container.querySelector('.repo-head .status-cloning, .repo-head .status-error')).toBe(
      null,
    );
  });
});

describe('repo home tabs', () => {
  it('links Overview, Issues and Settings to their own URLs; no CRs tab on a forge repo', async () => {
    await mountRepoHome();
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');

    expect(container.querySelector('nav.repo-tabs')?.getAttribute('aria-label')).toBe('Repository');
    expect(tabs().map((a) => a.getAttribute('href'))).toEqual([
      BASE,
      `${BASE}/issues`,
      `${BASE}/settings`,
    ]);
    expect(tabLabels()).toEqual(['Overview', 'Issues', 'Settings']);
  });

  it('adds the CRs tab for a builtin-bound repo', async () => {
    h.repo = baseRepo({ tracker_binding: 'builtin', forge_kind: 'none' });
    await mountRepoHome();
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');

    expect(tabs().map((a) => a.getAttribute('href'))).toEqual([
      BASE,
      `${BASE}/issues`,
      `${BASE}/crs`,
      `${BASE}/settings`,
    ]);
  });

  it('shows the open issue count when the summary knows it — 0 included', async () => {
    await mountRepoHome();
    const issues = await waitFor(
      () => container.querySelector(`nav.repo-tabs a[href="${BASE}/issues"] .count`),
      'count',
    );
    expect(issues.textContent).toBe('12');
    // The pill is decorative; the link's accessible text spells the count out.
    expect(issues.getAttribute('aria-hidden')).toBe('true');
    expect(
      container.querySelector(`nav.repo-tabs a[href="${BASE}/issues"] .visually-hidden`)
        ?.textContent,
    ).toBe(' (12 open)');

    h.repo = baseRepo({ summary: { ...baseRepo().summary, open_issues: 0 } });
    emit('repo.changed', { repoID: REPO_ID });
    await settle();
    expect(
      container.querySelector(`nav.repo-tabs a[href="${BASE}/issues"] .count`)?.textContent,
    ).toBe('0');
  });

  it('shows no count while the summary does not know it', async () => {
    h.repo = baseRepo({ summary: { ...baseRepo().summary, open_issues: null } });
    await mountRepoHome();
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');

    const issues = container.querySelector(`nav.repo-tabs a[href="${BASE}/issues"]`);
    expect(issues?.querySelector('.count')).toBeNull();
    expect(issues?.textContent).toBe('Issues');
  });

  const currentFor: Array<[path: string, tab: string]> = [
    [BASE, BASE],
    [`${BASE}/issues`, `${BASE}/issues`],
    [`${BASE}/issues/12`, `${BASE}/issues`],
    [`${BASE}/issues/new`, `${BASE}/issues`],
    [`${BASE}/labels`, `${BASE}/issues`],
    [`${BASE}/crs`, `${BASE}/crs`],
    [`${BASE}/crs/3`, `${BASE}/crs`],
    [`${BASE}/settings`, `${BASE}/settings`],
    [`${BASE}/settings/agents`, `${BASE}/settings`],
    [`${BASE}/settings/schedules/new`, `${BASE}/settings`],
  ];
  for (const [path, tab] of currentFor) {
    it(`marks ${tab.slice(BASE.length) || 'Overview'} current at ${path}`, async () => {
      h.repo = baseRepo({ tracker_binding: 'builtin', forge_kind: 'none' });
      await mountRepoHome(path);
      await waitFor(() => container.querySelector('.repo-head h1'), 'header');
      expect(currentTab()).toEqual([tab]);
    });
  }

  it('moves aria-current when a tab is followed, keeping the frame mounted', async () => {
    await mountRepoHome();
    const heading = await waitFor(() => container.querySelector('.repo-head h1'), 'header');

    container.querySelector<HTMLAnchorElement>(`nav.repo-tabs a[href="${BASE}/issues"]`)?.click();
    await settle();

    expect(routerHistory.get()).toBe(`${BASE}/issues`);
    expect(currentTab()).toEqual([`${BASE}/issues`]);
    // Same frame: its heading node survived and the repo was fetched once.
    expect(container.querySelector('.repo-head h1')).toBe(heading);
    expect(repoGets()).toBe(1);
  });
});

describe('activeRepoTab', () => {
  it('maps each repo path to its tab', () => {
    expect(activeRepoTab('/repos/r')).toBe('overview');
    expect(activeRepoTab('/repos/r/')).toBe('overview');
    expect(activeRepoTab('/repos/r/issues/4')).toBe('issues');
    expect(activeRepoTab('/repos/r/labels')).toBe('issues');
    expect(activeRepoTab('/repos/r/crs/2')).toBe('crs');
    expect(activeRepoTab('/repos/r/settings/schedules/s1')).toBe('settings');
    expect(activeRepoTab('/repos/r/elsewhere')).toBeNull();
  });
});

describe('repo home routes', () => {
  // One probe per deep link: an element only that page renders.
  const pages: Array<[path: string, probe: () => Element | null]> = [
    [BASE, () => container.querySelector('.repo-overview')],
    [`${BASE}/issues`, () => container.querySelector('.filter-row')],
    [`${BASE}/issues/new`, () => container.querySelector('input[name="title"]')],
    [
      `${BASE}/issues/12`,
      () =>
        Array.from(container.querySelectorAll('.section-head h2')).find((el) =>
          el.textContent?.includes('Fix login'),
        ) ?? null,
    ],
    [`${BASE}/labels`, () => container.querySelector('ul.label-list')],
    [`${BASE}/crs`, () => container.querySelector('[aria-label="Filter by state"]')],
    [`${BASE}/crs/3`, () => container.querySelector('.cr-branches')],
    // The one-page settings: every section renders at either URL.
    [`${BASE}/settings`, () => container.querySelector('section#settings-agents h2')],
    [`${BASE}/settings/branches`, () => container.querySelector('input[name="default_branch"]')],
  ];
  for (const [path, probe] of pages) {
    it(`renders ${path} inside the frame`, async () => {
      h.repo = baseRepo({ tracker_binding: 'builtin', forge_kind: 'none' });
      await mountRepoHome(path);
      await waitFor(probe, `page at ${path}`);

      // Inside the frame: one <main>, the frame's own, holding the page.
      const mains = container.querySelectorAll('main');
      expect(mains).toHaveLength(1);
      expect(mains[0]?.classList.contains('repo-home')).toBe(true);
      expect(container.querySelector('.repo-home-body')?.contains(probe())).toBe(true);
      expect(container.querySelector('.repo-head h1')?.textContent).toBe('coding-lab');
      expect(routerHistory.get()).toBe(path);
    });
  }

  it('still resolves /repos/new to Add repository, outside the frame', async () => {
    await mountRepoHome('/repos/new');
    await waitFor(
      () => (container.textContent?.includes('Add repository') ? container : null),
      'add repository',
    );

    expect(container.querySelector('.repo-home')).toBeNull();
    expect(h.requests).not.toContain('GET /api/v1/repos/new');
  });

  it('shows the error banner in the frame when the repo fails to load', async () => {
    h.repo = null;
    await mountRepoHome();
    const banner = await waitFor(() => container.querySelector('.repo-head .banner.error'), 'b');

    expect(banner.textContent).toContain('repo lookup failed');
    expect(container.querySelector('.repo-head h1')).toBeNull();
    // The way out and the tabs stay usable.
    expect(container.querySelector('a.back-link')?.getAttribute('href')).toBe('/repos');
    expect(currentTab()).toEqual([BASE]);
  });

  it('refetches its one repo on repo.changed for this repo only', async () => {
    await mountRepoHome(`${BASE}/issues`);
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');
    expect(repoGets()).toBe(1);

    emit('repo.changed', { repoID: 'repo_other' });
    await settle();
    expect(repoGets()).toBe(1);

    h.repo = baseRepo({ name: 'renamed-lab' });
    emit('repo.changed', { repoID: REPO_ID });
    await settle();
    expect(repoGets()).toBe(2);
    expect(container.querySelector('.repo-head h1')?.textContent).toBe('renamed-lab');
  });
});

describe('repo home summary refreshes', () => {
  const waitDebounce = () =>
    new Promise<void>((resolve) => setTimeout(resolve, SUMMARY_REFRESH_MS + 50));
  const issuesCount = () =>
    container.querySelector(`nav.repo-tabs a[href="${BASE}/issues"] .count`)?.textContent;

  const triggers: Array<[string, Record<string, unknown>]> = [
    ['issue.changed', { repoID: REPO_ID }],
    ['run.changed', { repoID: REPO_ID, runID: 'run_1' }],
    ['parked.changed', { repoID: REPO_ID }],
    ['provider.auth.changed', { provider: 'agent-a' }],
  ];
  for (const [type, payload] of triggers) {
    it(`re-reads the repo once on a burst of ${type}`, async () => {
      // Labels of a forge repo: a tab that reads nothing itself, so every
      // request after the event is the frame's.
      await mountRepoHome(`${BASE}/labels`);
      await waitFor(() => container.querySelector('.repo-head h1'), 'header');
      expect(repoGets()).toBe(1);

      h.repo = baseRepo({
        summary: { ...baseRepo().summary, open_issues: 13, claimable: 4 },
      });
      const before = h.requests.length;
      emit(type, payload);
      emit(type, payload);
      await settle();
      expect(repoGets()).toBe(1); // inside the debounce window
      await waitDebounce();
      await settle();

      expect(repoGets()).toBe(2);
      expect(issuesCount()).toBe('13');
      // Lab's own repo read and nothing else: no request that reaches a forge.
      expect(h.requests.slice(before).filter((r) => r.startsWith('GET /api/v1/repos'))).toEqual([
        `GET /api/v1/repos/${REPO_ID}`,
      ]);
    });
  }

  it('ignores the issue, run and parked events of another repo', async () => {
    await mountRepoHome(`${BASE}/issues`);
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');

    emit('issue.changed', { repoID: 'repo_other' });
    emit('run.changed', { repoID: 'repo_other', runID: 'run_9' });
    emit('parked.changed', { repoID: 'repo_other' });
    await waitDebounce();
    await settle();
    expect(repoGets()).toBe(1);
  });
});

describe('repo home after a failed refetch', () => {
  it('keeps the repo and the tab content, and shows the error', async () => {
    await mountRepoHome();
    await waitFor(() => container.querySelector('.afk-card'), 'afk card');

    h.repo = null;
    h.repoError = 'the store is locked';
    emit('repo.changed', { repoID: REPO_ID });
    await settle();

    expect(container.querySelector('.repo-head .banner')?.textContent).toContain(
      'the store is locked',
    );
    expect(container.querySelector('.repo-head h1')?.textContent).toBe('coding-lab');
    expect(container.querySelector('.readiness')).not.toBeNull();
    expect(container.querySelector('.afk-card')).not.toBeNull();
  });
});

describe('repo home moving between repos', () => {
  const OTHER = 'repo_b';
  const other = baseRepo({
    id: OTHER,
    name: 'other-lab',
    remote_url: 'git@github.com:x/other.git',
  });

  it('shows nothing of the previous repo while the next loads, and drops its toast', async () => {
    let releaseOther: () => void = () => {};
    h.handle = (method, url) => {
      if (method === 'POST' && url === `/api/v1/repos/${REPO_ID}/afk/start`) {
        return jsonResponse(202, { run: { id: 'run_9', issue_number: 7 } });
      }
      if (method === 'GET' && url === `/api/v1/repos/${OTHER}`) {
        return new Promise((resolve) => {
          releaseOther = () => resolve(jsonResponse(200, other));
        }) as unknown as ReturnType<typeof jsonResponse>;
      }
      if (method === 'GET' && url === `/api/v1/repos/${OTHER}/readiness`) {
        return jsonResponse(200, other.summary.readiness);
      }
      if (method === 'GET' && url === `/api/v1/repos/${OTHER}/parked`) {
        return jsonResponse(200, { parked: [] });
      }
      return undefined;
    };
    await mountRepoHome();
    const runOne = await waitFor(
      () =>
        Array.from(container.querySelectorAll('button')).find((b) =>
          b.textContent?.includes('Run one'),
        ),
      'Run one',
    );
    runOne.click();
    await settle();
    expect(container.querySelector('.toast')?.textContent).toBe('Started an AFK run on #7');

    routerHistory.set({ value: `/repos/${OTHER}` });
    await settle();
    // repo_1's toast is gone, and nothing of repo_1 shows while repo_b loads.
    expect(container.querySelector('.toast')).toBeNull();
    expect(container.querySelector('.repo-head h1')).toBeNull();
    expect(container.querySelector('.repo-overview')?.textContent).toBe('');
    expect(container.textContent).not.toContain('coding-lab');

    releaseOther();
    await settle();
    expect(container.querySelector('.repo-head h1')?.textContent).toBe('other-lab');
  });
});

describe('repo home route notice', () => {
  // A stand-in for a page that navigates into the repo home after an action
  // (Add repository), handing over its confirmation through router state.
  function Launcher() {
    const navigate = useNavigate();
    return (
      <button
        type="button"
        onClick={() => navigate(BASE, { state: noticeState('Added coding-lab. Cloning started.') })}
      >
        Launch
      </button>
    );
  }
  const extra = () => <Route path="/launch" component={Launcher} />;

  it('shows a notice that arrives with the navigation once, then clears it', async () => {
    await mountRepoHome('/launch', extra);
    const launch = await waitFor(
      () =>
        Array.from(container.querySelectorAll('button')).find((b) => b.textContent === 'Launch'),
      'launcher',
    );
    launch.click();
    await settle();

    expect(routerHistory.get()).toBe(BASE);
    const toast = await waitFor(() => container.querySelector('.toast'), 'toast');
    // Inside the always-present status region, so it is announced.
    expect(toast.closest('[role="status"]')).not.toBeNull();
    expect(toast.textContent).toBe('Added coding-lab. Cloning started.');

    // One toast, inside the frame (the clearing of the state itself is
    // lib/routeNotice.test.tsx's to prove).
    expect(container.querySelectorAll('.toast')).toHaveLength(1);
    expect(container.querySelector('.repo-home')?.contains(toast)).toBe(true);
  });

  it('shows no toast when the navigation carries no notice', async () => {
    await mountRepoHome();
    await waitFor(() => container.querySelector('.repo-head h1'), 'header');
    expect(container.querySelector('.toast')).toBeNull();
  });
});
