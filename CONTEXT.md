# lab

Self-hosted orchestrator for remote coding agents: the operator adds git repositories and credentials in a phone-first web UI, then starts interactive or unattended agent sessions against them. This is the repo's single context; the vocabulary below is carried verbatim from the v0 reference ADRs (`docs/reference/lab-v0/adr/`) and the agent brief §6 — identifiers, UI copy, and docs use these terms and nothing else.

**Global avoid-list**: never *job*, *task*, *workspace*, or *merge request* for any concept below. The v0 deleted concepts are a grep-guard and must not reappear: `in-progress` claim label, slots, directory scanning, `tea`, fallback base, auto-retry/auto-requeue.

## Language

### Sessions & worktrees

**Instance**:
A running agent session in its own worktree on its own branch, forked from freshly-fetched `origin/<default>`, identified by the tmux session name `<repo>~<label>`.
_Avoid_: job, task, workspace, slot

**Runner**:
The per-repo pick of where an instance's pane command executes (`repos.runner`, NOT NULL, default `host`). `host` runs the provider CLI directly on the host under the prlimit nofile cap — the unsandboxed break-glass, labeled "unsandboxed — full host access" in the UI. `container` makes the pane command a rootless podman + crun `podman run -it --rm` in the repo's **dev image** (`repos.image_ref`, else the global `--container-image` default; ADR-0053), carrying ADR-0052's mount inventory (worktree and bare rw at host-identical paths, the agent socket directory, the instance HOME at `/home/agent`, the per-run runtime dir, each **read-only import**'s snapshot read-only at its host-identical path, the **agent-tools image** read-only at `/opt/lab`), while tmux stays host-side and keeps owning liveness, attach, send-keys, and capture. A container spawn on a host that fails the startup preflight is refused with the collected actionable failures.
_Avoid_: sandbox mode, executor, isolation level, backend

**Title**:
A user-set display name on an instance, stored as nullable `runs.title`; it overrides the label-derived title everywhere the UI names the instance — chat header, runs rail, History — but never becomes identity, since branch, worktree, and tmux session name all stay keyed off the label regardless (ADR-0040).
_Avoid_: nickname, alias, custom label, session name

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
The provider's slash commands plus lab's own **lab commands**, surfaced in the Chat composer as autocomplete the moment the input starts with `/` (filtered as you type by name, description, and argument hint). Served per instance — project- and user-level commands are discovered relative to the worktree — and curated to **chat-safe** commands only: one that would strand the agent's TUI in a picker lab cannot see is withheld. The entry tagged `role=clear` (Claude Code's `/clear`) also backs a **New conversation** action that clears the instance's context in place; a provider command executes down the ordinary reply path as pasted text, never a dedicated endpoint, while a **lab command** is intercepted by the server and never reaches the provider.
_Avoid_: command palette, quick switcher, command menu

**Lab command**:
A composer slash command the server intercepts and executes itself, never forwarded to the provider — surfaced in the **slash-command catalog** alongside provider commands (source `lab`) for every instance, AFK runs included. `/pull-base` is the first: it fetches the **reference repo**, merges `origin/<base>` into the instance's worktree (aborting with the worktree untouched on conflict), re-materializes every **read-only import** in place, and repairs the context layer by injecting a digest of what changed down the ordinary reply path — one line per import included, a failed import refresh reported there rather than aborting the pull.
_Avoid_: server command, built-in command, magic command

**Reference repo**:
The lab-owned bare clone at `<state>/repos/<id>.git` — the worktree parent and host for all fetch/branch/worktree git ops, never an instance's cwd. (v0 meant the human's main checkout; bare means structurally never dirty.)
_Avoid_: main checkout, scan root, mirror

