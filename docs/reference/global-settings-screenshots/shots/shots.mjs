// Screenshots of the one-page global Settings (issue #85, ADR-0080) at 390x844 @2x and 1280x900,
// against a stubbed API — no lab server and no network needed.
//
//   node docs/reference/global-settings-screenshots/shots/shots.mjs [--only settings-top,settings-error]
//
// Environment:
//   SHOTS_DIST    a built SPA to serve; unset = build web/ into a temp dir first (npx vite build)
//   CHROMIUM      the Chromium binary (default /usr/bin/chromium)
//   SHOTS_OUT     where the PNGs go (default: the folder above this one)
//   SHOTS_SCHEME  light (default) or dark
//
// Prints one line per PNG: its size, scrollWidth vs innerWidth (no horizontal page scroll) and
// any unstubbed API call or page error. Exits 1 when one of those checks fails.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "../../../../web/node_modules/@playwright/test/index.mjs";
import * as data from "./data.mjs";
import { serveDist, stubApi } from "./stubs.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WEB = path.resolve(HERE, "../../../../web");
const OUT = path.resolve(process.env.SHOTS_OUT ?? path.join(HERE, ".."));
const SCHEME = process.env.SHOTS_SCHEME === "dark" ? "dark" : "light";
const onlyArg = process.argv.indexOf("--only");
const ONLY = onlyArg > 0 ? process.argv[onlyArg + 1].split(",") : null;

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

function api() {
  // PATCH /settings answers the whole settings with the patch applied, as the server does.
  const saved = { ...data.globalSettings };
  const patch = async (request) => {
    const body = request.postDataJSON();
    for (const [key, value] of Object.entries(body)) {
      saved[key] = key === "spawn_options_afk" ? JSON.stringify(value) : value;
    }
    return saved;
  };
  return {
    "GET /api/v1/auth/state": {
      setup_required: false,
      authenticated: true,
      username: "operator",
    },
    "GET /api/v1/instances": { instances: [] },
    "GET /api/v1/repos": { repos: data.repos },
    "GET /api/v1/providers": { providers: data.providers },
    "GET /api/v1/settings": () => saved,
    "PATCH /api/v1/settings": patch,
    "GET /api/v1/onecli/health": data.gatewayHealth,
    "GET /api/v1/warpgate/health": data.bastionHealth,
    // The app shell checks each agent's login; the Notifications section lists push devices.
    "GET /api/v1/providers/agent-a/auth/status": data.authStatus,
    "GET /api/v1/providers/agent-b/auth/status": data.authStatus,
    "GET /api/v1/push/subscriptions": { subscriptions: data.pushDevices },
  };
}

const EVENTS = [];

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
  page
    .locator(`button[role="radio"][name="${name}"][value="${value}"]`)
    .click();
const saveBar = (page) => page.locator(".settings-savebar button.primary");

const PAGE = '.global-settings .settings-sections [data-field="max_instances"]';
const VIEWS = {
  "settings-top": { path: "/settings", ready: PAGE },
  "settings-changed": {
    path: "/settings",
    ready: PAGE,
    // Three edits in two sections: a model and the capacity cap (Agents), a memory limit (Runner).
    act: async (page) => {
      await pick(page, "spawn_model_default", "Medium");
      await page.locator('[name="max_instances"]').fill("6");
      await page.locator('[name="container_memory"]').fill("16g");
      await page.evaluate(() => window.scrollTo(0, 0));
    },
  },
  "settings-error": {
    path: "/settings",
    ready: PAGE,
    // Two problems the browser finds before anything is sent: no instances at all (floor 1) and a
    // retention window past its 365-day cap. Save scrolls to the first and focuses it.
    act: async (page) => {
      await page.locator('[name="max_instances"]').fill("0");
      await page.locator('[name="transcript_retention_days"]').fill("400");
      await saveBar(page).click();
    },
  },
  "settings-host-dialog": {
    path: "/settings/runner",
    ready: PAGE,
    // The runner default switched to Host, Save pressed: the in-page dialog asks first.
    act: async (page) => {
      await segment(page, "runner_default", "host");
      await saveBar(page).click();
      await page.waitForSelector('[role="alertdialog"]');
    },
  },
};

// --- run ----------------------------------------------------------------------------------------

// A build this script made is removed again when it is done; SHOTS_DIST is never touched.
let built = null;
function dist() {
  if (process.env.SHOTS_DIST) return path.resolve(process.env.SHOTS_DIST);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "lab-shots-"));
  built = dir;
  execFileSync("npx", ["vite", "build", "--outDir", dir, "--emptyOutDir"], {
    cwd: WEB,
    stdio: "inherit",
  });
  return dir;
}

fs.mkdirSync(OUT, { recursive: true });
const server = await serveDist(dist());
const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM ?? "/usr/bin/chromium",
  headless: true,
  args: ["--no-sandbox"],
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
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (error) => errors.push(String(error)));
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
          (wide ? " HORIZONTAL SCROLL" : " ok") +
          (misses.length
            ? `  unstubbed: ${[...new Set(misses)].join(", ")}`
            : "") +
          (errors.length ? `  errors: ${errors.join(" | ")}` : ""),
      );
      await context.close();
    }
  }
} finally {
  await browser.close();
  server.close();
  if (built) fs.rmSync(built, { recursive: true, force: true });
}
process.exit(failed ? 1 : 0);
