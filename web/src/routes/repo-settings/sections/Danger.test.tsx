// Danger zone (issue #61): the delete dialog in its plain, forced and blocked
// forms. DangerZone is mounted on its own inside a MemoryRouter with its own
// fetch stub — it takes the repo as a prop, so it needs no settings page
// around it. Focus handling and Escape belong to the shared Dialog (its own
// suite); here the dialog's role and accessible name are asserted.

import { MemoryRouter, Route, createMemoryHistory, useLocation } from '@solidjs/router';
import type { MemoryHistory } from '@solidjs/router';
import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Instance, ParkedEntry, Repo, RepoImport } from '../../../api';
import DangerZone from './Danger';

const REPO_ID = 'repo_1';
const REPO_PATH = `/api/v1/repos/${REPO_ID}`;
const DANGER_PATH = `/repos/${REPO_ID}/settings/danger`;

function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}
type StubResponse = ReturnType<typeof jsonResponse>;

function instance(over: Partial<Instance>): Instance {
  return {
    id: 'run_x',
    repo_id: REPO_ID,
    repo_name: 'coding-lab',
    kind: 'manual',
    provider: 'agent-a',
    issue_number: null,
    pull_number: null,
    branch: 'lab/x',
    worktree_path: '/wt/x',
    session_name: 'coding-lab~x',
    title: null,
    model: 'model-a',
    effort: 'high',
    remote: false,
    deep_link_url: null,
    started_at: '2026-07-06T00:00:00Z',
    budget_deadline: null,
    ended_at: null,
    outcome: 'active',
    failure_reason: null,
    live: true,
    connecting: false,
    state: 'working',
    ...over,
  };
}

/** A complete, ready, forge-bound repo (inlined: no settings harness needed). */
function baseRepo(over: Partial<Repo> = {}): Repo {
  return {
    id: REPO_ID,
    name: 'coding-lab',
    remote_url: 'git@github.com:Cloonar/coding-lab.git',
    credential_id: null,
    forge_credential_id: null,
    tracker_binding: 'forge',
    forge_kind: 'github',
    default_branch: 'main',
    provider: null,
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
    created_at: '2026-07-06T00:00:00Z',
    last_opened_at: null,
    summary: { claimable: 3, open_issues: 12, readiness: { state: 'passing', checks: [] } },
    autoland_enabled: false,
    max_fix_attempts: 2,
    auto_merge: true,
    lander_provider: null,
    lander_model: null,
    lander_effort: null,
    runner: 'container',
    container_memory: null,
    container_pids: null,
    container_nofile: null,
    image_ref: null,
    ...over,
  };
}

const parked = (branch: string, worktree_path: string): ParkedEntry => ({
  branch,
  worktree_path,
  dirty: true,
  commits_ahead: 2,
  unpushed: 2,
});

/** Server state the stub answers from; each test shapes it. */
interface State {
  importers: RepoImport[] | number;
  instances: Instance[] | number;
  parked: ParkedEntry[] | number;
  schedules: number;
  secrets: number;
  /** Status codes the DELETEs answer, in order; 204 once they run out. */
  deleteStatuses: number[];
  deleteError: string;
  /** Called after a DELETE answered, before the next request (state changes "meanwhile"). */
  afterDelete?: () => void;
}
let s: State;
let requests: string[];
let deletes: string[];

const answer = (value: unknown[] | number, key: string): StubResponse =>
  typeof value === 'number'
    ? jsonResponse(value, { error: `${key} unavailable` })
    : jsonResponse(200, { [key]: value });

