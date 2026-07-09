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
The captured `https://claude.ai/code/<id>` URL of an instance — read from claude's session registry by worktree-cwd match shortly after spawn — through which the operator drives the session from any device. It is an **optional provider capability** (`DeepLinker`, ADR-0017): only a provider with a web surface captures one, and a miss degrades to that provider's generic **fallback-open** link with a loud log; it never blocks Start. A provider with no web session captures nothing (`deep_link_url` stays NULL) and its rows offer a copyable `tmux attach` instead.
_Avoid_: attach URL, share link, session URL

**Chat**:
The rendered conversation of an instance inside lab's UI (the `/runs/:id` view) — user and assistant messages, tool chips, and pending dialogs — where the operator can reply, answer a dialog, or interrupt. It complements the deep link (the escape hatch), never replaces it, and applies to every instance (manual and AFK).
_Avoid_: terminal, console, session view, thread

**Transcript**:
The provider-native session file the Chat reads through (Claude Code: the live JSONL under `~/.claude/projects/<cwd-slug>/<sessionId>.jsonl`), located by worktree-cwd match and mapped behind the provider seam to the universal message schema (`text | tool | dialog | lifecycle`). Never rendered raw; its path is the only chat state lab persists (`runs.transcript_path`). A retired file degrades to "transcript no longer available".
_Avoid_: log, history, raw output, session file (in UI copy)

**Conversational state**:
The chat tailer's per-instance signal derived from the transcript tail — *working*, *needs input*, *question pending*, or *idle* — served on the instance list and shown as a live state dot on the runs rail and a badge in the chat header. Distinct from `live` (tmux liveness) and the run's terminal outcome.
_Avoid_: status, activity, progress

**Slash-command catalog**:
The provider's slash commands, surfaced in the Chat composer as autocomplete the moment the input starts with `/` (filtered as you type by name, description, and argument hint). Served per instance — project- and user-level commands are discovered relative to the worktree — and curated to **chat-safe** commands only: one that would strand the agent's TUI in a picker lab cannot see is withheld. The entry tagged `role=clear` (Claude Code's `/clear`) also backs a **New conversation** action that clears the instance's context in place; every command executes down the ordinary reply path as pasted text, never a dedicated endpoint.
_Avoid_: command palette, quick switcher, command menu

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
The per-repo choice of tracker backend: `forge` (a forge REST client — Forgejo or GitHub, selected by the forge credential's flavor, ADR-0015) or `builtin`.
_Avoid_: integration, tracker sync, connector

**Run token**:
A short-lived credential (`lab_run_…`) scoped to one run's repo, handed to the session as `LAB_TOKEN` — the agent's only tracker surface, via `labctl`.
_Avoid_: PAT, API token (those are the operator's), personal login

### Providers & seeding

**Provider**:
An `AgentProvider` implementation — spawn argv, auth flow, worktree seeding, model/effort catalog, chat surface — plus optional capabilities it may advertise by type assertion (`DeepLinker` for deep-link capture + fallback-open metadata, `ConnectingReporter` for the connecting pulse; ADR-0017). `claude-code` is the only MVP implementation and implements both. The **effective provider** of a spawn is resolved per spawn by three-level layering — per-spawn pick → repo override → global default, with symmetric AFK overrides — never a fixed per-repo stamp (ADR-0030).
_Avoid_: backend, engine, vendor

**Spawn options**:
The provider-owned, provider-declared bag of spawn settings that sits beside the typed model/effort. The provider *declares* its schema (`SpawnOptions() []OptionSpec`), lab *stores, validates, and renders* the bag generically, and the provider *applies* it (never as positional argv). Validated against the resolving repo's provider schema exactly like model/effort — an unknown key or bad value is a 400. claude-code's only entry is the AFK-only `ultracode` boolean, applied by prepending a provider-owned directive to a non-empty initial prompt; a promptless manual spawn is a natural no-op. Future providers (#2 Codex, Gemini) declare their own — the typed core never churns (ADR-0021).
_Avoid_: flags, provider config, params, feature toggles

**AFK default vs manual pre-fill**:
Two resolutions of the same model/effort/provider/options knobs. A **manual pre-fill** is *soft* — the value the Start form shows pre-filled, which the operator overrides per spawn (request → repo base → global base). An **AFK default** is *hard* — with no operator, it is literally what runs, resolved by layering an optional AFK-override over the base (repo.afk ?? global.afk ?? repo.base ?? global.base); an empty override inherits the base. The base is the shared `spawn_model_default` / `model_default` etc.; the AFK-override slots and the options bag are additive (ADR-0021); provider is layered the same way (ADR-0030). Default-layer resolution is skip-layer: a default not in the effective provider's catalog is treated as unset and falls through; an empty catalog resolves the knob to nothing and omits it; an explicit request value stays strict (a 400).
_Avoid_: preset, profile, per-run override

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
- A manual **instance**'s **deep link** is the operator's handle to it; the deep link is captured best-effort (only for a provider with the `DeepLinker` capability) and survives restarts on the run row — a link-less provider's rows offer a copyable `tmux attach` instead.
- The **chat** reads an instance's **transcript** through the provider seam and lets the operator reply/answer/interrupt; it complements the deep link and applies to every instance. Replying to or interrupting an **AFK run** is a **neutral** intervention — it never touches the **budget clock**, **claim**, or **three-strikes pause**. The tailer's **conversational state** feeds the instance list's live badges.
- **Guarded teardown** runs at all four teardown sites (manual Stop, AFK reaper, startup reconciliation, merged-sweep) and produces **parked work**; the **unguarded Discard** is the only way to destroy it and the only requeue.
- A **neutral Stop** parks the claim and never feeds the **three-strikes pause** counter.
- Each repo has exactly one **tracker binding** and an *effective provider* resolved by layering (the repo override is optional), and may enable **incogni mode**.
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
- **"PR"** — reserved for the forge object. The built-in tracker's object is a **change request**. The one deliberate blur is the agent `labctl pr` surface (`create` / `view` / `list` / `merge` / `checks`), which routes to either object behind the Tracker interface.
- **"reference repo"** — in v0 this was the human's `~/projects/<name>` checkout; here it is always the lab-owned bare clone. The invariant carried over: lab never touches its HEAD or working tree, only refs and worktrees.
- **"token"** — three distinct things: a **PAT** (operator API, `lab_pat_…`), a **run token** (agent, `lab_run_…`), and a *forge token* (a credential kind in the vault, server-side only — never in session env or materialized files; it also carries the forge *flavor* — forgejo or github — and the API origin the REST client targets, ADR-0015).
