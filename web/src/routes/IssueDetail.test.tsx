// IssueDetail behavioral contract:
// - title and body are editable on every binding through the Edit affordance,
//   which sends one PATCH carrying only the changed fields ({title} and/or
//   {body}, never state) — the server routes it through the tracker seam;
// - state, labels and comments stay builtin-only: the add-comment box, the
//   open/close toggle and the label editor appear on builtin repos and hide on
//   forge-bound repos behind the managed-on-the-forge note (a deep link when
//   the remote parses);
// - the comment form POSTs {body}; the state toggle PATCHes {state}; the
//   label editor PUTs the replaced set built with the toggle algebra;
// - it renders inside the repo home frame's Issues tab (issue #61) with an
//   "Issues / #N" trail; the frame names the repo;
// - body and comments render markdown (issue #103) with task lists on, on both
//   bindings; `#N` links per binding (builtin → in-app route, forge → forge
//   issue page in a new tab, plain text when no forge URL derives); the body
//   card's Copy markdown copies the raw body; both forms share the Write /
//   Preview control; a comment opening with an `Agent Brief` heading is tagged.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ForgeKind, IssueDetail as Issue, Label, TrackerBinding } from '../api';
import App from '../App';
import RepoRoutes from './repo-home/routes';

const REPO_ID = 'repo_1';
const NUMBER = 7;

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

const LABELS: Label[] = [
  { id: 'lbl_1', name: 'bug', color: '#d73a4a', description: '' },
  { id: 'lbl_2', name: 'ui', color: '#1d76db', description: '' },
];

let binding: TrackerBinding;
let forgeKind: ForgeKind;
let remoteUrl: string;
let repoFails: boolean;
let issueOnServer: Issue;
let commentBodies: Record<string, unknown>[];
let labelPuts: Record<string, unknown>[];
let patches: Record<string, unknown>[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function baseIssue(): Issue {
  return {
    number: NUMBER,
    title: 'Fix login',
    body: 'Steps to reproduce…',
    state: 'open',
    labels: ['bug'],
    comments: [{ author: 'operator', body: 'first', created_at: '2026-07-06T01:00:00.000Z' }],
    created_at: '2026-07-06T00:00:00.000Z',
    updated_at: '2026-07-06T01:00:00.000Z',
  };
}

function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
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
      if (url === '/api/v1/auth/state' && method === 'GET') {
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
        );
      }
      if (url === `/api/v1/repos/${REPO_ID}` && method === 'GET') {
        if (repoFails) {
          return Promise.resolve(jsonResponse(500, { error: 'repo lookup failed' }));
        }
        return Promise.resolve(
          jsonResponse(200, {
            id: REPO_ID,
            name: 'coding-lab',
            tracker_binding: binding,
            forge_kind: forgeKind,
            remote_url: remoteUrl,
          }),
        );
      }
      if (url === `/api/v1/repos/${REPO_ID}/issues/${NUMBER}` && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...issueOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/issues/${NUMBER}` && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as Record<string, unknown>;
        patches.push(patch);
        issueOnServer = { ...issueOnServer, ...patch };
        return Promise.resolve(jsonResponse(200, { ...issueOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/issues/${NUMBER}/comments` && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as { body: string };
        commentBodies.push(body);
        const comment = {
          author: 'operator',
          body: body.body,
          created_at: '2026-07-06T02:00:00.000Z',
        };
        issueOnServer = { ...issueOnServer, comments: [...issueOnServer.comments, comment] };
        return Promise.resolve(jsonResponse(201, comment));
      }
      if (url === `/api/v1/repos/${REPO_ID}/issues/${NUMBER}/labels` && method === 'PUT') {
        const body = JSON.parse(String(init?.body)) as { labels: string[] };
        labelPuts.push(body);
        issueOnServer = { ...issueOnServer, labels: body.labels };
        return Promise.resolve(jsonResponse(200, { ...issueOnServer }));
      }
      if (url === `/api/v1/repos/${REPO_ID}/labels` && method === 'GET') {
        if (binding !== 'builtin') {
          return Promise.reject(new Error('labels endpoint hit for a forge repo'));
        }
        return Promise.resolve(jsonResponse(200, { labels: LABELS }));
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mountDetail(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: `/repos/${REPO_ID}/issues/${NUMBER}` });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <RepoRoutes />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function buttonByText(text: string): HTMLButtonElement | null {
  const buttons = Array.from(container.querySelectorAll('button'));
  return buttons.find((b) => b.textContent?.trim() === text) ?? null;
}

