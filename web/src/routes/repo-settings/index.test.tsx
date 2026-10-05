// The one-page repo settings (issue #61): all ten sections render together,
// in the order and under the headings of REPO_SETTINGS_CATEGORIES; every
// section slug of issue #198 still deep-links — to the page scrolled to that
// section; `?field=` scrolls a field into view and focuses its control; and
// the page renders as the Settings tab of the repo home frame.
//
// jsdom has no layout, so "scrolled to" is asserted through the page's layout
// seam: the harness records every scroll the page asks for in `h.scrolls`.

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  container,
  fieldWrapper,
  h,
  installRepoSettingsHooks,
  mountSettings,
  pageSection,
  routerHistory,
  settle,
  unmount,
  waitFor,
} from './harness';
import { REPO_SETTINGS_CATEGORIES } from './categories';

installRepoSettingsHooks();

const BASE = `/repos/${REPO_ID}/settings`;

const buttonByText = (text: string): HTMLButtonElement | null =>
  Array.from(container.querySelectorAll('button')).find((b) => b.textContent?.trim() === text) ??
  null;

const waitForPage = () => waitFor(() => container.querySelector('#settings-danger'), 'the page');

/**
 * The page arrived at `target` (a section's element id, or a field's PATCH
 * key): it scrolled there — at once, not animated, because this is an arrival
 * and not a jump inside the page — and nowhere else. It may go there more
 * than once: a deep link is re-applied as late content lands above it.
 */
function expectArrivedAt(target: string): void {
  expect(h.scrolls.length).toBeGreaterThan(0);
  expect(new Set(h.scrolls.map((scroll) => scroll.target))).toEqual(new Set([target]));
  expect(h.scrolls.every((scroll) => !scroll.smooth)).toBe(true);
}

// One DOM probe per section: an element only that section renders.
const probes: Record<string, () => Element | null> = {
  agents: () => container.querySelector('button[name="afk_provider_default"]'),
  runner: () => container.querySelector('button[name="runner"]'),
  autoland: () => container.querySelector('button[name="autoland_enabled"]'),
  schedules: () => buttonByText('+ Add schedule'),
  secrets: () => buttonByText('+ Add secret'),
  imports: () => buttonByText('+ Add import'),
  general: () => container.querySelector('input[name="name"]'),
  integrations: () => container.querySelector('button[name="tracker_binding"]'),
  branches: () => container.querySelector('input[name="default_branch"]'),
  danger: () => buttonByText('Delete repository'),
};

