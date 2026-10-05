// Repositories list contract (issue #61), through the real route tree:
// - rows: name (+ incogni chip), remote as host plus path, live runs with the
//   waiting count, the claimable count (a dash when unknown, never 0), the AFK
//   state (paused included), Autoland and the last run (none for a repo that
//   never had one); a cloning row shows its progress; failed clone and
//   not-ready repos say so in words;
// - order: latest run first, never-run repos after, newest first;
// - the filter narrows by name and by remote, hides Needs you, and says so
//   when nothing matches;
// - Needs you: one entry per failed clone (Retry), three-strikes pause
//   (Reset) and failing readiness check (Fix → the field), absent when empty;
// - phone rows below 1024px, a six-column table from 1024px; the whole row
//   opens the repo home; a route notice shows once in the toast.

import { Route, useNavigate } from '@solidjs/router';
import { describe, expect, it } from 'vitest';
import type { ReadinessCheck, Repo } from '../api';
import { noticeState } from '../lib/routeNotice';
import {
  baseInstance,
  baseRepo,
  container,
  emit,
  h,
  installRepoHomeHooks,
  jsonResponse,
  mountRepoHome,
  routerHistory,
  setDesktop,
  settle,
  waitFor,
} from './repo-home/harness';

installRepoHomeHooks();

const MIN = 60_000;
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

function repo(id: string, over: Partial<Repo> = {}): Repo {
  return baseRepo({
    id,
    name: id,
    remote_url: `git@github.com:example/${id}.git`,
    last_opened_at: null,
    ...over,
  });
}

function summary(claimable: number | null, checks: ReadinessCheck[] = []): Repo['summary'] {
  return {
    claimable,
    open_issues: 1,
    readiness: {
      state: checks.some((c) => c.state === 'failing') ? 'failing' : 'passing',
      checks,
    },
  };
}

const trackerFailing: ReadinessCheck = {
  id: 'tracker',
  state: 'failing',
  detail: 'The forge token was rejected. AFK runs cannot read issues.',
  fix: { scope: 'repo', section: 'integrations', field: 'forge_credential_id' },
};

/** A sample of the mockup's states. */
function sample(): Repo[] {
  return [
    repo('website', {
      last_opened_at: ago(30 * 60 * MIN),
      consecutive_failures: 3,
      summary: summary(5),
      autoland_enabled: true,
    }),
    repo('coding-lab', {
      remote_url: 'git@github.com:Cloonar/coding-lab.git',
      last_opened_at: ago(4 * MIN),
      afk_auto_enabled: true,
      autoland_enabled: true,
      summary: summary(3),
    }),
    repo('mobile-app', {
      last_opened_at: ago(12 * MIN),
      clone_status: 'cloning',
      summary: summary(null),
    }),
    repo('billing-api', {
      remote_url: 'https://git.example.com/acme/billing-api.git',
      incogni: true,
      last_opened_at: ago(5 * 60 * MIN),
      summary: summary(null),
    }),
    repo('auth-service', {
      remote_url: 'https://git.example.com/acme/auth-service.git',
      last_opened_at: ago(40 * 60 * MIN),
      summary: summary(2, [trackerFailing]),
    }),
    repo('infra-docs', {
      last_opened_at: ago(3 * 24 * 60 * MIN),
      clone_status: 'error',
      clone_error: 'the remote rejected the git credential.',
      summary: summary(null, [
        { id: 'clone', state: 'failing', detail: 'The last clone failed.', action: 'retry_clone' },
      ]),
    }),
    repo('fresh', { created_at: '2026-09-01T00:00:00Z', summary: summary(0) }),
    repo('older-fresh', { created_at: '2026-08-01T00:00:00Z', summary: summary(0) }),
  ];
}

const q = <T extends Element = HTMLElement>(selector: string): T | null =>
  container.querySelector<T>(selector);

