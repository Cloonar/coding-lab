// Playwright smoke config (design §11: SPA vitest unit; Playwright smoke M8).
//
// One-time setup:  npx playwright install chromium
// Run with:        npm run test:e2e
//
// NixOS: the downloaded chromium is dynamically linked and will not exec.
// Point Playwright at nix-built browsers instead — when nixpkgs'
// playwright-driver lags @playwright/test, symlink the shipped revision dirs
// to the expected names (revision skew of a minor version is fine in practice):
//
//   B=$(nix build nixpkgs#playwright-driver.browsers --no-link --print-out-paths)
//   mkdir -p /tmp/pw && for d in "$B"/*; do ln -sfn "$d" /tmp/pw/$(basename "$d"); done
//   # if e.g. chromium_headless_shell-1228 is expected but 1223 shipped:
//   # ln -sfn "$B/chromium_headless_shell-1223" /tmp/pw/chromium_headless_shell-1228
//   PLAYWRIGHT_BROWSERS_PATH=/tmp/pw PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=true npm run test:e2e
//
// The webServer command is self-contained (e2e/serve.sh): it builds the SPA,
// embeds it into a fresh `lab` binary (-tags ui) and starts that binary on a
// throwaway state dir, so the smoke exercises the real serving path — webui
// embed, SPA fallback, auth middleware — not the Vite dev server.

import { defineConfig } from '@playwright/test';

const port = 8378;
const baseURL = `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: './e2e',
  timeout: 60_000,
  use: {
    baseURL,
    browserName: 'chromium',
  },
  webServer: {
    command: './e2e/serve.sh',
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    timeout: 300_000,
    stdout: 'pipe',
    stderr: 'pipe',
    env: { E2E_ADDR: `127.0.0.1:${port}` },
  },
});
