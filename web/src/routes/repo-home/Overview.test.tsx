// Overview tab contract (issue #61), through the real route tree:
// - on load it requests only the readiness report, the instance list and the
//   parked list (besides the frame's repo) — nothing that reaches a forge;
// - Readiness: collapsed to "Ready to run" when every check passes; open with
//   failing checks first otherwise; a failing check's ONE action is Retry
//   clone or a Fix that opens the named settings field; while the clone runs,
//   the clone check shows live progress and only Readiness is shown;
// - Live runs: this repo's live instances in words, each linking to its Chat;
//   Stop all asks in place, then stops and toasts the count;
// - AFK: the summary's claimable count, Run one / Auto / Reset;
// - Parked work: the card, whose Discard asks in place.

import { describe, expect, it } from 'vitest';
import type { Readiness, ReadinessCheck } from '../../api';
import {
  REPO_ID,
  baseInstance,
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

installRepoHomeHooks();

const BASE = `/repos/${REPO_ID}`;
const API = `/api/v1/repos/${REPO_ID}`;

const passing = (id: ReadinessCheck['id'], detail = `${id} is fine.`): ReadinessCheck => ({
  id,
  state: 'passing',
  detail,
});

const sixPassing: Readiness = {
  state: 'passing',
  checks: [
    passing('clone', 'The reference repo is up to date.'),
    passing('git_credential'),
    passing('tracker'),
    passing('agent_login'),
    passing('dev_image'),
    passing('imports'),
  ],
};

const trackerFailing: ReadinessCheck = {
  id: 'tracker',
  state: 'failing',
  detail: 'The forge token was rejected, so issues cannot be read.',
  fix: { scope: 'repo', section: 'integrations', field: 'forge_credential_id' },
};

const q = <T extends Element = HTMLElement>(selector: string): T | null =>
  container.querySelector<T>(selector);

function buttonNamed(name: string): HTMLButtonElement | undefined {
  return Array.from(container.querySelectorAll('button')).find(
    (b) => b.textContent?.trim() === name,
  );
}

const checkTitles = () =>
  Array.from(container.querySelectorAll('.readiness-check .readiness-check-title')).map((el) =>
    // Drop the visually hidden state word ("Failing: ").
    (el.lastChild?.textContent ?? '').trim(),
  );

const gets = (url: string) => h.requests.filter((r) => r === `GET ${url}`).length;

async function mountOverview(): Promise<void> {
  await mountRepoHome();
  await waitFor(() => q('.readiness'), 'readiness block');
}

describe('Overview on load', () => {
  it('requests only the readiness report, the instances and the parked list', async () => {
    await mountOverview();
    await waitFor(() => q('.parked-card'), 'parked card');

    expect(new Set(h.requests)).toEqual(
      new Set([
        'GET /api/v1/auth/state',
        'GET /api/v1/instances',
        `GET ${API}`,
        `GET ${API}/readiness`,
        `GET ${API}/parked`,
      ]),
    );
    // Above all, no ready-queue read: that one reaches the forge.
    expect(h.requests.some((r) => r.includes('/ready'))).toBe(false);
  });

  it('shows the blocks in order: readiness, live runs, AFK, parked work', async () => {
    await mountOverview();
    await waitFor(() => q('.parked-card'), 'parked card');

    const headings = Array.from(container.querySelectorAll('.repo-overview h2')).map(
      (el) => el.textContent?.trim() ?? '',
    );
    expect(headings[0]).toContain('Ready to run');
    expect(headings.slice(1)).toEqual(['Live runs', 'AFK', 'Parked work (0)']);
  });
});

describe('Overview readiness', () => {
  it('collapses to one line when every check passes, and expands on demand', async () => {
    h.readiness = sixPassing;
    await mountOverview();

    const toggle = q<HTMLButtonElement>('.readiness-toggle');
    expect(toggle?.getAttribute('aria-expanded')).toBe('false');
    expect(q('.readiness-head .readiness-title')?.textContent).toBe('Ready to run');
    expect(q('.readiness-head .readiness-detail')?.textContent).toBe('All 6 checks pass.');
    expect(q('.readiness-checks')?.hidden).toBe(true);

    toggle?.click();
    await settle();
    expect(toggle?.getAttribute('aria-expanded')).toBe('true');
    expect(q('.readiness-checks')?.hidden).toBe(false);
    expect(checkTitles()).toEqual([
      'Clone',
      'Git credential',
      'Tracker',
      'Agent',
      'Dev image',
      'Imports',
    ]);
  });

  it('keeps the operator’s open block open across a refetch that changes nothing', async () => {
    h.readiness = sixPassing;
    await mountOverview();
    q<HTMLButtonElement>('.readiness-toggle')?.click();
    await settle();

    emit('run.changed', {});
    await settle();
    expect(gets(`${API}/readiness`)).toBe(2);
    expect(q('.readiness-toggle')?.getAttribute('aria-expanded')).toBe('true');
  });

  it('reads "Ready to run" for a passing report with no checks', async () => {
    h.readiness = { state: 'passing', checks: [] };
    await mountOverview();
    expect(q('.readiness-title')?.textContent).toBe('Ready to run');
  });

  it('opens with failing checks first and lists only the checks reported', async () => {
    h.readiness = {
      state: 'failing',
      checks: [passing('clone'), passing('git_credential'), trackerFailing, passing('imports')],
    };
    await mountOverview();

    expect(q('.readiness-toggle')?.getAttribute('aria-expanded')).toBe('true');
    expect(q('.readiness')?.classList.contains('failing')).toBe(true);
    expect(q('.readiness-head .readiness-title')?.textContent).toBe('Not ready, 1 problem');
    expect(checkTitles()).toEqual(['Tracker', 'Clone', 'Git credential', 'Imports']);
    // The state is in words too, not colour alone.
    expect(q('.readiness-check.failing .visually-hidden')?.textContent).toBe('Failing: ');
    expect(q('.readiness-check.failing .readiness-detail')?.textContent).toBe(
      'The forge token was rejected, so issues cannot be read.',
    );
    // ONE action on the failing check, none on the passing ones.
    expect(container.querySelectorAll('.readiness-check.failing button')).toHaveLength(1);
    expect(container.querySelectorAll('.readiness-check.passing button')).toHaveLength(0);
  });

  it('Fix opens the named settings field', async () => {
    h.readiness = { state: 'failing', checks: [trackerFailing, passing('clone')] };
    await mountOverview();

    buttonNamed('Change credential')?.click();
    await settle();
    expect(routerHistory.get()).toBe(`${BASE}/settings/integrations?field=forge_credential_id`);
  });

  it('Retry clone restarts a failed clone and says so', async () => {
    h.repo = baseRepo({ clone_status: 'error', clone_error: 'auth failed' });
    h.readiness = {
      state: 'failing',
      checks: [
        { id: 'clone', state: 'failing', detail: 'The last clone failed.', action: 'retry_clone' },
      ],
    };
    const posts: string[] = [];
    h.handle = (method, url) => {
      if (method === 'POST' && url === `${API}/clone/retry`) {
        posts.push(url);
        return jsonResponse(202, {});
      }
      return undefined;
    };
    await mountOverview();

    buttonNamed('Retry clone')?.click();
    await settle();

    expect(posts).toEqual([`${API}/clone/retry`]);
    expect(q('.toast')?.textContent).toBe('Retrying the clone of coding-lab');
  });

  it('shows the running clone’s progress and nothing but readiness', async () => {
    h.repo = baseRepo({
      clone_status: 'cloning',
      summary: { claimable: null, open_issues: 0, readiness: { state: 'pending', checks: [] } },
    });
    h.readiness = {
      state: 'pending',
      checks: [
        { id: 'clone', state: 'pending', detail: 'Cloning.' },
        passing('git_credential'),
        passing('tracker'),
      ],
    };
    await mountOverview();

    expect(q('.readiness-toggle')?.getAttribute('aria-expanded')).toBe('true');
    expect(q('.readiness-head .readiness-title')?.textContent).toBe('Getting ready');
    expect(q('.readiness-head .readiness-detail')?.textContent).toBe(
      'Runs can start when the clone has finished.',
    );
    expect(checkTitles()[0]).toBe('Clone');
    const bar = q('.readiness-check.pending [role="progressbar"]');
    expect(bar?.getAttribute('aria-label')).toBe('Clone progress');
    expect(bar?.hasAttribute('aria-valuenow')).toBe(false); // unknown yet: indeterminate

    emit('clone.progress', { repoID: REPO_ID, phase: 'receiving objects', percent: 62, line: '' });
    await settle();
    expect(bar?.getAttribute('aria-valuenow')).toBe('62');
    expect(q('.readiness-progress .progress-meta')?.textContent).toContain('Receiving objects');
    expect(q('.readiness-progress .progress-meta')?.textContent).toContain('62%');

    // Live runs, AFK and parked work wait for the clone.
    expect(q('.live-runs')).toBeNull();
    expect(q('.afk-card')).toBeNull();
    expect(q('.parked-card')).toBeNull();
    expect(h.requests).not.toContain(`GET ${API}/parked`);
  });

  it('refetches the report on provider.auth.changed, run.changed and this repo’s repo.changed', async () => {
    await mountOverview();
    expect(gets(`${API}/readiness`)).toBe(1);

    emit('provider.auth.changed', {});
    await settle();
    expect(gets(`${API}/readiness`)).toBe(2);

    emit('run.changed', {});
    await settle();
    expect(gets(`${API}/readiness`)).toBe(3);

    emit('repo.changed', { repoID: 'repo_other' });
    await settle();
    expect(gets(`${API}/readiness`)).toBe(3);

    h.readiness = { state: 'failing', checks: [trackerFailing] };
    emit('repo.changed', { repoID: REPO_ID });
    await settle();
    expect(gets(`${API}/readiness`)).toBe(4);
    // Newly failing → the block opens.
    expect(q('.readiness-toggle')?.getAttribute('aria-expanded')).toBe('true');
  });

  it('falls back to the summary’s report when the endpoint cannot be read', async () => {
    h.readiness = null;
    h.repo = baseRepo({
      summary: {
        claimable: 3,
        open_issues: 1,
        readiness: { state: 'failing', checks: [trackerFailing] },
      },
    });
    await mountOverview();
    expect(q('.readiness-title')?.textContent).toBe('Not ready, 1 problem');
  });
});

describe('Overview live runs', () => {
  it('lists this repo’s live runs in words, waiting first, each linking to its Chat', async () => {
    h.instances = [
      baseInstance({
        id: 'run_afk',
        title: null,
        session_name: 'coding-lab~afk-61',
        kind: 'afk_manual',
        issue_number: 61,
        state: 'working',
        budget_deadline: new Date(Date.now() + 72 * 60_000 + 30_000).toISOString(),
      }),
      baseInstance({ id: 'run_wait', title: 'Fix chat dock overlap', state: 'needs_input' }),
      baseInstance({ id: 'run_idle', title: 'Bump inputs', state: 'idle' }),
      baseInstance({ id: 'run_gone', title: 'Ended', live: false }),
      baseInstance({ id: 'run_other', title: 'Elsewhere', repo_id: 'repo_other' }),
    ];
    await mountOverview();
    await waitFor(() => q('.live-run'), 'live runs');

    const rows = Array.from(container.querySelectorAll<HTMLAnchorElement>('a.live-run'));
    expect(rows.map((a) => a.getAttribute('href'))).toEqual([
      '/runs/run_wait',
      '/runs/run_afk',
      '/runs/run_idle',
    ]);
    expect(rows.map((a) => a.querySelector('.live-run-title')?.textContent)).toEqual([
      'Fix chat dock overlap',
      'AFK #61',
      'Bump inputs',
    ]);
    expect(rows.map((a) => a.querySelector('.live-run-state')?.textContent)).toEqual([
      'Waiting for you',
      'Working · ~1h 12m left',
      'Idle',
    ]);

    // A state change patches in place.
    emit('run.messages.changed', { runID: 'run_idle', state: 'question' });
    await settle();
    expect(container.querySelector('a[href="/runs/run_idle"] .live-run-state')?.textContent).toBe(
      'Waiting for you',
    );
  });

  it('says "No live runs." and offers no Stop all without any', async () => {
    await mountOverview();
    await waitFor(() => q('.live-runs'), 'live runs');
    expect(q('.live-runs')?.textContent).toContain('No live runs.');
    expect(buttonNamed('Stop all (0)')).toBeUndefined();
  });

  it('Stop all asks in place, then stops and toasts the count', async () => {
    h.instances = [
      baseInstance({ id: 'a', state: 'working' }),
      baseInstance({ id: 'b', state: 'needs_input' }),
    ];
    const posts: string[] = [];
    h.handle = (method, url) => {
      if (method === 'POST' && url === `${API}/stop-all`) {
        posts.push(url);
        return jsonResponse(200, { stopped: 2 });
      }
      return undefined;
    };
    await mountOverview();
    await waitFor(() => q('.live-run'), 'live runs');

    buttonNamed('Stop all (2)')?.click();
    await settle();
    expect(posts).toEqual([]); // asked, not acted
    expect(buttonNamed('Cancel')).toBeDefined();

    buttonNamed('Cancel')?.click();
    await settle();
    expect(buttonNamed('Stop all (2)')).toBeDefined();

    buttonNamed('Stop all (2)')?.click();
    await settle();
    buttonNamed('Stop 2 runs')?.click();
    await settle();

    expect(posts).toEqual([`${API}/stop-all`]);
    expect(q('.toast')?.textContent).toBe('Stopped 2 runs in coding-lab.');
  });
});

describe('Overview AFK', () => {
  it('shows the summary’s claimable count without reading the ready queue', async () => {
    await mountOverview();
    await waitFor(() => q('.afk-card'), 'afk card');
    expect(q('.afk-card-count')?.textContent).toBe('3 issues ready for an agent.');
    expect(h.requests.some((r) => r.includes('/ready'))).toBe(false);
  });

  it('Run one starts an AFK run and toasts it, staying on the page', async () => {
    h.handle = (method, url) =>
      method === 'POST' && url === `${API}/afk/start`
        ? jsonResponse(202, { run: { id: 'run_9', issue_number: 7 } })
        : undefined;
    await mountOverview();
    await waitFor(() => q('.afk-card'), 'afk card');

    buttonNamed('Run one')?.click();
    await settle();

    expect(h.requests).toContain(`POST ${API}/afk/start`);
    expect(q('.toast')?.textContent).toBe('Started an AFK run on #7');
    expect(routerHistory.get()).toBe(BASE);
  });

  it('Auto applies at once and refetches the frame’s repo', async () => {
    let body: unknown;
    h.handle = (method, url, init) => {
      if (method === 'PUT' && url === `${API}/afk/auto`) {
        body = JSON.parse(String(init?.body));
        h.repo = baseRepo({ afk_auto_enabled: true });
        return jsonResponse(200, h.repo);
      }
      return undefined;
    };
    await mountOverview();
    await waitFor(() => q('.afk-card'), 'afk card');
    const repoGets = gets(API);

    q<HTMLButtonElement>('.afk-card button[role="switch"]')?.click();
    await settle();

    expect(body).toEqual({ enabled: true });
    expect(gets(API)).toBe(repoGets + 1);
    expect(q('.afk-card button[role="switch"]')?.getAttribute('aria-checked')).toBe('true');
    expect(q('.toast')?.textContent).toBe('Auto-spawn on for coding-lab');
  });

  it('Reset lifts the three-strikes pause', async () => {
    h.repo = baseRepo({ consecutive_failures: 3 });
    h.handle = (method, url) => {
      if (method === 'POST' && url === `${API}/afk/reset`) {
        h.repo = baseRepo();
        return jsonResponse(200, h.repo);
      }
      return undefined;
    };
    await mountOverview();
    await waitFor(() => q('.afk-card-paused'), 'paused banner');

    buttonNamed('Reset')?.click();
    await settle();

    expect(h.requests).toContain(`POST ${API}/afk/reset`);
    expect(q('.afk-card-paused')).toBeNull();
    expect(q('.toast')?.textContent).toBe('AFK runs resumed for coding-lab');
  });
});

describe('Overview parked work', () => {
  it('lists parked work; Discard asks in place, then discards and toasts', async () => {
    h.parked = [
      {
        branch: 'lab/try-sse-backoff',
        worktree_path: '/wt/try',
        dirty: true,
        commits_ahead: 0,
        unpushed: 0,
      },
    ];
    const bodies: unknown[] = [];
    h.handle = (method, url, init) => {
      if (method === 'POST' && url === `${API}/parked/discard`) {
        bodies.push(JSON.parse(String(init?.body)));
        h.parked = [];
        return jsonResponse(204, {});
      }
      return undefined;
    };
    await mountOverview();
    await waitFor(() => q('.parked-entry'), 'parked entry');
    expect(q('.parked-card .parked-state')?.textContent).toBe('Worktree has uncommitted changes');

    q<HTMLButtonElement>('button.parked-discard')?.click();
    await settle();
    expect(bodies).toEqual([]); // asked in place
    const input = q<HTMLInputElement>('.parked-entry input[name="confirm-branch"]');
    expect(buttonNamed('Discard forever')?.disabled).toBe(true);

    if (input) {
      input.value = 'lab/try-sse-backoff';
      input.dispatchEvent(new Event('input', { bubbles: true }));
    }
    buttonNamed('Discard forever')?.click();
    await settle();

    expect(bodies).toEqual([{ branch: 'lab/try-sse-backoff' }]);
    expect(q('.toast')?.textContent).toBe('Discarded lab/try-sse-backoff');
    expect(q('.parked-card')?.textContent).toContain('Nothing parked.');
  });

  it('renders no parked block when the endpoint is unavailable', async () => {
    h.parked = null;
    await mountOverview();
    await waitFor(() => q('.afk-card'), 'afk card');
    await settle();
    expect(q('.parked-card')).toBeNull();
  });
});
