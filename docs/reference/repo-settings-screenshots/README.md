# Repositories and repo settings: screenshots

Screenshots of the built app for issue #61, taken against a stubbed API at 390 × 844 (2× pixel
density, phone layout) and at 1280 × 900 (desktop layout), light scheme. The sample data mirrors
the reference mockup [`../repo-settings-mockup.html`](../repo-settings-mockup.html): fifteen
repositories, coding-lab with two live runs and three ready issues, a cloning repository, a paused
one, a failed clone, a repository whose tracker check fails, three Schedules and a repository that
another one imports. Agents and models carry placeholder names.

| View                                          | 390 px                                                       | 1280 px                                                        | Mockup state     |
| --------------------------------------------- | ------------------------------------------------------------ | -------------------------------------------------------------- | ---------------- |
| Repositories list with Needs you              | [list-390.png](list-390.png)                                 | [list-1280.png](list-1280.png)                                 | list (no token)  |
| Overview, all checks passing                  | [overview-390.png](overview-390.png)                         | [overview-1280.png](overview-1280.png)                         | `overview`       |
| Overview, not ready (failing tracker check)   | [overview-not-ready-390.png](overview-not-ready-390.png)     | [overview-not-ready-1280.png](overview-not-ready-1280.png)     | `not-ready`      |
| Settings, top of the page                     | [settings-top-390.png](settings-top-390.png)                 | [settings-top-1280.png](settings-top-1280.png)                 | `settings`       |
| Settings with pending changes in two sections | [settings-changed-390.png](settings-changed-390.png)         | [settings-changed-1280.png](settings-changed-1280.png)         | `changed`        |
| Settings with a field error after Save        | [settings-error-390.png](settings-error-390.png)             | [settings-error-1280.png](settings-error-1280.png)             | `error`          |
| Runner on host, container fields folded       | [settings-runner-host-390.png](settings-runner-host-390.png) | [settings-runner-host-1280.png](settings-runner-host-1280.png) | `runner`         |
| Schedules section                             | [settings-schedules-390.png](settings-schedules-390.png)     | [settings-schedules-1280.png](settings-schedules-1280.png)     | `schedules`      |
| Schedule editor (full screen / side panel)    | [schedule-editor-390.png](schedule-editor-390.png)           | [schedule-editor-1280.png](schedule-editor-1280.png)           | `schedule`       |
| Delete dialog                                 | [delete-dialog-390.png](delete-dialog-390.png)               | [delete-dialog-1280.png](delete-dialog-1280.png)               | `delete`         |
| Delete dialog blocked by an importer          | [delete-blocked-390.png](delete-blocked-390.png)             | [delete-blocked-1280.png](delete-blocked-1280.png)             | `delete-blocked` |
| Add repository, URL pasted, name derived      | [add-repository-390.png](add-repository-390.png)             | [add-repository-1280.png](add-repository-1280.png)             | `add`            |

The mockup state is the URL hash that opens it: `repo-settings-mockup.html#changed` at a phone
width, `#desktop.changed` for the desktop frame.

## Regenerate

From the repository root, with `npm ci` done in `web/`:

```sh
node docs/reference/repo-settings-screenshots/shots/shots.mjs
```

The script builds `web/` into a temporary directory, serves it with every `/api/v1` call answered
from [`shots/data.mjs`](shots/data.mjs), refuses any other network request, and overwrites the PNGs
here. It prints each file's size and checks that no 390 px view scrolls sideways. Optional
environment: `SHOTS_DIST` (serve an existing build instead), `CHROMIUM` (the browser binary,
default `/usr/bin/chromium`), `SHOTS_OUT` (another output folder), `SHOTS_SCHEME=dark`. Pass
`--only list,overview` for a subset.
