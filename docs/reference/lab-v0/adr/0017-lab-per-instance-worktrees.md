# Every lab instance runs in its own git worktree

Every manual `lab` **instance** (see CONTEXT.md) used to spawn in the project's **main checkout** (`~/projects/<project>`): `handleStart` passed `dir = projectDir(project)` for all of them, so two concurrent instances stomped each other's working tree, index, and branch. Only AFK runs were isolated — [ADR-0007](0007-lab-drives-afk-runs.md) gave each its own worktree. This ADR makes **every** instance — manual *and* AFK — run in its own worktree, and unifies their identity, branch, worktree, and teardown onto one model.

## Core model

- Every instance runs in its own worktree on its own branch, forked from a freshly-fetched `origin/<default>`. `~/projects/<project>` is demoted to the **reference repo**: the scanner target, the worktree parent, and the host for `lab`'s fetch/branch/worktree git ops. It is never an instance's cwd, and `lab` never touches its HEAD or working tree, so a human's own work in the main checkout is undisturbed.
- No carry-over of uncommitted main-checkout edits into an instance (out of scope by design — an instance starts from published mainline).
- A repo with no usable origin can't launch an instance until its remote is fixed. There is **no fallback base**: "fail loud so I can see something's wrong" beats silently forking off a stale local HEAD.

## Identity — drop slots, unify on `<project>~<label>`

A session is named `<project>~<label>`, and the label carries everything:

- **AFK run:** `afk-<N>` (manual) / `afk-auto-<N>` (auto), recovered by `parseAFKLabel`.
- **Manual:** `<userlabel>-<timestamp>` when labelled, bare `<timestamp>` when not. `<timestamp>` is `YYYYMMDD-HHMM` (readable, lexically sortable); a same-minute collision bumps the minute until the session name is free.

From the label `lab` derives the run **kind** (`parseAFKLabel`), the **branch** (`afk/<N>` for a run, `lab/<label>` for a manual instance), the **worktree dir** (`<project>-<N>` for a run, `<project>-<label>` for a manual instance), and the **rendered identity** (`AFK #N`, or `<label> · 15:30` / `15:30`). `parseSessionName` splits on the first `~` — project names and labels are both `~`-free after sanitising, so that boundary is unambiguous.

This replaces the old **slot** scheme: `allocateSlot` / `takenSlots`, the `Slot` field, the "lone instance = bare project name" rendering, and AFK's "take slot ≥2 to reserve slot 1" rule are all deleted. Slots existed to multiplex instances that shared one checkout; once each instance has its own worktree there is nothing to multiplex, and the label alone identifies it. The worktree dir keeps its `-` join (never `~`): a `<name>~<digit>` path component matches the Windows 8.3 short-name pattern (`PROGRA~1`) that claude flags as a path-confusion risk, which would stall every unattended edit.

## Start — synchronous, fail-loud

`handleStart` is synchronous: auth check → `git fetch origin` → base `origin/<default>` → `git worktree add -b <branch> <wt> origin/<default>` → `SeedTrust(<wt>)` → spawn in `<wt>`. Any failure (no origin, a failing fetch, an unresolvable `origin/<default>`, a worktree-add or spawn failure) **aborts** Start with the git cause surfaced in the banner, and rolls back any partial worktree + branch (mirroring AFK's `teardownClaim`), so a failed Start leaves nothing behind. A timeout bounds the git ops so a stalled remote fails the request loudly instead of hanging it.

## Teardown — one guarded rule

A single guarded rule governs an instance's worktree + branch:

- **dirty** worktree → keep worktree **and** branch (unsaved work is never destroyed).
- **clean** worktree → remove the worktree; delete the branch **iff** it is already merged into `origin/<default>`, else keep the branch (so unmerged commits aren't lost).

This is the end state for every teardown site — manual Stop, the AFK reaper (success and failure), startup orphan reconciliation, and the runtime merged-sweep. On uncertainty (an unreadable dirty/merged status) it keeps everything, since a clean worktree is reproducible from its branch but destroyed work is not. AFK keeps only its **outcome accounting** on top of the rule: the consecutive-failure counter and the budget-clock neutrality that make a hand-Stop neutral (never reaped as a death), and its Stop keeps the unmerged `afk/<N>` branch so the claim/park ([ADR-0013](0013-afk-claim-is-the-branch.md)) survives.

## Rollout

The design lands in slices so each is independently reviewable:

1. **Slice 1 (#134):** the identity unification (slots dropped), per-instance worktrees for **manual** Start, and the guarded teardown on the **manual** Stop path. AFK Start already used a worktree; it is re-expressed through the shared `worktreePath` / `instanceBranch` derivation but behaves identically (`afk/<N>` branch, `<project>-<N>` dir). AFK Stop stays neutral per ADR-0007 (kills the session, forgets the budget clock, keeps the worktree + `afk/<N>` branch). *(Slice 1 left the reaper removing the worktree on success and keeping it on failure; slice 2 folds it onto the one rule.)*
2. **Slice 2 (#135):** fold the AFK reaper (success **and** failure), startup orphan reconciliation, and a throttled runtime merged-sweep onto the one guarded rule, so a clean worktree is reclaimed and merged `lab/` **and** `afk/` branches and their clean worktrees are GC'd automatically (lab used to keep merged `afk/<N>` branches forever). AFK keeps only its outcome accounting (the consecutive-failure counter and the budget-clock neutrality of a hand Stop).
3. **Slice 3 (#136):** a per-project **Parked** view surfacing dirty/unmerged worktrees and branches with a per-entry Discard.

## Considered options

- **Keep the shared main checkout for manual instances** (slots just multiplex names). Rejected: two instances of one project corrupt each other's index/working tree/branch — the exact bug this fixes. Slots never gave isolation.
- **Carry the main checkout's uncommitted edits into the worktree.** Rejected as out of scope and surprising: an instance starts from published mainline, deterministically. A human who wants their in-progress edits works in the main checkout directly.
- **A fallback base when origin is missing** (fork off local HEAD). Rejected: it hides a broken remote and forks off whatever the main checkout happens to be sitting on. Failing loudly is the point.
- **Keep slots, add worktrees.** Rejected: the slot and the worktree dir/branch would be two identities for one instance, and the "slot 1 = bare name, ≥2 = numbered" special-case is exactly the branch this unification removes. One label, one derivation.

## Consequences

- Manual Start now does a synchronous `git fetch`, so it is slower than the old in-checkout spawn and can fail — intended (fail loud). A project that can't fetch its origin can't launch an instance.
- Manual instances' branches accumulate under `refs/heads/lab/`; the guarded teardown deletes them when clean-and-merged, and the later runtime sweep GCs the rest. A dirty instance's worktree + branch are kept for the human to find (slice 3 surfaces them).
- `lab`'s git ops only ever touch refs + worktrees in the reference repo, never its HEAD/working tree, so manual work in `~/projects/<project>` is undisturbed.
- The session-name change (`<project>~<label>`, no slot) is re-derived from live sessions on restart, so in-flight instances are re-adopted across a `lab` restart exactly as before.
- A non-Forgejo project can still run **manual** instances (it only needs a fetchable origin); AFK runs remain Forgejo-only, since they claim issues via `tea`.
