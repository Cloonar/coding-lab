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
  unmount,
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
        // Not the Overview's: the app shell reads the provider catalog and the
        // global default for its More-tab logged-out dot (issue #76). The set
        // still fails on any request the Overview adds beyond these.
        'GET /api/v1/providers',
        'GET /api/v1/settings',
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

describe('Overview readiness, robust and scoped', () => {
  it('refetches on run.changed for this repo only', async () => {
    await mountOverview();
    expect(gets(`${API}/readiness`)).toBe(1);

    emit('run.changed', { repoID: 'repo_other', runID: 'run_9' });
    await settle();
    expect(gets(`${API}/readiness`)).toBe(1);

    emit('run.changed', { repoID: REPO_ID, runID: 'run_1' });
    await settle();
    expect(gets(`${API}/readiness`)).toBe(2);
  });

  it('renders a state from a newer server as pending, and a report without checks', async () => {
    h.readiness = {
      state: 'passing',
      checks: [{ ...passing('tracker'), state: 'degraded' as ReadinessCheck['state'] }],
    };
    await mountOverview();
    expect(q('.readiness-head .readiness-title')?.textContent).toBe('Getting ready');
    expect(q('.readiness-check.pending .visually-hidden')?.textContent).toBe('Pending: ');

    unmount();
    h.readiness = { state: 'passing' } as unknown as Readiness;
    h.repo = { ...baseRepo(), summary: undefined } as unknown as typeof h.repo;
    await mountOverview();
    expect(q('.readiness-head .readiness-title')?.textContent).toBe('Ready to run');
    expect(q('.afk-card-count')).not.toBeNull();
  });

  it('ties each action to its check, so two "Change credential" buttons differ', async () => {
    h.readiness = {
      state: 'failing',
      checks: [
        {
          id: 'git_credential',
          state: 'failing',
          detail: 'The git credential was rejected.',
          fix: { scope: 'repo', section: 'integrations', field: 'credential_id' },
        },
        trackerFailing,
      ],
    };
    await mountOverview();
    const buttons = Array.from(container.querySelectorAll<HTMLButtonElement>('.readiness-action'));
    expect(buttons.map((b) => b.textContent)).toEqual(['Change credential', 'Change credential']);
    expect(
      buttons.map((b) => document.getElementById(b.getAttribute('aria-describedby') ?? '')),
    ).toEqual(Array.from(container.querySelectorAll('.readiness-check-title')));
    expect(
      buttons.map((b) =>
        document
          .getElementById(b.getAttribute('aria-describedby') ?? '')
          ?.lastChild?.textContent?.trim(),
      ),
    ).toEqual(['Git credential', 'Tracker']);
  });

  it('keeps a check row and the focus on its Fix button through a refetch', async () => {
    h.readiness = { state: 'failing', checks: [trackerFailing, passing('clone')] };
    await mountOverview();
    const fix = buttonNamed('Change credential')!;
    fix.focus();

    h.readiness = {
      state: 'failing',
      checks: [{ ...trackerFailing, detail: 'Still rejected.' }, passing('clone')],
    };
    emit('provider.auth.changed', {});
    await settle();

    expect(q('.readiness-check.failing .readiness-detail')?.textContent).toBe('Still rejected.');
    expect(buttonNamed('Change credential')).toBe(fix);
    expect(document.activeElement).toBe(fix);
  });

  it('moves focus to the block’s toggle once Retry clone turns the check pending', async () => {
    h.repo = baseRepo({ clone_status: 'error', clone_error: 'auth failed' });
    h.readiness = {
      state: 'failing',
      checks: [
        { id: 'clone', state: 'failing', detail: 'The last clone failed.', action: 'retry_clone' },
      ],
    };
    h.handle = (method, url) => {
      if (method === 'POST' && url === `${API}/clone/retry`) {
        h.repo = baseRepo({ clone_status: 'cloning' });
        h.readiness = {
          state: 'pending',
          checks: [{ id: 'clone', state: 'pending', detail: 'Cloning.' }],
        };
        return jsonResponse(202, {});
      }
      return undefined;
    };
    await mountOverview();
    const retry = buttonNamed('Retry clone')!;
    retry.focus();
    retry.click();
    await settle();

    expect(buttonNamed('Retry clone')).toBeUndefined();
    expect(q('.readiness-head .readiness-title')?.textContent).toBe('Getting ready');
    expect(document.activeElement).toBe(q('.readiness-toggle'));
  });
});

