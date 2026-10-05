// Screenshots of the repositories list, the repo home and the one-page repo settings (issue #61)
// at 390x844 @2x and 1280x900, against a stubbed API — no lab server and no network needed.
//
//   node docs/reference/repo-settings-screenshots/shots/shots.mjs [--only list,overview]
//
// Environment:
//   SHOTS_DIST    a built SPA to serve; unset = build web/ into a temp dir first (npx vite build)
//   CHROMIUM      the Chromium binary (default /usr/bin/chromium)
//   SHOTS_OUT     where the PNGs go (default: the folder above this one)
//   SHOTS_SCHEME  light (default) or dark
//
// Prints one line per PNG: its size, scrollWidth vs innerWidth (no horizontal page scroll) and
// any unstubbed API call or page error. Exits 1 when one of those checks fails.
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
const REST = { 390: [195, 30], 1280: [130, 450] };

// --- the stubbed API ----------------------------------------------------------------------------

const byId = (id) => data.repos.find((repo) => repo.id === id);
const named = (repo) => ({ id: repo.id, name: repo.name });

function cronPreview(request) {
  const expr = new URL(request.url()).searchParams.get('expr') ?? '';
  const valid = expr.trim().split(/\s+/).length === 5;
  return valid
    ? {
        expr,
        valid,
        error: null,
        next: [],
        next_display: ['Mon 2026-10-12 08:00', 'Mon 2026-10-19 08:00', 'Mon 2026-10-26 08:00'],
      }
    : { expr, valid, error: 'cron: expected 5 fields', next: null, next_display: null };
}

function api() {
  const table = {
    'GET /api/v1/auth/state': { setup_required: false, authenticated: true, username: 'operator' },
    'GET /api/v1/instances': { instances: data.instances },
    'GET /api/v1/repos': { repos: data.repos },
    'GET /api/v1/credentials': { credentials: data.credentials },
    'GET /api/v1/providers': { providers: data.providers },
    'GET /api/v1/settings': data.globalSettings,
    'GET /api/v1/schedule-flows': { flows: data.flows },
    'GET /api/v1/cron/preview': cronPreview,
    'GET /api/v1/onecli/pool': { configured: true, secrets: [], connections: [] },
    'GET /api/v1/onecli/dashboard': { mode: 'port', url: 'https://lab.example.com:8443' },
  };
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
  return table;
}

// The cloning repo's progress arrives over the event stream, as from a real clone.
const EVENTS = [
  ['clone.progress', { repoID: 'mobile-app', phase: 'receiving objects', percent: 62, line: '' }],
];

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
const pick = async (page, name, label) => {
  await page.locator(`button[name="${name}"][aria-haspopup="listbox"]`).click();
  await page.locator('[role="option"]', { hasText: label }).first().click();
};
const segment = (page, name, value) =>
  page.locator(`button[role="radio"][name="${name}"][value="${value}"]`).click();
const openDelete = async (page) => {
  await page.locator('.danger-zone button.danger').click();
  await page.waitForSelector('[role="alertdialog"]');
};

const SETTINGS = '#settings-danger';
const VIEWS = {
  list: { path: '/repos', ready: '.repos-head h1' },
  overview: { path: '/repos/coding-lab', ready: '.afk-card' },
  'overview-not-ready': { path: '/repos/auth-service', ready: '.afk-card' },
  'settings-top': { path: '/repos/coding-lab/settings', ready: SETTINGS },
  'settings-changed': {
    path: '/repos/coding-lab/settings',
    ready: SETTINGS,
    // Three edits in two sections: a model and a remote control (Agents), a memory limit (Runner).
    act: async (page) => {
      await pick(page, 'model_default', 'Medium');
      await segment(page, 'remote_default', 'false');
      await page.locator('[name="container_memory"]').fill('16g');
      await page.evaluate(() => window.scrollTo(0, 0));
    },
  },
  'settings-error': {
    path: '/repos/coding-lab/settings',
    ready: SETTINGS,
    act: async (page) => {
      await page.locator('[name="afk_branch_pattern"]').fill('afk/');
      await page.locator('.settings-savebar button.primary').click();
    },
  },
  'settings-runner-host': { path: '/repos/cloonar-nixos/settings/runner', ready: SETTINGS },
  'settings-schedules': { path: '/repos/coding-lab/settings/schedules', ready: SETTINGS },
  'schedule-editor': {
    path: '/repos/coding-lab/settings/schedules/sched_2',
    ready: '.schedule-editor',
  },
  'delete-dialog': { path: '/repos/coding-lab/settings/danger', ready: SETTINGS, act: openDelete },
  'delete-blocked': {
    path: '/repos/cloonar-nixos/settings/danger',
    ready: SETTINGS,
    act: openDelete,
  },
  'add-repository': {
    path: '/repos/new',
    ready: 'input[name="remote_url"]',
    act: async (page) => {
      await page
        .locator('input[name="remote_url"]')
        .fill('git@github.com:example/search-indexer.git');
      await page.locator('select[name="credential_id"]').selectOption('c1');
      await page.locator('select[name="forge_credential_id"]').selectOption('c3');
      await page.locator('input[name="remote_url"]').blur();
    },
  },
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
      const context = await browser.newContext({ ...options, colorScheme: SCHEME });
      // Nothing leaves the machine: anything but the local server is refused.
      await context.route(
        (url) => !url.href.startsWith(server.url),
        (route) => route.abort(),
      );
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', (error) => errors.push(String(error)));
      const { misses } = await stubApi(page, api(), EVENTS);
      await page.goto(server.url + view.path);
      await page.waitForSelector(view.ready, { timeout: 10_000 });
      await page.waitForTimeout(600); // fonts, late resources, inherited values
      await view.act?.(page);
      await page.mouse.move(...REST[width]);
      await settle(page);
      await page.waitForTimeout(400); // sheet and toast transitions
      const file = path.join(OUT, `${name}-${width}.png`);
      await page.screenshot({ path: file });
      const { scrollWidth, innerWidth } = await page.evaluate(() => ({
        scrollWidth: document.documentElement.scrollWidth,
        innerWidth: window.innerWidth,
      }));
      const wide = scrollWidth > innerWidth;
      if (wide || misses.length || errors.length) failed = true;
      const kb = Math.round(fs.statSync(file).size / 1024);
      console.log(
        `${path.basename(file).padEnd(30)} ${String(kb).padStart(4)} KB  scrollWidth ${scrollWidth}/${innerWidth}` +
          (wide ? ' HORIZONTAL SCROLL' : ' ok') +
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
