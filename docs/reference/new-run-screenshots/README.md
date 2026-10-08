# New run page: screenshots

Screenshots of the built app for issue #66, taken against a stubbed API at 390 × 844 (2× pixel
density, phone layout) and at 1280 × 900 (desktop layout), light scheme. The sample data mirrors
the reference mockup [`../new-run-mockup.html`](../new-run-mockup.html): nine repositories, with
coding-lab (twelve open issues, among them #47 `needs-triage` and #56 `ready-for-agent`, Auto on, one
AFK run live), data-pipeline with Auto off, cloonar-nixos on the host Runner, billing-api, a paused
website, auth-service whose tracker check fails, a mobile-app cloning at 62 %, and an infra-docs
whose clone failed. The pills are coding-lab, data-pipeline, cloonar-nixos and billing-api (the
stored recent list), and two agents are configured. Agents and models carry placeholder names.

| View                                    | 390 px                                             | 1280 px                                              | Mockup state                |
| --------------------------------------- | -------------------------------------------------- | ---------------------------------------------------- | --------------------------- |
| Default, no state picked                | [home-390.png](home-390.png)                       | [home-1280.png](home-1280.png)                       | (default, no hash)          |
| Model picker (sheet / popover)          | [model-390.png](model-390.png)                     | [model-1280.png](model-1280.png)                     | `model`                     |
| Repository picker, a query typed        | [repo-390.png](repo-390.png)                       | [repo-1280.png](repo-1280.png)                       | `repo`                      |
| All open issues picker                  | [issues-390.png](issues-390.png)                   | [issues-1280.png](issues-1280.png)                   | `issues` (`many` for 50)    |
| More options                            | [more-390.png](more-390.png)                       | [more-1280.png](more-1280.png)                       | `more`                      |
| Issue action sheet, Triage suggested    | [action-390.png](action-390.png)                   | [action-1280.png](action-1280.png)                   | `action`                    |
| Triage #47 attached, a note typed       | [triage-390.png](triage-390.png)                   | [triage-1280.png](triage-1280.png)                   | `triage`                    |
| Auto off with Run one                   | [autooff-390.png](autooff-390.png)                 | [autooff-1280.png](autooff-1280.png)                 | `autooff`                   |
| Agent logged out, field disabled        | [loggedout-390.png](loggedout-390.png)             | [loggedout-1280.png](loggedout-1280.png)             | `loggedout`                 |
| Cloning repository, field disabled      | [cloning-390.png](cloning-390.png)                 | [cloning-1280.png](cloning-1280.png)                 | (cloning repo, no state)    |
| Clone failed with Retry, field disabled | [clone-failed-390.png](clone-failed-390.png)       | [clone-failed-1280.png](clone-failed-1280.png)       | (failed clone, no state)    |
| Tracker check failing, field enabled    | [tracker-failing-390.png](tracker-failing-390.png) | [tracker-failing-1280.png](tracker-failing-1280.png) | (failing tracker, no state) |
| Host Runner warning                     | [host-390.png](host-390.png)                       | [host-1280.png](host-1280.png)                       | (cloonar-nixos, no state)   |

The mockup state is the URL hash that opens it: `new-run-mockup.html#model` at a phone width,
`#desktop.model` for the desktop frame. A repository that cannot run yet is only preselected when
no repository can, so the cloning and clone-failed views are a lab with that one repository (the
pill row then reads "All 1"). The tracker view opens on auth-service as the most recent repository.

## Regenerate

From the repository root, with `npm ci` done in `web/`:

```sh
node docs/reference/new-run-screenshots/shots/shots.mjs
```

The script builds `web/` into a temporary directory, serves it with every `/api/v1` call answered
from [`shots/data.mjs`](shots/data.mjs), refuses any other network request, and overwrites the PNGs
here. It prints each file's size and checks that no 390 px view scrolls sideways; at 390 px it also
prints the docked composer's bottom edge and how many controls are shorter than 44 px. The static
server and the API stub are the ones of
[`../repo-settings-screenshots/shots/stubs.mjs`](../repo-settings-screenshots/shots/stubs.mjs).
Optional environment: `SHOTS_DIST` (serve an existing build instead), `CHROMIUM` (the browser
binary, default `/usr/bin/chromium`), `SHOTS_OUT` (another output folder), `SHOTS_SCHEME=dark`.
Pass `--only home,model` for a subset.