**Read-only import**:
Another lab repo whose code this repo's **instances** may read — declared consumer-side in the importing repo's settings (`repo_imports`, targets by repo ID, registered repos only), so a repo's whole readable world is one list in one place. The grant is directional and flat: mutual imports are legal, self-import and unknown targets are rejected at save, and a target's own imports never come along. At every spawn — every run kind, by construction through the one `instance.Launch` — lab fetches the target's **reference repo** and materializes a snapshot of `origin/<default>`'s tree (no `.git`, therefore no history) into the per-run directory at `<state>/instances/<runID>/imports/<name>/`, a third sibling of the instance HOME and runtime dir, outside the worktree; the `container`-**Runner** mounts each snapshot read-only at its host-identical path and the `host` runner best-effort `chmod a-w`s the same path. The generated context file names each import with its absolute path and snapshotted commit. A failed import fetch refuses the spawn *before* the **claim**, naming the target, so a target-side outage never parks an issue. The snapshot is a dead copy: never written back, never shared or cached across runs, refreshed only when `/pull-base` re-materializes it in place, and gone with the per-run directory at teardown. Code only — the **run token** stays scoped to the run's own repo, so an import never opens its target's issues or PRs. Deleting a repo other repos import is refused with the importers named, and `force` does not bypass that (ADR-0063).
_Avoid_: dependency, submodule, link, mount (the mechanism, not the concept), sibling, referenced repo

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
A PR or change request whose head branch equals the run's branch (state open or merged) — session exit is never the done-signal, because the agent CLI idles at its composer after finishing, remote control on or off. The old reasoning ("because `--remote-control` idles") was wrong about the cause, not the conclusion: a live A/B against claude 2.1.206 (compat §12, the issue #163 spike) found a session spawned WITHOUT `--remote-control` still sleeping at its prompt ~12 minutes past turn end, exactly like the remote control arm. Session death without a PR is a failure, never a completion.
_Avoid_: session exit, exit code, completion event

**Ready queue**:
A repo's open issues carrying the `ready-for-agent` label, exactly as its tracker reports them (`Tracker.ReadyIssues`) — the only pool AFK selection draws from.
_Avoid_: backlog, todo list, queue table

**Claimable**:
The ready queue minus already-branched issues and minus issues whose `## Blocked by` body section references a still-open issue (ADR-0042); the auto-loop's `(N ready)` hint and its launch predicate both count the *claimable* set, so a repo whose only ready issues are all parked or all blocked reads zero and does not loop (reference ADR-0013).
_Avoid_: available, unassigned, free

