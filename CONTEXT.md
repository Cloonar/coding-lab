# lab

Self-hosted orchestrator for remote coding agents: the operator adds git repositories and credentials in a phone-first web UI, then starts interactive or unattended agent sessions against them. This is the repo's single context; the vocabulary below is carried verbatim from the v0 reference ADRs (`docs/reference/lab-v0/adr/`) and the agent brief §6 — identifiers, UI copy, and docs use these terms and nothing else.

**Global avoid-list**: never *job*, *task*, *workspace*, or *merge request* for any concept below. The v0 deleted concepts are a grep-guard and must not reappear: `in-progress` claim label, slots, directory scanning, `tea`, fallback base, auto-retry/auto-requeue.

## Language

### Sessions & worktrees

**Instance**:
A running agent session in its own worktree on its own branch, forked from freshly-fetched `origin/<default>`, identified by the tmux session name `<repo>~<label>`.
_Avoid_: job, task, workspace, slot

**AFK run**:
An unattended instance that takes one `ready-for-agent` issue from the repo's tracker, resolves it, and opens a PR (or change request).
_Avoid_: background job, batch run, bot run

**Deep link**:
The captured `https://claude.ai/code/<id>` URL of an instance — read from claude's session registry by worktree-cwd match shortly after spawn — through which the operator drives the session from any device. Capture failure degrades to the generic `https://claude.ai/code` link with a loud log; it never blocks Start.
_Avoid_: attach URL, share link, session URL

**Reference repo**:
The lab-owned bare clone at `<state>/repos/<id>.git` — the worktree parent and host for all fetch/branch/worktree git ops, never an instance's cwd. (v0 meant the human's main checkout; bare means structurally never dirty.)
_Avoid_: main checkout, scan root, mirror

**Parked work / Parked view**:
Worktrees and branches that guarded teardown declined to destroy (dirty, or unmerged); the Parked view lists them per repo with a per-entry Discard.
_Avoid_: stale, orphaned, garbage, leftovers

**Guarded teardown**:
The one teardown rule at every teardown site: dirty keeps everything; clean removes the worktree and deletes the branch only if merged into `origin/<default>`; uncertainty keeps everything.
_Avoid_: cleanup, force delete

**Unguarded Discard**:
The single deliberate exception to guarded teardown — a per-entry, human-initiated removal from the Parked view; deleting the branch is also the only requeue action.
_Avoid_: auto-requeue, purge

### AFK engine

**Claim**:
The existence of the run's branch (rendered from the repo's `afk_branch_pattern`, default `afk/<N>`); creating the branch is claiming the issue, tearing it down releases it.
_Avoid_: `in-progress` label, lock, assignment, claim row/flag in the DB

**Done-signal**:
A PR or change request whose head branch equals the run's branch (state open or merged) — session exit is never the done-signal, because `--remote-control` idles after finishing.
_Avoid_: session exit, exit code, completion event

**Ready queue**:
A repo's open issues carrying the `ready-for-agent` label, exactly as its tracker reports them (`Tracker.ReadyIssues`) — the only pool AFK selection draws from.
_Avoid_: backlog, todo list, queue table

**Claimable**:
The ready queue minus already-branched issues; the auto-loop's `(N ready)` hint and its launch predicate both count the *claimable* set, so a repo whose only ready issues are all parked reads zero and does not loop (reference ADR-0013).
_Avoid_: available, unassigned, free

**Budget clock**:
An AFK run's wall-clock budget — `afk_budget_minutes` (default 120, per-repo override), persisted as `budget_deadline` on the run row at launch (D12b) so a restart re-adopts the run with its deadline intact. Expiry without a done-signal classifies the run as timeout.
_Avoid_: idle timeout, deadline extension, reset-on-restart

**Three-strikes pause**:
Three consecutive AFK failures (death or timeout) pause a repo's auto runs until an explicit human Reset from the UI.
_Avoid_: auto-retry, backoff, cooldown

**Neutral Stop**:
A user-initiated Stop that never counts as a failure or death, keeps the worktree and unmerged branch (the claim/park survives), and leaves the failure counter untouched.
_Avoid_: cancel, abort, kill (the tmux kill is a mechanism, not the outcome)

### Trackers

**Change request**:
A lab-internal PR in the built-in tracker: head branch, base branch, title, body with `Closes #N`, state open|merged|closed, reviewable and mergeable from the UI.
_Avoid_: merge request, pull request ("PR" is the forge object)

**Tracker binding**:
The per-repo choice of tracker backend: `forge` (Forgejo REST; GitHub is fast-follow) or `builtin`.
_Avoid_: integration, tracker sync, connector