describe('Overview live runs across refetches', () => {
  it('keeps each run’s link, and the focus on it, through run.changed', async () => {
    h.instances = [
      baseInstance({ id: 'run_a', title: 'Alpha', state: 'working' }),
      baseInstance({ id: 'run_b', title: 'Beta', state: 'idle' }),
    ];
    await mountOverview();
    const link = await waitFor(
      () => q<HTMLAnchorElement>('a.live-run[href="/runs/run_b"]'),
      'live run',
    );
    link.focus();

    h.instances = [
      baseInstance({ id: 'run_a', title: 'Alpha', state: 'working' }),
      baseInstance({ id: 'run_b', title: 'Beta renamed', state: 'idle' }),
    ];
    emit('run.changed', { repoID: REPO_ID, runID: 'run_b' });
    await settle();

    expect(q('a.live-run[href="/runs/run_b"] .live-run-title')?.textContent).toBe('Beta renamed');
    expect(q('a.live-run[href="/runs/run_b"]')).toBe(link);
    expect(document.activeElement).toBe(link);
  });

  it('moves focus to the block’s heading once Stop all empties the list', async () => {
    h.instances = [baseInstance({ id: 'a', state: 'working' })];
    h.handle = (method, url) => {
      if (method === 'POST' && url === `${API}/stop-all`) {
        h.instances = [];
        return jsonResponse(200, { stopped: 1 });
      }
      return undefined;
    };
    await mountOverview();
    await waitFor(() => q('.live-run'), 'live runs');

    buttonNamed('Stop all (1)')?.click();
    await settle();
    const confirm = buttonNamed('Stop 1 run')!;
    confirm.focus();
    confirm.click();
    await settle();

    expect(q('.live-runs')?.textContent).toContain('No live runs.');
    expect(document.activeElement).toBe(q('.live-runs h2'));
  });
});

describe('Overview after a failed repo refetch', () => {
  it('Auto that succeeded shows on the switch with no error, though the refetch failed', async () => {
    h.handle = (method, url) => {
      if (method === 'PUT' && url === `${API}/afk/auto`) {
        const updated = baseRepo({ afk_auto_enabled: true });
        h.repo = null; // the follow-up GET answers 500
        return jsonResponse(200, updated);
      }
      return undefined;
    };
    await mountOverview();
    await waitFor(() => q('.afk-card'), 'afk card');

    q<HTMLButtonElement>('.afk-card button[role="switch"]')?.click();
    await settle();

    expect(q('.afk-card button[role="switch"]')?.getAttribute('aria-checked')).toBe('true');
    expect(q('.toast')?.textContent).toBe('Auto-spawn on for coding-lab');
    expect(container.textContent).not.toContain('Something went wrong');
    // The failure is reported in the frame; every card stays.
    expect(q('.repo-head .banner')?.textContent).toContain('repo lookup failed');
    expect(q('.readiness')).not.toBeNull();
    expect(q('.live-runs')).not.toBeNull();
    expect(q('.afk-card')).not.toBeNull();
    expect(q('.parked-card')).not.toBeNull();
  });
});