function mustButton(text: string): HTMLButtonElement {
  const el = buttonByText(text);
  if (!el) throw new Error(`missing button ${text}`);
  return el;
}

function stubClipboard(): ReturnType<typeof vi.fn> {
  const writeText = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
  return writeText;
}

function tab(form: string, name: string): HTMLButtonElement {
  const el = Array.from(
    container.querySelectorAll<HTMLButtonElement>(`${form} [role="tablist"] [role="tab"]`),
  ).find((b) => b.textContent?.trim() === name);
  if (!el) throw new Error(`missing ${name} tab in ${form}`);
  return el;
}

const RICH_BODY = [
  '## Summary',
  '',
  'Some **bold** words.',
  '',
  '```ts',
  'const x = 1;',
  '```',
  '',
  '| a | b |',
  '| - | - |',
  '| 1 | 2 |',
  '',
  '- [x] done item',
  '- [ ] open item',
].join('\n');

function expectRichBody(): void {
  const body = container.querySelector('section.card > .issue-md');
  if (!body) throw new Error('missing rendered body');
  expect(body.querySelector('h2.md-h')?.textContent).toBe('Summary');
  expect(body.querySelector('strong')?.textContent).toBe('bold');
  expect(body.querySelector('.md-codeblock .md-codeblock-lang')?.textContent).toBe('ts');
  expect(body.querySelector('.md-codeblock .copy-btn')).not.toBeNull();
  expect(body.querySelector('.md-codeblock pre')?.textContent).toBe('const x = 1;');
  expect(body.querySelector('.md-table-wrap table.md-table')).not.toBeNull();
  const boxes = Array.from(
    body.querySelectorAll<HTMLInputElement>('ul.md-task li.md-task-item input[type="checkbox"]'),
  );
  expect(boxes.map((b) => [b.checked, b.disabled])).toEqual([
    [true, true],
    [false, true],
  ]);
  expect(body.querySelector('li.md-task-item.done')?.textContent).toContain('done item');
  // No raw markdown syntax leaks into the rendered body.
  expect(body.textContent).not.toContain('##');
  expect(body.textContent).not.toContain('**');
}

