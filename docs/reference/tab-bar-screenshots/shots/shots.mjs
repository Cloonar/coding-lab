// Screenshots of the navigation redesign (issue #76: bottom tab bar on the phone, Runs at /,
// composer at /new) at 390x844 @2x and 1280x900, against a stubbed API — no lab server and no
// network needed.
//
//   node docs/reference/tab-bar-screenshots/shots/shots.mjs [--only runs,history]
//
// Environment:
//   SHOTS_DIST    a built SPA to serve; unset = build web/ into a temp dir first (npx vite build)
//   CHROMIUM      the Chromium binary (default /usr/bin/chromium)
//   SHOTS_OUT     where the PNGs go (default: the folder above this one)
//   SHOTS_SCHEME  light (default) or dark
//
// Prints one line per PNG: its size, scrollWidth vs innerWidth (no horizontal page scroll), at 390
// whether the tab bar shows and where its top edge sits, and any unstubbed API call or page error.
// Exits 1 when the scroll, stub or error check fails.
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from '../../../../web/node_modules/@playwright/test/index.mjs';
import * as data from './data.mjs';
import { serveDist, stubApi } from './stubs.mjs';

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
const REST = { 390: [380, 4], 1280: [1270, 890] };

// --- the stubbed API ----------------------------------------------------------------------------

const byId = (id) => data.repos.find((repo) => repo.id === id);
const named = (repo) => ({ id: repo.id, name: repo.name });

/**
 * `scenario.instances`: the live runs (default: the mockup's six); `scenario.loggedOut`: the
 * default agent provider's login.
 */
function api(scenario) {
  const instances = scenario.instances ?? data.instances;
  const table = {
    'GET /api/v1/auth/state': { setup_required: false, authenticated: true, username: 'operator' },
    'GET /api/v1/instances': { instances },
    'GET /api/v1/runs': { runs: data.runs },
    'GET /api/v1/repos': { repos: data.repos },
    'GET /api/v1/credentials': { credentials: data.credentials },
    'GET /api/v1/providers': { providers: data.providers },
    'GET /api/v1/settings': {
      ...data.globalSettings,
      git_author_name: 'lab-bot',
      git_author_email: 'lab-bot@example.com',
    },
    'GET /api/v1/tokens': { tokens: data.tokens },
    'GET /api/v1/schedule-flows': { flows: data.flows },
    'GET /api/v1/onecli/pool': { configured: true, secrets: [], connections: [] },
    'GET /api/v1/onecli/dashboard': { mode: 'port', url: 'https://lab.example.com:8443' },
    // Integrations not configured ("off" is normal, never an error): Settings' General section.
    'GET /api/v1/onecli/health': {
      state: 'off',
      api: { configured: false, reachable: false },
      gateway: { configured: false, reachable: false },
    },
    'GET /api/v1/warpgate/health': {
      state: 'off',
      api: { configured: false, reachable: false },
      ssh: { configured: false, reachable: false },
    },
  };
  for (const provider of data.providers) {
    table[`GET /api/v1/providers/${provider.id}/auth/status`] = {
      logged_in: !scenario.loggedOut,
      email: scenario.loggedOut ? '' : 'operator@example.com',
      method: 'oauth',
      checked_at: '',
    };
  }
  for (const repo of data.repos) {
    const p = `/api/v1/repos/${repo.id}`;
    const claimable = repo.summary.claimable ?? 0;
    Object.assign(table, {
      [`GET ${p}`]: repo,
      [`POST ${p}/inherited`]: data.inherited,
      [`GET ${p}/readiness`]: repo.summary.readiness,
      [`GET ${p}/afk`]: { claimable },
      [`GET ${p}/ready`]: { issues: data.issues.slice(0, claimable) },
      [`GET ${p}/issues`]: { binding: repo.tracker_binding, issues: data.issues },
      [`GET ${p}/labels`]: { labels: [] },
      [`GET ${p}/crs`]: { crs: [] },
      [`GET ${p}/parked`]: { parked: data.parked[repo.id] ?? [] },
      [`GET ${p}/schedules`]: { schedules: data.schedules[repo.id] ?? [] },
      [`GET ${p}/secrets`]: { secrets: data.secrets[repo.id] ?? [] },
      [`GET ${p}/imports`]: { imports: (data.imports[repo.id] ?? []).map((id) => named(byId(id))) },
      [`GET ${p}/importers`]: {
        importers: data.repos.filter((r) => data.imports[r.id]?.includes(repo.id)).map(named),
      },
      [`GET ${p}/onecli/grants`]: { configured: true, grants: [] },
      [`GET ${p}/warpgate/targets`]: { configured: true, targets: data.sshTargets },
    });
  }
  for (const instance of instances) {
    const p = `/api/v1/runs/${instance.id}`;
    Object.assign(table, {
      [`GET ${p}`]: instance,
      [`GET ${p}/messages`]: data.messages,
      [`GET ${p}/commands`]: { commands: [] },
    });
  }
  return table;
}

