# Chat-first shell: runs rail, composer-first Home, warm reskin

ADR-0019 made the existing chrome usable on a phone, but the information
architecture stayed dashboard-first: a horizontal topbar over a repo-card pile,
with live runs buried inside the cards and no way to move between runs without
going through a list page. Issue #41 (maintainer design interview, 2026-07-08)
pivots the app to the Claude-Code-web IA: the conversation list is the spine,
starting a run is composing a message, and everything else is secondary
navigation. This ADR records the pivot; the issue comment is the full spec.

The decisions, pinned:

- **The runs rail is the app's spine on every authenticated page.** At ≥1024px
  a persistent 260px `<nav>` rail; below that the SAME component as a left
  overlay drawer (hamburger + left-edge swipe to open; scrim / leftward swipe /
  `Esc` / route change to close). Anatomy, top to bottom: brand · `+ New run` ·
  ACTIVE (live instances only) · Repos · History · Credentials · Tokens ·
  Settings · username / Log out / SSE live dot. Rows carry **no destructive
  action** — Stop stays in the chat header. The rail is driven by the existing
  `listInstances()` + `run.changed` refetch (plus a debounced
  `run.messages.changed` refetch so conversational-state dots stay live); SSE
  only, no polling. The old topbar and its hamburger dropdown are deleted.
- **Attention-first ordering, ended runs drop out.** needs-input/question runs
  float to the top with an accent dot, working runs follow, idle runs last;
  newest `started_at` first within a group (the API exposes no per-run
  last-activity timestamp — explicitly out of scope). Dead/ended runs leave the
  rail; the History page (the old Runs list, renamed, `/runs` redirects) is the
  archive. When the drawer is closed on mobile, the hamburger carries an
  attention badge whenever any live run is needs-input/question. Two
  continuity details the rows keep from the old instance list: AFK rows show
  their budget countdown on the secondary line (the deadline lives nowhere
  else), and since the state dot is color-only, each row's accessible name
  carries the state word for screen readers.