describe('repo settings on one page', () => {
  it('probes cover exactly the declared sections', () => {
    expect(Object.keys(probes).sort()).toEqual(REPO_SETTINGS_CATEGORIES.map((c) => c.slug).sort());
  });

  it('renders all ten sections in order: Runs, Automation, Access, Setup, then Danger zone', async () => {
    await mountSettings(BASE);
    await waitForPage();

    const sections = Array.from(container.querySelectorAll('section.settings-section'));
    expect(sections.map((section) => section.id)).toEqual([
      'settings-agents',
      'settings-runner',
      'settings-autoland',
      'settings-schedules',
      'settings-secrets',
      'settings-imports',
      'settings-general',
      'settings-integrations',
      'settings-branches',
      'settings-danger',
    ]);
    expect(
      sections.map((section) => section.querySelector('.settings-section-head h2')?.textContent),
    ).toEqual([
      'Agents',
      'Runner',
      'Autoland',
      'Schedules',
      'Secrets',
      'Imports',
      'General',
      'Integrations',
      'Branches',
      'Danger zone',
    ]);
    expect(REPO_SETTINGS_CATEGORIES.map((c) => c.group)).toEqual([
      'Runs',
      'Runs',
      'Automation',
      'Automation',
      'Access',
      'Access',
      'Setup',
      'Setup',
      'Setup',
      null,
    ]);

    // Every section renders its own content inside its own <section>, named
    // by its heading and described in one line.
    for (const category of REPO_SETTINGS_CATEGORIES) {
      const section = pageSection(category.slug);
      await waitFor(() => probes[category.slug]?.() ?? null, `${category.slug} content`);
      expect(section.contains(probes[category.slug]?.() ?? null)).toBe(true);
      expect(section.getAttribute('aria-labelledby')).toBe(`settings-${category.slug}-title`);
      expect(section.querySelector('.settings-section-head p')?.textContent).toBe(
        category.description,
      );
    }
    // Danger zone is pinned last and marked as such.
    expect(sections.at(-1)?.classList.contains('danger')).toBe(true);
    expect(sections.slice(0, -1).every((s) => !s.classList.contains('danger'))).toBe(true);
  });

  it('tags exactly the sections whose rows act at once', async () => {
    await mountSettings(BASE);
    await waitForPage();

    const tagged = REPO_SETTINGS_CATEGORIES.filter(
      (c) => pageSection(c.slug).querySelector('.settings-tag') !== null,
    ).map((c) => c.slug);
    expect(tagged).toEqual(['schedules', 'secrets', 'imports']);
    expect(pageSection('schedules').querySelector('.settings-tag')?.textContent).toBe(
      'applies immediately',
    );
  });

  it('groups the Agents fields as Runs you start, AFK runs and AFK capacity', async () => {
    await mountSettings(BASE);
    await waitForPage();
    await waitFor(() => container.querySelector('input[name="afk_options.ultracode"]'), 'options');

    const groups = Array.from(pageSection('agents').querySelectorAll('[role="group"]')).filter(
      (group) => group.querySelector(':scope > h3') !== null,
    );
    expect(groups.map((group) => group.querySelector('h3')?.textContent)).toEqual([
      'Runs you start',
      'AFK runs',
      'AFK capacity',
    ]);
    const names = (group: Element | undefined) =>
      Array.from(group?.querySelectorAll('[data-field]') ?? []).map((el) =>
        el.getAttribute('data-field'),
      );
    expect(names(groups[0])).toEqual([
      'provider',
      'model_default',
      'effort_default',
      'remote_default',
    ]);
    // The option bag, the seed prompt (with Customize and the done-signal
    // hint) stay — inside AFK runs.
    expect(names(groups[1])).toEqual([
      'afk_provider_default',
      'afk_model_default',
      'afk_effort_default',
      'afk_remote_default',
      'afk_options',
      'afk_prompt',
    ]);
    expect(groups[1]?.contains(buttonByText('Customize'))).toBe(true);
    expect(fieldWrapper('afk_prompt').textContent).toContain(
      'The run is detected as done only by an open PR on its branch',
    );
    expect(names(groups[2])).toEqual([
      'afk_auto_enabled',
      'budget_minutes',
      'max_instances_override',
    ]);
  });

  it('has no Save button and no banner of its own in any section', async () => {
    await mountSettings(BASE);
    await waitForPage();

    expect(buttonByText('Save changes')).toBeNull();
    expect(container.querySelector('.settings-page .banner.success')).toBeNull();
    // The six form sections are plain cards, not forms.
    for (const slug of ['agents', 'runner', 'autoland', 'general', 'integrations', 'branches']) {
      expect(pageSection(slug).querySelector('form')).toBeNull();
    }
  });
});

describe('repo settings deep links', () => {
  for (const category of REPO_SETTINGS_CATEGORIES) {
    it(`${BASE}/${category.slug} opens the page scrolled to ${category.title}`, async () => {
      await mountSettings(`${BASE}/${category.slug}`);
      await waitFor(() => probes[category.slug]?.() ?? null, `${category.slug} content`);
      await settle();

      // The whole page rendered, and it scrolled to that section.
      expect(container.querySelectorAll('section.settings-section')).toHaveLength(10);
      expectArrivedAt(`settings-${category.slug}`);
      expect(routerHistory.get()).toBe(`${BASE}/${category.slug}`);
    });
  }

  it('the bare settings URL stays at the top of the page', async () => {
    await mountSettings(BASE);
    await waitForPage();
    await settle();

    expect(h.scrolls).toEqual([]);
    expect(routerHistory.get()).toBe(BASE);
  });

  it('an unknown slug falls back to the top of the page', async () => {
    await mountSettings(`${BASE}/nonsense`);
    await waitForPage();
    await settle();

    expect(container.querySelectorAll('section.settings-section')).toHaveLength(10);
    expect(h.scrolls).toEqual([]);
  });

  it('the schedule editor URLs reach the page and open it at Schedules', async () => {
    for (const path of [`${BASE}/schedules/new`, `${BASE}/schedules/sched_1`]) {
      h.scrolls = [];
      await mountSettings(path);
      await waitFor(() => buttonByText('+ Add schedule'), `schedules section at ${path}`);
      await settle();

      expect(routerHistory.get()).toBe(path); // no redirect away from the editor URL
      expectArrivedAt('settings-schedules');
      unmount();
    }
  });
});

