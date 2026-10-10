// The bounded pages never give the document scroll range (issue #99).
//
// The Chat and the phone's New run size themselves from window.visualViewport
// as --vv-height / --vv-top (lib/visualViewport.ts) and are placed with
// position: relative + top. A relative offset extends the document's
// scrollable overflow, and the two values arrive through separate events, so
// around the keyboard's open and close a reading can pair the full height with
// a stale offset. chat.css and newrun.css clamp both to the layout viewport.
// jsdom applies no CSS, so this is the seam: the built stylesheet over the two
// pages' DOM shapes in a real Chromium, with the variables set by hand to each
// inconsistent reading, asserting that the document never overflows.
//
// The page is served through a route handler (no server round-trip): the
// stylesheet is read from web/dist, which the webServer's build step produces.

import { expect, test, type Page } from '@playwright/test';

interface NodeFs {
  readdirSync(path: string): string[];
  readFileSync(path: string, encoding: 'utf8'): string;
}

/** A Node built-in module by name; the non-literal specifier keeps tsc (DOM
 *  types only in web/) out of it, as in gitRemote.ts. */
async function nodeModule<T>(name: string): Promise<T> {
  const specifier: string = `node:${name}`;
  return (await import(specifier)) as T;
}

async function builtStylesheet(): Promise<string> {
  const fs = await nodeModule<NodeFs>('fs');
  const dir = 'dist/assets';
  const css = fs.readdirSync(dir).filter((f) => f.endsWith('.css'));
  expect(css, 'web/dist must hold the built stylesheet (npm run build)').toHaveLength(1);
  return fs.readFileSync(`${dir}/${css[0]}`, 'utf8');
}

type Kind = 'chat' | 'newrun';

/** The shell plus one page, with the real class names: enough structure for
 *  the page-level sizing rules, filler for the inner scroll containers. */
function markup(kind: Kind): string {
  const composer =
    '<div class="composer"><form class="composer-form"><textarea class="composer-input" rows="1"></textarea>' +
    '<div class="composer-bar"><div class="composer-chips"><button class="composer-chip">chip</button></div>' +
    '<button class="composer-send icon-btn">send</button></div></form></div>';
  const page =
    kind === 'chat'
      ? `<main class="page chat-page"><div class="chat-column"><div class="chat-body">` +
        `<div class="chat-header"><button class="icon-btn chat-back">back</button></div>` +
        `<div class="chat-stream" role="log">${'<p>line</p>'.repeat(120)}</div></div>${composer}</div></main>`
      : `<main class="page newrun newrun-docked"><div class="newrun-head"><div class="section-head"><h2>New run</h2></div></div>` +
        `<div class="newrun-dock"><div class="newrun-pills"><div class="repo-pills"><button class="repo-pill">repo</button></div></div>${composer}</div>` +
        `<div class="newrun-issues">${'<p>issue</p>'.repeat(80)}</div><div class="toast-region"></div></main>`;
  const tabbar =
    kind === 'newrun'
      ? '<nav class="tabbar"><a class="tab">Runs</a><a class="tab">New</a></nav>'
      : '';
  return (
    `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
    `<meta name="viewport" content="width=device-width, initial-scale=1, interactive-widget=resizes-content">` +
    `<link rel="stylesheet" href="/vv-harness/app.css"><title>harness</title></head>` +
    `<body><div id="root"><div class="shell${kind === 'newrun' ? ' has-tabbar' : ''}">` +
    `<div class="shell-rail"></div><div class="shell-content">${page}</div>${tabbar}</div></div></body></html>`
  );
}

interface Reading {
  /** --vv-height in px, or null to unset. */
  height: number | null;
  /** --vv-top in px, or null to unset. */
  top: number | null;
  /** .tabbar-out on the shell (the keyboard reads open on /new). */
  tabbarOut?: boolean;
}

interface Measure {
  overflow: number;
  scrollY: number;
  pageTop: number;
  pageBottom: number;
  layoutHeight: number;
}

async function openPage(page: Page, kind: Kind): Promise<void> {
  const css = await builtStylesheet();
  await page.route('**/vv-harness/**', (route) => {
    const url = route.request().url();
    if (url.endsWith('/app.css')) return route.fulfill({ contentType: 'text/css', body: css });
    return route.fulfill({ contentType: 'text/html', body: markup(kind) });
  });
  await page.goto('/vv-harness/');
}

async function apply(page: Page, reading: Reading): Promise<Measure> {
  return page.evaluate((r) => {
    const main = document.querySelector('main')!;
    const shell = document.querySelector('.shell')!;
    if (r.height === null) main.style.removeProperty('--vv-height');
    else main.style.setProperty('--vv-height', `${r.height}px`);
    if (r.top === null) main.style.removeProperty('--vv-top');
    else main.style.setProperty('--vv-top', `${r.top}px`);
    shell.classList.toggle('tabbar-out', r.tabbarOut === true);
    const root = document.documentElement;
    const box = main.getBoundingClientRect();
    return {
      overflow: root.scrollHeight - root.clientHeight,
      scrollY: window.scrollY,
      pageTop: box.top,
      pageBottom: box.bottom,
      layoutHeight: root.clientHeight,
    };
  }, reading);
}

for (const kind of ['chat', 'newrun'] as const) {
  test.describe(`${kind} at phone width`, () => {
    test.use({ viewport: { width: 390, height: 664 } });

    test('a legitimate keyboard reading keeps the page on the visual viewport', async ({
      page,
    }) => {
      await openPage(page, kind);
      // iOS: the visual viewport shrinks by the keyboard and pans down by it.
      const m = await apply(page, { height: 664 - 300, top: 300, tabbarOut: kind === 'newrun' });
      expect(m.overflow).toBe(0);
      expect(m.pageTop).toBe(300);
      expect(m.pageBottom).toBe(664);
    });

    test('the full height with a stale offset never overflows', async ({ page }) => {
      await openPage(page, kind);
      // The close's resize landed before its scroll: height back to the layout
      // viewport, --vv-top still the keyboard's pan. Unclamped, this is 300px
      // (minus the tab bar's room on /new) of document scroll range.
      const m = await apply(page, { height: 664, top: 300 });
      expect(m.overflow).toBe(0);
      expect(m.pageTop).toBe(0);
      expect(m.pageBottom).toBeLessThanOrEqual(m.layoutHeight);
    });

    test('a height a fraction above the layout viewport never overflows', async ({ page }) => {
      await openPage(page, kind);
      const m = await apply(page, { height: 664.5, top: 0 });
      expect(m.overflow).toBe(0);
    });

    test('an open-then-close sequence leaves the document unscrolled', async ({ page }) => {
      await openPage(page, kind);
      const out = kind === 'newrun';
      // Open, scroll event first: the pan with the old full height.
      expect((await apply(page, { height: null, top: 300 })).overflow).toBe(0);
      expect((await apply(page, { height: 364, top: 300, tabbarOut: out })).overflow).toBe(0);
      // Close, resize event first: the full height with the old pan.
      expect((await apply(page, { height: 664, top: 300 })).overflow).toBe(0);
      const m = await apply(page, { height: 664, top: 0 });
      expect(m.overflow).toBe(0);
      expect(m.scrollY).toBe(0);
    });
  });
}

test.describe('chat at desktop width', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('a fractional visual-viewport height never overflows', async ({ page }) => {
    await openPage(page, 'chat');
    expect((await apply(page, { height: 800.4, top: 0 })).overflow).toBe(0);
    expect((await apply(page, { height: 800, top: 0 })).overflow).toBe(0);
  });
});