function setValue(selector: string, value: string): void {
  const el = container.querySelector<HTMLInputElement | HTMLTextAreaElement>(selector);
  if (!el) throw new Error(`missing field ${selector}`);
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

beforeEach(() => {
  binding = 'builtin';
  forgeKind = 'forgejo';
  remoteUrl = 'git@git.cloonar.com:Cloonar/coding-lab.git';
  repoFails = false;
  issueOnServer = baseIssue();
  commentBodies = [];
  labelPuts = [];
  patches = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
});

describe('IssueDetail (builtin repo)', () => {
  it('renders the trail within the Issues tab (Issues / #N), leaf inert', async () => {
    await mountDetail();

    // The repo segment moved into the frame (issue #61): its header names the
    // repo and the Issues tab stays current on an issue page.
    expect(container.querySelector('.repo-head h1')?.textContent).toBe('coding-lab');
    expect(
      container
        .querySelector(`nav.repo-tabs a[href="/repos/${REPO_ID}/issues"]`)
        ?.getAttribute('aria-current'),
    ).toBe('page');
    const crumb = container.querySelector('p.crumb');
    expect(crumb?.textContent).toBe('Issues / #7');
    // "Issues" links back to the issues list.
    expect(crumb?.querySelectorAll(`a[href="/repos/${REPO_ID}/issues"]`)).toHaveLength(1);
    // The #7 leaf is the current page: inert text, not a link.
    expect(crumb?.querySelector(`a[href="/repos/${REPO_ID}/issues/${NUMBER}"]`)).toBeNull();
  });

  it('renders body, labels and comments with the mutation controls', async () => {
    await mountDetail();

    expect(container.textContent).toContain('#7');
    expect(container.textContent).toContain('Fix login');
    expect(container.textContent).toContain('Steps to reproduce…');
    expect(container.textContent).toContain('first');
    expect(container.querySelector('textarea[name="comment"]')).not.toBeNull();
    expect(buttonByText('Close issue')).not.toBeNull();
    expect(buttonByText('Edit labels')).not.toBeNull();
    // Title/body edit is available on builtin too.
    expect(buttonByText('Edit')).not.toBeNull();
    expect(container.textContent).not.toContain('managed on the forge');
  });

  it('POSTs the comment body and shows the refreshed thread', async () => {
    await mountDetail();

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[name="comment"]');
    if (!textarea) throw new Error('missing comment textarea');
    textarea.value = 'looks good';
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    const form = container.querySelector('form.comment-form');
    if (!form) throw new Error('missing comment form');
    form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
    await settle();

    expect(commentBodies).toEqual([{ body: 'looks good' }]);
    expect(container.textContent).toContain('looks good');
    expect(container.textContent).toContain('Comments (2)');
  });

  it('PATCHes the state on Close issue', async () => {
    await mountDetail();

    mustButton('Close issue').click();
    await settle();

    expect(patches).toEqual([{ state: 'closed' }]);
    expect(buttonByText('Reopen issue')).not.toBeNull();
  });

  it('PUTs the replaced label set from the editor', async () => {
    await mountDetail();

    mustButton('Edit labels').click();
    await settle();

    // Save is disabled until the selection differs from the issue's labels.
    expect(mustButton('Save labels').disabled).toBe(true);

    // Toggle 'ui' on: picker chips are the repo's labels (bug, ui).
    const chips = Array.from(container.querySelectorAll<HTMLButtonElement>('button.chip-toggle'));
    const ui = chips.find((b) => b.textContent?.includes('ui'));
    if (!ui) throw new Error('missing ui picker chip');
    ui.click();
    await settle();

    expect(mustButton('Save labels').disabled).toBe(false);
    mustButton('Save labels').click();
    await settle();

    expect(labelPuts).toEqual([{ labels: ['bug', 'ui'] }]);
  });

  it('PATCHes the changed title and body from the editor', async () => {
    await mountDetail();

    mustButton('Edit').click();
    await settle();

    // Seeded with the current values; Save disabled until something differs.
    expect(container.querySelector<HTMLInputElement>('input[name="issue-title"]')?.value).toBe(
      'Fix login',
    );
    expect(container.querySelector<HTMLTextAreaElement>('textarea[name="issue-body"]')?.value).toBe(
      'Steps to reproduce…',
    );
    expect(mustButton('Save').disabled).toBe(true);

    setValue('input[name="issue-title"]', 'Fix login flow');
    // An empty body is a legal change (clears it).
    setValue('textarea[name="issue-body"]', '');
    await settle();

    mustButton('Save').click();
    await settle();

    expect(patches).toEqual([{ title: 'Fix login flow', body: '' }]);
    expect(container.textContent).toContain('Fix login flow');
    expect(container.textContent).toContain('No description.');
  });
});

describe('IssueDetail (repo fetch fails)', () => {
  beforeEach(() => {
    repoFails = true;
  });

  it('keeps the loaded issue readable and surfaces the repo error', async () => {
    await mountDetail();

    // The successfully loaded issue must not be blanked by the repo failure.
    expect(container.textContent).toContain('#7');
    expect(container.textContent).toContain('Fix login');
    expect(container.textContent).toContain('Steps to reproduce…');
    expect(container.textContent).toContain('first');
    // The failure is surfaced in the frame's banner…
    const banner = container.querySelector('.repo-head .banner.error');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain('repo lookup failed');
    // …and the view degrades to read-only: the binding is unknown, so no
    // mutation controls and no misleading forge note.
    expect(container.querySelector('textarea[name="comment"]')).toBeNull();
    expect(buttonByText('Close issue')).toBeNull();
    expect(buttonByText('Edit labels')).toBeNull();
    // The binding is unknown, so even the title/body Edit affordance is hidden.
    expect(buttonByText('Edit')).toBeNull();
    expect(container.textContent).not.toContain('managed on the forge');
  });
});

describe('IssueDetail (forge repo)', () => {
  beforeEach(() => {
    binding = 'forge';
  });

  it('hides comment box, state toggle and label editor behind the forge note', async () => {
    await mountDetail();

    expect(container.textContent).toContain('managed on the forge');
    expect(container.querySelector('textarea[name="comment"]')).toBeNull();
    expect(buttonByText('Close issue')).toBeNull();
    expect(buttonByText('Edit labels')).toBeNull();
    // The read view keeps working.
    expect(container.textContent).toContain('Steps to reproduce…');
    expect(container.textContent).toContain('first');
  });

  it('exposes the title/body Edit affordance and PATCHes both fields at once', async () => {
    await mountDetail();

    // The Edit affordance renders even though the builtin controls do not.
    expect(buttonByText('Edit')).not.toBeNull();
    expect(container.querySelector('textarea[name="comment"]')).toBeNull();
    expect(buttonByText('Close issue')).toBeNull();
    expect(buttonByText('Edit labels')).toBeNull();

    mustButton('Edit').click();
    await settle();

    setValue('input[name="issue-title"]', 'Fix login flow');
    setValue('textarea[name="issue-body"]', 'Reworded steps');
    await settle();

    mustButton('Save').click();
    await settle();

    // Exactly one PATCH, carrying both changed fields and never state.
    expect(patches).toEqual([{ title: 'Fix login flow', body: 'Reworded steps' }]);
    // The reworded forge note still renders and still deep-links.
    expect(container.textContent).toContain('managed on the forge');
    const link = container.querySelector<HTMLAnchorElement>('.forge-note a');
    expect(link?.getAttribute('href')).toBe(
      `https://git.cloonar.com/Cloonar/coding-lab/issues/${NUMBER}`,
    );
  });

  it('sends only the changed field when the body is untouched', async () => {
    await mountDetail();

    mustButton('Edit').click();
    await settle();

    setValue('input[name="issue-title"]', 'Fix login flow');
    await settle();

    mustButton('Save').click();
    await settle();

    // No body key — only the title travelled.
    expect(patches).toEqual([{ title: 'Fix login flow' }]);
  });

  it('disables Save for a whitespace title or when nothing changed', async () => {
    await mountDetail();

    mustButton('Edit').click();
    await settle();

    // Nothing differs yet.
    expect(mustButton('Save').disabled).toBe(true);

    // Whitespace-only title stays blocked.
    setValue('input[name="issue-title"]', '   ');
    await settle();
    expect(mustButton('Save').disabled).toBe(true);

    // A real edit enables it…
    setValue('input[name="issue-title"]', 'Fix login flow');
    await settle();
    expect(mustButton('Save').disabled).toBe(false);

    // …and reverting back to the original disables it again.
    setValue('input[name="issue-title"]', 'Fix login');
    await settle();
    expect(mustButton('Save').disabled).toBe(true);
  });

  it('deep-links the forge note to this exact issue on the forge', async () => {
    await mountDetail();

    const link = container.querySelector<HTMLAnchorElement>('.forge-note a');
    expect(link).not.toBeNull();
    expect(link?.getAttribute('href')).toBe(
      `https://git.cloonar.com/Cloonar/coding-lab/issues/${NUMBER}`,
    );
    expect(link?.getAttribute('target')).toBe('_blank');
    expect(link?.getAttribute('rel')).toBe('noreferrer');
    expect(link?.textContent).toContain('managed on the forge');
  });

  it('keeps the note plain text when forge_kind is none (no forge URL)', async () => {
    forgeKind = 'none';
    await mountDetail();

    expect(container.textContent).toContain('managed on the forge');
    expect(container.querySelector('.forge-note a')).toBeNull();
  });
});

describe('IssueDetail markdown (issue #103)', () => {
  it('renders the body as markdown on a builtin repo', async () => {
    issueOnServer = { ...baseIssue(), body: RICH_BODY };
    await mountDetail();
    expectRichBody();
  });

  it('renders the body as markdown on a forge repo', async () => {
    binding = 'forge';
    issueOnServer = { ...baseIssue(), body: RICH_BODY };
    await mountDetail();
    expectRichBody();
  });

  it('renders every comment as markdown', async () => {
    issueOnServer = {
      ...baseIssue(),
      comments: [
        { author: 'a', body: 'one **strong**', created_at: '2026-07-06T01:00:00.000Z' },
        { author: 'b', body: '- [ ] todo\n- [x] done', created_at: '2026-07-06T02:00:00.000Z' },
      ],
    };
    await mountDetail();

    const bodies = container.querySelectorAll('.comment .issue-md.comment-body');
    expect(bodies).toHaveLength(2);
    expect(bodies[0]?.querySelector('p.md-p strong')?.textContent).toBe('strong');
    const boxes = Array.from(bodies[1]?.querySelectorAll<HTMLInputElement>('input') ?? []);
    expect(boxes.map((b) => b.checked)).toEqual([false, true]);
    // Author and time stay as before.
    expect(container.querySelector('.comment-meta')?.textContent).toMatch(/^a · /);
  });

  it('keeps the "No description." fallback for an empty body', async () => {
    issueOnServer = { ...baseIssue(), body: '' };
    await mountDetail();

    const fallback = container.querySelector('p.muted.issue-body');
    expect(fallback?.textContent).toBe('No description.');
    expect(container.querySelector('section.card > .issue-md')).toBeNull();
    // Nothing to copy from an empty body.
    expect(buttonByText('Copy markdown')).toBeNull();
  });

  it('links #N in-app on a builtin repo; foo#N and code stay literal', async () => {
    issueOnServer = { ...baseIssue(), body: 'see #5, not foo#5 or `#5`' };
    await mountDetail();

    const refs = container.querySelectorAll<HTMLAnchorElement>('section.card > .issue-md a.md-ref');
    expect(refs).toHaveLength(1);
    expect(refs[0]?.getAttribute('href')).toBe(`/repos/${REPO_ID}/issues/5`);
    expect(refs[0]?.getAttribute('target')).toBeNull();
    expect(refs[0]?.textContent).toBe('#5');
    const body = container.querySelector('section.card > .issue-md');
    expect(body?.textContent).toBe('see #5, not foo#5 or #5');
    expect(body?.querySelector('code.md-code')?.textContent).toBe('#5');
  });

  it('links #N to the forge issue in a new tab on a forge repo', async () => {
    binding = 'forge';
    issueOnServer = { ...baseIssue(), body: 'see #5' };
    await mountDetail();

    const ref = container.querySelector<HTMLAnchorElement>('section.card > .issue-md a.md-ref');
    expect(ref?.getAttribute('href')).toBe('https://git.cloonar.com/Cloonar/coding-lab/issues/5');
    expect(ref?.getAttribute('target')).toBe('_blank');
    expect(ref?.getAttribute('rel')).toBe('noopener noreferrer');
  });

  it('leaves #N as plain text on a forge repo with an unparseable remote', async () => {
    binding = 'forge';
    remoteUrl = '/srv/git/coding-lab.git';
    issueOnServer = { ...baseIssue(), body: 'see #5' };
    await mountDetail();

    const body = container.querySelector('section.card > .issue-md');
    expect(body?.querySelector('a')).toBeNull();
    expect(body?.textContent).toBe('see #5');
  });

  it('leaves #N as plain text on a forge repo with forge_kind none', async () => {
    binding = 'forge';
    forgeKind = 'none';
    issueOnServer = { ...baseIssue(), body: 'see #5' };
    await mountDetail();

    const body = container.querySelector('section.card > .issue-md');
    expect(body?.querySelector('a')).toBeNull();
    expect(body?.textContent).toBe('see #5');
  });

  it('tags comments whose first block is an Agent Brief heading', async () => {
    issueOnServer = {
      ...baseIssue(),
      comments: [
        { author: 'a', body: '## Agent Brief\n\nDo it.', created_at: '2026-07-06T01:00:00.000Z' },
        { author: 'b', body: '# agent brief', created_at: '2026-07-06T02:00:00.000Z' },
        { author: 'c', body: 'intro\n\n## Agent Brief', created_at: '2026-07-06T03:00:00.000Z' },
        { author: 'd', body: '## Agent Briefing', created_at: '2026-07-06T04:00:00.000Z' },
        { author: 'e', body: 'plain', created_at: '2026-07-06T05:00:00.000Z' },
      ],
    };
    await mountDetail();

    const tagged = Array.from(container.querySelectorAll('.comment')).map(
      (li) => li.querySelector('.comment-meta .brief-tag')?.textContent ?? null,
    );
    expect(tagged).toEqual(['Agent brief', 'Agent brief', null, null, null]);
  });

  it('Copy markdown copies the raw body verbatim', async () => {
    const writeText = stubClipboard();
    issueOnServer = { ...baseIssue(), body: RICH_BODY };
    await mountDetail();

    const btn = container.querySelector<HTMLButtonElement>(
      '.card .body-foot button[aria-label="Copy markdown"]',
    );
    if (!btn) throw new Error('missing Copy markdown button');
    expect(btn.textContent).toContain('Copy markdown');
    btn.click();
    await settle();
    expect(writeText).toHaveBeenCalledWith(RICH_BODY);
  });

  it('editor: Write / Preview renders the draft and keeps it; Save PATCHes only the body', async () => {
    await mountDetail();

    mustButton('Edit').click();
    await settle();

    const write = tab('.issue-editor', 'Write');
    const preview = tab('.issue-editor', 'Preview');
    // Opens on Write.
    expect(write.getAttribute('aria-selected')).toBe('true');
    expect(preview.getAttribute('aria-selected')).toBe('false');
    expect(container.querySelector('.issue-editor .preview-pane')?.hasAttribute('hidden')).toBe(
      true,
    );
    // The textarea keeps an accessible name.
    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[name="issue-body"]');
    const labelId = textarea?.getAttribute('aria-labelledby') ?? '';
    expect(document.getElementById(labelId)?.textContent).toBe('Description');

    setValue('textarea[name="issue-body"]', '## Draft\n\nsee #5');
    await settle();
    preview.click();
    await settle();

    const pane = container.querySelector('.issue-editor .preview-pane');
    expect(pane?.hasAttribute('hidden')).toBe(false);
    expect(preview.getAttribute('aria-selected')).toBe('true');
    expect(pane?.querySelector('h2.md-h')?.textContent).toBe('Draft');
    expect(pane?.querySelector('a.md-ref')?.getAttribute('href')).toBe(
      `/repos/${REPO_ID}/issues/5`,
    );

    // An empty (whitespace) draft previews as "Nothing to preview".
    tab('.issue-editor', 'Write').click();
    setValue('textarea[name="issue-body"]', '   ');
    tab('.issue-editor', 'Preview').click();
    await settle();
    expect(pane?.textContent).toBe('Nothing to preview');

    // Back to Write: the draft is intact.
    tab('.issue-editor', 'Write').click();
    setValue('textarea[name="issue-body"]', 'Reworded steps');
    tab('.issue-editor', 'Preview').click();
    await settle();
    tab('.issue-editor', 'Write').click();
    await settle();
    expect(container.querySelector<HTMLTextAreaElement>('textarea[name="issue-body"]')?.value).toBe(
      'Reworded steps',
    );

    mustButton('Save').click();
    await settle();
    expect(patches).toEqual([{ body: 'Reworded steps' }]);
  });

  it('comment form: Preview renders the draft; posting still sends {body} and returns to Write', async () => {
    await mountDetail();

    const textarea = container.querySelector<HTMLTextAreaElement>('textarea[name="comment"]');
    const labelId = textarea?.getAttribute('aria-labelledby') ?? '';
    expect(document.getElementById(labelId)?.textContent).toBe('Add a comment');

    tab('.comment-form', 'Preview').click();
    await settle();
    expect(container.querySelector('.comment-form .preview-pane')?.textContent).toBe(
      'Nothing to preview',
    );

    tab('.comment-form', 'Write').click();
    setValue('textarea[name="comment"]', '  **ship** it  ');
    tab('.comment-form', 'Preview').click();
    await settle();
    expect(
      container.querySelector('.comment-form .preview-pane .issue-md strong')?.textContent,
    ).toBe('ship');

    const form = container.querySelector('form.comment-form');
    if (!form) throw new Error('missing comment form');
    form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
    await settle();

    expect(commentBodies).toEqual([{ body: '**ship** it' }]);
    expect(tab('.comment-form', 'Write').getAttribute('aria-selected')).toBe('true');
    expect(container.querySelector<HTMLTextAreaElement>('textarea[name="comment"]')?.value).toBe(
      '',
    );
    // The posted comment renders as markdown in the thread.
    const last = Array.from(container.querySelectorAll('.comment .comment-body')).at(-1);
    expect(last?.querySelector('strong')?.textContent).toBe('ship');
  });
});
