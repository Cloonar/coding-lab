# Navigation with the tab bar: screenshots

Screenshots of the built app for issue #76, taken against a stubbed API at 390 × 844 (2× pixel
density, phone layout) and at 1280 × 900 (desktop layout), light scheme. The sample data mirrors
the reference mockup [`../tab-bar-mockup.html`](../tab-bar-mockup.html): six live runs, two that
need you (one asking a question, one waiting for input), two working (one an AFK run with about
1 h 12 min of budget left), two idle (one 3 commits behind its base), and six ended runs over three
days, among them an escalated autoland run with Re-arm and a run that died. The repositories are
the fifteen of the [repo-settings screenshots](../repo-settings-screenshots/README.md), reused from
their data. Agents and models carry placeholder names.

| View                                         | 390 px                                           | 1280 px                                    | Mockup state               |
| -------------------------------------------- | ------------------------------------------------ | ------------------------------------------ | -------------------------- |
| Runs at `/`: grouped list / Runs table       | [runs-390.png](runs-390.png)                     | [runs-1280.png](runs-1280.png)             | `runs`                     |
| Runs with no live runs                       | [runs-empty-390.png](runs-empty-390.png)         | [runs-empty-1280.png](runs-empty-1280.png) | `empty`                    |
| Ended (`/history`), grouped by day           | [history-390.png](history-390.png)               | [history-1280.png](history-1280.png)       | `history` (phone only)     |
| New run composer at `/new`                   | [new-390.png](new-390.png)                       | [new-1280.png](new-1280.png)               | `new`                      |
| Repos tab lit (`/repos`)                     | [repos-390.png](repos-390.png)                   | [repos-1280.png](repos-1280.png)           | `repos` (phone only)       |
| More (`/more`); desktop lands on `/settings` | [more-390.png](more-390.png)                     | [more-1280.png](more-1280.png)             | `more` (phone only)        |
| More, agent provider logged out              | [more-loggedout-390.png](more-loggedout-390.png) | not taken (no More page on desktop)        | `loggedout`                |
| Chat with a question: no tab bar             | [chat-390.png](chat-390.png)                     | [chat-1280.png](chat-1280.png)             | `chat`                     |
| Rail collapsed                               | none (no rail on the phone)                      | [collapsed-1280.png](collapsed-1280.png)   | `collapsed` (desktop only) |

The mockup state is the URL hash that opens it: `tab-bar-mockup.html#phone.runs` at a phone width,
`#desktop.runs` for the desktop frame. A few things differ from the mockup on purpose:

- `more-1280.png` shows where `/more` sends you on desktop: `/settings`, which opens its first
  category.
- The mockup draws `desktop.collapsed` over the chat. Here it is taken over the Runs table, so the
  table can be seen at full width.
- The mockup's "merged" and "PR open" chips need PR state that a run row does not carry. The
  Ended rows show the run's own outcome instead (done, escalated, died, stopped, timed out).
- The mockup's version line under the account block is missing because no API reports the lab
  version.
- The Install app row on More only shows when the app can be installed, and headless Chromium
  never can, so it is absent here.

The event stream is held open with a heartbeat, so the live dot reads Live and the browser tab
title reads `(2) lab`. Neither shows in a screenshot.

## Regenerate

From the repository root, with `npm ci` done in `web/`:

```sh
node docs/reference/tab-bar-screenshots/shots/shots.mjs
```

The script builds `web/` into a temporary directory, serves it with every `/api/v1` call answered
from [`shots/data.mjs`](shots/data.mjs), refuses any other network request, and overwrites the PNGs
here. For each file it prints the size, checks that no 390 px view scrolls sideways, and at 390 px
says whether the tab bar shows and where its top edge sits. It also prints the final path and the
document title. The static server and the API stub ([`shots/stubs.mjs`](shots/stubs.mjs)) are
adapted from the repo-settings round. The one change is that `/api/v1/events` is a real open stream
rather than a fulfilled route. Optional environment: `SHOTS_DIST` (serve an existing build
instead), `CHROMIUM` (the browser binary, default `/usr/bin/chromium`), `SHOTS_OUT` (another output
folder), `SHOTS_SCHEME=dark`. Pass `--only runs,history` for a subset.
