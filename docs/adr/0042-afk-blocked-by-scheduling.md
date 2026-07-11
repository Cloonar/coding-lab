# The AFK scheduler reads `## Blocked by` as a machine contract, gating a claimable issue on its referenced blockers still being open

AFK selection is `open + ready-for-agent, minus already-claimed, lowest
number first` (ADR-0010). The `## Blocked by` section the to-issues skill
writes into every issue body is prose the scheduler never reads — so a slice
whose blocker is still open is claimed the moment it carries the label,
racing unmerged work the author explicitly sequenced behind. Dependency
order lived only in the human's head and the issue text; the engine ignored
both.

Issue #136 grills this to consensus (2026-07-11): promote the `## Blocked by`
body section to a machine-read contract — the scheduler must not claim an
issue while any issue that section references is still open. The lever is
already in hand. Issue bodies arrive with `ReadyIssues` on all three tracker
backends (forgejo, github, builtin), and the open-issue set is one existing
`Tracker.Issues(ctx, StateOpen)` call, so this reads a section that already
exists against a query that already exists — **zero new tracker API
surface**, and the convention the to-issues skill already emits becomes the
contract it always implied.

The decisions, pinned (do not re-litigate):

- **Source of truth is the body section, not a forge feature or a label.**
  The `## Blocked by` markdown section is the one truth. Not Forgejo-native
  issue dependencies — those are forge-specific and simply absent on the
  builtin tracker, so the tracker seam (ADR-0009) would fracture across
  backends. Not a dedicated blocking label — that duplicates a fact the body
  already states and demands a write to stay in sync. The body already
  carries the order; the scheduler learns to read it.

