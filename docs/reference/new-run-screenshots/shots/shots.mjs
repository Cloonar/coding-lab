// Screenshots of the New run page (issue #66) at 390x844 @2x and 1280x900, against a stubbed API:
// no lab server and no network needed.
//
//   node docs/reference/new-run-screenshots/shots/shots.mjs [--only home,model]
//
// Environment:
//   SHOTS_DIST    a built SPA to serve; unset = build web/ into a temp dir first (npx vite build)
//   CHROMIUM      the Chromium binary (default /usr/bin/chromium)
//   SHOTS_OUT     where the PNGs go (default: the folder above this one)
//   SHOTS_SCHEME  light (default) or dark
//
// Prints one line per PNG: its size, scrollWidth vs innerWidth (no horizontal page scroll), at 390
// the bottom edge of the docked composer and the controls shorter than 44 px (informational), and
// any unstubbed API call or page error. Exits 1 when one of those checks fails.
//
// The static server and the API stub are shared with the repo-settings screenshots.
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from '../../../../web/node_modules/@playwright/test/index.mjs';
import { serveDist, stubApi } from '../../repo-settings-screenshots/shots/stubs.mjs';
import * as data from './data.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WEB = path.resolve(HERE, '../../../../web');
const OUT = path.resolve(process.env.SHOTS_OUT ?? path.join(HERE, '..'));
const SCHEME = process.env.SHOTS_SCHEME === 'dark' ? 'dark' : 'light';
const onlyArg = process.argv.indexOf('--only');
const ONLY = onlyArg > 0 ? process.argv[onlyArg + 1].split(',') : null;

const WIDTHS = {
  390: {
    viewport: { width: 390, height: 844 },
    deviceScaleFactor: 2,
    isMobile: true,
    hasTouch: true,
  },
  1280: { viewport: { width: 1280, height: 900 }, deviceScaleFactor: 1 },
};
// A blank spot the pointer is parked on before each shot, so no hover state shows.
const REST = { 390: [195, 30], 1280: [40, 450] };

// --- the stubbed API ----------------------------------------------------------------------------

/** The repositories pills show, most recent first (the page's lab.last-repo). */
const RECENT = ['coding-lab', 'data-pipeline', 'cloonar-nixos', 'billing-api'];

/** `scenario.repos`: the repositories the lab has (default: all nine); `loggedOut`: the agents. */
function api(scenario) {
  const repos = scenario.repos ?? data.repos;
  const table = {
    'GET /api/v1/auth/state': {
      setup_required: false,
      authenticated: true,
      username: 'operator',
    },
    'GET /api/v1/instances': { instances: data.instances },
    'GET /api/v1/repos': { repos },
    'GET /api/v1/providers': { providers: data.providers },
    'GET /api/v1/settings': data.globalSettings,
  };
  for (const provider of data.providers) {
    table[`GET /api/v1/providers/${provider.id}/auth/status`] = {
      logged_in: !scenario.loggedOut,
      email: 'operator@example.com',
      method: 'oauth',
      checked_at: '',
    };
  }
  for (const repo of repos) {
    const p = `/api/v1/repos/${repo.id}`;
    const open = data.issues[repo.id] ?? [];
    Object.assign(table, {
      [`GET ${p}`]: repo,
      [`GET ${p}/readiness`]: repo.summary.readiness,
      [`GET ${p}/afk`]: { claimable: repo.summary.claimable ?? 0 },
      [`GET ${p}/issues`]: { binding: repo.tracker_binding, issues: open },
      [`GET ${p}/labels`]: { labels: [] },
    });
  }
  return table;
}

// The cloning repo's progress arrives over the event stream, as from a real clone.
const EVENTS = [
  ['clone.progress', { repoID: 'mobile-app', phase: 'receiving objects', percent: 62, line: '' }],
];

const byId = (id) => data.repos.find((repo) => repo.id === id);

// --- the views ----------------------------------------------------------------------------------

const tap = (page, selector) => page.locator(selector).first().click();
const pill = (name) => (page) => tap(page, `.repo-pill:has-text("${name}")`);
const openRepoPicker = (page) => tap(page, '.repo-pill-all');
const openIssue = (page, number) =>
  page.locator('.issues-card .issue-row', { hasText: `#${number}` }).click();

