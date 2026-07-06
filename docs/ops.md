# lab — operations

Skeleton during M1–M7; sections marked *(filled in by M8)* are completed in the hardening milestone.

## Deployment

### NixOS module (recommended)

Import `nixosModules.lab` from this repo's flake. Options (see `nix/module.nix` for authoritative defaults):

- `enable`
- `package` — the lab package (default: this flake's `packages.lab`)
- `claudePackage` — the Claude Code package put on the unit PATH
- `user`, `group`
- `stateDir` — holds `lab.db`, `master.key`, `repos/`, `worktrees/`, `runtime/`
- `listenAddr` (default `:8080`)
- `baseUrl` — public URL; drives Secure-cookie detection and CSRF Origin checks
- `db` — default sqlite path under `stateDir`; a `postgres://` DSN switches backend
- `environmentFile` — secret DSN via `LAB_DB` env (`LoadCredential`-friendly)
- `masterKeyFile` — point at a sops-nix/`LoadCredential` path; auto-generated 0600 if absent
- `maxInstances` (default 6)
- `sessionNofile` (default 16384; prlimit cap per spawned session)
- `proxyAuth.{enable,header,trustedProxies}` — trusted-proxy auth (e.g. behind Authelia)
- `openFirewall` (default false)
- `extraFlags`

Unit invariants: `Type=simple`, **`KillMode=process`** (sessions survive service restarts — do not change), `Restart=on-failure`, `RestartSec=5`; unit PATH includes git, tmux, openssh, util-linux, and `claudePackage`. openssh is load-bearing: git forks `ssh` off PATH.

**nixpkgs pin**: the flake input must ship `go_1_26` (nixpkgs unstable, or the first release that does). Record the exact pin here when it changes.

Reverse-proxy / Authelia example (auth_request wiring, sharp edges from the v0 deployment): *(filled in by M8)*

### Bare metal

Any Linux host works:

1. Build or download `lab` and `labctl` (static binaries, CGO-free).
2. Ensure on PATH: `git`, `tmux`, `claude`, `ssh` (openssh), `prlimit` (util-linux).
3. Run `lab` with `--state-dir` (default `~/.local/state/lab`); flags per brief §8.5. Migrations apply on startup.
4. Put `labctl` on PATH for agent sessions (the NixOS module does this for you).

Systemd unit template for bare metal: *(filled in by M8)*

## CI runner prerequisites

The Forgejo Actions workflow (`.forgejo/workflows/ci.yml`) requires runners with:

- nix with flakes enabled
- egress (or mirrors) for `proxy.golang.org`, `registry.npmjs.org`, `cache.nixos.org`, and the flake inputs

The workflow's `runs-on: nix` label is a placeholder — adjust it to the labels your Forgejo instance's runners actually advertise. From M2 on, CI also provides a postgres service container exporting `LAB_TEST_POSTGRES_DSN` for the store suite.

## Backup surface

Back up, consistently together:

- `<state>/lab.db` (or the Postgres database)
- `<state>/master.key` (or the sops-managed key file) — without it, credential payloads are unrecoverable
- `<state>/repos/` (bare reference clones)

Explicitly **excluded**: `<state>/runtime/` (materialized key files, known_hosts — ephemeral, 0700, regenerated) and `<state>/worktrees/` (reproducible from branches; dirty worktrees are parked work the operator resolves before decommissioning a host).

Restore procedure, sqlite backup mechanics (WAL), Postgres dump/restore: *(filled in by M8)*

## Incogni mode

*(stub — completed by M8; the measures below are live since M7)*

Per-repo flag; when set, all seven measures of brief D15 §9 apply:

1. **Attribution off at the source** — every spawn seeds the worktree's `.claude/settings.local.json` with `attribution{commit:"",pr:"",sessionUrl:false}` + `includeCoAuthoredBy:false` (keys verified against Claude Code 2.1.198; `internal/compat/compat.md` §4, `claudecode.SeedAttributionOff`).
2. **Seed prompt** — the AFK seed prompt's commit step appends "No AI attribution, no Co-Authored-By, no generated-with footers anywhere." (`afk.SeedPrompt`).
3. **Server-side body sanitization** — the agent API strips Co-Authored-By/generated-with/Claude-Session lines from PR/CR bodies before they reach the tracker (`agentapi.sanitizeBody`).
4. **Neutral branch names** — incogni repos default to `issue-<N>` / `wip/`; claim parsing always uses the repo's configured pattern, never a literal `afk/`.
5. **Real git identity** — spawned sessions and CR merges author as the repo's configured `git_author_name`/`git_author_email` (falling back to the global settings), never a bot identity.
6. **Nothing lab seeds is committed** — `.claude/`, `CLAUDE.local.md` (and the seeded settings) are listed in `.git/info/exclude`, never `.gitignore`.
7. **Pre-push guard** — a pre-push hook in the bare reference repo (shared by all its worktrees) rejects pushes whose outgoing commits carry AI attribution in the message or touch lab-seeded files, naming the offending commit. Installed when incogni turns on, removed when it turns off.

**Honesty note**: incogni cannot hide the forge account identity of the token used (pushes and PRs appear under that account), nor statistical style/timing signals of agent-authored work. It removes explicit AI attribution markers; it does not make the work's origin undetectable.

## Monitoring

`/healthz`, `/readyz`, `/metrics` (Prometheus). Metric catalog and alerting suggestions: *(filled in by M8)*