**Run token**:
A short-lived credential (`lab_run_…`) scoped to one run's repo, handed to the session as `LAB_TOKEN` — the agent's only tracker surface, via `labctl`.
_Avoid_: PAT, API token (those are the operator's), personal login

### Providers & seeding

**Provider**:
An `AgentProvider` implementation — spawn argv, auth flow, deep-link capture, worktree seeding, model/effort catalog; `claude-code` is the only MVP implementation.
_Avoid_: backend, engine, vendor

**Skills bundle**:
The vendored, pinned skill set at `assets/skills/`, embedded in the binary and copied into each worktree's `.claude/skills/` at spawn (listed in `.git/info/exclude`, never committable).
_Avoid_: plugins, user-level skills

**Incogni mode**:
A per-repo flag applying seven leak-prevention measures so a run's output carries no AI attribution — neutral branch patterns, sanitized bodies, real git identity, pre-push guard. It cannot hide the forge account of the token used, nor style/timing signals.
_Avoid_: stealth mode, anonymous mode

### Security

**Master key**:
The 32-byte key (64 hex chars in a 0600 file, path configurable for sops-nix/LoadCredential) that encrypts credentials at rest with AES-256-GCM.
_Avoid_: password, keyring, vault key file synonyms

## Relationships

- An **instance** is manual or an **AFK run**; every instance runs in its own worktree forked from the **reference repo**'s freshly-fetched `origin/<default>` — no fallback base, ever.
- An **AFK run**'s **claim** is its branch and nothing else; selection skips issues whose branch exists and never consults the PR list — the PR/CR list is the reaper's **done-signal** only.
- The scheduler counts the **claimable** set (**ready queue** minus existing claims); an AFK run that outlives its **budget clock** without a done-signal is a timeout, and timeouts (like deaths) feed the **three-strikes pause**.
- A manual **instance**'s **deep link** is the operator's handle to it; the deep link is captured best-effort and survives restarts on the run row.
- **Guarded teardown** runs at all four teardown sites (manual Stop, AFK reaper, startup reconciliation, merged-sweep) and produces **parked work**; the **unguarded Discard** is the only way to destroy it and the only requeue.
- A **neutral Stop** parks the claim and never feeds the **three-strikes pause** counter.
- Each repo has exactly one **tracker binding** and one **provider**, and may enable **incogni mode**.
- Every AFK run receives one **run token**; every spawn (manual or AFK) seeds the **skills bundle** and `CLAUDE.local.md`.
- The **master key** encrypts every credential; a repo's git credential reaches sessions via `GIT_SSH_COMMAND`/`GIT_ASKPASS`, its forge token never does.
- On a `forge`-bound repo the done-signal is a PR; on a `builtin`-bound repo it is a **change request** — one contract, one reaper.

## Example dialogue

> **Dev:** "When an **AFK run** finishes its issue, do we mark the run done when the session exits?"
> **Domain expert:** "Never — `--remote-control` idles after opening the PR. The **done-signal** is a PR or **change request** whose head branch equals the run's branch. Session death *without* that PR is a failure."
>
> **Dev:** "And the failed run's branch — should the reaper delete it so the issue re-enters the queue?"
> **Domain expert:** "No. **Guarded teardown** keeps unmerged branches, which parks the issue. Requeue is a human decision: the **unguarded Discard** in the Parked view deletes the branch, and only that releases the **claim**. Auto-requeue is the runaway-cost nightmare we rejected twice."
>
> **Dev:** "If the operator hits Stop mid-run, is that strike two?"
> **Domain expert:** "No — a **neutral Stop** never counts. Only deaths and timeouts feed the **three-strikes pause**."

## Flagged ambiguities

- **"session" vs "instance"** — a tmux session is the process container; the domain object is the **instance**. Resolution: say *instance* everywhere; "session name" appears only as the tmux identity `<repo>~<label>`.
- **"PR"** — reserved for the forge object. The built-in tracker's object is a **change request**. The one deliberate blur is the agent command `labctl pr create`, which routes to either behind the Tracker interface.
- **"reference repo"** — in v0 this was the human's `~/projects/<name>` checkout; here it is always the lab-owned bare clone. The invariant carried over: lab never touches its HEAD or working tree, only refs and worktrees.
- **"token"** — three distinct things: a **PAT** (operator API, `lab_pat_…`), a **run token** (agent, `lab_run_…`), and a *forge token* (a credential kind in the vault, server-side only — never in session env or materialized files).