describe('Overview moving between repos', () => {
  const B = 'repo_b';
  const B_API = `/api/v1/repos/${B}`;
  const repoB = () =>
    baseRepo({
      id: B,
      name: 'other-lab',
      remote_url: 'git@github.com:x/other-lab.git',
      clone_status: 'cloning',
      summary: { claimable: null, open_issues: 0, readiness: { state: 'pending', checks: [] } },
    });

  /** Serves repo_b's reads; `held` POSTs for repo_1 wait for release(). */
  function serveB(held: Record<string, unknown> = {}): Record<string, () => void> {
    const releases: Record<string, () => void> = {};
    h.handle = (method, url) => {
      if (method === 'GET' && url === B_API) return jsonResponse(200, repoB());
      if (method === 'GET' && url === `${B_API}/readiness`) {
        return jsonResponse(200, {
          state: 'pending',
          checks: [{ id: 'clone', state: 'pending', detail: 'Cloning other-lab.' }],
        });
      }
      if (method === 'GET' && url === `${B_API}/parked`) return jsonResponse(200, { parked: [] });
      const key = `${method} ${url}`;
      if (key in held) {
        return new Promise((resolve) => {
          releases[key] = () => resolve(jsonResponse(200, held[key]));
        }) as unknown as ReturnType<typeof jsonResponse>;
      }
      return undefined;
    };
    return releases;
  }

  it('re-keys every block to the new repo and shows nothing of the old one', async () => {
    h.instances = [
      baseInstance({ id: 'run_a', title: 'Alpha run' }),
      baseInstance({ id: 'run_b', title: 'Beta run', repo_id: B }),
    ];
    h.parked = [
      { branch: 'lab/a-parked', worktree_path: '', dirty: false, commits_ahead: 1, unpushed: 0 },
    ];
    h.readiness = { state: 'failing', checks: [trackerFailing] };
    serveB();
    await mountOverview();
    await waitFor(() => q('.parked-entry'), 'parked');
    expect(container.textContent).toContain('Alpha run');
    const before = q('.readiness');

    routerHistory.set({ value: `/repos/${B}` });
    await settle();
    await waitFor(() => q('.readiness-check'), 'repo_b readiness');

    // Readiness is repo_b's (a fresh block), with repo_b's clone progress.
    expect(q('.readiness')).not.toBe(before);
    expect(q('.readiness-check .readiness-detail')?.textContent).toBe('Cloning other-lab.');
    emit('clone.progress', { repoID: REPO_ID, phase: 'receiving objects', percent: 90, line: '' });
    emit('clone.progress', { repoID: B, phase: 'resolving deltas', percent: 40, line: '' });
    await settle();
    expect(q('.readiness-progress [role="progressbar"]')?.getAttribute('aria-valuenow')).toBe('40');
    // Nothing of repo_1 in the repo home (the side rail lists every run):
    // its runs, its parked work, its failing tracker, its name.
    const home = q('main.repo-home')?.textContent ?? '';
    expect(home).toContain('other-lab');
    expect(home).not.toContain('Alpha run');
    expect(home).not.toContain('lab/a-parked');
    expect(home).not.toContain('forge token was rejected');
    expect(home).not.toContain('coding-lab');
  });

  it('shows the new repo’s live runs, AFK count and parked work once it is ready', async () => {
    h.instances = [
      baseInstance({ id: 'run_a', title: 'Alpha run' }),
      baseInstance({ id: 'run_b', title: 'Beta run', repo_id: B }),
    ];
    h.parked = [
      { branch: 'lab/a-parked', worktree_path: '', dirty: false, commits_ahead: 1, unpushed: 0 },
    ];
    serveB();
    const ready = baseRepo({
      id: B,
      name: 'other-lab',
      summary: { claimable: 9, open_issues: 2, readiness: { state: 'passing', checks: [] } },
    });
    const serve = h.handle!;
    h.handle = (method, url, init) => {
      if (method === 'GET' && url === B_API) return jsonResponse(200, ready);
      if (method === 'GET' && url === `${B_API}/readiness`) {
        return jsonResponse(200, ready.summary.readiness);
      }
      if (method === 'GET' && url === `${B_API}/parked`) {
        return jsonResponse(200, {
          parked: [
            {
              branch: 'lab/b-parked',
              worktree_path: '',
              dirty: true,
              commits_ahead: 0,
              unpushed: 0,
            },
          ],
        });
      }
      return serve(method, url, init);
    };
    await mountOverview();
    await waitFor(() => q('.parked-entry'), 'parked');
    expect(q('.afk-card-count')?.textContent).toBe('3 issues ready for an agent.');

    routerHistory.set({ value: `/repos/${B}` });
    await settle();
    await waitFor(() => q('.parked-entry'), 'repo_b parked');

    const home = () => q('main.repo-home')?.textContent ?? '';
    expect(
      Array.from(container.querySelectorAll('.live-run-title')).map((el) => el.textContent),
    ).toEqual(['Beta run']);
    expect(q('.afk-card-count')?.textContent).toBe('9 issues ready for an agent.');
    expect(q('.parked-branch')?.textContent).toBe('lab/b-parked');
    expect(home()).not.toContain('Alpha run');
    expect(home()).not.toContain('lab/a-parked');
  });

  it('drops a Run one answer for the old repo instead of toasting it on the new one', async () => {
    const releases = serveB({
      [`POST ${API}/afk/start`]: { run: { id: 'run_9', issue_number: 7 } },
    });
    await mountOverview();
    await waitFor(() => q('.afk-card'), 'afk card');

    q<HTMLButtonElement>('.afk-card-start')?.click();
    await settle();
    routerHistory.set({ value: `/repos/${B}` });
    await settle();
    await waitFor(() => q('.readiness-check'), 'repo_b readiness');

    releases[`POST ${API}/afk/start`]?.();
    await settle();
    expect(q('.toast')).toBeNull();
    expect(container.textContent).not.toContain('Something went wrong');
    // repo_b's view got nothing of it: no extra repo_b read was triggered.
    expect(h.requests.filter((r) => r === `GET ${B_API}`)).toHaveLength(1);
  });

  it('drops a Stop all answer for the old repo too', async () => {
    h.instances = [baseInstance({ id: 'a' })];
    const releases = serveB({ [`POST ${API}/stop-all`]: { stopped: 1 } });
    await mountOverview();
    await waitFor(() => q('.live-run'), 'live runs');

    buttonNamed('Stop all (1)')?.click();
    await settle();
    buttonNamed('Stop 1 run')?.click();
    await settle();
    routerHistory.set({ value: `/repos/${B}` });
    await settle();
    await waitFor(() => q('.readiness-check'), 'repo_b readiness');

    releases[`POST ${API}/stop-all`]?.();
    await settle();
    expect(q('.toast')).toBeNull();
    expect(container.textContent).not.toContain('Stopped 1 run');
  });
});
