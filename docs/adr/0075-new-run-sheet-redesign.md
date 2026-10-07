# New run redesign: repository pills, an Issues card with an issue action, Model and Effort chips with direct pickers, More options for the rest, and blockers shown only at the docked composer

The New run page (`/`) was a composer with its options hidden behind chips that wrapped to two or three rows on a phone. The bar held a Repository chip, an Agent chip (ADR-0030, only with two or more providers), Model and Effort selects and a `…` popover for the label and the remote-control knob (ADR-0025, ADR-0045). The AFK strip sat under it, although Auto usually runs the ready queue by itself and the strip pushed the page down for a control used now and then. The page showed no issues at all, and there was no way to start a run *about* an issue (triage it, implement it, discuss it) without opening the Issues tab, copying a number and typing the instruction.

The maintainer reviewed three mockups and chose direction A, "Sheet", revision 2 (issue #66; the mockup is `docs/reference/new-run-mockup.html`, and screenshots of the built views at 390px and 1280px are in `docs/reference/new-run-screenshots/`). This ADR records what that supersedes and the decisions the build added where the issue was silent. It needed no server change: the issue action rides the existing `first_message` (issue #96).

The decisions, pinned:

- **The page is three things: repository pills, an Issues card, the composer.** Below 1024px, the app's rail breakpoint, the pills row is at the top, the Issues card follows, and the composer is docked sticky at the bottom edge with the safe-area inset. The page root is `min-height: 100%`, so the dock sits at the bottom edge even when the card is short. From 1024px the page is a centered 720px column in the content area: pills, composer, Issues card, with the existing `clamp(1.5rem, 8vh, 5rem)` top margin. The dock has the Chat's shape (ADR-0072), so the composer is where the Chat composer will be a second later. Nothing on the page navigates away except a blocker's remedy.

- **Repository pills replace the Repository chip.** The pills are the repositories used recently, last used first and preselected, at most four, plus an "All N" pill. A pill carries the repo's readiness dot from the summary's roll-up: green when it passes, red when it fails. "All N" opens the repository picker and never the Repositories page. The picker is a search field first (name or host as you type, Enter picks the first enabled match, the unified Select's `select-search` input reused), then a Recent group, then All repositories. Rows that cannot start a run (cloning with the live percent, clone failed) are shown disabled with their status text.

- **A repository whose tracker check fails stays pickable.** Its row says "tracker failing". The failing-tracker banner leaves the composer enabled, because a manual run can still start without the tracker, so the picker may not stop the operator from choosing it. Only cloning and clone-failed rows are disabled.

- **The page always selects a repository, and the recent list holds startable repos only.** When no repository is startable, the page still selects one, the most recent else the first, so that its cloning or clone-failed banner explains why the field is disabled. A blank page with a disabled field and no reason would be worse. The recent list lives in the existing `lab.last-repo` localStorage key, now a JSON array: most recent first, deduplicated, startable repos only, ten kept, four shown as pills. The old single-id value is still read, as a list of one.

- **Composer: one merged field, chips on the left, the accent send on the right.** The field has an optional attachment row, an auto-growing textarea and a bottom bar. The chip row scrolls horizontally on a phone (no wrapping, scrollbar hidden), so the field keeps one height. The chips are **Model** (the resolved model label), **Effort** (hidden when the resolved model has no efforts, as before) and **⋯** (More options). There is no Repository chip and no Agent chip in the bar. A chip whose value differs from the inherited default, a per-spawn pick, is outlined in the accent (`--accent` border, `--link` text). The ⋯ chip takes the outline when anything inside More options is set.

- **Model and Effort open a picker on the first tap.** It is a bottom sheet below 1024px and a popover anchored to the chip from 1024px. It has one row per option, the inherited default marked "default", a check on the current value and a one-line hint: where the default comes from (the repo's settings or global Settings) and that a pick applies to this run only. Picking closes it. Changing the model is two taps, never three.

- **More options holds Agent, Remote control, Label and the Runner sentence.** Agent is a segmented control, shown only with two or more providers (ADR-0030's rule, carried forward; the agent pick moves out of the bar). Remote control is a switch reading "inherited · on/off" or "set here", disabled with "<Agent> ignores this" when the provider has no remote knob (ADR-0045). Label is optional, 32 characters at most. One sentence names the resolved Runner and links to the repo's Runner settings.

- **Resolution is unchanged.** Per-spawn pick, then repo override, then global default, mirroring the server. Stale picks reset on a provider or model change (issue #156). Only explicit picks ride the request. Bare Enter sends on fine-pointer setups and Cmd/Ctrl+Enter everywhere (ADR-0031); an empty box is still a plain spawn through the send button or Cmd/Ctrl+Enter.

- **One shared Picker surface, portaled.** The repository picker, the Model and Effort pickers, the issue picker and the issue action sheet are one component: a bottom sheet below 1024px, a popover anchored to its chip or pill from 1024px, and a centered dialog for the issue action sheet. It renders in a portal, so the horizontally scrolling chip row, or any overflow on the page, never clips it. This also closes the clipping ADR-0030 fixed for the old repo popover, without a second implementation of it.

- **The Issues card replaces the AFK strip under the composer.** The heading reads "Issues" with the open count. The head holds the Auto switch, hidden while AFK is paused or the tracker check fails. Below it is one AFK line: `Auto on · N ready · M AFK runs live · next claim when a slot frees`. With Auto off the line ends with a **Run one** button. Paused reads `AFK paused after 3 failed runs · N ready` with **Reset**. Then come up to four open issues, newest first: the number in mono, the title clamped to two lines, the triage label as a chip (`ready-for-agent` in the run tint, `needs-triage` in the notice tint, `needs-info` in the idle tint, nothing when unlabeled) and the age. Other labels are not shown. "All N open issues", shown only with more than four, opens the issue picker in place. With the tracker check failing the card shows the check's detail instead of rows, and with no open issues it reads "No open issues." The repo home's AFK card is unchanged.

- **The Issues card adds no forge request of its own.** It reads the open issues with the same bounded read the Issues tab uses (ADR-0050), so a fresh list serves both. It makes no ready-queue read: the ready count is the repo summary's claimable count. While the tracker check fails it skips the read entirely, because the read would only fail again and the banner already says why.

- **The issue picker filters by number or by words.** A query of digits only, with or without a leading `#`, is an issue-number prefix (`4` matches 4, 47, 412). Anything else is title words, all of which must occur, in any order, case-insensitively. State chips (All, needs-triage, ready-for-agent, needs-info, unlabeled) show their counts, and a zero-count chip is hidden. The counts are over the whole open list, not over the rows the search leaves, so the chips stay put while the operator types. The search field is pinned while the rows scroll.

- **Tapping an issue opens the action sheet; the action attaches to the composer.** The sheet shows the title, labels and age, "What should the agent do with it?", and three actions with one-line descriptions. **Triage** is marked Suggested for `needs-triage`, **Implement** for `ready-for-agent`, **Discuss** otherwise. Choosing one attaches a removable chip (`Triage #47 · <title>`) to the field, changes the placeholder ("Anything the agent should know? (optional)"; Discuss: "Say what you want to discuss about #47…") and relabels Send as "Start: Triage #47". Changing the repository clears the attachment.

- **The action rides `first_message` (issue #96).** The action's fixed line comes first, then the typed text, trimmed, on its own line when there is any (empty text adds no trailing newline):
  - Triage: `/triage #47`
  - Implement: ``Implement issue #47 "<title>". Read it with `labctl issue view 47` first; it is your brief.``
  - Discuss: ``Let's discuss issue #47 "<title>". Read it with `labctl issue view 47`, then wait for my questions.``

  When an action is attached and no label was typed, the run's label defaults to `<action>-<number>` (`triage-47`); a typed label wins. A bare Enter on an empty box sends when an action is attached, since the attachment is the message. Implement is a manual run with a seeded brief, not a claim: the AFK seed prompt and the AFK machinery are untouched.

- **Blockers show at the composer, and nowhere else.** The page has no readiness line, and a repo that can run shows nothing about it. A blocker is a banner directly above the field. Cloning is a notice with the live percent, and the field is disabled. Clone failed is an error with the clone error and a **Retry** that triggers the existing clone retry, field disabled. Agent logged out is an error with **Reconnect** to `/credentials`, field disabled (it replaces the old `newrun-warn`). Tracker check failing is a warning with the check's detail and **Fix**, which opens the repo settings field the readiness remedy names, and the field stays enabled. The host-Runner warning ("Runs on the host, unsandboxed, with full host access.") shows above the field whenever the effective Runner is `host`. The zero-repositories empty state ("No repositories yet — add one to get started.") is unchanged, and the Playwright smoke asserts on it.

- **Removed.** The AFK strip under the composer (the `AFKStrip` default export and its `afk-strip*` CSS; `AFKCard` stays for the repo home), the `…` popover (`MoreChip`) and the summary-line and Select chips of the old bar.

## Status

Accepted 2026-10-07. Resolves issue #66, the GitHub issue; the "issue #66" ADR-0030 cites is an older number from the Forgejo tracker.

- **ADR-0025:** supersedes the composer's bar and the AFK strip under it. The strip "under the composer, following the composer's repo chip" is replaced by the AFK line in the Issues card, and the `…` popover that held the label is replaced by More options. Its "slim logged-out banner on the composer surface" stands, now as the Reconnect banner above the field. The runs rail, the composer-first Home, the `first_message` delivery of the typed text (issue #96) and the rest of the shell stand.
- **ADR-0073:** supersedes "the strip under the New-run composer stays". The strip appears on the repo home's Overview only, as `AFKCard`, which is unchanged. The repo home, Repositories and repo settings are untouched.
- **ADR-0030:** supersedes the conditional Agent chip in the composer bar, and the unified Select as the composer's chip trigger and repo picker. The agent pick moves into More options. The rule itself carries forward: it appears only with two or more providers. Three-level provider resolution, skip-layer defaults and the unified Select on the settings pages stand.
- **ADR-0031:** unchanged. Bare Enter sends on fine-pointer setups and Cmd/Ctrl+Enter everywhere; the only addition is that an attached issue action counts as content for a bare Enter on an empty box.
- **ADR-0045:** unchanged. The remote-control knob, its "inherited / set here" semantics and the "ignored by this provider" state move from the `…` popover into More options.
- **ADR-0050:** unchanged and relied on. The Issues card is one more reader of the bounded issue list, not a new kind of read.

The user-facing docs for the New run page follow in the same change (the `update-docs` skill).

## Considered options

- **Direction B, "Launchpad": repository first, a master–detail layout.** A repo list on one side and the selected repo's issues and composer on the other. Rejected by the maintainer in round 1, and not to be blended back in.
- **Direction C, "Intent": Task, Issue and Schedule tabs.** Three tabs on top of one composer, one per kind of run. Rejected by the maintainer in round 1, and not to be blended back in.
- **Keep the AFK strip under the composer and add the Issues card above.** Rejected: Auto usually runs the ready queue by itself, so the strip spent the page's most visible line on a control used now and then, and it was the second place to look for the same count.
- **Keep Agent as a chip in the bar.** Rejected: the old bar's chips wrapped to two or three rows on a phone. Model and Effort stay one chip away; the agent moves into More options.
- **Navigate from "All N" to the Repositories page.** Rejected: the operator wants a repository, not a page. A searchable picker on the spot keeps the tap on the page.
- **Disable a repository row whose tracker check fails.** Rejected. The banner leaves the composer enabled for the same case, and a picker stricter than the field would stop runs the field allows.
- **Let the page show no repository when none is startable.** Rejected: the field would be disabled with no banner to say why.
- **Read the ready queue for the Issues card.** Rejected: it is a second forge read for a count the repo summary already carries as the claimable count.
- **Add a server endpoint for starting a run about an issue.** Rejected: `first_message` already delivers a seeded message on the spawn argv (issue #96), and a new endpoint would be a second way to start a manual run.
- **Complete `#` in the composer into an issue.** Out of scope for this change, as is claiming a specific issue through the AFK machinery.

## Consequences

- A phone shows one row of pills, a short card and one docked field, with the options one tap away. At 390px no element overflows, the chip row scrolls, and every control is at least 44px tall.
- The New run page now shows issue data. It adds no forge request while the list is fresh, and none while the tracker check fails. A repo without a working tracker shows its detail in the card, not an empty list.
- `lab.last-repo` holds a JSON array. The old single-id value is still read, and the next selection rewrites the key as a list.
- Any code that opened a spawn chip or `MoreChip` by its old label, and the tests that asserted the AFK strip or the `…` popover, change with this ADR. The chip, picker, issue filter, first-message and recent-list logic is covered by unit tests in `web/src/lib/newRun.test.ts` and `web/src/components/Picker.test.tsx`.
- The Picker is the one sheet-or-popover surface for the page. The Chat's Run details and `•••` surfaces (ADR-0072) have their own implementations; folding them into the Picker is open.