// --- the views ----------------------------------------------------------------------------------

/** Waits until the page's (smooth) scroll has come to rest. */
async function settle(page) {
  let last = -1;
  for (let still = 0, i = 0; i < 60 && still < 4; i += 1) {
    const y = await page.evaluate(() => Math.round(window.scrollY));
    still = y === last ? still + 1 : 0;
    last = y;
    await page.waitForTimeout(80);
  }
}

// `widths`: the widths the view is shot at (default both); `ready`: the selector the shot waits
// for (or one per width); `scenario`: what the stub answers; `collapsed`: the rail starts collapsed.
const VIEWS = {
  runs: { path: '/', ready: 'main .runlist-row, main .runs-table-row' },
  'runs-empty': { path: '/', ready: 'main .empty', scenario: { instances: [] } },
  history: { path: '/history', ready: '.ended-run' },
  new: { path: '/new', ready: '.newrun-dock' },
  repos: { path: '/repos', ready: '.repos-head h1' },
  // From 1024px /more redirects to /settings: the 1280 shot shows where it lands.
  more: { path: '/more', ready: { 390: '.more-account', 1280: '.settings-split' } },
  'more-loggedout': {
    path: '/more',
    ready: '.more-alert',
    scenario: { loggedOut: true },
    widths: ['390'],
  },
  chat: { path: '/runs/run_1', ready: '.chat-dialog-asking, .chat-stream' },
  collapsed: { path: '/', ready: 'main .runs-table-row', collapsed: true, widths: ['1280'] },
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
      if (view.widths && !view.widths.includes(width)) continue;
      const context = await browser.newContext({ ...options, colorScheme: SCHEME });
      // Nothing leaves the machine: anything but the local server is refused.
      await context.route(
        (url) => !url.href.startsWith(server.url),
        (route) => route.abort(),
      );
      if (view.collapsed) {
        await context.addInitScript(() => localStorage.setItem('lab.rail-collapsed', '1'));
      }
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', (error) => errors.push(String(error)));
      const { misses } = await stubApi(page, api(view.scenario ?? {}));
      await page.goto(server.url + view.path);
      const ready = typeof view.ready === 'string' ? view.ready : view.ready[width];
      await page.waitForSelector(ready, { timeout: 10_000 });
      // The live dot: the event stream is open (it is in every view; a miss is only reported).
      const live = await page
        .waitForFunction(() => !!document.querySelector('.live-dot.on'), null, { timeout: 4_000 })
        .then(() => true)
        .catch(() => false);
      await page.waitForTimeout(600); // fonts, late resources
      await page.mouse.move(...REST[width]);
      await settle(page);
      await page.waitForTimeout(400); // transitions
      const file = path.join(OUT, `${name}-${width}.png`);
      await page.screenshot({ path: file });
      const m = await page.evaluate(() => {
        const bar = document.querySelector('.tabbar');
        const shown = bar !== null && getComputedStyle(bar).display !== 'none';
        return {
          scrollWidth: document.documentElement.scrollWidth,
          innerWidth: window.innerWidth,
          innerHeight: window.innerHeight,
          bar: shown ? Math.round(bar.getBoundingClientRect().top) : null,
          path: location.pathname,
          title: document.title,
        };
      });
      const wide = m.scrollWidth > m.innerWidth;
      if (wide || misses.length || errors.length) failed = true;
      const kb = Math.round(fs.statSync(file).size / 1024);
      console.log(
        `${path.basename(file).padEnd(24)} ${String(kb).padStart(4)} KB  scrollWidth ${m.scrollWidth}/${m.innerWidth}` +
          (wide ? ' HORIZONTAL SCROLL' : ' ok') +
          (width === '390'
            ? `  tab bar ${m.bar === null ? 'hidden' : `top ${m.bar}/${m.innerHeight}`}`
            : '') +
          `  ${m.path}  "${m.title}"` +
          (live ? '' : '  live dot not on') +
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