**Spawn pass**:
The fleet's one serialized launch decision (`SpawnOnce`, ADR-0049) and the sole consumer of a repo's live-instance cap: producers *gather* launch candidates and never launch themselves — a **lander run** per open **claim** PR awaiting validation, a **fix run** per rejected claim PR under its attempt bound (#182, sharing its middle **stage rank** with the escalate run that concludes the loop), a new **AFK run** per **claimable** issue — then the pass stable-sorts by stage rank and launches down the list while the repo is under cap. The ordering rule is **drain before fill**: work already in flight outranks starting new work (`lander > fix > new AFK work`), so a repo at cap validates and lands its queued PRs before opening more. Replaces the two independent loops (the scheduler tick and the reaper's autoland sweep) that each read the cap against their own snapshot and raced for it; the pass now runs on the reaper tick (after the reap), the scheduler tick, and the toggle-on/reset kicks, with the per-launch cap guards under the engine lock as the backstop.
_Avoid_: spawn loop, scheduler loop, spawn queue, spawn worker

**Budget clock**:
An AFK run's wall-clock budget — `afk_budget_minutes` (default 120, per-repo override), persisted as `budget_deadline` on the run row at launch (D12b) so a restart re-adopts the run with its deadline intact. Expiry without a done-signal classifies the run as timeout.
_Avoid_: idle timeout, deadline extension, reset-on-restart

**Three-strikes pause**:
Three consecutive AFK failures (death or timeout) pause a repo's auto runs until an explicit human Reset from the UI. Only AFK-run outcomes feed the counter: a **lander run**'s outcome never moves it in either direction — a lander death or timeout must not pause unrelated AFK work, and a lander's reject-success must not clear the strikes the rejected work earned (ADR-0049) — and a **scheduled run**'s outcome never moves it in either direction either, feeding its **Schedule**'s own consecutive-failure counter instead, just as this pause never stops a repo's Schedules (ADR-0062). The **budget clock** stays shared across all run kinds.
_Avoid_: auto-retry, backoff, cooldown

**Neutral Stop**:
A user-initiated Stop that never counts as a failure or death, keeps the worktree and unmerged branch (the claim/park survives), and leaves the failure counter untouched.
_Avoid_: cancel, abort, kill (the tmux kill is a mechanism, not the outcome)

**Autoland**:
The per-repo, default-off pipeline that closes ADR-0024's deferred reject-loop: a state-derived poller reads the PR's verdict state (lander verdicts plus human native reviews, ADR-0048) and the runs store, and gathers **lander run** candidates (validate a **claim**'s PR against the validation core, then merge / approve / reject) and **fix run** candidates for the fleet **spawn pass** (ADR-0049) — nothing is dispatched by message, and the engine never writes to a forge. Forge-only for now; the builtin binding joins once it grows PR-comment writes.
_Avoid_: merge bot, merge queue, auto-merge pipeline, webhook

**Fix-forward**:
Bounded re-engagement of a rejected PR — a **fix run** spawned onto the existing **claim** branch carrying the rejection's findings as new information, its **done-signal** an explicit `labctl pr rerequest` (never a fresh PR, since the claim's PR already exists), bounded by fix-launch ATTEMPTS per branch (`max_fix_attempts`, counted in `autoland_attempts` — intents ever, never rejection verdicts and never surviving runs rows, so a fix run that dies on the launch pad still burns an attempt; a runs-row count could not, because Start's rollback deletes the row and the pre-CreateRun failures never write one). A human's native changes-requested review feeds the same loop as a lander rejection, but is cleared only by that human's newer review — the loop never merges over one. Explicitly not auto-requeue: blind retry that carries no new information stays forbidden.
_Avoid_: auto-requeue, auto-retry, re-queue, respin

**Escalation**:
The fix-forward loop's terminal hand-off (#182): at the attempt bound with the PR still rejected, the poller spawns an **escalate run** — a lander-class run that writes the round-history digest to the linked issue, flips its label `ready-for-agent` → `ready-for-human`, and posts the terminal `labctl pr escalate` marker, all agent-executed on the run-token boundary (the engine never writes to a forge; the engine's part is the push notification off the run's `escalated` outcome). An escalated-outcome run makes its PR invisible to **Autoland** — scoped to the PR itself, not the **claim** branch, and supersedable rather than permanent (#188). Re-entry is two paths: a human running the interactive land-pr skill, or a human **re-arming** the PR — an SPA action or the `lab autoland rearm` CLI verb, human-authenticated only and never a `labctl` run-token verb, since an agent able to lift its own terminal hand-off would make the bound decorative — which clears the terminality and restores the PR's spent `fix`/`escalate` attempt budgets in the same atomic operation. A fresh escalation after a re-arm is terminal again, indefinitely repeatable. The hand-off is itself bounded (`MaxEscalateAttempts`, 3): terminality is written only when the marker lands, so an escalate run that dies first leaves the PR rejected-at-bound and would otherwise be re-escalated every tick, forever, ahead of new work and un-braked by three-strikes. Past that bound autoland goes quiet on the PR and says so at error level.
_Avoid_: give up, abandon, dead-letter, auto-close

**Schedule**:
The per-repo domain object that gives lab a cadence: a name, the cadence itself stored as a cron expression (server-local time, edited phone-first as daily/weekly/monthly presets that render to cron, with a raw-cron escape hatch previewed server-side from the same parser), a freeform prompt, an ordered selection of built-in **flows**, an enabled flag, optional budget/model/effort/provider overrides, and its own consecutive-failure counter and paused state (`schedules` table, `runs.schedule_id` linking a firing's run). A due firing gathers a **scheduled run** candidate for the fleet's **spawn pass** — never an independent launch path, because a second launch path is exactly the cap race ADR-0049 removed. Two rules keep a cadence from stacking unattended cost: skip-on-overlap (a firing due while the Schedule's previous run is still live is skipped loudly — consumed, never queued behind it) and no catch-up (slots missed while the server was down are gone, startup only logs per Schedule which ones were; the next cron match fires normally) (ADR-0062).
_Avoid_: task, job, cron entry, timer, recurring run

**Scheduled run**:
The fourth unattended run kind (`runs.kind = 'scheduled'`, attributed to its **Schedule** by `runs.schedule_id`), spawned by a due firing with full spawn parity to an **AFK run** — its own worktree and branch off freshly-fetched `origin/<default>`, a **run token**, **skills bundle** plus context-file seeding, the repo's **Runner** and **dev image**, **incogni mode** screening — and inheriting them by construction, not by new code. Its initial prompt is the Schedule's freeform prompt plus the selected **flows**' blocks in fixed catalog order, and its knobs resolve by per-schedule overrides sitting as one more skip-layer rung above the **AFK default** layering. It carries no issue, no **claim**, and no **done-signal**: a PR is not expected, so the reaper never reads a pull listing for it, and termination is the **budget clock** alone (default 30 minutes, per-schedule override), expiry classifying the run success through **guarded teardown**. Session death before expiry is a failure feeding that Schedule's own three-strikes — three consecutive deaths pause the Schedule until a human re-enables it — never the repo's AFK counter, and the repo's **three-strikes pause** never stops Schedules (ADR-0062).
_Avoid_: cron run, periodic run, batch run

**Flow**:
A built-in, lab-vendored block of routing instructions appended to a **Schedule**'s freeform prompt at spawn, in fixed catalog order rather than selection order — composition is deterministic and catalog-owned, so two Schedules selecting the same flows brief their runs identically. It is pure prompt text composed provider-agnostically, the same mechanism precedent as the ultracode directive (ADR-0021), and versioned with the binary in the **skills bundle** spirit so the `labctl` verbs and label names it names stay correct as lab evolves. The shipped catalog is two: the **autolander flow** (investigate per the prompt, then file fully-specified `ready-for-agent` issues and stop — the ordinary AFK machinery claims them and, where **Autoland** is on, lands them) and the **human-triage flow** (the same, labeled `needs-triage` for the triage state machine), both baking in dedup against that Schedule's own earlier open issues. Selecting zero flows is legal — a pure-prompt Schedule. Flows are text, not machinery: neither Autoland nor the **lander run** changes for them (ADR-0062).
_Avoid_: workflow, pipeline, playbook, template

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

**Conformance suite**:
The two-tier bar an `AgentProvider` adapter must pass before it ships: Tier 1, `providertest.Conformance` run in CI against every registered adapter (patterns dialect, seeding, scrub markers, catalogs, and more); Tier 2, four required live spikes (transcript identity, dialog/interrupt hazard-check, context-file discovery, incogni attribution ground truth) evidenced by a committed compat record at `internal/compat/<providerID>/compat.md` (ADR-0036). Passing both, not review alone, is the adapter's definition of done.
_Avoid_: test suite, checklist (alone), acceptance criteria

**Spawn options**:
The provider-owned, provider-declared bag of spawn settings that sits beside the typed model/effort. The provider *declares* its schema (`SpawnOptions() []OptionSpec`), lab *stores, validates, and renders* the bag generically, and the provider *applies* it (never as positional argv). Validated against the resolving repo's provider schema exactly like model/effort — an unknown key or bad value is a 400. claude-code's only entry is the AFK-only `ultracode` boolean, applied by prepending a provider-owned directive to a non-empty initial prompt; a promptless manual spawn is a natural no-op. Future providers (#2 Codex, Gemini) declare their own — the typed core never churns (ADR-0021).
_Avoid_: flags, provider config, params, feature toggles

**AFK default vs manual pre-fill**:
Two resolutions of the same model/effort/provider/options knobs. A **manual pre-fill** is *soft* — the value the Start form shows pre-filled, which the operator overrides per spawn (request → repo base → global base). An **AFK default** is *hard* — with no operator, it is literally what runs, resolved by layering an optional AFK-override over the base (repo.afk ?? global.afk ?? repo.base ?? global.base); an empty override inherits the base. The base is the shared `spawn_model_default` / `model_default` etc.; the AFK-override slots and the options bag are additive (ADR-0021); provider is layered the same way (ADR-0030). Default-layer resolution is skip-layer: a default not in the effective provider's catalog is treated as unset and falls through; an empty catalog resolves the knob to nothing and omits it; an explicit request value stays strict (a 400).
_Avoid_: preset, profile, per-run override

**Skills bundle**:
The vendored, pinned skill set at `assets/skills/`, embedded in the binary (a single `go:embed` source) and copied verbatim into each worktree at the provider's declared `SeedMeta.SkillsDir` at spawn (claude: `.claude/skills/`; listed in `.git/info/exclude`, never committable). The same bundle seeds every provider; a provider without native skill discovery instead learns of it through a **skills index** appended to its generated context file — one pointer line per skill, rendered from the skill's frontmatter (ADR-0035).
_Avoid_: plugins, user-level skills

**Incogni mode**:
A per-repo flag applying seven leak-prevention measures so a run's output carries no AI attribution — neutral branch patterns, sanitized bodies, real git identity, pre-push guard. Sanitized bodies and the pre-push guard both screen against the union of every registered provider's declared markers, not just the repo's own provider (ADR-0033), so a per-session provider override (ADR-0030) is never screened by the wrong provider's patterns. It cannot hide the forge account of the token used, nor style/timing signals.
_Avoid_: stealth mode, anonymous mode

**Agent-tools image**:
The per-provider OCI image (`agent-tools:<provider>-<cli-version>` on the forge package registry) carrying exactly one provider's self-contained CLI (claude-code's native musl `claude`, codex's static `codex`) plus a static `labctl`, and nothing else — `FROM scratch`, never run as a container. The container runner mounts it read-only at `/opt/lab` into the repo's chosen **dev image** (`repos.image_ref`) and prepends `/opt/lab/bin` to PATH, so the agent surface (provider CLI + `labctl`) travels with lab instead of being baked into the dev image. The `/opt/lab` destination is a hard contract: the claude binary's ELF interpreter is rewritten to `/opt/lab/lib/ld-musl-x86_64.so.1` at build time, so it runs on a glibc or musl base without either's libc. Images rebuild only when their inputs change; the NixOS module defaults `toolsImages` to the version tags its own `versions.env` pins, the deploy pin advances only once those refs resolve, and preflight re-pulls the tags per boot (ADR-0054). Hand-set consumers pin the immutable `@sha256:` digest, never a moving tag (ADR-0051).
_Avoid_: tools container, sidecar image, base image

**Dev image**:
The per-repo OCI image a `container`-**Runner** instance runs in — `repos.image_ref` (nullable; repo settings → **Runner**, the "Dev image" field), NULL inheriting the global `--container-image` **default**. The operator owns its userland (a shell, coreutils, `git`, an ssh client for ssh-remoted repos) and nothing lab-specific: lab injects only the **agent-tools image** read-only at `/opt/lab` and the repo's **read-only imports** at their host-identical paths, reserving every mount point it uses. A fully-qualified ref, resolved tag→digest against the registry (anonymous, HTTPS-only) and stored pinned as `host/path:tag@sha256:…` on save — so what runs is exactly what was reviewed — and pulled if missing at spawn, before the run claims anything (ADR-0053).
_Avoid_: base image, container image, sandbox image

### Security

**Master key**:
The 32-byte key (64 hex chars in a 0600 file, path configurable for sops-nix/LoadCredential) that encrypts credentials at rest with AES-256-GCM.
_Avoid_: password, keyring, vault key file synonyms

**VAPID key**:
The P-256 keypair (64 hex chars in a 0600 file, path configurable) that signs the VAPID JWT (RFC 8292) authenticating lab to a browser's push service. Carries the **master key**'s file contract verbatim — auto-generated on first start, refuses loose permissions or malformed content, never overwritten — but is a separate key: it signs Web Push sends, it never touches vault-encrypted credentials. Rotating or deleting it strands every **push subscription**.
_Avoid_: push key, notification key, subscription key

**Credential gateway**:
The OneCLI sidecar (`github.com/onecli/onecli`) a run's outbound HTTPS is routed through, so an **instance** holds no real secret value at all: the gateway matches each outbound request against the run's repo's **grants** and injects the real credential at the network layer, fronted on lab's side by the `internal/onecli` package. Two ports, deliberately two URL settings, because lab reaches the sidecar on loopback while a containerized run cannot: 10254 is the dashboard/API lab's own REST client dials (`--onecli-url`, paired with `--onecli-api-key-file`, unset leaving the integration off); 10255 is the gateway proxy a run is handed as `HTTPS_PROXY` (`--onecli-gateway-url`, independently settable — the run wiring and the reachability probe consume it directly, never through the REST API). A run is *wired* only when the REST pair and the gateway URL are all set; a lab with the REST pair alone gets the health surface and unchanged spawns, which is not a refusal. Because the proxy terminates TLS, a wired run needs a fourth setting: `--onecli-ca-file` names the gateway's interception CA on the host, which lab composes with the host's system roots into a per-run trust bundle in the run's runtime dir — reaching a `container` run through that dir's existing host-identical bind mount — and points `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, and `GIT_SSL_CAINFO` at that bundle, never at the bare interception CA, which would leave the run unable to verify the direct HTTPS its `NO_PROXY` protects (the lab host, the repo's forge host, and the resolving **provider**'s declared direct API hosts, so a gateway outage takes down secret-backed API calls and nothing else). The **Runner**'s `container` mode is exactly why the two cannot collapse into one setting: it pins `host.containers.internal` and `host.docker.internal` to `127.0.0.1` *inside* the container (ADR-0052, issue #216), so a containerized run cannot reach anything bound only to the host's loopback — the gateway port must therefore be bound on, and named by, an address routable from the container's netns (ADR-0067). Not the **vault**: the vault encrypts lab's own credentials at rest and materializes git auth per operation (ADR-0006) and is untouched by the gateway, whose scope is repo secrets only.
_Avoid_: proxy (bare), secret manager, vault (for this), sidecar (alone), credential broker

**Grant**:
One **credential gateway** resource — a stored secret or an app connection in the lab's single OneCLI project pool — made usable by one **agent identity**. Per-repo secret assignment *is* the grant set on that repo's agent; there is no parallel lab-side model. Attached and detached through OneCLI's REST API (`PUT`/`DELETE /v1/agents/{id}/grants/{secrets|connections}/{id}`), never by copying a value (ADR-0067). Not the **Read-only import**'s "grant" — that one is a directional, flat repo→repo code-read permission and a wholly different concept; see **Flagged ambiguities**.
_Avoid_: permission, assignment, binding, share, ACL entry

**Agent identity**:
The OneCLI agent object lab maps one-to-one onto a lab **repo**: the unit the **credential gateway** authenticates (a per-agent token in the `Proxy-Authorization` header) and authorizes (its **grants**). One OneCLI project for the whole lab, one agent per repo, created lazily and idempotently by name — the repo's store ID, never its display name, so the **grants** survive a rename — so concurrent spawns converge on one object rather than duplicating it. Its proxy token rides in the run's `HTTPS_PROXY` as userinfo, which makes that variable a credential (forwarded by name through tmux, never in a podman argv), and lab mints it at most once per identity per lab process because minting regenerates it: a lab restart therefore leaves an already-running run holding a token the gateway no longer honours, and that run's proxied calls fail per request until it is re-started (ADR-0067). Not a lab **run token** — that is lab's own tracker-scoped credential, `lab_run_…` — and not a **provider**: a OneCLI agent is a credential-scoping identity, while "agent" everywhere else in this repo means the coding agent; see **Flagged ambiguities**.
_Avoid_: service account, principal, client, identity (bare), agent (bare, in secret contexts)

### Notifications

**Push subscription**:
A device's Web Push registration — endpoint URL plus the browser's `p256dh`/`auth` keys — stored one row per device, not per user: it survives logout, is never vault-encrypted (unlike a credential), and is removed only explicitly (Settings → Notifications → Remove) or by the sender reaping it after a gateway 404/410. Enabling one is a user-gesture action from the settings page, never scripted or pre-provisioned.
_Avoid_: device token, registration, push token

**Needs-input trigger**:
The edge-triggered push fired when a run's **conversational state** settles into `needs_input`/`question` for the flap-debounce window (~2s) — an injected seam at the chat tailer's transition edge, never a bus subscriber (the bus drops events for a slow subscriber by design; a dropped event may not cost a notification). One send per episode, tag = run ID so a newer question replaces the stale lock-screen item, route is the run's PWA-internal chat path. Re-adopting a run already awaiting the operator deliberately re-fires; a run ending fires nothing. No periodic re-reminders.
_Avoid_: reminder, alert, nag, bus subscriber

**Done-signal trigger**:
The single push fired when the reaper's terminal classification of an **AFK run** is success — its **done-signal** landed. It rides the reaper's idempotent **claim**, so it sends exactly once per run with no debounce state (a reaped row leaves the active set and is never a reap candidate again). Title names the PR/**change request** by the repo's **tracker binding** (`<repo>~<label> opened PR #n` on `forge`, `… opened change request #n` on `builtin`), body is that pull's title (degrading to the bare number form when the detail fetch fails), tag = run ID so it replaces the same run's **needs-input trigger** item on the lock screen, route is the run's PWA-internal chat path — never the forge URL. Deaths, timeouts, a **neutral Stop**, and the **three-strikes pause**: no send.
_Avoid_: forge link-out, completion email, per-outcome alerts

**Schedule-paused trigger**:
The single push fired when a **Schedule**'s own three-strikes pauses it — `Schedule <name> paused after 3 failures` — riding the same Web Push machinery as the other two through the afk notification seam. It sends once, at the pause transition, and it is the only push a Schedule ever costs: firings and **scheduled run** completions stay silent, so a healthy weekly Schedule is worth zero notifications. Route is PWA-internal — the repo's Schedules settings, never a forge URL (ADR-0062).
_Avoid_: failure alert, error notification, per-firing push

## Relationships

- An **instance** is manual or an **AFK run**; every instance runs in its own worktree forked from the **reference repo**'s freshly-fetched `origin/<default>` — no fallback base, ever.
- A repo's **Runner** decides where each of its **instances** executes: `container` turns the pane command into rootless `podman run` in the repo's **dev image** (`repos.image_ref`, else the global `--container-image` default; digest-pinned on save, pulled if missing before the run claims anything, ADR-0053), mounting the worktree, the **reference repo**, the agent socket directory, the per-run HOME/runtime tree, and the **agent-tools image** at `/opt/lab` — and is refused at spawn while the startup preflight finds the host unready, when no effective image resolves (repo ref blank *and* global unset), or when the image cannot be pulled (for AFK work the refusal lands before the **claim**, so no issue is parked); `host` is the prlimit-capped break-glass.
- A repo's **read-only imports** are the other lab repos its **instances** may read — consumer-declared, directional, flat, and code-only: each is materialized at every spawn as a `.git`-less snapshot of the target's freshly-fetched `origin/<default>` into the run's own directory outside the worktree, mounted read-only at a host-identical path, refreshed only by `/pull-base`, and destroyed with the run's directory at teardown. A fetch failure refuses the spawn before the **claim** (naming the target, so the issue stays claimable), deleting a repo others import is refused with the importers named and `force` does not bypass it, and the **run token** never widens — an import grants code, never a target's tracker.
- An **AFK run**'s **claim** is its branch and nothing else; selection skips issues whose branch exists and never consults the PR list — the reaper's **done-signal** is a bounded per-branch pull lookup (open or merged, head = the run's branch), never a listing.
- The scheduler counts the **claimable** set (**ready queue** minus existing claims, minus issues whose `## Blocked by` section references a still-open issue); an AFK run that outlives its **budget clock** without a done-signal is a timeout, and timeouts (like deaths) feed the **three-strikes pause**.
- **Autoland** (per-repo, default-off, forge-only) reads the PR's verdict state — lander verdicts plus human native reviews — to feed the fleet **spawn pass** a **lander run** candidate that validates a **claim**'s PR and a **fix run** candidate that re-engages a rejected one on the existing claim branch; the fix run's **done-signal** is an explicit `labctl pr rerequest`, not a fresh PR, because the claim's PR already exists.
- A manual **instance**'s **deep link** is the operator's handle to it; the deep link is captured best-effort (only for a provider with the `DeepLinker` capability) and survives restarts on the run row — a link-less provider's rows offer a copyable `tmux attach` instead.
- The **chat** reads an instance's **transcript** through the provider seam and lets the operator reply/answer/interrupt; it complements the deep link and applies to every instance. Replying to or interrupting an **AFK run** is a **neutral** intervention — it never touches the **budget clock**, **claim**, or **three-strikes pause**. The tailer's **conversational state** feeds the instance list's live badges.
- **Guarded teardown** runs at all four teardown sites (manual Stop, AFK reaper, startup reconciliation, merged-sweep) and produces **parked work**; the **unguarded Discard** is the only way to destroy it and the only requeue.
- A **neutral Stop** parks the claim and never feeds the **three-strikes pause** counter.
- Each repo has exactly one **tracker binding** and an *effective provider* resolved by layering (the repo override is optional), and may enable **incogni mode**.
- Every AFK run receives one **run token**; every spawn (manual or AFK) seeds the **skills bundle** and `CLAUDE.local.md`.
- The **master key** encrypts every credential; a repo's git credential reaches sessions via `GIT_SSH_COMMAND`/`GIT_ASKPASS`, its forge token never does.
- On a `forge`-bound repo the done-signal is a PR; on a `builtin`-bound repo it is a **change request** — one contract, one reaper.
- The **VAPID key** signs every send to a **push subscription**; unlike the **master key**, it never touches the vault — a subscription is device-level trust, not a stored credential, and rotating the **VAPID key** strands every **push subscription** until each device re-enables.
- The **needs-input trigger** rides the tailer's **conversational state** edge and broadcasts to every **push subscription** — device targeting, re-reminders, and escalation are all non-features in the v1 model.
- The **done-signal trigger** rides the reaper's idempotent **claim** and broadcasts to every **push subscription** exactly once when a run reaps as success — never on a death, timeout, or **neutral Stop**.
- A **Schedule**'s due firing gathers a **scheduled run** candidate for the one **spawn pass** at stage rank `lander > fix > scheduled > new AFK` — in-flight claim work still drains first, but a time-anchored firing outranks fresh AFK selection because its slot has already passed — and the repo's live-instance cap binds it like every other candidate; a firing due while that Schedule's previous run is still live is skipped loudly rather than queued, and slots missed while the server was down are never caught up.
- A **scheduled run** has no **claim** and no **done-signal**: its **budget clock** alone terminates it, expiry classifying as success and session death before expiry as failure, and those outcomes feed only its **Schedule**'s own three-strikes — independent of the repo's **three-strikes pause** in both directions, exactly the independence a **lander run** has — with the **Schedule-paused trigger** the one new push in the model.
- A repo's secrets reach a gateway-wired **instance** only as **grants** on that repo's **agent identity**, never as values materialized in the instance itself — its generated context file says exactly that, and drops the legacy "use a value carefully" norm; an unwired instance still gets lab's own `repo_secrets` values and the legacy norm with them, until a later epic retires that system. The **master key**/vault path for git credentials (ADR-0006) is unchanged and stays independent of the **credential gateway**.
- The **credential gateway** is fail-closed at spawn: an unreachable sidecar — or a gateway URL configured without its interception CA file, an unobtainable **agent identity** or proxy token, an unusable CA file, or a host with no system CA roots — refuses the run rather than starting it credential-less, and every one of those refusals lands *before* the **claim**, so a sidecar outage never parks an issue. Only the **grant** listing is best-effort: it feeds the run's context file, and documentation is never why a run refuses to start. A lab that has not set the gateway settings is not wired at all and spawns exactly as it always did — never a refusal.

## Example dialogue

> **Dev:** "When an **AFK run** finishes its issue, do we mark the run done when the session exits?"
> **Domain expert:** "Never — the agent idles at its composer after opening the PR, with or without remote control; an interactive CLI doesn't quit when a turn ends. The **done-signal** is a PR or **change request** whose head branch equals the run's branch. Session death *without* that PR is a failure."
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
- **"grant"** — two senses: the **Read-only import**'s directional, flat repo→repo code-read permission, and the **credential gateway**'s attachment of one secret or connection to an **agent identity** (OneCLI's `/v1/agents/{id}/grants/{secrets|connections}/{id}`). Resolution: bare "grant" in secret/credential contexts always means the gateway sense; the import sense is written out in full as "read-only import."
- **"agent"** — two senses: the coding agent (the sense used everywhere else in this repo) and OneCLI's **agent identity**, a credential-scoping object with one instance per repo. Resolution: bare "agent" always means the coding agent; the OneCLI sense is always qualified as "agent identity" or "OneCLI agent."