/** An element's visible text: without its visually hidden parts. */
function visibleText(el: Element | null | undefined): string | undefined {
  if (!el) return undefined;
  const clone = el.cloneNode(true) as HTMLElement;
  clone.querySelectorAll('.visually-hidden').forEach((hidden) => hidden.remove());
  return (clone.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function buttonNamed(name: string): HTMLButtonElement | undefined {
  return Array.from(container.querySelectorAll('button')).find((b) => visibleText(b) === name);
}

/** The accessible name of a control without aria-label: its whole text. */
const textName = (el: Element | null | undefined) =>
  el?.getAttribute('aria-label') ?? (el?.textContent ?? '').replace(/\s+/g, ' ').trim();

const rowNames = () =>
  Array.from(container.querySelectorAll('.repo-row .repo-row-link')).map((a) => a.textContent);

function phoneRow(name: string): HTMLElement {
  const link = Array.from(container.querySelectorAll('li.repo-row .repo-row-link')).find(
    (a) => a.textContent === name,
  );
  const row = link?.closest<HTMLElement>('li.repo-row');
  if (!row) throw new Error(`no row ${name}`);
  return row;
}

/** The visible status line, chips and words, without the visually hidden text. */
function statusParts(row: HTMLElement): string[] {
  const line = row.querySelector('.repo-row-status');
  if (!line) return [];
  const clone = line.cloneNode(true) as HTMLElement;
  clone.querySelectorAll('.visually-hidden').forEach((el) => el.remove());
  return Array.from(clone.querySelectorAll(':scope > *')).map((el) =>
    (el.textContent ?? '').replace(/\s+/g, ' ').trim(),
  );
}

async function mountList(): Promise<void> {
  await mountRepoHome('/repos');
  await waitFor(() => q('.repos-head h1'), 'list heading');
  await settle();
}

function typeFilter(value: string): void {
  const input = q<HTMLInputElement>('.repos-filter input');
  if (!input) throw new Error('no filter');
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
}

describe('repositories list rows (phone)', () => {
  it('shows live runs, the waiting count, the claimable count and the AFK state', async () => {
    h.repos = sample();
    h.instances = [
      baseInstance({ id: 'a', repo_id: 'coding-lab', state: 'working' }),
      baseInstance({ id: 'b', repo_id: 'coding-lab', state: 'needs_input' }),
      baseInstance({ id: 'c', repo_id: 'coding-lab', state: 'needs_input', live: false }),
    ];
    await mountList();

    // No table below 1024px.
    expect(q('table')).toBeNull();
    const row = phoneRow('coding-lab');
    expect(row.querySelector('.repo-row-remote')?.textContent).toBe(
      'github.com/Cloonar/coding-lab',
    );
    expect(statusParts(row)).toEqual(['2 live 1 waiting', '3 ready', 'Auto on']);
    expect(statusParts(phoneRow('website'))).toEqual(['No live runs', '5 ready', 'AFK paused']);
    // Unknown claimable count: a dash, never 0.
    expect(statusParts(phoneRow('billing-api'))).toEqual(['No live runs', '– ready', 'Auto off']);
    expect(phoneRow('billing-api').querySelector('.chip.incogni')?.textContent).toBe('incogni');
    // A failing readiness check is named in words.
    expect(statusParts(phoneRow('auth-service'))).toEqual([
      'No live runs',
      '2 ready',
      'Auto off Not ready',
    ]);
    expect(statusParts(phoneRow('infra-docs'))).toEqual(['Clone failed']);
  });

  it('updates the waiting count as a run’s state changes', async () => {
    h.repos = [repo('coding-lab', { last_opened_at: ago(MIN) })];
    h.instances = [baseInstance({ id: 'a', repo_id: 'coding-lab', state: 'working' })];
    await mountList();
    expect(statusParts(phoneRow('coding-lab'))[0]).toBe('1 live');

    const row = phoneRow('coding-lab');
    emit('run.messages.changed', { runID: 'a', state: 'question' });
    await settle();
    expect(statusParts(phoneRow('coding-lab'))[0]).toBe('1 live 1 waiting');
    // Updated in place: the row (and a keyboard user's focus in it) survives.
    expect(phoneRow('coding-lab')).toBe(row);
  });

  it('shows a cloning repo’s progress, “Cloning” until the percent is known', async () => {
    h.repos = sample();
    await mountList();
    expect(statusParts(phoneRow('mobile-app'))).toEqual(['Cloning']);

    const row = phoneRow('mobile-app');
    emit('clone.progress', { repoID: 'mobile-app', phase: 'receiving objects', percent: 62 });
    await settle();
    expect(statusParts(phoneRow('mobile-app'))).toEqual(['Cloning 62%']);
    expect(phoneRow('mobile-app')).toBe(row);
  });

  it('orders latest run first, then never-run repos newest first', async () => {
    h.repos = sample();
    await mountList();
    expect(rowNames()).toEqual([
      'coding-lab',
      'mobile-app',
      'billing-api',
      'website',
      'auth-service',
      'infra-docs',
      'fresh',
      'older-fresh',
    ]);
    expect(q('#repos-list-heading')?.textContent).toBe('All repositories (8)');
  });

  it('opens the repo home from anywhere on the row, and from its link', async () => {
    h.repos = sample();
    await mountList();

    phoneRow('website').querySelector<HTMLElement>('.repo-row-remote')?.click();
    await settle();
    expect(routerHistory.get()).toBe('/repos/website');
  });

  it('links each name to its repo home', async () => {
    h.repos = sample();
    await mountList();
    expect(phoneRow('coding-lab').querySelector('.repo-row-link')?.getAttribute('href')).toBe(
      '/repos/coding-lab',
    );
  });
});

describe('repositories list filter', () => {
  it('narrows by name and by remote, case-insensitively', async () => {
    h.repos = sample();
    await mountList();

    typeFilter('CODING');
    await settle();
    expect(rowNames()).toEqual(['coding-lab']);
    expect(q('#repos-list-heading')?.textContent).toBe('Matches (1)');

    typeFilter('git.example.com/acme');
    await settle();
    expect(rowNames()).toEqual(['billing-api', 'auth-service']);
  });

  it('hides Needs you while a filter is active', async () => {
    h.repos = sample();
    await mountList();
    expect(q('section.needs-you')).not.toBeNull();

    typeFilter('web');
    await settle();
    expect(q('section.needs-you')).toBeNull();

    // Clearing brings it back.
    q<HTMLButtonElement>('.repos-filter-clear')?.click();
    await settle();
    expect(q<HTMLInputElement>('.repos-filter input')?.value).toBe('');
    expect(q('section.needs-you')).not.toBeNull();
  });

  it('says so when nothing matches', async () => {
    h.repos = sample();
    await mountList();

    typeFilter('zzz');
    await settle();
    expect(rowNames()).toEqual([]);
    expect(q('.repos-all .empty')?.textContent).toBe('No repository matches that filter.');
    expect(q('#repos-list-heading')?.textContent).toBe('Matches (0)');
  });
});

describe('Needs you', () => {
  const entries = () =>
    Array.from(container.querySelectorAll<HTMLElement>('.needs-you-item')).map((li) => ({
      name: li.querySelector('.needs-you-name')?.textContent,
      message: li.querySelector('.needs-you-message')?.lastChild?.textContent,
      action: visibleText(li.querySelector('.needs-you-action')),
      warning: li.classList.contains('warning'),
    }));

  it('lists each cause once, in list order, with its action', async () => {
    h.repos = sample();
    await mountList();

    expect(q('#needs-you-heading')?.textContent).toBe('Needs you (3)');
    expect(entries()).toEqual([
      {
        name: 'website',
        message: 'AFK paused after 3 failed runs. 5 issues waiting.',
        action: 'Reset',
        warning: true,
      },
      {
        name: 'auth-service',
        message: 'The forge token was rejected. AFK runs cannot read issues.',
        action: 'Fix',
        warning: false,
      },
      {
        name: 'infra-docs',
        message: 'Clone failed: the remote rejected the git credential.',
        action: 'Retry',
        warning: false,
      },
    ]);
    // Severity in words for assistive tech, not colour alone.
    expect(
      Array.from(container.querySelectorAll('.needs-you-message .visually-hidden')).map(
        (el) => el.textContent,
      ),
    ).toEqual(['Warning: ', 'Problem: ', 'Problem: ']);
  });

  it('is absent when nothing qualifies', async () => {
    h.repos = [repo('a', { last_opened_at: ago(MIN) }), repo('b', { clone_status: 'cloning' })];
    await mountList();
    expect(q('section.needs-you')).toBeNull();
  });

  it('Retry restarts the clone, confirms in a toast and refreshes the list', async () => {
    h.repos = sample();
    const posts: string[] = [];
    h.handle = (method, url) => {
      if (method === 'POST' && url === '/api/v1/repos/infra-docs/clone/retry') {
        posts.push(url);
        h.repos = sample().map((r) =>
          r.id === 'infra-docs' ? { ...r, clone_status: 'cloning', summary: summary(null) } : r,
        );
        return jsonResponse(202, {});
      }
      return undefined;
    };
    await mountList();
    const repoListGets = h.requests.filter((r) => r === 'GET /api/v1/repos').length;

    buttonNamed('Retry')?.click();
    await settle();

    expect(posts).toEqual(['/api/v1/repos/infra-docs/clone/retry']);
    expect(q('.toast')?.textContent).toBe('Retrying the clone of infra-docs');
    expect(h.requests.filter((r) => r === 'GET /api/v1/repos').length).toBe(repoListGets + 1);
    expect(entries().map((e) => e.name)).toEqual(['website', 'auth-service']);
  });

  it('Reset lifts the pause and confirms in a toast', async () => {
    h.repos = sample();
    h.handle = (method, url) => {
      if (method === 'POST' && url === '/api/v1/repos/website/afk/reset') {
        h.repos = sample().map((r) => (r.id === 'website' ? { ...r, consecutive_failures: 0 } : r));
        return jsonResponse(200, {});
      }
      return undefined;
    };
    await mountList();

    buttonNamed('Reset')?.click();
    await settle();

    expect(h.requests).toContain('POST /api/v1/repos/website/afk/reset');
    expect(q('.toast')?.textContent).toBe('AFK runs resumed for website');
    expect(entries().map((e) => e.name)).toEqual(['auth-service', 'infra-docs']);
  });

  const fixes: Array<[string, ReadinessCheck['fix'], string]> = [
    [
      'a repo field',
      { scope: 'repo', section: 'integrations', field: 'forge_credential_id' },
      '/repos/auth-service/settings/integrations?field=forge_credential_id',
    ],
    ['a global setting', { scope: 'global', section: 'runner' }, '/settings/runner'],
    ['the agent login', { scope: 'credentials' }, '/credentials'],
  ];
  for (const [what, fix, url] of fixes) {
    it(`Fix opens ${what}`, async () => {
      h.repos = [
        repo('auth-service', {
          last_opened_at: ago(MIN),
          summary: summary(2, [{ ...trackerFailing, fix }]),
        }),
      ];
      await mountList();

      const button = buttonNamed('Fix');
      expect(button?.getAttribute('aria-label')).toBe('Fix tracker in auth-service');
      button?.click();
      await settle();
      expect(routerHistory.get()).toBe(url);
    });
  }
});

describe('repositories list layouts', () => {
  const headers = () =>
    Array.from(container.querySelectorAll('table.repo-table thead th')).map((th) => [
      th.textContent,
      th.getAttribute('scope'),
    ]);

  function tableRow(name: string): string[] {
    const link = Array.from(container.querySelectorAll('tr.repo-row .repo-row-link')).find(
      (a) => a.textContent === name,
    );
    const tr = link?.closest('tr');
    if (!tr) throw new Error(`no table row ${name}`);
    return Array.from(tr.children).map((cell) => {
      const clone = cell.cloneNode(true) as HTMLElement;
      clone.querySelectorAll('.visually-hidden').forEach((el) => el.remove());
      return (clone.textContent ?? '').replace(/\s+/g, ' ').trim();
    });
  }

  it('renders a six-column table from 1024px', async () => {
    setDesktop(true);
    h.repos = sample();
    h.instances = [
      baseInstance({ id: 'a', repo_id: 'coding-lab', state: 'working' }),
      baseInstance({ id: 'b', repo_id: 'coding-lab', state: 'question' }),
    ];
    await mountList();

    expect(q('ul.repo-list')).toBeNull();
    expect(headers()).toEqual([
      ['Repository', 'col'],
      ['Runs', 'col'],
      ['Ready', 'col'],
      ['AFK', 'col'],
      ['Autoland', 'col'],
      ['Last run', 'col'],
    ]);
    // The repository cell heads its row.
    expect(q('tbody tr th')?.getAttribute('scope')).toBe('row');

    expect(tableRow('coding-lab')).toEqual([
      'coding-labgithub.com/Cloonar/coding-lab',
      '2 live 1 waiting',
      '3',
      'Auto on',
      'On',
      '4 min ago',
    ]);
    expect(tableRow('website')).toEqual([
      'websitegithub.com/example/website',
      'None',
      '5',
      'Paused',
      'On',
      'yesterday',
    ]);
    expect(tableRow('mobile-app')).toEqual([
      'mobile-appgithub.com/example/mobile-app',
      'Cloning',
      '–',
      '–',
      '–',
      '12 min ago',
    ]);
    expect(tableRow('billing-api')).toEqual([
      'billing-apiincognigit.example.com/acme/billing-api',
      'None',
      '–',
      'Off',
      'Off',
      '5 h ago',
    ]);
    expect(tableRow('auth-service')[3]).toBe('Off Not ready');
    expect(tableRow('infra-docs').slice(1, 5)).toEqual(['Clone failed', '–', '–', '–']);
    // Never had a run: no time.
    expect(tableRow('fresh')[5]).toBe('');
  });

  it('opens the repo home from a click anywhere on a table row', async () => {
    setDesktop(true);
    h.repos = sample();
    await mountList();

    const link = Array.from(container.querySelectorAll('tr.repo-row .repo-row-link')).find(
      (a) => a.textContent === 'billing-api',
    );
    link?.closest('tr')?.querySelector<HTMLElement>('td:nth-of-type(4)')?.click();
    await settle();
    expect(routerHistory.get()).toBe('/repos/billing-api');
  });

  it('switches layout as the viewport crosses 1024px', async () => {
    h.repos = sample();
    setDesktop(false);
    await mountList();
    expect(q('ul.repo-list')).not.toBeNull();

    setDesktop(true);
    await settle();
    expect(q('ul.repo-list')).toBeNull();
    expect(q('table.repo-table')).not.toBeNull();
  });
});

describe('repositories list chrome', () => {
  it('offers Add, and an obvious way to add the first repository', async () => {
    h.repos = [];
    await mountList();

    expect(q<HTMLAnchorElement>('a.repos-add')?.getAttribute('href')).toBe('/repos/new');
    expect(q('a.repos-add')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('Add repository');
    expect(q('.empty')?.textContent).toContain('No repositories yet');
    expect(q<HTMLAnchorElement>('.empty a')?.getAttribute('href')).toBe('/repos/new');
    // No filter, no Needs you, no list without repos.
    expect(q('.repos-filter')).toBeNull();
    expect(q('section.needs-you')).toBeNull();
  });

  it('leaves Stop all, parked work and the Issues/CRs/Settings links to the repo home', async () => {
    h.repos = sample();
    h.instances = [baseInstance({ id: 'a', repo_id: 'coding-lab' })];
    await mountList();

    expect(container.textContent).not.toContain('Stop all');
    expect(container.textContent).not.toContain('Parked');
    const hrefs = Array.from(container.querySelectorAll('main a')).map((a) =>
      a.getAttribute('href'),
    );
    expect(hrefs.some((href) => /\/(issues|crs|settings)$/.test(href ?? ''))).toBe(false);
    // And nothing per repo: one list call, no forge reads.
    expect(h.requests.filter((r) => r.startsWith('GET /api/v1/repos/'))).toEqual([]);
  });

  it('shows a route notice once in the toast', async () => {
    h.repos = sample();
    function Launcher() {
      const navigate = useNavigate();
      return (
        <button
          type="button"
          onClick={() => navigate('/repos', { state: noticeState('Deleted coding-lab from lab') })}
        >
          Launch
        </button>
      );
    }
    await mountRepoHome('/launch', () => <Route path="/launch" component={Launcher} />);
    const launch = await waitFor(() => buttonNamed('Launch'), 'launcher');
    launch.click();
    await settle();

    expect(routerHistory.get()).toBe('/repos');
    const toast = await waitFor(() => q('.toast'), 'toast');
    expect(toast.textContent).toBe('Deleted coding-lab from lab');
    expect(container.querySelectorAll('.toast')).toHaveLength(1);
  });
});

describe('repositories list identity across refetches', () => {
  it('keeps every row node, and the focus in one, through a repo.changed for another repo', async () => {
    h.repos = sample();
    await mountList();
    const nodes = Array.from(container.querySelectorAll('li.repo-row'));
    const link = phoneRow('website').querySelector<HTMLAnchorElement>('.repo-row-link')!;
    link.focus();
    expect(document.activeElement).toBe(link);
    const gets = h.requests.filter((r) => r === 'GET /api/v1/repos').length;

    // Another repo changed: a fresh response, every object new.
    h.repos = sample().map((r) =>
      r.id === 'coding-lab' ? { ...r, summary: summary(7), afk_auto_enabled: false } : r,
    );
    emit('repo.changed', { repoID: 'coding-lab' });
    await settle();

    expect(h.requests.filter((r) => r === 'GET /api/v1/repos').length).toBe(gets + 1);
    // The change landed in place…
    expect(statusParts(phoneRow('coding-lab'))).toEqual(['No live runs', '7 ready', 'Auto off']);
    // …and no row was rebuilt: same nodes, focus still on the link.
    expect(Array.from(container.querySelectorAll('li.repo-row'))).toEqual(nodes);
    expect(document.activeElement).toBe(link);
  });

  it('keeps the table rows too', async () => {
    setDesktop(true);
    h.repos = sample();
    await mountList();
    const nodes = Array.from(container.querySelectorAll('tr.repo-row'));
    const link = container.querySelector<HTMLAnchorElement>('tr.repo-row .repo-row-link')!;
    link.focus();

    h.repos = sample().map((r) => (r.id === 'website' ? { ...r, autoland_enabled: false } : r));
    emit('repo.changed', { repoID: 'website' });
    await settle();

    expect(Array.from(container.querySelectorAll('tr.repo-row'))).toEqual(nodes);
    expect(document.activeElement).toBe(link);
  });

  it('keeps the Needs you entries through a refetch', async () => {
    h.repos = sample();
    await mountList();
    const items = Array.from(container.querySelectorAll('.needs-you-item'));
    const fix = buttonNamed('Fix')!;
    fix.focus();

    emit('repo.changed', { repoID: 'coding-lab' });
    await settle();

    expect(Array.from(container.querySelectorAll('.needs-you-item'))).toEqual(items);
    expect(document.activeElement).toBe(fix);
  });

  it('keeps the list it had when a refetch fails, with the error above it', async () => {
    h.repos = sample();
    await mountList();
    h.handle = (method, url) =>
      method === 'GET' && url === '/api/v1/repos'
        ? jsonResponse(500, { error: 'the store is locked' })
        : undefined;
    emit('repo.changed', { repoID: 'website' });
    await settle();

    expect(container.querySelector('.banner')?.textContent).toContain('the store is locked');
    expect(rowNames()).toHaveLength(8);
  });
});

describe('repositories list refreshes', () => {
  const listGets = () => h.requests.filter((r) => r === 'GET /api/v1/repos').length;
  const waitDebounce = () => new Promise<void>((resolve) => setTimeout(resolve, 300));

  const triggers: Array<[string, Record<string, unknown>]> = [
    ['issue.changed', { repoID: 'coding-lab' }],
    ['run.changed', { repoID: 'coding-lab', runID: 'run_1' }],
    ['parked.changed', { repoID: 'coding-lab' }],
    ['provider.auth.changed', { provider: 'agent-a' }],
  ];
  for (const [type, payload] of triggers) {
    it(`re-reads the list once on a burst of ${type}`, async () => {
      h.repos = sample();
      await mountList();
      const before = listGets();

      emit(type, payload);
      emit(type, payload);
      emit(type, payload);
      await settle();
      expect(listGets()).toBe(before); // still inside the debounce window
      await waitDebounce();
      await settle();

      expect(listGets()).toBe(before + 1);
      // Lab's own list only: no per-repo read, nothing that reaches a forge.
      expect(h.requests.filter((r) => r.startsWith('GET /api/v1/repos/'))).toEqual([]);
    });
  }

  it('moves the Ready count and the order when a run starts', async () => {
    h.repos = sample();
    await mountList();
    expect(rowNames()[0]).toBe('coding-lab');

    h.repos = sample().map((r) =>
      r.id === 'website' ? { ...r, last_opened_at: ago(0), summary: summary(4) } : r,
    );
    emit('run.changed', { repoID: 'website', runID: 'run_9' });
    await waitDebounce();
    await settle();

    expect(rowNames()[0]).toBe('website');
    expect(statusParts(phoneRow('website'))[1]).toBe('4 ready');
  });
});

describe('Needs you actions', () => {
  /** A fetch answer the test releases by hand. */
  function deferred(): { promise: Promise<unknown>; release: () => void } {
    let release = () => {};
    const promise = new Promise<unknown>((resolve) => {
      release = () => resolve(jsonResponse(202, {}));
    });
    return { promise, release };
  }

  it('sends nothing on a second Retry while the first is pending, across a refetch', async () => {
    h.repos = sample();
    const answer = deferred();
    const posts: string[] = [];
    h.handle = (method, url) => {
      if (method === 'POST' && url === '/api/v1/repos/infra-docs/clone/retry') {
        posts.push(url);
        return answer.promise as unknown as ReturnType<typeof jsonResponse>;
      }
      return undefined;
    };
    await mountList();

    const retry = buttonNamed('Retry')!;
    retry.focus();
    retry.click();
    await settle();
    expect(posts).toHaveLength(1);

    // Busy: worded and announced, still focusable, and not sent again.
    const busy = buttonNamed('Retrying…')!;
    expect(busy).toBe(retry);
    expect(busy.getAttribute('aria-disabled')).toBe('true');
    expect(busy.disabled).toBe(false);
    expect(textName(busy)).toBe('Retrying the clone of infra-docs…');
    expect(q('.needs-you [role="status"]')?.textContent).toBe('Retrying the clone of infra-docs…');
    busy.click();
    await settle();
    expect(posts).toHaveLength(1);

    // A refetch mid-request (another repo changed) keeps the busy state.
    emit('repo.changed', { repoID: 'website' });
    await settle();
    expect(buttonNamed('Retrying…')).toBe(retry);
    buttonNamed('Retrying…')?.click();
    await settle();
    expect(posts).toHaveLength(1);

    // Done: the entry leaves the block, and focus moves to its heading.
    h.repos = sample().map((r) =>
      r.id === 'infra-docs' ? { ...r, clone_status: 'cloning', summary: summary(null) } : r,
    );
    answer.release();
    await settle();
    expect(q('.toast')?.textContent).toBe('Retrying the clone of infra-docs');
    expect(
      container.querySelector('.needs-you-item .needs-you-name[href="/repos/infra-docs"]'),
    ).toBeNull();
    expect(document.activeElement).toBe(q('#needs-you-heading'));
    expect(q('.needs-you [role="status"]')?.textContent).toBe('');
  });

  it('moves focus to the list heading when Reset empties the block', async () => {
    h.repos = [repo('website', { last_opened_at: ago(MIN), consecutive_failures: 3 })];
    h.handle = (method, url) => {
      if (method === 'POST' && url === '/api/v1/repos/website/afk/reset') {
        h.repos = [repo('website', { last_opened_at: ago(MIN) })];
        return jsonResponse(200, {});
      }
      return undefined;
    };
    await mountList();
    const reset = buttonNamed('Reset')!;
    expect(textName(reset)).toBe('Reset AFK in website');
    reset.focus();
    reset.click();
    await settle();

    expect(q('section.needs-you')).toBeNull();
    expect(document.activeElement).toBe(q('#repos-list-heading'));
  });

  it('ties each action to its problem and names the fallback link with the repo', async () => {
    h.repos = [
      repo('auth-service', {
        last_opened_at: ago(MIN),
        summary: summary(2, [{ ...trackerFailing, fix: undefined }]),
      }),
    ];
    await mountList();
    const open = q<HTMLAnchorElement>('a.needs-you-action')!;
    expect(visibleText(open)).toBe('Open');
    expect(textName(open)).toBe('Open auth-service');
    const described = open.getAttribute('aria-describedby');
    expect(document.getElementById(described ?? '')?.textContent).toContain(
      'The forge token was rejected.',
    );
  });
});

describe('repositories list row clicks', () => {
  function clickRow(row: HTMLElement, init: MouseEventInit = {}): void {
    row
      .querySelector<HTMLElement>('.repo-row-remote')
      ?.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, ...init }));
  }

  it('leaves modified clicks to the browser', async () => {
    h.repos = sample();
    await mountList();
    for (const init of [{ ctrlKey: true }, { metaKey: true }, { shiftKey: true }]) {
      clickRow(phoneRow('website'), init);
      await settle();
      expect(routerHistory.get()).toBe('/repos');
    }
    clickRow(phoneRow('website'));
    await settle();
    expect(routerHistory.get()).toBe('/repos/website');
  });

  it('does not follow a click that ends a text selection in the row', async () => {
    h.repos = sample();
    await mountList();
    const remote = phoneRow('website').querySelector('.repo-row-remote')!;
    const range = document.createRange();
    range.selectNodeContents(remote);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);

    clickRow(phoneRow('website'));
    await settle();
    expect(routerHistory.get()).toBe('/repos');
    window.getSelection()?.removeAllRanges();
  });
});