// `scenario` is what the stub answers; `act` drives the page from its loaded state.
const VIEWS = {
  home: {},
  model: { act: (page) => tap(page, 'button.run-chip[aria-label^="Model:"]') },
  repo: {
    act: async (page) => {
      await openRepoPicker(page);
      await page.locator('input.select-search').fill('cloonar');
    },
  },
  issues: {
    act: async (page) => {
      await tap(page, '.issues-card-all');
      await page.locator('.issue-picker input').first().waitFor();
    },
  },
  more: { act: (page) => tap(page, 'button[aria-label="More options"]') },
  action: { act: (page) => openIssue(page, 47) },
  // #47 -> Triage -> a note typed: the attachment chip, the placeholder and the Send label change.
  triage: {
    act: async (page) => {
      await openIssue(page, 47);
      await page.locator('.issue-action', { hasText: 'Triage' }).click();
      await page.locator('.composer-input').fill('Check whether it is still reproducible.');
      await page.locator('.composer-input').blur();
    },
  },
  autooff: { act: pill('data-pipeline') },
  loggedout: { scenario: { loggedOut: true } },
  // A repository that cannot run yet is only preselected when none can: a lab with that one repo.
  cloning: { scenario: { repos: [byId('mobile-app')] } },
  'clone-failed': { scenario: { repos: [byId('infra-docs')] } },
  // auth-service as the preselected (most recent) repository. Not picked from another repo: the
  // Issues card then still counts the previous repo's issues (see the README).
  'tracker-failing': {
    recent: ['auth-service', 'coding-lab', 'data-pipeline', 'billing-api'],
  },
  host: { act: pill('cloonar-nixos') },
};

// --- run ----------------------------------------------------------------------------------------

function dist() {
  if (process.env.SHOTS_DIST) return path.resolve(process.env.SHOTS_DIST);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lab-shots-'));
  execFileSync('npx', ['vite', 'build', '--outDir', dir, '--emptyOutDir'], {
    cwd: WEB,
    stdio: 'inherit',
  });
  return dir;
}

fs.mkdirSync(OUT, { recursive: true });
const server = await serveDist(dist());
const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM ?? '/usr/bin/chromium',
  headless: true,
  args: ['--no-sandbox'],
});
let failed = false;
try {
  for (const [name, view] of Object.entries(VIEWS)) {
    if (ONLY && !ONLY.includes(name)) continue;
    for (const [width, options] of Object.entries(WIDTHS)) {
      const context = await browser.newContext({
        ...options,
        colorScheme: SCHEME,
      });
      // Nothing leaves the machine: anything but the local server is refused.
      await context.route(
        (url) => !url.href.startsWith(server.url),
        (route) => route.abort(),
      );
      const recent = view.recent ?? RECENT;
      await context.addInitScript(
        (ids) => localStorage.setItem('lab.last-repo', JSON.stringify(ids)),
        recent,
      );
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', (error) => errors.push(String(error)));
      const { misses } = await stubApi(page, api(view.scenario ?? {}), EVENTS);
      await page.goto(`${server.url}/`);
      await page.waitForSelector('.newrun-dock', { timeout: 10_000 });
      // Rows (or the card's note) in, so a shot never catches "Loading issues…".
      await page
        .waitForFunction(
          () =>
            !document.querySelector('.issues-card-loading') &&
            !!document.querySelector('.repo-pill'),
          null,
          { timeout: 10_000 },
        )
        .catch(() => {});
      await page.waitForTimeout(600); // fonts, late resources, inherited values
      await view.act?.(page);
      await page.mouse.move(...REST[width]);
      await page.waitForTimeout(600); // sheet and popover transitions
      const file = path.join(OUT, `${name}-${width}.png`);
      await page.screenshot({ path: file });
      const m = await page.evaluate(() => {
        const dock = document.querySelector('.newrun-dock')?.getBoundingClientRect();
        const roots = [
          document.querySelector('main'),
          ...document.querySelectorAll('.picker, [role="dialog"]'),
        ].filter(Boolean);
        let small = 0;
        for (const root of roots) {
          for (const el of root.querySelectorAll(
            'button, input, textarea, select, [role="switch"]',
          )) {
            const r = el.getBoundingClientRect();
            if (r.width > 0 && r.height > 0 && r.height < 43.5) small += 1;
          }
        }
        return {
          scrollWidth: document.documentElement.scrollWidth,
          innerWidth: window.innerWidth,
          innerHeight: window.innerHeight,
          dockBottom: dock ? Math.round(dock.bottom) : null,
          small,
        };
      });
      const wide = m.scrollWidth > m.innerWidth;
      if (wide || misses.length || errors.length) failed = true;
      const kb = Math.round(fs.statSync(file).size / 1024);
      console.log(
        `${path.basename(file).padEnd(26)} ${String(kb).padStart(4)} KB  scrollWidth ${m.scrollWidth}/${m.innerWidth}` +
          (wide ? ' HORIZONTAL SCROLL' : ' ok') +
          (width === '390'
            ? `  dock bottom ${m.dockBottom}/${m.innerHeight}  controls<44px: ${m.small}`
            : '') +
          (misses.length ? `  unstubbed: ${[...new Set(misses)].join(', ')}` : '') +
          (errors.length ? `  errors: ${errors.join(' | ')}` : ''),
      );
      await context.close();
    }
  }
} finally {
  await browser.close();
  server.close();
}
process.exit(failed ? 1 : 0);