function stubFetch(): void {
  vi.stubGlobal('scrollTo', vi.fn());
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      requests.push(`${method} ${url}`);
      if (method === 'DELETE' && url.startsWith(REPO_PATH)) {
        deletes.push(url);
        const status = s.deleteStatuses.shift() ?? 204;
        s.afterDelete?.();
        return Promise.resolve(
          status === 204
            ? { ok: true, status, json: () => Promise.resolve({}), text: () => Promise.resolve('') }
            : jsonResponse(status, { error: s.deleteError }),
        );
      }
      if (method === 'GET') {
        switch (url) {
          case `${REPO_PATH}/importers`:
            return Promise.resolve(answer(s.importers, 'importers'));
          case '/api/v1/instances':
            return Promise.resolve(answer(s.instances, 'instances'));
          case `${REPO_PATH}/parked`:
            return Promise.resolve(answer(s.parked, 'parked'));
          case `${REPO_PATH}/schedules`:
            return Promise.resolve(
              jsonResponse(200, {
                schedules: Array.from({ length: s.schedules }, (_, i) => ({ id: `sch_${i}` })),
              }),
            );
          case `${REPO_PATH}/secrets`:
            return Promise.resolve(
              s.secrets < 0
                ? jsonResponse(-s.secrets, { error: 'secrets are not configured' })
                : jsonResponse(200, {
                    secrets: Array.from({ length: s.secrets }, (_, i) => ({ id: `sec_${i}` })),
                  }),
            );
        }
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flush();
}

let container: HTMLDivElement;
let dispose: (() => void) | undefined;
let history: MemoryHistory;
/** The router state the list was opened with after a delete. */
let listState: unknown;
const [repo, setRepo] = createSignal<Repo>(baseRepo());

function ListProbe() {
  const location = useLocation();
  listState = location.state;
  return <p class="list-probe">repositories</p>;
}

async function mountDanger(over: Partial<Repo> = {}): Promise<void> {
  setRepo(baseRepo(over));
  container = document.createElement('div');
  document.body.appendChild(container);
  history = createMemoryHistory();
  history.set({ value: DANGER_PATH });
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route path="/repos/:id/settings/danger" component={() => <DangerZone repo={repo} />} />
        <Route path="/repos" component={ListProbe} />
        <Route path="*" component={() => <p class="elsewhere">elsewhere</p>} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

/** Any button in the document (the dialog renders into the same container). */
function button(label: string): HTMLButtonElement | undefined {
  return Array.from(document.querySelectorAll<HTMLButtonElement>('button')).find(
    (b) => b.textContent?.trim() === label,
  );
}

function dialog(): HTMLElement | null {
  return document.querySelector<HTMLElement>('[role="alertdialog"]');
}

/** The dialog's accessible name: the text of the element aria-labelledby names. */
function dialogName(): string | null | undefined {
  const id = dialog()?.getAttribute('aria-labelledby');
  return id ? document.getElementById(id)?.textContent : null;
}

function consequences(): string[] {
  return Array.from(document.querySelectorAll('.delete-consequences li')).map(
    (li) => li.textContent ?? '',
  );
}

function nameInput(): HTMLInputElement | null {
  return document.querySelector<HTMLInputElement>('input[name="confirm_name"]');
}

function typeName(value: string): void {
  const el = nameInput();
  if (!el) throw new Error('missing the name input');
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

async function openDialog(): Promise<void> {
  button('Delete repository')!.click();
  await settle();
  if (!dialog()) throw new Error('the delete dialog did not open');
}

/** The dialog's Delete button (the section's trigger carries the same label). */
function deleteButton(): HTMLButtonElement | undefined {
  return Array.from(dialog()?.querySelectorAll<HTMLButtonElement>('button') ?? []).find(
    (b) => b.textContent?.trim() === 'Delete repository',
  );
}

const ALWAYS = "Removes lab's clone, this repository's settings and its run history.";

beforeEach(() => {
  s = {
    importers: [],
    instances: [],
    parked: [],
    schedules: 0,
    secrets: 0,
    deleteStatuses: [],
    deleteError: '',
  };
  requests = [];
  deletes = [];
  listState = undefined;
  stubFetch();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('Danger zone section', () => {
  it('says what deletion removes and opens no dialog until asked', async () => {
    await mountDanger();

    const card = container.querySelector('.danger-zone');
    expect(card?.textContent).toContain('Removes coding-lab from lab');
    expect(card?.textContent).toContain('The remote repository is not touched.');
    expect(button('Delete repository')).toBeDefined();
    expect(dialog()).toBeNull();
    expect(requests).toEqual([]);
  });

  it('never falls back to a browser confirm', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    await mountDanger();
    await openDialog();

    expect(confirmSpy).not.toHaveBeenCalled();
    confirmSpy.mockRestore();
  });
});

describe('delete dialog — consequences', () => {
  it('lists only the consequences that apply, with their counts', async () => {
    s.instances = [
      instance({ id: 'run_1' }),
      instance({ id: 'run_2', state: 'needs_input' }),
      instance({ id: 'run_3', live: false }), // ended: not stopped by the delete
      instance({ id: 'run_4', repo_id: 'repo_2', repo_name: 'website' }), // another repo
    ];
    s.parked = [parked('afk/57', '/wt/afk-57')];
    s.schedules = 3;
    s.secrets = 3;
    await mountDanger();
    await openDialog();

    expect(dialogName()).toBe('Delete coding-lab?');
    expect(dialog()?.getAttribute('aria-modal')).toBe('true');
    expect(consequences()).toEqual([
      'Stops 2 live instances.',
      "Deletes 1 parked branch with lab's clone; its worktree folder stays on disk.",
      'Deletes 3 Schedules and 3 secrets.',
      ALWAYS,
    ]);
    expect(dialog()?.textContent).toContain(
      'The remote on github.com is not touched. This cannot be undone.',
    );
  });

  it('a repo with nothing attached lists only the clone and settings line', async () => {
    await mountDanger();
    await openDialog();

    expect(consequences()).toEqual([ALWAYS]);
  });

  it('leaves out a count whose endpoint fails instead of showing 0, and still opens', async () => {
    s.instances = [instance({})];
    s.parked = 404; // optional on this server
    s.secrets = -501; // not configured
    s.schedules = 2;
    await mountDanger();
    await openDialog();

    expect(consequences()).toEqual(['Stops 1 live instance.', 'Deletes 2 Schedules.', ALWAYS]);
    expect(dialog()?.textContent).not.toMatch(/\b0 /);
    expect(nameInput()).not.toBeNull();
  });

  it('a running clone is listed and the builtin tracker issues are named', async () => {
    await mountDanger({ clone_status: 'cloning', tracker_binding: 'builtin' });
    await openDialog();

    expect(consequences()).toEqual([
      'Abandons the running clone.',
      "Deletes the issues and change requests kept in lab's built-in tracker.",
      ALWAYS,
    ]);
  });
});

describe('delete dialog — confirming', () => {
  it('keeps Delete disabled until the typed name matches exactly', async () => {
    await mountDanger();
    await openDialog();

    const input = nameInput()!;
    const label = document.querySelector(`label[for="${input.id}"]`);
    expect(label?.textContent).toBe('Type coding-lab to confirm');
    expect(deleteButton()?.disabled).toBe(true);

    for (const wrong of ['coding-la', 'Coding-lab', ' coding-lab', 'coding-lab-x']) {
      typeName(wrong);
      expect(deleteButton()?.disabled).toBe(true);
    }
    typeName('coding-lab');
    expect(deleteButton()?.disabled).toBe(false);
  });

  it('the plain delete sends no force and returns to the list with a notice', async () => {
    s.schedules = 1;
    await mountDanger();
    await openDialog();
    typeName('coding-lab');
    deleteButton()!.click();
    await settle();

    expect(deletes).toEqual([REPO_PATH]);
    expect(history.get()).toBe('/repos');
    expect(listState).toEqual({ notice: 'Deleted coding-lab from lab' });
  });

  it('with live instances one confirmation sends the forced delete', async () => {
    s.instances = [instance({})];
    await mountDanger();
    await openDialog();
    typeName('coding-lab');
    deleteButton()!.click();
    await settle();

    expect(deletes).toEqual([`${REPO_PATH}?force=true`]);
    expect(history.get()).toBe('/repos');
  });

  it('with a running clone one confirmation sends the forced delete', async () => {
    await mountDanger({ clone_status: 'cloning' });
    await openDialog();
    typeName('coding-lab');
    deleteButton()!.click();
    await settle();

    expect(deletes).toEqual([`${REPO_PATH}?force=true`]);
  });

  it('Enter in the name field confirms once the name matches', async () => {
    await mountDanger();
    await openDialog();
    typeName('coding-lab');
    nameInput()!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();

    expect(deletes).toEqual([REPO_PATH]);
  });

  it('a 409 on the plain delete shows the reason and refreshes, never forcing on its own', async () => {
    s.deleteStatuses = [409];
    s.deleteError = 'repo has live instances; retry with force';
    // A run started after the dialog loaded.
    s.afterDelete = () => {
      s.instances = [instance({})];
    };
    await mountDanger();
    await openDialog();
    expect(consequences()).toEqual([ALWAYS]);
    typeName('coding-lab');
    deleteButton()!.click();
    await settle();

    expect(deletes).toEqual([REPO_PATH]); // one plain attempt, no automatic retry
    expect(dialog()?.querySelector('.banner.error')?.textContent).toContain(
      'repo has live instances; retry with force',
    );
    expect(consequences()).toEqual(['Stops 1 live instance.', ALWAYS]);
    expect(history.get()).toBe(DANGER_PATH);

    // The operator has now read it: the next confirmation forces.
    s.afterDelete = undefined;
    deleteButton()!.click();
    await settle();
    expect(deletes).toEqual([REPO_PATH, `${REPO_PATH}?force=true`]);
    expect(history.get()).toBe('/repos');
  });

  it('a refusal shows the server message in the dialog and keeps it open', async () => {
    s.deleteStatuses = [500];
    s.deleteError = 'removing the clone failed';
    await mountDanger();
    await openDialog();
    typeName('coding-lab');
    deleteButton()!.click();
    await settle();

    expect(dialog()?.textContent).toContain('removing the clone failed');
    expect(deleteButton()?.disabled).toBe(false);
    expect(history.get()).toBe(DANGER_PATH);
  });

  it('Cancel closes the dialog without a request', async () => {
    await mountDanger();
    await openDialog();
    typeName('coding-lab');
    button('Cancel')!.click();
    await settle();

    expect(dialog()).toBeNull();
    expect(deletes).toEqual([]);
  });
});

describe('delete dialog — blocked by importers', () => {
  it('names the importers, offers no way to delete and links to the first one', async () => {
    s.importers = [
      { id: 'repo_2', name: 'website' },
      { id: 'repo_3', name: 'docs-site' },
    ];
    s.instances = [instance({})];
    await mountDanger();
    await openDialog();

    expect(dialogName()).toBe('coding-lab cannot be deleted yet');
    expect(dialog()?.textContent).toContain(
      'website and docs-site import this repository. Remove those imports first.',
    );
    expect(nameInput()).toBeNull();
    expect(deleteButton()).toBeUndefined();
    expect(document.querySelector('.delete-consequences')).toBeNull();

    const link = dialog()?.querySelector<HTMLAnchorElement>('a.link-button');
    expect(link?.textContent).toBe('Open website imports');
    expect(link?.getAttribute('href')).toBe('/repos/repo_2/settings/imports');

    button('Close')!.click();
    await settle();
    expect(dialog()).toBeNull();
    expect(deletes).toEqual([]);
  });

  it('one importer reads in the singular, and its link opens its Imports section', async () => {
    s.importers = [{ id: 'repo_2', name: 'website' }];
    await mountDanger();
    await openDialog();

    expect(dialog()?.textContent).toContain(
      'website imports this repository. Remove that import first.',
    );
    dialog()!.querySelector<HTMLAnchorElement>('a.link-button')!.click();
    await settle();
    expect(history.get()).toBe('/repos/repo_2/settings/imports');
    expect(dialog()).toBeNull();
  });

  it('a failed importers lookup falls back to the normal dialog', async () => {
    s.importers = 500;
    await mountDanger();
    await openDialog();

    expect(dialogName()).toBe('Delete coding-lab?');
    expect(nameInput()).not.toBeNull();
  });

  it('an importer declared meanwhile turns the refusal into the blocked dialog', async () => {
    s.deleteStatuses = [409];
    s.deleteError = 'repository is imported by website; remove those imports first';
    s.afterDelete = () => {
      s.importers = [{ id: 'repo_2', name: 'website' }];
    };
    await mountDanger();
    await openDialog();
    typeName('coding-lab');
    deleteButton()!.click();
    await settle();

    expect(dialogName()).toBe('coding-lab cannot be deleted yet');
    expect(dialog()?.textContent).toContain('repository is imported by website');
    expect(deleteButton()).toBeUndefined();
  });
});