- **The Dashboard dissolves; `/` is the New-run composer.** Home is a big
  centered composer — repo chip · model/effort chips · `…` (optional label) ·
  textarea · send — that never auto-navigates. Send = `startInstance` →
  navigate to the chat; typed text becomes a **client-side queued first
  message** that renders as a pending user bubble and auto-sends (via the
  existing Reply endpoint) exactly once when the transcript becomes ready. No
  backend change: the queue is an in-memory per-run map that survives SPA route
  changes and is lost on tab close — accepted and documented in code. Spawn
  failure returns the text to the composer with the server's 409 verbatim.
  [Retired 2026-07-10 (#96): the client-side queued first message is gone —
  the New-run composer text now rides `SpawnSpec.InitialPrompt` on the spawn
  argv, so no message ever waits on transcript availability.]
  Dashboard pieces re-home: repo cards (clone status/progress, stop-all,
  parked) → `/repos`; ClaudeAuthCard → Credentials, plus a slim "Claude is
  logged out — reconnect" banner on the composer surface; the AFK machinery
  (claimable count · Run one · Auto toggle · three-strikes paused banner with
  Reset) becomes a compact per-repo strip under the composer, following the
  composer's repo chip — one surface for both ways of dispatching work.
- **Manual spawn still takes label/model/effort only.** The `…` popover holds
  the label; provider spawn-options stay out because the instances endpoint
  accepts no options bag (`internal/httpapi/instances.go`) and this redesign
  makes no backend change. #21 (the AFK options 2-state-over-3-state checkbox)
  remains open on its own surfaces.
- **Warm reskin at token level; one vendored variable serif.** `base.css`
  swaps token values only: bone/ivory surfaces and warm near-black text in
  light, warm charcoal (not blue-black) in dark, terracotta accent
  (`#d97757`). Source Serif 4 (OFL) ships as two self-hosted variable woff2
  files under `web/public/fonts/` — a static asset, **not** an npm dependency —
  and covers headings and assistant chat prose; UI chrome stays system sans,
  code stays mono. The PWA manifest and theme-color metas follow the palette.
  Theme selection stays OS-driven — no toggle, no persisted theme state
  (ADR-0005's principle survives the repaint). Two contrast-driven cuts of the
  accent: `--accent-text` is a warm coffee-brown, not white (white on
  `#d97757` is 3.12:1 and fails AA), and terracotta used as *text* — links,
  active nav, tinted chips — goes through `--link`, a darker/lighter cut per
  theme, because the raw accent reads 2.96:1 on bone.
- **Chat restyle, Claude-style, on top of #35's behaviors.** Assistant
  messages lose the bubble and render as serif prose on the background at full
  column width; user messages become a soft warm-neutral right-aligned bubble;
  tool chips flatten to quiet bordered mono rows with the same expand
  behavior. Accent is reserved for actions and attention (send, interrupt,
  jump-pill emphasis, attention dots) — never message surfaces. Everything
  issue #35 landed — quick-return header, jump-to-latest pill, merged
  composer with the Interrupt morph, in-stream needs-input note — is preserved
  unchanged.

## Status

Accepted. A pure SPA + docs diff on top of ADR-0019's foundations: the shell
(`AppShell.tsx`, `SideNav.tsx`, `lib/railOrder.ts`), the composer surface
(`routes/NewRun.tsx`, `lib/queuedMessage.ts`, `components/AFKStrip.tsx`), the
re-homed pages (`routes/Repos.tsx`, `routes/History.tsx`, Credentials), the
token/typography swap in `base.css`, and the chat restyle in `RunChat.tsx` +
`base.css`. `Dashboard.tsx`, `TopBar.tsx`, `InstanceList.tsx`,
`StartInstanceForm.tsx`, and `AFKSection.tsx` are deleted (the rail, the
composer, and the strip are their successors). No message schema, provider
seam, API, SSE, or migration change.

## Considered options

- **A rail bolted onto the existing dashboard-first app.** Rejected by the
  design interview: it keeps two competing entry points (dashboard cards vs
  rail) and leaves the dashboard's pile of per-repo controls as the de-facto
  home. The pivot re-homes those controls where they are used.
- **A backend "initial prompt" spawn parameter** instead of the client-side
  queued first message. Rejected for this slice: the Reply endpoint already
  delivers text to a live session; a spawn-time parameter would thread prompt
  state through spawn → tmux → provider for no user-visible gain over
  queue-and-send, and the queue's failure modes (tab close loses it) are
  accepted and documented. Clean later add-on if the loss window ever matters.
- **Recently-ended runs in the rail** (a "just finished" section). Rejected:
  the rail answers "what needs me / what is moving"; History answers "what
  happened". Mixing them re-grows the dashboard inside the rail.
- **A theme toggle alongside the reskin.** Rejected again (ADR-0005): OS-driven
  only. The repaint changes values, not the principle.
- **An npm webfont dependency** (fontsource et al.). Rejected: two woff2 files
  and an @font-face are a static asset; a package manager has no business in
  the font-loading path (no-heavy-deps ethos, ADR-0018/0019).
- **Cmd+K quick switcher, run-switch shortcuts, bottom sheets.** Deferred
  non-goals — the rail plus `/history` cover switching; a palette is a clean
  later add-on.

## Consequences

- The rail's conversational-state dots inherit the chat tailer's staleness
  bugs: a running workflow that reads as idle (#39) now lies in the rail the
  same way it lies in the badge today. Fixing the tailer fixes both surfaces at
  once — the rail adds no new state derivation.
- `listInstances()` is now fetched by the shell on every `run.changed` and
  (debounced) `run.messages.changed` across the whole app, not just on the
  dashboard. Chatty transcripts refetch a small JSON list at most ~4×/s — the
  debounce is the load-bearing guard; keep it if the rail is ever refactored.
- The left-edge swipe is best-effort on iOS standalone PWAs (the OS may claim
  the edge for history-back on some versions); the hamburger is the reliable
  path. Documented in `AppShell.tsx` — do not "fix" it by widening the hot
  zone, that starts eating horizontal scrolls.
- The queued first message is the only piece of chat state that lives outside
  the server (in-memory map). Anything that needs it to survive a reload must
  move it server-side (the rejected spawn parameter) — do not reach for
  localStorage, a stale auto-send into a re-used run id is worse than a lost
  draft.
- E2E and component tests now assume the rail: authenticated page tests stub
  `/api/v1/instances`, and the Playwright smoke asserts the composer's
  zero-repos empty state on `/` and Log out in the rail.