- **Grammar: a `Blocked by` heading, then every `#N` under it.** A
  case-insensitive `Blocked by` ATX heading at any level opens the section;
  the section runs to the next heading of any level (or end of body). Every
  `#(\d+)` inside that span is a blocker — no closing keyword required, since
  the heading already scopes intent. Refs anywhere else in the body ("relates
  to #12", a `Closes #9` trailer) never block. A section with no refs —
  including the template's literal "None - can start immediately", or any
  prose without a `#N` — reads as unblocked. Multiple refs block until all
  resolve. The `#(\d+)` match reuses the right-boundary discipline of
  `closesDirectiveRe` in `internal/tracker/closes.go`: bounded so `#70` is
  never `#7` and `#7abc` is never `#7`, parsed through `Atoi` so `#07` is
  issue 7 — the two grammars agree on what a reference is.

- **Semantics: a ref blocks iff the issue is open; everything ambiguous
  fails toward progress.** A reference blocks exactly when the issue exists
  and is currently open. A closed blocker unblocks regardless of *how* it
  closed — merged, wontfix, or closed with no PR at all — because "closed"
  is the only done-signal the scheduler owns here. A dangling ref (a number
  that names no issue, or a deleted one) unblocks: on data ambiguity we fail
  toward doing the work, never stall a queue on a typo. An open blocker that
  is itself claimed or in-flight **still blocks** — unmerged work is not done
  work, and the whole point is to not race it. A cycle (A blocked by B,
  B blocked by A) simply never schedules either issue; there is deliberately
  **no cycle detection** — a cycle is a triage data bug, and it surfaces
  plainly in the skip log rather than being papered over by the engine.

- **Placement: inside `FilterClaimable`, the one choke point.** The logic is
  two pure functions beside the existing decision functions (the `decide.go`
  style) in `internal/afk/blocked.go` — `ParseBlockedBy(body)` extracts the
  section's refs, `PartitionBlocked` splits a ready set against the open-issue
  numbers — applied inside `(*Service).FilterClaimable`. That is the single
  filter the auto scheduler, `StartManualAFK`, the UI's claimable queue
  (`ClaimableIssuesFor`), and `GET /repos/{id}/ready` all funnel through, so
  none of them can ever disagree about what "claimable" means. A manual AFK
  start skips a blocked issue identically to the auto loop — there is no
  targeted-issue start today, so manual and auto see the same filtered set
  and this stays true by construction.

- **Cost and failure profile: lazy fetch, all-blocked ≡ empty, fail-closed on
  infrastructure.** The open-issue set comes from the existing
  `Tracker.Issues(ctx, StateOpen)`, fetched **only when at least one
  claimable ready issue actually contains blocker refs** — a repo not using
  the convention pays zero added forge calls per tick. A queue whose ready
  issues are all blocked behaves *exactly* like an empty ready queue: no
  launch, no consecutive-failure increment, no three-strikes pause — nothing
  was attempted, so nothing failed (the same stance ADR-0010 takes for an
  empty claimable set). If the `Issues(StateOpen)` fetch itself fails, the
  repo's tick is skipped per the existing Tracker contract: fail-closed on an
  infrastructure error (never guess a blocker is resolved when we could not
  ask), fail-open only on data ambiguity (a dangling ref never blocks).
  Cross-repo (`owner/repo#N`) and full-URL refs are unevaluable in a
  single-repo open-set, so the filter ignores them — but logs them, so a real
  cross-repo dependency never silently schedules. Every skipped issue emits a
  structured log line naming its blockers (e.g. `afk: repo X skipped blocked
  issues: #87 (blocked by #74)`), the operator's only window into an ordering
  the engine is now enforcing.

## Status

Accepted. Extends ADR-0010 (the AFK engine's selection model — claim-is-the-
branch, lowest-number-first, three-strikes) with one more filter stage inside
`FilterClaimable`, and leans on ADR-0009's tracker seam: because `ReadyIssues`
and `Issues(StateOpen)` are already uniform across forgejo, github, and
builtin, the contract holds identically on every backend with no per-backend
code. Settled via issue #136. Non-breaking: a repo whose issues carry no
`## Blocked by` refs schedules byte-for-byte as it does today, and pays no
extra forge call. The parser and partition are table-tested in the
`decide_test.go` style; an engine-level test proves a blocked lower-numbered
issue is skipped in favor of the next unblocked one, then claimed on a later
sweep once its blocker closes.

## Considered options

- **Forgejo-native issue dependencies.** Rejected: forge-specific, and wholly
  absent on the builtin tracker, so the source of truth would fork across
  backends and break the single-contract promise the tracker seam (ADR-0009)
  exists to keep. The body section is already uniform on all three.

- **A dedicated blocking label (e.g. `blocked`).** Rejected: it duplicates a
  fact the `## Blocked by` body already states, and — worse — needs a *write*
  to add and remove as blockers open and close, a second record to keep in
  sync with the first and drift out of it. The body is the record that
  already exists and already moves with the issue.

- **Surface blockers in the UI only, leave the scheduler untouched.**
  Rejected: it does not stop the race. A "blocked" badge tells a human what
  the auto loop is about to do wrong; it does not stop the auto loop from
  claiming the issue at 3am. The gate has to live where the claim decision
  lives.

## Consequences

- New `internal/afk/blocked.go`: `ParseBlockedBy` and `PartitionBlocked` as
  pure, table-tested functions beside the existing decision logic; no I/O,
  no engine state.
- `FilterClaimable` gains a partition stage: parse the ready set's bodies,
  and *only if* any carries a blocker ref, fetch `Issues(StateOpen)` once and
  drop the blocked issues, logging each with its blockers.
- The skip log line is the sole operator-visible surface in this slice. A
  "blocked" badge in the UI (reading the same parse) is a deliberate
  follow-up candidate, explicitly out of scope here — this slice stops the
  race; showing it is the next issue.
- Docs updated to name the contract: `assets/skills/to-issues/SKILL.md` (the
  `## Blocked by` section is machine-read; write blockers as `#N`),
  the triage skill and `docs/agents/triage-labels.md` (`ready-for-agent`
  plus a live blocker ref is a valid pairing — the scheduler orders it), and
  `CONTEXT.md`'s **Claimable** definition.
