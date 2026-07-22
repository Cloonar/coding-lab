# lab — operations

Deployment, configuration, state, backup, CI, and observability for the `lab` server. The product contract is [`agent-brief.md`](agent-brief.md); design decisions are in [`adr/`](adr/).

## Deployment

### NixOS module (recommended)

Import `nixosModules.lab` from this repo's flake. Options (authoritative defaults in [`nix/module.nix`](../nix/module.nix)):

| Option | Default | Meaning |
|---|---|---|
| `enable` | `false` | Enable the service. |
| `package` | this flake's `packages.lab` | The lab package; ships both `lab` and `labctl`, both land on the unit PATH. |
| `agentPackages` | `{ "claude-code" = pkgs.claude-code; codex = pkgs.codex; }` | Agent-CLI packages whose `bin/` is added to the unit PATH, an `attrsOf (nullOr package)` **keyed by lab provider ID** (the same strings the provider registry, the DB `provider` column, and the API use). Defaults merge **per key** (injected at `mkOptionDefault` priority), so `agentPackages."claude-code" = null` drops claude while the codex default survives, and adding a key keeps both defaults. `claude-code` is **unfree** in nixpkgs: allow it (`nixpkgs.config.allowUnfreePredicate = p: lib.getName p == "claude-code";`) or set the key to `null`. |
| `extraPackages` | `[ ]` | Extra `package`s appended to the unit PATH. Purely additive — never affects `agentPackages` or the fixed tools baseline. The sanctioned knob for a host-specific tool a session needs. |
| `claudePackage` | `null` | **Deprecated alias** for `agentPackages."claude-code"`. When non-null it populates that key and emits a deprecation warning; setting both it and an explicit `agentPackages."claude-code"` fails eval with an assertion naming both options. Prefer `agentPackages`. |
| `user` / `group` | `lab` / `lab` | Service identity. The default creates a `lab` system user with its home at `stateDir`; claude's own auth/config state lives under that HOME. |
| `stateDir` | `/var/lib/lab` | State root (layout below). Managed via systemd `StateDirectory` when left at the default; otherwise the operator provides the directory (lab creates missing children itself, 0700). |
| `listenAddr` | `":8080"` | Passed as `--addr`. |
| `baseUrl` | `null` | Passed as `--base-url`. Drives Secure-cookie detection and the CSRF Origin check — set it whenever lab sits behind TLS. |
| `agentUrl` | `"http://127.0.0.1:<port>"` | Passed as `--agent-url`. Session-facing base URL handed to `labctl` as `LAB_URL`. Defaults to a loopback URL derived from `listenAddr` so agent traffic reaches lab directly and never hairpins out through `baseUrl` and any SSO/auth proxy in front of it. Override only when sessions run off-host; set to `null` to fall back to lab's own precedence (baseUrl, else loopback). |
| `db` | `null` | Passed as `--db` (`sqlite:<path>` or `postgres://…`). `null` keeps lab's derived sqlite default **and** lets a `LAB_DB` entry in `environmentFile` take effect (precedence is flag > env > default — a `--db` flag would shadow `LAB_DB`). |
| `environmentFile` | `null` | systemd `EnvironmentFile=` for secret env vars (`LAB_DB` with a password-bearing postgres DSN, etc.). `LoadCredential`-friendly. |
| `masterKeyFile` | `"${stateDir}/master.key"` | Passed as `--master-key-file`. lab auto-generates it 0600 when absent and refuses to start on loose permissions or malformed content. |
| `vapidKeyFile` | `"${stateDir}/vapid.key"` | Passed as `--vapid-key-file`. P-256 VAPID key for Web Push (see [Push notifications](#push-notifications)) — lab auto-generates it 0600 when absent and refuses to start on loose permissions or malformed content, same as `masterKeyFile`. |
| `seedUser` | `null` | Passed as `--seed-user`. Username of the initial operator user, reconciled on every boot — config, not the database, is this credential's source of truth (see [Seeding the initial operator user](#seeding-the-initial-operator-user)). Requires exactly one of `seedPasswordHash` or `seedPasswordHashFile`. |
| `seedPasswordHash` | `null` | Passed as `--seed-password-hash`. PHC-encoded argon2id hash for `seedUser`, generated with `lab hash-password`. Inline is safe in world-readable config — the NixOS `hashedPassword` model. `seedPasswordHashFile` wins when both are set. |
| `seedPasswordHashFile` | `null` | Passed as `--seed-password-hash-file`. File containing the hash for `seedUser` (one trailing newline stripped). Wins over `seedPasswordHash` when both are set — `LoadCredential`-friendly, same contract as `environmentFile`. |
| `maxInstances` | `6` | Passed as `--max-instances`. Seeds the `max_instances` settings row on first start; thereafter the in-app setting wins. |
| `sessionNofile` | `16384` | Passed as `--session-nofile`; RLIMIT_NOFILE prlimit cap per spawned session, 0 disables. A runaway session hits its own EMFILE and dies alone. |
| `proxyAuth.enable` | `false` | Trust a reverse-proxy auth header as the authenticated username — only from `trustedProxies` peers. The module asserts that enabling it requires at least one trusted proxy. |
| `proxyAuth.header` | `"Remote-User"` | Passed as `--proxy-auth-header`. |
| `proxyAuth.trustedProxies` | `[ ]` | CIDRs, passed as `--trusted-proxies` whenever non-empty — also without `proxyAuth.enable`: the list gates X-Forwarded-Proto trust (Secure-cookie detection behind a TLS-terminating proxy with lab's own login). |
| `openFirewall` | `false` | Open the firewall for the port in `listenAddr`. |
| `extraFlags` | `[ ]` | Extra flags appended to `ExecStart` (e.g. `[ "--provider-bin" "claude-code=/run/current-system/sw/bin/claude" ]`). |

Full example — sops-provided master key, Postgres DSN via `environmentFile`:

```nix
{
  inputs.coding-lab.url = "git+https://git.cloonar.com/Cloonar/coding-lab";

  # in the host config:
  imports = [ coding-lab.nixosModules.lab ];

  sops.secrets."lab/master.key" = {
    owner = config.services.lab.user;
    mode = "0600";
  };
  # /run/secrets/lab.env contains one line:
  #   LAB_DB=postgres://lab:PASSWORD@10.0.0.5/lab?sslmode=require
  sops.secrets."lab/env" = { };

  # agentPackages defaults to claude-code + codex; claude-code is unfree, so
  # allow it (or set `services.lab.agentPackages."claude-code" = null;`):
  nixpkgs.config.allowUnfreePredicate = p: lib.getName p == "claude-code";

  services.lab = {
    enable = true;
    baseUrl = "https://lab.example.com";
    masterKeyFile = config.sops.secrets."lab/master.key".path;
    environmentFile = config.sops.secrets."lab/env".path;   # LAB_DB; leave `db` at null
    proxyAuth = {
      enable = true;                     # only if fronted by Authelia or similar
      trustedProxies = [ "10.0.0.0/8" ];
    };
  };
}
```

`systemd LoadCredential` works too: set `systemd.services.lab.serviceConfig.LoadCredential = "master.key:/run/secrets/lab-master.key";` and `services.lab.masterKeyFile = "/run/credentials/lab.service/master.key";`.

**Unit invariants** (asserted by the `nixos-module` flake check — do not change):

- `KillMode=process` is **load-bearing**: the tmux server lab spawns (and every agent session under it) lives in this unit's cgroup. `KillMode=process` makes a restart/deploy kill only the lab process; lab re-adopts the surviving tmux server on start. A config switch never drops a session.
- `Type=simple`, `Restart=on-failure`, `RestartSec=5`.
- Unit PATH includes git, tmux, **openssh**, util-linux, `package` (for `labctl`), the fixed tools baseline (`gawk`, `gnutar`, `gzip`, `xz`, `zstd`, `unzip`, `curl`, `jq`, `file`, `patch`, `procps` (`ps`), `ripgrep` (`rg`), and `nix` via `config.nix.package` — so a session can run a project flake's devshell for per-project toolchains), every non-null `agentPackages` value, and `extraPackages`. The baseline is fixed, not an option (a contract every provider's session can assume; `extraPackages` is the additive knob); language toolchains are deliberately excluded and come from each project's flake. openssh is load-bearing: origins are SSH remotes and git forks `ssh` off PATH — without it every fetch dies with "cannot run ssh".
- `ExecStart` uses systemd escaping (`escapeSystemdExecArgs`), not shell quoting — `%` and `$` in a DSN survive.

**nixpkgs pin**: the flake input must ship `go_1_26`. Currently `github:NixOS/nixpkgs/nixos-unstable`, locked at `d407951447dcd00442e97087bf374aad70c04cea`. Record pin changes here.

**Debugging sessions**: the module installs tmux system-wide; attach to a live agent session with `sudo -u lab tmux attach -t '<repo>~<label>'` (detach with `C-b d` — never kill the pane; Stop from the UI so the guarded teardown runs).

### Reverse proxy / Authelia

lab works bare (its own login) or behind a forward-auth proxy. Behind nginx + Authelia:

```nginx
location / {
    # Authelia auth_request wiring per its docs, then:
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Remote-User $remote_user;      # from auth_request
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Host $host;

    # SSE (/api/v1/events): disable buffering, allow long-lived streams.
    proxy_buffering off;
    proxy_read_timeout 1h;
    proxy_http_version 1.1;
}
```

Sharp edges (all enforced server-side):

- The proxy header is trusted **only** when the TCP peer (never X-Forwarded-For) is inside `--trusted-proxies` **and** the header value exactly equals the admin username; on mismatch lab falls through to its own auth and logs once per distinct value.
- Set `--base-url` (`services.lab.baseUrl`) to the public https URL. It drives the CSRF Origin check and Secure cookies. Cookies are Secure when the request came over TLS, or `--base-url` is https, or `X-Forwarded-Proto: https` arrives from a trusted proxy — if none of these can ever hold, lab logs a prominent warning at startup.
- **Do not route agent traffic through the proxy.** `labctl` inside a session authenticates to `/agent/v1` with a run token only — no SSO session, no cookies. If its `LAB_URL` pointed at the external origin, every call would hairpin out to the proxy, get 302'd to the login portal, and fail. The NixOS module defaults `services.lab.agentUrl` (`--agent-url`) to `http://127.0.0.1:<port>`, so `LAB_URL` stays on loopback and bypasses the proxy entirely — no operator action needed. Only override it when sessions run off-host (then expose `/agent/v1` to those hosts by network reach, not by widening the public vhost). If you ever see `labctl` fail with an HTML/redirect error, `LAB_URL` is aimed at the proxy — check `agentUrl`.
- Keep `proxy_buffering off` for `/api/v1/events` or the SSE stream stalls.

### Bare metal

Any Linux host works:

1. Build or download `lab` and `labctl` (static binaries, CGO-free): `make lab labctl` or `nix build .#lab`.
2. Ensure on PATH: `git`, `tmux`, `claude`, `ssh` (openssh), `prlimit` (util-linux) — and `labctl` (agent sessions resolve it from PATH).
3. Run `lab` with the flags below (defaults: `--state-dir ~/.local/state/lab`, sqlite). Migrations apply on startup; the master key is auto-generated 0600 on first start.

Systemd unit template:

```ini
[Unit]
Description=lab — phone-first control panel for Claude Code agent sessions
After=network.target

[Service]
Type=simple
User=lab
Group=lab
Environment=HOME=/var/lib/lab
EnvironmentFile=-/etc/lab/lab.env
ExecStart=/usr/local/bin/lab --state-dir /var/lib/lab --base-url https://lab.example.com
# LOAD-BEARING: only the lab process dies on restart; the tmux server and
# its agent sessions survive and are re-adopted.
KillMode=process
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

## Configuration reference (brief §8.5)

Precedence: **flag > env > default**. Env overrides exist only where listed.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--addr` | `LAB_ADDR` | `:8080` | Listen address. |
| `--state-dir` | `LAB_STATE_DIR` | `~/.local/state/lab` | State root (layout below). With no HOME and no value, lab refuses to start. |
| `--db` | `LAB_DB` | `sqlite:<state-dir>/lab.db` | DSN. `sqlite:<path>` or `postgres://…` / `postgresql://…` switches backend. |
| `--master-key-file` | `LAB_MASTER_KEY_FILE` | `<state-dir>/master.key` | Vault master key: 64 hex chars (32 bytes), 0600. Auto-generated when absent; loose perms or malformed content refuse startup. |
| `--vapid-key-file` | `LAB_VAPID_KEY_FILE` | `<state-dir>/vapid.key` | P-256 VAPID key for Web Push: 64 hex chars (32 bytes), 0600. Auto-generated when absent; loose perms or malformed content refuse startup. |
| `--seed-user` | `LAB_SEED_USER` | (empty) | Username of the initial operator user, reconciled on every boot (see [Seeding the initial operator user](#seeding-the-initial-operator-user)). Must be set together with at least one hash source below, else lab refuses to start. |
| `--seed-password-hash` | `LAB_SEED_PASSWORD_HASH` | (empty) | PHC-encoded argon2id hash for `--seed-user`, inline — generate it with `lab hash-password`. A hash is safe in world-readable config, same model as NixOS `hashedPassword`. |
| `--seed-password-hash-file` | `LAB_SEED_PASSWORD_HASH_FILE` | (empty) | File holding the hash for `--seed-user`; exactly one trailing newline stripped. Wins over `--seed-password-hash` when both are set. |
| `--provider-bin` | `LAB_PROVIDER_BIN_<ID>` | adapter default | Per-provider agent binary, **repeatable** as `id=path` keyed by provider ID (`--provider-bin claude-code=/usr/bin/claude`). The env `<ID>` is the provider ID uppercased with dashes → underscores (`claude-code` → `LAB_PROVIDER_BIN_CLAUDE_CODE`). An unknown ID in the flag is a boot error listing the registered IDs (env keys are only read for registered IDs — a typoed env var name is inert). With no entry the adapter fills its own default (claude-code: `claude` via PATH lookup). |
| `--provider-config` | `LAB_PROVIDER_CONFIG_<ID>` | adapter default | Per-provider global config file, **repeatable** as `id=path` keyed by provider ID (`--provider-config claude-code=/var/lib/lab/.claude.json`); same `<ID>` env mapping. claude-code's config is the folder-trust seeding target — with no HOME and no configured claude-code config path, instance/AFK features stay unmounted and lab serves the rest with a loud warning. With no entry the adapter fills its own default (claude-code: `~/.claude.json` from HOME). |
| `--claude` | — | (see `--provider-bin`) | **Deprecated alias** for `--provider-bin claude-code=<path>`. Prefer the generic flag. |
| `--claude-config` | `LAB_CLAUDE_CONFIG` | (see `--provider-config`) | **Deprecated alias** for `--provider-config claude-code=<path>` (env alias for `LAB_PROVIDER_CONFIG_CLAUDE_CODE`). Prefer the generic flag. |
| `--tmux` | — | `tmux` (PATH lookup) | tmux binary. |
| `--git` | — | `git` (PATH lookup) | git binary. |
| `--prlimit` | — | `prlimit` (PATH lookup) | prlimit binary (session NOFILE cap). |
| `--max-instances` | — | `6` | Global live-instance cap. Seeds the `max_instances` settings row on **first start only**; thereafter the runtime setting wins. Must be ≥ 1. |
| `--session-nofile` | — | `16384` | RLIMIT_NOFILE (soft+hard) for spawned sessions via prlimit; `0` disables the cap. |
| `--proxy-auth` | — | off | Accept the proxy auth header from trusted proxies. |
| `--proxy-auth-header` | — | `Remote-User` | Header carrying the proxy-authenticated username. |
| `--trusted-proxies` | — | (empty) | Comma-separated CIDRs of trusted reverse proxies (also gates `X-Forwarded-Proto` / `X-Forwarded-For` trust). |
| `--base-url` | `LAB_BASE_URL` | (empty) | Absolute http(s) external URL. Drives Secure cookies and the CSRF Origin check. Also seeds the `LAB_URL` handed to sessions **unless** `--agent-url` is set. |
| `--agent-url` | `LAB_AGENT_URL` | (empty) | Absolute http(s) session-facing URL handed to `labctl` as `LAB_URL`. Precedence: `--agent-url` → `--base-url` → `http://127.0.0.1:<port>`. Set it (or the NixOS `agentUrl` default) to a loopback URL so agent traffic bypasses any front proxy. |

The per-provider host settings (`--provider-bin` / `--provider-config` and their `--claude` / `--claude-config` aliases) resolve **per provider entry**, highest wins: **generic flag > generic env > alias flag > alias env** — the generic form always beats the claude-named alias for the same setting, and within each pair a flag beats its env. The registered provider IDs come from `cmd/lab`, so a new provider's binary and config path are two entries under its ID with no config change (ADR-0034).

Runtime-mutable knobs live in the `settings` table (Settings UI / `PATCH /api/v1/settings`), not flags:

| Key | Default | Meaning |
|---|---|---|
| `spawn_model_default` | `opus[1m]` | Default model for spawns (per-repo and per-spawn overrides exist). |
| `spawn_effort_default` | `max` | Default effort. |
| `max_instances` | seeded from `--max-instances` | Global live-instance cap (login session excluded). |
| `afk_budget_minutes` | `120` | AFK budget clock; per-repo override on the repo row. |
| `afk_tick_seconds` | `30` | Reaper loop interval. |
| `afk_schedule_seconds` | `45` | Scheduler loop interval (separate goroutine, v0 parity). |
| `sweep_interval_minutes` | `10` | Throttled merged-sweep + runtime-credential sweep cadence. |
| `git_author_name` / `git_author_email` | (blank) | Global git identity fallback for sessions and CR merges; per-repo overrides on the repo row. |

## Seeding the initial operator user

`--seed-user` / `--seed-password-hash` / `--seed-password-hash-file` (NixOS: `seedUser` / `seedPasswordHash` / `seedPasswordHashFile`, above) let a declarative deployment provision its operator account from config instead of the browser first-run wizard. Only a hash is ever accepted — there is no plaintext seed-password flag.

Generate the hash with `lab hash-password`: it reads the password from stdin (a pipe) or prompts with echo off on a TTY, **never** as an argument (shell history and `ps` would leak it). It enforces the same ≥ 8 character rule as the setup page and prints the PHC `$argon2id$…` string on stdout, hashed with the same pinned parameters the login path itself uses.

Boot semantics — this reconciles on **every** boot, before the HTTP listener opens, and the config is always the credential's source of truth, not the database:

- Zero users exist: create the seeded user. No session is created — the SPA lands on the login page, not the setup wizard.
- A user with that username exists and its stored hash differs byte-wise from the configured one: update it. Existing sessions stay valid, so rotating the password is edit config + restart, nothing more.
- Users exist but none matches the seeded username: lab refuses to start, naming the existing username in the error.
- The configured hash is malformed, or `--seed-password-hash-file` is unreadable: lab refuses to start.
- Deployments that never set `--seed-user` are unaffected — the first-run setup page behaves exactly as before.

```console
$ lab hash-password
Password: ********
$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$eB1n6H...

# services.lab.seedUser = "admin"; services.lab.seedPasswordHash = "$argon2id$...";
# rotating later: edit the hash (or the file it points at) and restart the service.
```

## Forge credentials

A repo whose **tracker binding** is `forge` reads and writes issues/PRs through a **forge token** credential — a `forge_token` in the vault, server-side only (never materialized, never in a session's env). The credential carries a **flavor** and an **API host** (ADR-0015):

| Flavor | API host to enter | PAT scopes |
|---|---|---|
| **Forgejo** | the instance host, bare (`git.cloonar.com`) — lab appends `/api/v1` | a token with issue + pull-request read/write on the repo |
| **GitHub** | `api.github.com` for github.com; a GitHub Enterprise instance's real API root verbatim (`ghe.example.com/api/v3`, or `api.ghe.example.com` under subdomain isolation) — no derivation | **fine-grained PAT**: Issues (RW), Pull requests (RW), Metadata (R). **classic PAT**: `repo`. |

The flavor is the routing authority: an unrecognized host (a second Forgejo instance, a GHE host — `forge_kind` detects as `none`) still binds `forge` when the operator selects it, and resolves from the credential alone. A `github.com` (or `git.cloonar.com`) remote whose credential flavor disagrees with the host is refused as a configuration conflict rather than silently 404-ing. Git push auth is a **separate** git credential (SSH key or HTTPS token); the forge token is only ever the tracker's REST auth. GitHub calls count against the account's hourly rate limit (~5000 req/h per user; a polling repo spends ~480/h at 30s ticks) — when a repo is throttled lab logs and skips the tick, and the AFK loop self-heals once the window resets.

## State directory layout

```
<state>/
  lab.db                     sqlite database (WAL mode; absent on postgres)
  master.key                 vault master key (64 hex chars, 0600)
  vapid.key                  Web Push VAPID key (64 hex chars, 0600)
  repos/<repoID>.git/        bare reference clones (worktree parents)
  worktrees/<repo>-<label>/  instance worktrees (manual: -<label>, AFK: -<N>)
  runtime/                   0700 — materialized credential files, per-op
                             (<credID>.<opID>.key/.askpass/.sshpass), known_hosts
```

Sessions are named `<repo>~<label>`; `~` never appears in paths (the Windows-8.3 lookalike pattern stalls Claude).

## Backup & restore

Back up, **consistently together** (one snapshot set):

- `<state>/lab.db` (or the Postgres database) — config, credentials (encrypted), built-in tracker, run history.
- `<state>/master.key` (or the sops-managed key file) — without it, credential payloads are unrecoverable.
- `<state>/repos/` — the bare reference clones (claims and parked branches live here as git refs).

Explicitly **excluded** (reconstructible or ephemeral):

- `<state>/runtime/` — materialized key files, known_hosts; 0700, regenerated per operation, swept at startup.
- `<state>/worktrees/` — recreated from branches; dirty worktrees are parked work the operator resolves *before* decommissioning a host (a backup cannot carry uncommitted changes safely).

Mechanics:

- **SQLite**: the DB runs in WAL mode. Either stop the service and copy `lab.db` (plus `lab.db-wal`/`lab.db-shm` if present), or — with the service running — use `sqlite3 <state>/lab.db ".backup '/backups/lab.db'"`, which produces a consistent single file. Never copy a live `lab.db` alone.
- **Postgres**: `pg_dump` on any schedule; restore with `pg_restore`/`psql` before starting lab (migrations are idempotent and apply on startup).

Restore procedure:

1. Stop the service.
2. Restore `master.key` **from the same backup set as the database**. Ordering constraint: the master key must be the one that encrypted the credentials in the restored DB — a newer or regenerated key decrypts nothing, and lab has no re-key command. If you rotate keys, re-enter credentials through the UI afterwards.
3. Restore `lab.db` (or the Postgres database) and `repos/`.
4. Fix ownership (`chown -R lab:lab <state>`) and perms (`master.key` 0600).
5. Start the service. Startup heals interrupted clones, re-adopts any surviving tmux sessions, reconciles worktrees/branches against the restored refs (guarded — nothing dirty or unmerged is destroyed), and sweeps `runtime/`.

## CI runner prerequisites

Two Forgejo Actions gates run on pull requests (ADR-0023):

- **The native gate** ([`.forgejo/workflows/ci.yml`](../.forgejo/workflows/ci.yml), job `native`) runs on **every** PR and is the **required** status check. It builds and tests directly on the stock `ubuntu-latest` runner — no nix — mirroring the flake's non-nix checks: the SPA's eslint + prettier + vitest + `vite build`, then the `ui`-tagged Go build and `go test -tags ui ./...` against the copied dist, then the untagged `golangci-lint run ./...` (`CGO_ENABLED=0` throughout). Common-path PRs finish in ~2–4 min instead of ~12–13. Its toolchain is version-matched to the flake: Go from `go.mod` (`go 1.26`), Node 24 (`pkgs.nodejs`), golangci-lint 2.12.2 (the version nixpkgs ships at the current `flake.lock`).
- **The hermetic gate** ([`.forgejo/workflows/ci-nix.yml`](../.forgejo/workflows/ci-nix.yml), job `flake-check`) runs the full `nix flake check` — the authoritative build, identical to the local command — but only when nix or the Go dependency set changes (`paths:` = `**/*.nix`, `flake.lock`, `go.mod`, `go.sum`). It is the same in-job Determinate install as before the split, and the deploy re-runs it as a merge-to-main backstop (`bump-nixos-pin` → `test-configuration` rebuilds `packages.lab`). `go.mod`/`go.sum` are load-bearing triggers: a dependency bump stales `nix/package.nix`'s `vendorHash`, which the native gate (live `go mod download`) cannot catch.

> **Required-check switch (one-time repo setting).** After this split, move branch protection's required status check from **`flake-check`** to **`native`**. The nix gate reports a status only when its paths match, so a rule that still requires `flake-check` would wedge every Go/TS-only PR at "expected". The `native` job id is kept stable for exactly this reason.

**Native gate** (ci.yml) — the runner needs:

- `actions/setup-go@v5` and `actions/setup-node@v4` to resolve — from the same source as the existing `actions/checkout@v4` (Forgejo's `DEFAULT_ACTIONS_URL` mirror, or an admin-set github). Pin actions by **tag** (`@v5`/`@v4`), never by a GitHub commit SHA: the mirror's SHAs differ from github's.
- The runner's **actions cache backend enabled** (the default — `cache.enabled: true` injects `ACTIONS_CACHE_URL`). `setup-go` caches the Go module + build cache by default and `setup-node` caches `~/.npm` (`cache: npm`); with no cache backend reachable those steps fail rather than silently skip.
- Outbound egress for `proxy.golang.org` (Go modules), `registry.npmjs.org` (npm), the actions source, and `raw.githubusercontent.com` + `github.com` (the pinned golangci-lint installer and release binary). No nix, no `cache.nixos.org`, no Determinate installer on this gate.
- `git`, `tmux`, and `prlimit` (util-linux) on PATH for `go test` (real subprocesses, the D17 bar). `git` and `prlimit` ship on the stock image; `tmux` does not, so the job `apt-get install`s it — the runner therefore also needs apt reachable and either runs as root or has `sudo`.

**Hermetic gate** (ci-nix.yml) — same prerequisites as before the split, now only on nix/dep changes:

- Outbound egress (or in-instance mirrors/substituters) for the installer (`install.determinate.systems`), `cache.nixos.org` (binary substitutes for the Go toolchain and nixpkgs, so they are not rebuilt from source), `proxy.golang.org`, `registry.npmjs.org`, and the flake inputs (github.com for nixpkgs).
- Steps run as root, or with `sudo`, so the installer can create `/nix` — the default for the stock Docker-backed runner.
- Enough disk for the nix store: the check builds the `lab`/`web` outputs and node_modules (via `importNpmLock`), runs the Go suite against real git/tmux/prlimit inside the sandbox, and evaluates the nixos module.

The store suite additionally runs against a real Postgres wherever `LAB_TEST_POSTGRES_DSN` is set; `ci.yml` carries a ready-made `store-postgres` job as a commented template (service container + DSN export, plus the same in-job nix install) — uncomment it once your runners support service containers.

**Agent-tools gate** ([`.forgejo/workflows/agent-tools.yml`](../.forgejo/workflows/agent-tools.yml), see [Agent-tools images](#agent-tools-images)) — path-gated to `containers/**` and the workflow itself, so it does not run on common-path PRs. Where it does run it needs, beyond the native gate's toolchain:

- **`podman`** on the runner. This is the first gate to need it; it ships on some stock images and not others, so both jobs install-if-missing (`command -v podman || apt-get install -y podman`) then dump `podman version` / `podman info` as the diagnostic if a runner cannot run podman at all — the same apt-reachable-and-root-or-sudo requirement the native gate's `tmux` install carries. Because the job itself executes inside the runner's Docker container (no `/dev/fuse`, an overlayfs root, no systemd/journald), both jobs write a CI-scoped podman config before first use: `vfs` storage (the default overlay driver cannot mount on an overlay backing store, and its fuse-overlayfs fallback needs the missing `/dev/fuse`), `cgroupfs`/`file` backends, `BUILDAH_ISOLATION=chroot` for the builds' RUN steps, and the smoke test's `podman run`s pass `--network=host --cgroups=disabled`. vfs stores every layer as a full filesystem copy, so the jobs also set `BUILDAH_LAYERS=false` (one commit per build stage instead of one per step — CI's empty per-run store makes layer caching worthless anyway) and `podman image prune -f` between provider builds, with `df -h` diagnostics in each step. None of this changes what the smoke test proves; a runner that still cannot run podman shows up in the `podman info` diagnostics.
- Outbound egress for `downloads.claude.ai` (the Claude Code `linux-x64-musl` binary + its `manifest.json`), `github.com` (the codex release asset), and `docker.io` (the `debian:stable-slim` and `alpine` base images the injection smoke test mounts into). The release leg additionally reaches the Forgejo package registry at `git.cloonar.com` to push.

## Agent-tools images

Per-provider **agent-tools** OCI images carry an agent CLI and a static `labctl` INTO an operator-chosen dev container, so the agent surface travels with lab instead of being baked into the base image. Built and published by [`.forgejo/workflows/agent-tools.yml`](../.forgejo/workflows/agent-tools.yml) from `containers/agent-tools/`; the design rationale (libc mechanism, alternatives) is [ADR-0051](adr/0051-agent-tools-oci-images.md), and the consumer is the container runner (issue #205). This complements the ADR-0033 tools baseline (which puts CLIs on the lab *unit* PATH) rather than replacing it: the baseline serves host-PATH sessions, these images serve sessions running inside an arbitrary container.

**What the images are.** One image per provider, `FROM scratch` (pure payload, never run as a container), tagged `git.cloonar.com/cloonar/agent-tools:<provider>-<cli-version>`. claude and codex ship today; gemini is deferred until the gemini adapter (#126) lands. Exact contents:

| Image | Root filesystem |
|---|---|
| `agent-tools:claude-<ver>` | `/bin/claude` (Claude Code `linux-x64-musl` native build), `/bin/labctl` (static, built from this repo), `/lib/ld-musl-x86_64.so.1` (the musl loader, from Alpine) |
| `agent-tools:codex-<ver>` | `/bin/codex` (upstream static-pie musl binary), `/bin/labctl` |

Nothing else — no shell, no userland. The image ships ONLY lab-owned binaries; the userland a session assumes (`tar`, `curl`, `jq`, `rg`, …) is the dev image's own business, deliberately NOT imposed by this mount (ADR-0033's baseline is for host-PATH sessions, not this seam).

**The injection contract.** The container runner mounts the image read-only into the operator's chosen dev container and prepends `/opt/lab/bin` to PATH:

```
podman run --mount type=image,src=git.cloonar.com/cloonar/agent-tools@sha256:…,dst=/opt/lab …
# binaries then resolve at /opt/lab/bin/claude, /opt/lab/bin/codex, /opt/lab/bin/labctl
```

`/opt/lab` is a **HARD contract**, not a convention: the claude binary's ELF interpreter (`PT_INTERP`) is rewritten at image-build time to the absolute path `/opt/lab/lib/ld-musl-x86_64.so.1`. Mount the image anywhere else and claude will not start. This is what lets claude run on both a glibc base (debian) and a musl base (alpine) without using the base image's libc — the one bundled musl loader IS the only libc claude consults. codex is static-pie with no interpreter and runs as-is; `labctl` is pure-Go `CGO_ENABLED=0` and needs no loader.

**Tagging + digest pinning.** The `<provider>-<cli-version>` tag is human-facing. The **digest** is the pinning contract: a same-tag re-push (e.g. a `labctl` refresh at an unchanged CLI version) produces a NEW digest under the SAME tag, so the tag is a moving reference and consumers pin `agent-tools@sha256:…`. The release job emits the digest-pinned reference to its job summary.

**The version catalog.** `containers/agent-tools/versions.env` is the single source of truth for provider CLI versions and artifact checksums, sourced by both the build scripts and the publish job:

- `CLAUDE_CODE_VERSION` + the sha256 of the `linux-x64-musl` binary (from Anthropic's per-version manifest at `downloads.claude.ai/claude-code-releases/<ver>/manifest.json`).
- `CODEX_VERSION` + the sha256 of `codex-x86_64-unknown-linux-musl.tar.gz` (from the GitHub release's per-asset digest).

These versions ARE the repo's compat-record pins (`internal/compat/compat.md` pins Claude Code, `internal/compat/codex/compat.md` pins codex-cli) — versions.env is what makes those prose pins actual build inputs. **Bump procedure**: re-verify the compat record against the new CLI version FIRST (the compat doc is the checklist), THEN bump the version + sha in versions.env. The PR runs the injection smoke test; the merge to main publishes. A version bump that skips the compat re-verification is the failure mode to guard against — it ships an unverified CLI under a pin that claims verification.

**CI legs** (deliberately asymmetric on secrets):

- **PR leg** (job `smoke`, on `pull_request`): builds both images locally and runs the injection smoke test — mount each into stock `debian:stable-slim` and `alpine` and run `claude --version` / `codex --version` / `labctl --help`. It references NO secret and NEVER pushes, so a PR from any branch (forks included) exercises the full build+inject path without ever touching the registry credential.
- **Release leg** (job `publish`, on `push` to `main` + `workflow_dispatch`): re-runs the smoke test (cheap insurance that exactly what is pushed passes injection), then pushes the digest-pinned tags to the registry. Both legs are path-gated to `containers/**` and the workflow file so unrelated merges don't re-release. `workflow_dispatch` is the manual re-release knob: `labctl` is built from the repo INSIDE the image build, so a labctl change that should be baked into fresh images is shipped by dispatching this workflow even with no `containers/**` diff.

**Operator provisioning.** The release leg needs one repository CI secret, gated before checkout so a missing token fails loud and early with these instructions rather than deep inside a push:

- **`FORGE_REGISTRY_TOKEN`** (required) — a Forgejo access token with package write (`write:package`) scope on `git.cloonar.com`. PRs never see it.
- **`FORGE_REGISTRY_USER`** (optional) — the token owner's username, used as the `podman login` user. Defaults to the repository owner; set it only when the token does not belong to that account.

**x86_64-only today.** Both upstreams publish arm64(-musl) artifacts, so an arm64 variant is a mechanical follow-up (a second set of tags off the same versions.env and Containerfiles) once an arm64 runner/host exists.

## Observability

- `GET /healthz` — liveness; 200 `ok` always (no dependencies).
- `GET /readyz` — readiness; 503 `database unavailable` while the DB is unreachable (2s probe timeout), 200 `ok` otherwise.
- `GET /metrics` — Prometheus text format. All three endpoints are mounted outside auth and CSRF — probes must work with the DB down.
- Logs: slog JSON on stdout (journald under systemd). Keys: `component`, `repo`, `session`, `run`, `err`. Secrets, tokens, key material, and DSN passwords never appear in logs.

## Metrics

Every label set is a fixed vocabulary — never run ids, repo ids, or free text — so cardinality stays bounded for the life of the process. Beyond the table, the standard Go and process collectors (`go_*`, `process_*`) are registered.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `lab_http_requests_total` | counter | `route` (the ServeMux pattern that handled the request — API 404s land under the `/api/v1/` catch-all and unknown UI paths under `/`; `unmatched` marks requests a middleware short-circuited before route dispatch, e.g. a CSRF 403), `method` (standard verbs, else `OTHER`), `code` | HTTP requests served. |
| `lab_http_request_duration_seconds` | histogram | `route`, `method` | HTTP request latency. |
| `lab_instances_active` | gauge | `kind` = `manual\|afk_manual\|afk_auto` | Active runs whose tmux session is live. Evaluated **at scrape time** from the runs table + tmux (a custom collector, never a maintained counter — it cannot drift). The series is absent when the instance stack is disabled (no claude config) or when the scrape-time snapshot fails (DB/tmux error); `/metrics` itself stays 200 either way. |
| `lab_afk_runs_total` | counter | `outcome` = `success\|death\|timeout\|stopped`, `kind` = `afk_manual\|afk_auto` | Terminal AFK run outcomes, incremented at every terminal-outcome writer: the reaper, Stop, and the parked-branch Discard kill. Deaths recorded by startup re-adoption (runs that died while lab was down) are **not** counted — the same v0-parity rule the three-strikes counter follows. |
| `lab_afk_run_duration_seconds` | histogram | `outcome` | AFK run duration, `started_at`→`ended_at`, observed together with the counter above. Buckets 60 s – 4 h (the default budget is 120 min). |
| `lab_tracker_requests_total` | counter | `binding` = `forge\|builtin`, `op` = `ready\|issues\|issue\|comment\|pulls\|create_pull\|close\|create_issue\|label_add\|label_remove\|labels\|label_ensure`, `result` = `ok\|error` | Tracker calls resolved through the registry seam (operator API, agent API, and AFK engine alike). Any non-nil error counts as `error` — domain conditions (not-found, duplicate PR) included. The seam reports only (binding, op, ok); error text and token bytes never cross it. |
| `lab_clone_jobs_total` | counter | `result` = `ready\|error` | Finished clone jobs. Jobs cancelled by a forced repo delete count neither result; startup healing of an interrupted clone is not a job and is not counted. |
| `lab_clones_in_flight` | gauge | — | Clone jobs currently running. |

Alerting suggestions:

- **AFK failures**: `increase(lab_afk_runs_total{outcome=~"death|timeout"}[1h]) > 3` — mirrors the three-strikes pause; check the repo page for the paused banner and the run history for `failure_reason`.
- **Tracker binding broken**: `rate(lab_tracker_requests_total{result="error"}[15m]) / rate(lab_tracker_requests_total[15m]) > 0.5` — a revoked forge token or unreachable forge starves the AFK done-signal (runs then die as timeouts).
- **Stuck instances**: `lab_instances_active` pinned at the cap for hours with no `lab_afk_runs_total` movement — agents running but never finishing.
- **Clone health**: `increase(lab_clone_jobs_total{result="error"}[1h]) > 0`, or `lab_clones_in_flight > 0` sustained longer than your largest repo needs.
- **Scrape source degraded**: `lab_instances_active` absent while the service is up (snapshot errors are deliberately silent per scrape).

## Push notifications

Standards Web Push (RFC 8292 VAPID) — no vendor account, no push-provider registration. lab signs every send with its own auto-generated VAPID keypair (`vapidKeyFile` / `--vapid-key-file` above) and talks directly to whichever gateway the operator's browser is subscribed through.

Three requirements, all outside lab's control:

- **The UI must be served over HTTPS.** `PushManager.subscribe()` only exists in a secure context; `baseUrl`/TLS termination (Deployment above) has to be in place first.
- **Outbound HTTPS from the server** to whichever push gateways the enrolled browsers use: `web.push.apple.com` (Safari/iOS), `fcm.googleapis.com` (Chrome/Edge/Android), Mozilla's autopush (Firefox). No inbound ports, no allowlisting beyond normal egress.
- **iOS needs 16.4+ with lab added to the Home Screen** and opened from there — Web Push on iOS only exists for an installed PWA, not the Safari tab. Enrolling always requires the enabling click itself: the browser only grants notification permission on a user gesture, so it can't be scripted or pre-provisioned.

**Enrolling a device**: Settings → Notifications → "Enable notifications on this device". Each browser/device that enables shows up in the device list below the button; the list is device-level, not per-user — a subscription survives logout and is removed only explicitly (per-device Remove) or by lab itself when a gateway reports the endpoint gone.

**Debugging**: each listed device has a Send test button. Delivery failure is deliberately silent in the UI (there is no user-facing error path for "the gateway rejected this") and loud in server logs (`component=push`) — check there first. A 404/410 from the gateway is expected lifecycle (lab reaps the row); anything else — including a gateway the server simply can't reach — logs and is dropped, nothing retries.

**Airgapped / no outbound HTTPS**: sends degrade to error logs, silently from the operator's point of view — there is no notification source yet other than Send test, so this only shows up when a real trigger (a future slice) starts firing pushes.

**Rotation consequence**: `vapid.key` carries the same never-overwrite contract as `master.key`, but replacing or deleting it (a restore from an older backup, a manual `rm`, key compromise) strands **every** subscription — the public key that signed them no longer matches, so pushes silently stop for all devices until each one re-enables from Settings.

## Incogni mode

Per-repo flag; when set, all seven measures of brief D15 §9 apply:

1. **Attribution off at the source** — every spawn seeds the worktree's `.claude/settings.local.json` with `attribution{commit:"",pr:"",sessionUrl:false}` + `includeCoAuthoredBy:false` (keys verified against Claude Code 2.1.198; `internal/compat/compat.md` §4, `claudecode.SeedAttributionOff`).
2. **Seed prompt** — the AFK seed prompt's commit step appends "No AI attribution, no Co-Authored-By, no generated-with footers anywhere." (`afk.SeedPrompt`).
3. **Server-side body sanitization** — the agent API strips Co-Authored-By/generated-with/Claude-Session lines from **every agent-authored body** — PR/CR, issue create, and comment create alike (ADR-0014) — before it reaches the tracker (`agentapi.sanitizeBody`).
4. **Neutral branch names** — incogni repos default to `issue-<N>` / `wip/`; claim parsing always uses the repo's configured pattern, never a literal `afk/`.
5. **Real git identity** — spawned sessions and CR merges author as the repo's configured `git_author_name`/`git_author_email` (falling back to the global settings), never a bot identity.
6. **Nothing lab seeds is committed** — `.claude/`, `CLAUDE.local.md` (and the seeded settings) are listed in `.git/info/exclude`, never `.gitignore`.
7. **Pre-push guard** — a pre-push hook in the bare reference repo (shared by all its worktrees) rejects pushes whose outgoing commits carry AI attribution in the message or touch lab-seeded files, naming the offending commit. Installed when incogni turns on, removed when it turns off.

**Triage workflows vs incogni**: the triage skill posts a body line disclosing AI-generated triage content. That line is body content, not an attribution trailer — the sanitizer passes it through by design. Running the triage workflow against an incogni repo is therefore an operator-level contradiction: pick one.

**Honesty note**: incogni cannot hide the forge account identity of the token used (pushes and PRs appear under that account), nor statistical style/timing signals of agent-authored work. It removes explicit AI attribution markers; it does not make the work's origin undetectable.
