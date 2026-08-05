# Getting started

A first session with lab, end to end: install, log in, add a credential and a repository, run an interactive agent instance, then let an unattended AFK run resolve an issue. Deployment details (reverse proxy, secrets, backup) live in [`ops.md`](ops.md); the vocabulary used here (**instance**, **AFK run**, **claim**, **change request**, …) is defined in [`CONTEXT.md`](../CONTEXT.md).

## 1. Install and start

Pick one:

- **NixOS**: import `nixosModules.lab` from the flake and set `services.lab.enable = true;` — see the README quickstart and the full option table in [`ops.md` § Deployment](ops.md#deployment). The module provisions everything the container runner needs (rootless podman, subuid ranges, lingering) out of the box.
- **Download a release binary**: grab `lab-linux-amd64`/`lab-linux-arm64` (and `labctl-linux-…` for host-runner sessions) from the [latest release](https://github.com/Cloonar/coding-lab/releases/latest), verify against `checksums.txt`, `chmod +x`, and put it on PATH (`~/.local/bin` or `/usr/local/bin`); `git`, `tmux`, `ssh`, and `prlimit` still have to be there too. Then run `lab`. See the README's [Download a release binary](../README.md#download-a-release-binary) section.
- **Build from source**: build `bin/lab` and `bin/labctl` with `make lab labctl` (or `nix build .#lab`), make sure `git`, `tmux`, `ssh`, and `prlimit` are on PATH, and run `./bin/lab`. Defaults: listen on `:8080`, state in `~/.local/state/lab`, SQLite. A systemd unit template is in [`ops.md` § Bare metal](ops.md#bare-metal).

On first start lab applies its database migrations and auto-generates the vault master key (`master.key`, 0600) in the state directory. Nothing else to prepare.

## 2. Log in

Open the web UI. With no users in the database, lab shows a **first-run setup page** — pick the operator username and password (≥ 8 characters) and you're in.

Declarative deployments can skip the wizard and seed the operator account from config instead (`--seed-user` + a password hash from `lab hash-password`); see [`ops.md` § Seeding the initial operator user](ops.md#seeding-the-initial-operator-user).

The UI is a PWA — add it to your phone's home screen; that's the intended way to drive lab day to day.

## 3. Log in the agent provider

Lab spawns agent CLIs (Claude Code, Codex) on your behalf, so the provider needs to be authenticated once, machine-level. The UI surfaces each provider's auth status and drives its login flow (for Claude Code: start login → open the OAuth URL → paste the code back). On a container-runner host the login itself also runs containerized — no provider CLI needed on the host at all.

## 4. Add a credential

Git access is done with credentials you add in the UI, encrypted at rest in lab's vault. Three kinds:

- **SSH key** — for `ssh://` remotes (with optional passphrase).
- **HTTPS token** — for `https://` remotes.
- **Forge token** — a Forgejo or GitHub API token, used *only* for tracker REST calls (issues, labels, PRs), never for git push/pull. Only needed for repos whose tracker binding is `forge`; scopes and API-host format are in [`ops.md` § Forge credentials](ops.md#forge-credentials).

Secrets are never displayed back after saving, and a credential can't be deleted while a repository still references it.

## 5. Add a repository

Add a repo by its remote URL and pick the git credential for it. Lab clones it asynchronously into a lab-owned bare **reference repo** (clone progress streams live into the UI) and detects the forge from the remote:

- **Tracker binding** — a repo on a recognized forge (Forgejo, GitHub — with a matching forge-token credential) binds to that forge's issues and PRs. Any other repo gets lab's **built-in tracker**: issues, labels, comments, and lab-internal **change requests** instead of PRs. Either way, agents see the exact same surface.
- **Runner** — per repo, sessions run on the **host** (provider CLI directly on the host — unsandboxed) or in a **container** (rootless podman; your chosen dev image with lab's agent tools injected). Pick the dev image per repo, or rely on the global default. Details: [`ops.md` § Container runner](ops.md#container-runner).
- **Triage labels** — lab seeds the five canonical triage labels (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`) on the repo's tracker; `ready-for-agent` is the one the AFK engine watches.

## 6. Start a manual instance

From the repo, start an **instance**: an interactive agent session in its own fresh git worktree on its own branch, forked from the repo's default branch. Optionally override the model and effort per spawn (defaults are configurable globally and per repo; recommendations in [`model-selection.md`](model-selection.md)).

Drive the session from lab's **chat**: the rendered conversation with the agent — reply, answer permission dialogs, interrupt, use slash commands. For providers with a web surface (Claude Code), lab also captures a **deep link** into the provider's own app as an escape hatch.

Inside the session, the agent has `labctl` on PATH with a run token scoped to this one repo — it can read and file issues, apply labels, and open a PR/CR against the repo's tracker, and nothing else.

When you **Stop** an instance, the **guarded teardown** rule applies: a clean, merged worktree is removed; anything dirty or unmerged is kept and shown in the **Parked** view, where discarding is an explicit per-entry human action. Lab never destroys uncommitted work on its own.

## 7. Run an issue AFK

The unattended loop:

1. **Write an issue** on the repo's tracker that fully specifies one self-contained change — an AFK run gets no follow-up questions, so the issue is the whole brief.
2. **Label it `ready-for-agent`.**
3. **Start an AFK run** on the repo (or enable the repo's auto toggle, and the scheduler starts runs by itself). The engine claims the lowest ready issue by creating the run's branch, spawns a session with a seed prompt pointing at the issue, and walks away.
4. **The done-signal is a PR** (or change request on built-in-tracker repos) whose head branch is the run's branch, with `Closes #N` in the body. Session death without a PR counts as a failure; each run also has a budget clock (default 2 h). Three consecutive failures pause AFK on that repo until you press Reset.
5. **Review and merge** — a forge PR on your forge, or a change request right in lab's UI with a live diff and one-tap merge; merging a CR closes the linked issue.

Everything is observable from the phone throughout: the runs rail shows each instance's live conversational state, run history records every outcome, and Web Push can notify you when a session needs input.

## 8. Where to go next

- [`ops.md`](ops.md) — reverse proxy / SSO, secrets management, backup & restore, metrics, the container runner, incogni mode.
- [`model-selection.md`](model-selection.md) — which model/effort to use at which stage of the workflow.
- [`../CONTEXT.md`](../CONTEXT.md) — the full domain glossary; useful as a map of what every screen and term means.
- [`adr/`](adr/) — why the system is shaped the way it is.