describe('repo settings ?field= (the readiness Fix links)', () => {
  it('scrolls the field into view and focuses its control', async () => {
    await mountSettings(`${BASE}/integrations?field=forge_credential_id`);
    await waitForPage();
    await settle();

    expectArrivedAt('forge_credential_id');
    const control = container.querySelector('select[name="forge_credential_id"]');
    expect(control).not.toBeNull();
    expect(document.activeElement).toBe(control);
    // The field is outlined for a moment, so the eye finds it too.
    expect(fieldWrapper('forge_credential_id').classList.contains('flash')).toBe(true);
  });

  it('focuses whatever kind of control the field has', async () => {
    const cases: Array<[field: string, control: string]> = [
      ['image_ref', 'input[name="image_ref"]'],
      ['afk_model_default', 'button[name="afk_model_default"]'],
      ['afk_auto_enabled', 'button[role="switch"][name="afk_auto_enabled"]'],
      ['tracker_binding', 'button[role="radio"][name="tracker_binding"][aria-checked="true"]'],
      ['afk_prompt', 'textarea[name="afk_prompt"]'],
      ['afk_options', 'input[name="afk_options.ultracode"]'],
    ];
    for (const [field, selector] of cases) {
      h.scrolls = [];
      await mountSettings(`${BASE}?field=${field}`);
      await waitFor(() => container.querySelector(selector), `${field} control`);
      await settle();

      // The option bag only exists once the provider catalog has loaded:
      // until then the page waits at the field's section.
      expect(h.scrolls.at(-1)).toMatchObject({ target: field, smooth: false });
      expect(document.activeElement).toBe(container.querySelector(selector));
      unmount();
    }
  });

  it("trusts the field's own section over a mismatched :section", async () => {
    await mountSettings(`${BASE}/agents?field=default_branch`);
    await waitForPage();
    await settle();

    expect(h.scrolls.at(-1)?.target).toBe('default_branch');
    expect(document.activeElement).toBe(container.querySelector('input[name="default_branch"]'));
  });

  it('ignores a field the page does not have and goes to the section', async () => {
    await mountSettings(`${BASE}/runner?field=remote_url`);
    await waitForPage();
    await settle();

    expectArrivedAt('settings-runner');
  });
});

describe('repo settings in the repo home frame', () => {
  const settingsTab = () =>
    container.querySelector<HTMLAnchorElement>(
      `nav.repo-tabs a[href="/repos/${REPO_ID}/settings"]`,
    );

  it('carries no crumb trail: the frame names the repo, Settings is the current tab', async () => {
    await mountSettings(BASE);
    await waitForPage();

    // The frame's header heads the page (baseRepo is mid-clone).
    expect(container.querySelector('p.crumb')).toBeNull();
    expect(container.querySelector('a.back-link')?.getAttribute('href')).toBe('/repos');
    expect(container.querySelector('.repo-head h1')?.textContent).toBe('coding-lab');
    expect(container.querySelector('.repo-head .chip.status-cloning')?.textContent).toBe('cloning');
    expect(container.querySelector('.repo-head-remote')?.textContent).toBe(
      'git.cloonar.com/Cloonar/coding-lab',
    );
    expect(settingsTab()?.getAttribute('aria-current')).toBe('page');
    // No category index, no per-section back header any more.
    expect(container.querySelector('a.settings-index-row')).toBeNull();
    expect(container.querySelector('.settings-back-head')).toBeNull();
  });

  it('a section URL keeps the Settings tab current', async () => {
    await mountSettings(`${BASE}/agents`);
    await waitFor(() => container.querySelector('button[name="provider"]'), 'agents section');

    expect(settingsTab()?.getAttribute('aria-current')).toBe('page');
  });

  it('never asks through a browser confirm', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    await mountSettings(BASE);
    await waitForPage();
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('one section failing to render leaves the other nine usable', async () => {
    // The imports picker reads the repo list in a place that throws when the
    // read fails; the page must contain that to the one section.
    const original = globalThis.fetch;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown, init?: RequestInit) =>
        String(input) === '/api/v1/repos' && (init?.method ?? 'GET') === 'GET'
          ? Promise.resolve({
              ok: false,
              status: 500,
              json: () => Promise.resolve({ error: 'repo list unavailable' }),
              text: () => Promise.resolve('{"error":"repo list unavailable"}'),
            })
          : original(input as RequestInfo, init),
      ),
    );
    await mountSettings(BASE);
    await waitForPage();
    await settle();

    expect(pageSection('imports').textContent).toContain('Imports could not be shown.');
    expect(pageSection('imports').textContent).toContain('repo list unavailable');
    expect(container.querySelectorAll('section.settings-section')).toHaveLength(10);
    expect(container.querySelector('input[name="default_branch"]')).not.toBeNull();
    expect(buttonByText('+ Add secret')).not.toBeNull();
  });
});
