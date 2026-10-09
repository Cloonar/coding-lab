# Global Settings: screenshots

Screenshots of the built app for issue #85 (ADR-0080), taken against a stubbed API at 390 × 844 (2×
pixel density, phone layout) and at 1280 × 900 (desktop layout), light scheme. Global Settings is one
page: Agents, Runner, General and Notifications on one scrolling page under sticky section chips
(phone) or a sticky outline with the group labels Runs, Setup and This device (desktop), with one
save bar for the whole page. The sample data is a lab with the seeded settings, one AFK override set
(the AFK model) and the rest inherited, a provider catalog with an AFK option bag, healthy credential
gateway and SSH bastion cards, two Web Push devices and fifteen repositories, twelve of which inherit
the runner default. Agents and models carry placeholder names.

| View                                                | 390 px                                                       | 1280 px                                                        |
| --------------------------------------------------- | ------------------------------------------------------------ | -------------------------------------------------------------- |
| Settings, top of the page                           | [settings-top-390.png](settings-top-390.png)                 | [settings-top-1280.png](settings-top-1280.png)                 |
| Pending changes in two sections, save bar visible   | [settings-changed-390.png](settings-changed-390.png)         | [settings-changed-1280.png](settings-changed-1280.png)         |
| Problems after Save, shown at the field and counted | [settings-error-390.png](settings-error-390.png)             | [settings-error-1280.png](settings-error-1280.png)             |
| Host-switch dialog (runner default changed to Host) | [settings-host-dialog-390.png](settings-host-dialog-390.png) | [settings-host-dialog-1280.png](settings-host-dialog-1280.png) |

- **Changed**: a model and Max instances (Agents) and a memory limit (Runner) are edited, so the save
  bar counts three changes and links Agents and Runner; both sections carry a mark in the chips and
  the outline, and the Model field is marked at its label.
- **Error**: Max instances is 0 (floor 1) and Transcript retention is 400 (cap 365). Save sends
  nothing, scrolls to the first problem and focuses it; the save bar counts the two problems and
  links the sections that hold them.
- **Host dialog**: Save with the runner default switched to Host opens the in-page dialog naming the
  inheriting repos; Cancel keeps every edit and sends nothing.

## Regenerate

From the repository root, with `npm ci` done in `web/`:

```sh
node docs/reference/global-settings-screenshots/shots/shots.mjs
```

The script builds `web/` into a temporary directory (removed afterwards), serves it with every
`/api/v1` call answered from [`shots/data.mjs`](shots/data.mjs), refuses any other network request,
and overwrites the PNGs here. It prints each file's size and checks that no 390 px view scrolls
sideways; any unstubbed API call, page error or sideways scroll makes it exit non-zero, so the exit
status is the layout check. Optional environment: `SHOTS_DIST` (serve an existing build instead),
`CHROMIUM` (the browser binary, default `/usr/bin/chromium`), `SHOTS_OUT` (another output folder),
`SHOTS_SCHEME=dark`. Pass `--only settings-top,settings-error` for a subset.
