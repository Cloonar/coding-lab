# lab — operations

Deployment, configuration, state, backup, CI, and observability for the `lab` server. New here? Start with [`getting-started.md`](getting-started.md). Design decisions are in [`adr/`](adr/); the original product contract is [`agent-brief.md`](agent-brief.md).

## Deployment

### NixOS module (recommended)

Import `nixosModules.lab` from this repo's flake. Options (authoritative defaults in [`nix/module.nix`](../nix/module.nix)):

| Option | Default | Meaning |
|---|---|---|
| `enable` | `false` | Enable the service. |
| `package` | this flake's `packages.lab` | The lab package; ships both `lab` and `labctl`, both land on the unit PATH. |
| `agentPackages` | `{ "claude-code" = pkgs.claude-code; codex = pkgs.codex; }` | Agent-CLI packages whose `bin/` is added to the unit PATH, an `attrsOf (nullOr package)` **keyed by lab provider ID** (the same strings the provider registry, the DB `provider` column, and the API use). Defaults merge **per key** (injected at `mkOptionDefault` priority), so `agentPackages."claude-code" = null` drops claude while the codex default survives, and adding a key keeps both defaults. `claude-code` is **unfree** in nixpkgs: allow it (`nixpkgs.config.allowUnfreePredicate = p: lib.getName p == "claude-code";`) or set the key to `null`. On a container-mode host every value may be `null` — provider CLIs there come from the agent-tools images, not the unit PATH (ADR-0057); the defaults are unchanged, so drop them explicitly. |
| `extraPackages` | `[ ]` | Extra `package`s appended to the unit PATH. Purely additive — never affects `agentPackages` or the fixed tools baseline. The sanctioned knob for a host-specific tool a session needs. |
| `claudePackage` | `null` | **Deprecated alias** for `agentPackages."claude-code"`. When non-null it populates that key and emits a deprecation warning; setting both it and an explicit `agentPackages."claude-code"` fails eval with an assertion naming both options. Prefer `agentPackages`. |
| `user` / `group` | `lab` / `lab` | Service identity. The default creates a `lab` system user with its home at `stateDir`; claude's own auth/config state lives under that HOME. |
| `stateDir` | `/var/lib/lab` | State root (layout below). Managed via systemd `StateDirectory` when left at the default; otherwise the operator provides the directory (lab creates missing children itself, 0700). |
| `listenAddr` | `":8080"` | Passed as `--addr`. |
| `baseUrl` | `null` | Passed as `--base-url`. Drives Secure-cookie detection and the CSRF Origin check — set it whenever lab sits behind TLS. |
| `agentUrl` | `null` | Passed as `--agent-url`. Session-facing base URL handed to `labctl` as `LAB_URL`. `null` (the default) leaves it unset, and lab hands every spawned session `unix://<state-dir>/agent/agent.sock` — the agent API's own unix socket (see [Agent socket](#agent-socket)), which never touches the network or any SSO/auth proxy in front of `baseUrl`. Set it only when sessions run off-host and must reach lab over TCP; never point it at the external/SSO-fronted origin (issue #30's failure mode). Container-runner sessions ignore a TCP value and always get the unix socket. |
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
| `extraFlags` | `[ ]` | Extra flags appended to `ExecStart` (e.g. `[ "--provider-bin" "claude-code=/run/current-system/sw/bin/claude" ]`). The container flags below are rendered **before** `extraFlags`, so a hand-rolled container flag here still wins during a migration. |
| `container.enable` | `true` | Master switch for container-runner host provisioning (see [Container runner](#container-runner)) — gates **all** the host mutations below, never their non-emptiness. Turns on `virtualisation.podman.enable` (crun + `policy.json`/`registries.conf`), puts podman + passt on the unit PATH, enables lingering for the service user (`users.users.lab.linger = true` — the permanent `user@<uid>.service` manager whose transient scopes place and cap every container, ADR-0060; no holder unit, no hygiene oneshot, no `RuntimeDirectory=` machinery), and adds the service user's subuid/subgid ranges — everything the container preflight verifies, in one option. **On by default** (ADR-0054): enabling lab makes the host container-ready out of the box. Set `false` for a host-only deployment — the unit is then byte-identical to the pre-container output (guarded by the `nixos-module` zero-diff check). Note an operator-forced `virtualisation.podman.enable = false` now conflicts at eval unless container mode is disabled too. |
| `container.toolsImageRepo` | `"ghcr.io/cloonar/agent-tools"` | OCI repository (no tag) the **default** `toolsImages` refs point into — where the agent-tools publish job ([`.github/workflows/agent-tools.yml`](../.github/workflows/agent-tools.yml)) pushes. The lab host pulls from it **anonymously** (the module provisions no registry credentials), which the publish job verifies after every push; the package must stay publicly readable ([Agent-tools images](#agent-tools-images) covers the one-time visibility flip). Only consulted by the `toolsImages` default. |
| `container.toolsImages` | versions.env tags | `attrsOf nonEmptyStr` keyed by **lab provider ID**, rendered into `--container-tools-image provider=ref[,provider=ref…]` (comma-joined) ahead of `extraFlags` ([Agent-tools images](#agent-tools-images), [Container runner](#container-runner)). Defaults to `<toolsImageRepo>:claude-<CLAUDE_CODE_VERSION>` / `<toolsImageRepo>:codex-<CODEX_VERSION>`, the CLI versions read from `containers/agent-tools/versions.env` (ADR-0054) — the committed pin the publish job builds from, so the default refs move exactly when the images they name are rebuilt under a new tag, and the deploy pipeline refuses to bump the host pin until they resolve in the registry. `container.enable` with this **explicitly emptied fails eval** (a container host with no tools image can spawn nothing). Keys are **not** validated against registered provider IDs at eval — the server's boot error names the registered IDs — and digest-pinning is documented, not enforced for hand-set refs. |
| `container.defaultImage` | `docker.io/library/buildpack-deps:stable-scm@sha256:07554a82a7a29ce00a048e0b29d18f454b5721b41940d43ee3be1ef59d55b114` | `nullOr nonEmptyStr`, rendered into `--container-image <ref>` (ahead of `extraFlags`) when set — the global **default** dev image for container repos whose own **Dev image** (`repos.image_ref`) is blank (see [Container runner](#container-runner)). Ships digest-pinned so a fresh deployment spawns out of the box with no container config at all (issue #230, ADR-0056). Explicit `null` remains a valid opt-out: when every container repo carries its own ref no global default is needed (ADR-0053), so there is still **no** assertion on it. |
| `container.subIdRange` | `{ start = 100000; count = 65536; }` | subuid/subgid range for the service `user` — rootless podman's `--userns=keep-id` cannot build its namespace without it. NixOS merges user attrs, so it also provisions ranges for an operator-brought `user`. Give a custom `start`/`count` to dodge collisions with other subid consumers on the host — worth an explicit look now that container mode defaults on: `100000` is also the range most tools hand a host's first user, and **neither NixOS nor lab detects an overlap** (overlapping ranges work but mean two users' containers share host uids). `null` opts out (bring your own `/etc/subuid`+`/etc/subgid` entries). Applied only under `container.enable`. |

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
- Unit PATH includes git, tmux, **openssh**, util-linux, `package` (for `labctl`), the fixed tools baseline (`gawk`, `gnutar`, `gzip`, `xz`, `zstd`, `unzip`, `curl`, `jq`, `file`, `patch`, `procps` (`ps`), `ripgrep` (`rg`), and `nix` via `config.nix.package` — so a session can run a project flake's devshell for per-project toolchains), every non-null `agentPackages` value, and `extraPackages`. The baseline is fixed, not an option (a contract every provider's session can assume; `extraPackages` is the additive knob); language toolchains are deliberately excluded and come from each project's flake. On a container-mode host every `agentPackages` value may be `null` — the agent-tools images are the CLI distribution there (ADR-0057). openssh is load-bearing: origins are SSH remotes and git forks `ssh` off PATH — without it every fetch dies with "cannot run ssh".
- `ExecStart` uses systemd escaping (`escapeSystemdExecArgs`), not shell quoting — `%` and `$` in a DSN survive.

**nixpkgs pin**: the flake input must ship `go_1_26`. Currently `github:NixOS/nixpkgs/nixos-unstable`, locked at `148bab9c1c3c53136ecb44a6ea356a0ed5b39b06`. Record pin changes here.

**Debugging sessions**: the module installs tmux system-wide; attach to a live agent session with `sudo -u lab tmux attach -t '<repo>~<label>'` (detach with `C-b d` — never kill the pane; Stop from the UI so the guarded teardown runs).

### Agent socket

Alongside its TCP listener (`--addr`, web UI / human auth only), lab always serves the agent API (`/agent/v1`, run-token auth) on a unix domain socket at `<state-dir>/agent/agent.sock` — mode 0700, owned by the service user. A stale socket file from an unclean shutdown is removed and recreated on every boot; the state dir itself is already 0700, so the socket needs no separate ACL.

**Path move (issue #205 / ADR-0052)**: the socket used to live at `<state-dir>/agent.sock`. It moved into its own directory so the [container runner](#container-runner) can bind-mount the socket's *directory* rather than the socket file — a socket-file bind would pin a dead inode across a server restart, while the directory mount makes the recreated socket visible to containers that outlive the restart. A back-compat symlink is kept at the old `<state-dir>/agent.sock`, so an operator-set `LAB_URL=unix://…/agent.sock` (or any tooling pointing at the old path) keeps working; no action is needed on upgrade.

This is what `LAB_URL` resolves to for every spawned session by default (`--agent-url` unset, the `null` default of `services.lab.agentUrl` — see the option table above): `unix://<state-dir>/agent/agent.sock`. `labctl` accepts `LAB_URL=unix:///abs/path` in every subcommand — including `secret exec`, `secret scan`, and therefore the pre-push guard hook — with http(s) `LAB_URL` values behaving exactly as before.

The socket directory is what the container runner bind-mounts into each instance's container, so containerized sessions reach the agent API without any host-network exposure — a container run always gets the unix `LAB_URL` (a TCP `--agent-url` is deliberately unreachable from inside, see [Container runner](#container-runner)).

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
- **Do not route agent traffic through the proxy.** `labctl` inside a session authenticates to `/agent/v1` with a run token only — no SSO session, no cookies. If its `LAB_URL` pointed at the external origin, every call would hairpin out to the proxy, get 302'd to the login portal, and fail. Out of the box this can't happen: lab serves `/agent/v1` on a unix socket (`<state-dir>/agent/agent.sock`, see [Agent socket](#agent-socket)) and hands sessions `LAB_URL=unix://<state-dir>/agent/agent.sock` by default — nothing hairpins through the proxy because agent traffic never touches TCP at all. The warning still applies if an operator sets `services.lab.agentUrl` (`--agent-url`) to reach lab over TCP (off-host sessions): point it at a host lab is reachable on directly, never at `baseUrl`'s public origin. If you ever see `labctl` fail with an HTML/redirect error after setting `agentUrl`, `LAB_URL` is aimed at the proxy — check the value.
- Keep `proxy_buffering off` for `/api/v1/events` or the SSE stream stalls.

### Bare metal

Any Linux host works:

1. Build or download `lab` and `labctl` (static binaries, CGO-free): `make lab labctl` or `nix build .#lab`.
2. Ensure on PATH: `git`, `tmux`, `ssh` (openssh), `prlimit` (util-linux) — and `labctl` (agent sessions resolve it from PATH). Provider CLIs (`claude`, `codex`) are required only for host-runner repos and for servers with no container config at all; a container-mode host needs none of them — the agent-tools image is the CLI distribution there ([Container runner](#container-runner), ADR-0057).
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
| `--prlimit` | — | `prlimit` (PATH lookup) | prlimit binary (session NOFILE cap, host-runner panes only). |
| `--podman` | — | `podman` (PATH lookup) | podman binary for container-runner panes (see [Container runner](#container-runner)). |
| `--container-image` | `LAB_CONTAINER_IMAGE` | (empty) | Global **default** dev image for containerized sessions — the ref a container repo runs in when its own **Dev image** setting (`repos.image_ref`, repo settings → Runner) is blank. A repo's own ref overrides it; lab ships no image of its own (the operator owns the container userland, ADR-0051). Empty is allowed now that repos can carry their own ref (ADR-0053): preflight no longer refuses on an unset global — a container spawn with no effective image (repo ref blank *and* this unset) is refused at spawn instead, naming both knobs. |
| `--container-tools-image` | `LAB_CONTAINER_TOOLS_IMAGE` | (empty) | Agent-tools image refs as `provider=ref[,provider=ref…]`, keyed by provider ID (unknown IDs and duplicate keys are boot errors). Refs should be `@sha256`-pinned per ADR-0051 (documented, not enforced — a local tag is fine during bring-up). The flag value replaces the env value wholesale. Empty = unconfigured: container spawns are refused by preflight. |
| `--max-instances` | — | `6` | Global live-instance cap. Seeds the `max_instances` settings row on **first start only**; thereafter the runtime setting wins. Must be ≥ 1. |
| `--session-nofile` | — | `16384` | RLIMIT_NOFILE (soft+hard) for spawned sessions via prlimit; `0` disables the cap. |
| `--proxy-auth` | — | off | Accept the proxy auth header from trusted proxies. |
| `--proxy-auth-header` | — | `Remote-User` | Header carrying the proxy-authenticated username. |
| `--trusted-proxies` | — | (empty) | Comma-separated CIDRs of trusted reverse proxies (also gates `X-Forwarded-Proto` / `X-Forwarded-For` trust). |
| `--base-url` | `LAB_BASE_URL` | (empty) | Absolute http(s) external URL. Drives Secure cookies and the CSRF Origin check. Does **not** feed `LAB_URL` — agent traffic never derives from it (see `--agent-url`). |
| `--agent-url` | `LAB_AGENT_URL` | (empty) | Session-facing URL handed to `labctl` as `LAB_URL` — absolute http(s), or `unix:///abs/path` for a custom socket. Precedence: `--agent-url` when set, else `unix://<state-dir>/agent/agent.sock` (see [Agent socket](#agent-socket)) — the old `--base-url` / loopback-TCP fallbacks are gone (they were issue #30's SSO-proxy hairpin failure mode; the socket always exists, so there's nothing left to fall back through). `labctl` accepts `LAB_URL=unix:///abs/path` in every subcommand alongside http(s); set `--agent-url` only for off-host sessions that must reach lab over TCP, or to point runs at a different socket path. |

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
| `container_memory` | `8g` | `--memory` for container-runner panes (podman's grammar: integer + optional b/k/m/g); per-repo override on the repo row. Ignored by host-runner repos. |
| `container_pids` | `4096` | `--pids-limit` for container-runner panes; per-repo override on the repo row. |
| `container_nofile` | `16384` | `--ulimit nofile` (soft+hard) inside the container — replaces the host prlimit cap for container-runner panes; per-repo override on the repo row. |

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
  agent/agent.sock           agent API unix socket, 0700, recreated on boot;
                             the directory is what the container runner mounts
  agent.sock                 back-compat symlink to agent/agent.sock (the
                             pre-#205 socket path)
  repos/<repoID>.git/        bare reference clones (worktree parents)
  worktrees/<repo>-<label>/  instance worktrees (manual: -<label>, AFK: -<N>)
  runtime/                   0700 — materialized credential files for repo-level
                             ops (clone/fetch: <credID>.<opID>.key/.askpass/
                             .sshpass), known_hosts
  instances/<runID>/home/    0700 — per-run private HOME (issue #202): the
                             provider credential copy, config, and transcripts;
                             created at launch, wiped at stop/rollback, swept at boot
  instances/<runID>/runtime/ 0700 — per-run runtime dir (issue #205): the run's
                             materialized git credential files, known_hosts,
                             dialog spool, and --settings file; same lifecycle
                             as home/, bind-mounted into the run's container
  logins/                    0700 — per-attempt scratch HOMEs for containerized
                             provider login (ADR-0057); wiped at login teardown
```

Sessions are named `<repo>~<label>`; `~` never appears in paths (the Windows-8.3 lookalike pattern stalls Claude).

**Sizing note — container image store.** When any repo uses the [container runner](#container-runner), rootless podman's image store also lives under the state dir, at `<state>/.local/share/containers` (the lab user's HOME is `<state>`). Dev images and agent-tools images land there and typically dominate disk usage — several GB and up. This is by design and not separately configurable; account for it when sizing the `stateDir` volume (see [Container runner](#container-runner)). It is reconstructible (pull-if-missing at spawn re-fetches), so it is not part of the backup set below.

## Backup & restore

Back up, **consistently together** (one snapshot set):

- `<state>/lab.db` (or the Postgres database) — config, credentials (encrypted), built-in tracker, run history.
- `<state>/master.key` (or the sops-managed key file) — without it, credential payloads are unrecoverable.
- `<state>/repos/` — the bare reference clones (claims and parked branches live here as git refs).

Explicitly **excluded** (reconstructible or ephemeral):

- `<state>/runtime/` — materialized key files, known_hosts; 0700, regenerated per operation, swept at startup.
- `<state>/instances/` — per-run private HOME + runtime trees (issues #202/#205) holding the provider credential copy, config, transcripts, and the run's materialized git credentials and spool; 0700, created at launch, wiped at stop/rollback, swept at boot — ephemeral, never restored.
- `<state>/logins/` — per-attempt scratch HOMEs for containerized provider login (ADR-0057); wiped at login teardown, never restored.
- `<state>/worktrees/` — recreated from branches; dirty worktrees are parked work the operator resolves *before* decommissioning a host (a backup cannot carry uncommitted changes safely).
- `<state>/agent/` (and the `<state>/agent.sock` compat symlink) — the agent API unix socket (see [Agent socket](#agent-socket)); a stale socket file is removed and recreated on every boot, so it carries no state to preserve.
- `<state>/.local/share/containers/` — rootless podman's image store on [container runner](#container-runner) hosts (often several GB, see the sizing note above); every ref is digest-pinned and re-pulled on demand (pull-if-missing at spawn), so it restores itself.

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

Pull requests are gated by **GitHub Actions**, on GitHub's hosted `ubuntu-latest` runners (ADR-0023). Two gates run on PRs, a third is path-gated to the container images, and the deploy — the only workflow still on Forgejo — polls from the other side (below).

- **The native gate** ([`.github/workflows/ci.yml`](../.github/workflows/ci.yml), job `native`) runs on **every** PR and is a **required** status check. It builds and tests directly on the stock `ubuntu-latest` runner — no nix — mirroring the flake's non-nix checks: the SPA's eslint + prettier + vitest + `vite build`, then the `ui`-tagged Go build and `go test -tags ui ./...` against the copied dist, then the untagged `golangci-lint run ./...` (`CGO_ENABLED=0` throughout). Common-path PRs finish in ~2–4 min instead of ~12–13. Its toolchain is version-matched to the flake: Go from `go.mod` (`go 1.26`), Node 24 (`pkgs.nodejs`), golangci-lint 2.12.2 (the version nixpkgs ships at the current `flake.lock`).
- **The hermetic gate** ([`.github/workflows/ci-nix.yml`](../.github/workflows/ci-nix.yml), job `flake-check`) runs the full `nix flake check` — the authoritative build, identical to the local command — but only when nix or the Go dependency set changes (`paths:` = `**/*.nix`, `flake.lock`, `go.mod`, `go.sum`). It installs nix in-job (Determinate installer, default daemon init — the hosted runner's job user is non-root, so store access goes through the nix-daemon; the daemonless `--init none` variant is only for root-in-container runners like the Forgejo deploy), and the deploy re-runs it as a merge-to-main backstop (`bump-nixos-pin` → `test-configuration` rebuilds `packages.lab`). `go.mod`/`go.sum` are load-bearing triggers: a dependency bump stales `nix/package.nix`'s `vendorHash`, which the native gate (live `go mod download`) cannot catch.

**Branch protection.** The required status checks are **`native`** and **`flake-check`** — both are job ids, and both ids are kept stable deliberately, because a branch-protection rule matches them as strings and a rename silently changes what protection requires. `flake-check` is path-gated, and on GitHub a *required* check that never runs leaves the PR pending on "Expected" forever, so [`.github/workflows/ci-nix-noop.yml`](../.github/workflows/ci-nix-noop.yml) exists as its twin: the same workflow name (`ci-nix`), the same job id (`flake-check`), the inverse trigger (`paths-ignore` mirroring the real workflow's `paths`), one `echo` and exit 0. Every PR therefore gets a `flake-check` result from one workflow or the other. The two filters are not strict complements — a PR touching both a gated and an ungated file matches both, both run, and two `flake-check` results report — which is harmless because the twin always passes; the case that *would* wedge a PR cannot occur, since a PR matching none of the `paths` necessarily matches `paths-ignore`.

**Superseded runs.** Each PR workflow declares a `concurrency` group keyed on workflow + ref with `cancel-in-progress`, so a force-push cancels the runs it obsoleted instead of paying for both. Two deviations are deliberate: `agent-tools` cancels on `pull_request` only (`cancel-in-progress: ${{ github.event_name == 'pull_request' }}`), because a publish run killed between two tag pushes leaves the registry holding half a release and one killed before the verify step skips the check that the tags are consumable; and the no-op twin keys its group on a literal prefix rather than `${{ github.workflow }}`, because that expression expands to the `name:` it shares with the real `ci-nix` workflow — one group for both, and on a PR where both run the later starter would cancel the earlier, reporting a *cancelled* `flake-check`, which is precisely the wedged required check the twin exists to prevent.

**Native gate** (ci.yml) — the runner needs:

- `actions/checkout@v7`, `actions/setup-go@v7` and `actions/setup-node@v7` to resolve from github.com. Actions are pinned by **major tag** (`@v7`; `@v6` for the `actions/cache/*` steps in the agent-tools workflow), not by commit SHA — a floating major that picks up the action's own fixes, which is the trade this repo takes.
- **GitHub's hosted actions cache.** `setup-go` caches the Go module + build cache (keyed on `go.sum`) by default and `setup-node` caches `~/.npm` (`cache: npm`); with the cache backend unreachable those steps fail rather than silently skip. The agent-tools workflow's `actions/cache/restore` + `actions/cache/save` steps depend on the same backend.
- Outbound egress for `proxy.golang.org` (Go modules), `registry.npmjs.org` (npm), and `raw.githubusercontent.com` + `github.com` (the actions themselves, plus the pinned golangci-lint installer script and its release binary). No nix, no `cache.nixos.org`, no Determinate installer on this gate.
- `git`, `tmux`, and `prlimit` (util-linux) on PATH for `go test` (real subprocesses, the D17 bar). `git` and `prlimit` ship on the hosted image; `tmux` does not, so the job `apt-get install`s whatever is missing — apt must be reachable, and the job user needs `sudo` (the hosted runner grants it passwordless).

**Hermetic gate** (ci-nix.yml) — the only gate that installs nix, and only on nix/dep changes:

- Outbound egress (or in-instance mirrors/substituters) for the installer (`install.determinate.systems`), `cache.nixos.org` (binary substitutes for the Go toolchain and nixpkgs, so they are not rebuilt from source), `proxy.golang.org`, `registry.npmjs.org`, and the flake inputs (github.com for nixpkgs).
- Root, or `sudo`, so the installer can create `/nix` — the hosted runner's job user has passwordless `sudo`.
- Enough disk for the nix store: the check builds the `lab`/`web` outputs and node_modules (via `importNpmLock`), runs the Go suite against real git/tmux/prlimit inside the sandbox, evaluates the nixos module, and builds the container-enabled NixOS system closure (`nixos-container-closure`, substituted from `cache.nixos.org`).

The store suite additionally runs against a real Postgres wherever `LAB_TEST_POSTGRES_DSN` is set; [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) carries a ready-made `store-postgres` job as a commented template (service container + DSN export, plus the same in-job nix install) — uncomment it once the postgres store lands.

**Agent-tools gate** ([`.github/workflows/agent-tools.yml`](../.github/workflows/agent-tools.yml), see [Agent-tools images](#agent-tools-images)) — path-gated to `containers/**` and the workflow itself, so it does not run on common-path PRs. Where it does run it needs, beyond the native gate's toolchain:

- **`podman`**, which ships preinstalled on the `ubuntu-latest` image. The runner is a full VM — real cgroups, a real `/proc`, no nested-container root — so the stock setup works untouched: default overlay storage, default (OCI-runtime) build isolation. Both jobs therefore *configure* nothing and only assert: `podman version` followed by `podman info`, so a runner-image change that moves the storage driver or the default runtime out from under us appears in the log instead of as a mystery build failure. That is also what the gate is worth: the injection smoke test exercises podman the same way the NixOS hosts do, not through a CI-only configuration.
- Outbound egress for `downloads.claude.ai` (the Claude Code `linux-x64-musl` binary + its `manifest.json`), `github.com` (the codex release asset), and `docker.io` (the `debian:stable-slim` and `alpine` base images the injection smoke test mounts into). The release leg additionally reaches `ghcr.io`, to push and then to pull the published tags back anonymously. The CLI artifacts are fetched by `build.sh` on the runner (sha256-verified against `versions.env`, re-verified in-stage) and cached across runs keyed on `versions.env`, so the CDN fetch happens once per version bump — repeated per-run pulls tripped per-IP throttling on `downloads.claude.ai`. Restore and save are separate steps (`actions/cache/restore` + `actions/cache/save`, `if: always()`) rather than the paired `actions/cache`, which only saves on job success and would discard the fetch of exactly the runs that most need it banked.

**The Forgejo deploy poll** ([`.forgejo/workflows/deploy.yml`](../.forgejo/workflows/deploy.yml)) is the only workflow left on Forgejo, and it is not a PR gate: it bumps the coding-lab pin in the private `Cloonar/nixos` repo. It stayed behind because its credential did — `NIXOS_PIN_TOKEN` grants write to that private repo on `git.cloonar.com`, and the point of the split is that the public GitHub repo never holds it, so the token deliberately never becomes a GitHub secret. Everything else about the workflow follows from living on the wrong side of the split:

- **It triggers on a 5-minute `schedule`** (plus `workflow_dispatch` as the manual knob), not on `push`. main only moves on GitHub now and this side is a mirror, so a push trigger would either never fire or fire on whatever the mirror happened to receive, whenever it received it; a schedule is the only event this forge reliably owns. A bump run can outlive the interval, so the workflow serializes on a single `deploy-nixos-pin` concurrency group with `cancel-in-progress: false`.
- **Every fact it acts on comes from GitHub, never from the Forgejo checkout** — the job has no `actions/checkout` at all. It resolves `refs/heads/main` on `github.com/Cloonar/coding-lab` with `git ls-remote`, then reads `containers/agent-tools/versions.env` and `nix/module.nix` out of an anonymous shallow fetch of exactly that rev, so the sha it pins, the agent-tools tags it probes and the dev-image digest it probes all come from one consistent commit. A mirror that has fallen behind polls with a stale copy of the workflow file, never with a stale rev.
- **A no-op poll costs seconds.** 288 runs a day, nearly all no-ops, so the idempotency check runs first: resolve the sha, clone `Cloonar/nixos`, apply the `sed`, and stop at "pin already current" when `git diff --quiet` holds — two clones, no nix install, no registry probes. Only a run that actually moves the pin installs nix, fetches the rev, probes the registries and runs `test-configuration`. The bump is *staged* before those gates and *pushed* only after them, so nothing reaches `Cloonar/nixos` until every default ref resolves and the NixOS config rebuilds green.
- **The bump path's prerequisites are the hermetic gate's**, plus two: whatever `test-configuration` needs beyond nix (e.g. KVM for VM tests), and anonymous pull access to `ghcr.io` (the agent-tools tags the pinned rev's `versions.env` names) and `docker.io` (`container.defaultImage`'s digest-pinned default) for the skopeo probes — which run credential-free on purpose, because that is the exact pull every host's preflight performs. A no-op poll needs none of it: `github.com` and `git.cloonar.com` only.

## Agent-tools images

Per-provider **agent-tools** OCI images carry an agent CLI and a static `labctl` INTO an operator-chosen dev container, so the agent surface travels with lab instead of being baked into the base image. Built and published by [`.github/workflows/agent-tools.yml`](../.github/workflows/agent-tools.yml) from `containers/agent-tools/`; the design rationale (libc mechanism, alternatives) is [ADR-0051](adr/0051-agent-tools-oci-images.md), and the consumer is the container runner (issue #205). This complements the ADR-0033 tools baseline (which puts CLIs on the lab *unit* PATH) rather than replacing it: the baseline serves host-PATH sessions, these images serve sessions running inside an arbitrary container.

**What the images are.** One image per provider, `FROM scratch` (pure payload, never run as a container), tagged `ghcr.io/cloonar/agent-tools:<provider>-<cli-version>`. claude and codex ship today; gemini is deferred until the gemini adapter (#126) lands. Exact contents:

| Image | Root filesystem |
|---|---|
| `agent-tools:claude-<ver>` | `/bin/claude` (Claude Code `linux-x64-musl` native build), `/bin/labctl` (static, built from this repo), `/lib/ld-musl-x86_64.so.1` (the musl loader, from Alpine) |
| `agent-tools:codex-<ver>` | `/bin/codex` (upstream static-pie musl binary), `/bin/labctl` |

Nothing else — no shell, no userland. The image ships ONLY lab-owned binaries; the userland a session assumes (`tar`, `curl`, `jq`, `rg`, …) is the dev image's own business, deliberately NOT imposed by this mount (ADR-0033's baseline is for host-PATH sessions, not this seam).

**The injection contract.** The container runner mounts the image read-only into the operator's chosen dev container and prepends `/opt/lab/bin` to PATH:

```
podman run --mount type=image,src=ghcr.io/cloonar/agent-tools@sha256:…,dst=/opt/lab …
# binaries then resolve at /opt/lab/bin/claude, /opt/lab/bin/codex, /opt/lab/bin/labctl
```

`/opt/lab` is a **HARD contract**, not a convention: the claude binary's ELF interpreter (`PT_INTERP`) is rewritten at image-build time to the absolute path `/opt/lab/lib/ld-musl-x86_64.so.1`. Mount the image anywhere else and claude will not start. This is what lets claude run on both a glibc base (debian) and a musl base (alpine) without using the base image's libc — the one bundled musl loader IS the only libc claude consults. codex is static-pie with no interpreter and runs as-is; `labctl` is pure-Go `CGO_ENABLED=0` and needs no loader.

**Tagging + digest pinning.** The `<provider>-<cli-version>` tag is what the NixOS module's `toolsImages` **default** derives from `versions.env` (ADR-0054) — the committed pin: the same file that drives the build supplies the consumer refs, so the default only ever moves together with a publish, and [deploy ordering](#container-runner) plus the host preflight's per-boot re-pull keep tag movement honest. For hand-set refs the **digest** is the strict pinning contract: a same-tag re-push (e.g. a `labctl` refresh at an unchanged CLI version, via `workflow_dispatch`) produces a NEW digest under the SAME tag, so the tag is a moving reference and strict consumers pin `agent-tools@sha256:…`. The release job emits the digest-pinned reference to its job summary.

**The version catalog.** `containers/agent-tools/versions.env` is the single source of truth for provider CLI versions and artifact checksums, sourced by both the build scripts and the publish job:

- `CLAUDE_CODE_VERSION` + the sha256 of the `linux-x64-musl` binary (from Anthropic's per-version manifest at `downloads.claude.ai/claude-code-releases/<ver>/manifest.json`).
- `CODEX_VERSION` + the sha256 of `codex-x86_64-unknown-linux-musl.tar.gz` (from the GitHub release's per-asset digest).

These versions ARE the repo's compat-record pins (`internal/compat/compat.md` pins Claude Code, `internal/compat/codex/compat.md` pins codex-cli) — versions.env is what makes those prose pins actual build inputs. **Bump procedure**: re-verify the compat record against the new CLI version FIRST (the compat doc is the checklist), THEN bump the version + sha in versions.env. The PR runs the injection smoke test; the merge to main publishes. A version bump that skips the compat re-verification is the failure mode to guard against — it ships an unverified CLI under a pin that claims verification.

**CI legs** (deliberately asymmetric on secrets):

- **PR leg** (job `smoke`, on `pull_request`): builds both images locally and runs the injection smoke test — mount each into stock `debian:stable-slim` and `alpine` and run `claude --version` / `codex --version` / `labctl --help`. It declares `permissions: contents: read`, references NO secret, and NEVER pushes, so a PR from any branch (forks included) exercises the full build+inject path without ever holding a write credential.
- **Release leg** (job `publish`, on `push` to `main` + `workflow_dispatch`, the only leg `pull_request` cannot reach): re-runs the smoke test (cheap insurance that exactly what is pushed passes injection), pushes the digest-pinned tags to `ghcr.io`, then **verifies the tags pull back anonymously** into a scratch storage root with matching digests: anonymous pull is the deployment contract (the NixOS module provisions no registry credentials), so a package accidentally made private fails the publish here instead of on every host's preflight. Both legs are path-gated to `containers/**` and the workflow file so unrelated merges don't re-release — images rebuild only when their inputs change (ADR-0054), and the module's default refs (from `versions.env`) move only together with such a rebuild. `workflow_dispatch` is the manual re-release knob: `labctl` is built from the repo INSIDE the image build, so a labctl change that should be baked into fresh images is shipped by dispatching this workflow even with no `containers/**` diff — hosts pick the re-pushed tag up at their next restart (preflight re-pulls per boot).

**Operator provisioning — nothing to provision, one thing to click.** Auth for the push is the built-in `secrets.GITHUB_TOKEN`, scoped by the `publish` job's `permissions: packages: write` (the `smoke` job, the leg fork PRs run, declares `contents: read` and reads no secret at all). There is no operator-provisioned registry secret: nothing to create before a fresh clone of this repo can release, and nothing to rotate. The registry host is hardcoded `ghcr.io` — it is a different host from `github.server_url`, so there is nothing in the Actions context to derive it from — while the namespace stays derived from `github.repository_owner`, lowercased for OCI, so a fork publishes into its own namespace instead of silently at ours.

The one manual step is **making the ghcr package public**, once: on the `cloonar` organization, **Packages → `agent-tools` → Package settings → Change visibility → Public**. A ghcr package is private on first publish and `GITHUB_TOKEN` cannot set package visibility, so the workflow cannot do it. It is not optional either — the NixOS module provisions no registry credentials, so every host's preflight pulls the default `toolsImages` refs anonymously and the deploy's skopeo probe resolves them anonymously too; while the package is private both fail. The release leg's anonymous-pull verify step is the catch: it logs out of the registry, pulls each published tag into a scratch storage root, and on failure fails the run with exactly that click path in the error — so a still-private package surfaces in CI rather than as spawn refusals on every host.

**x86_64-only today.** Both upstreams publish arm64(-musl) artifacts, so an arm64 variant is a mechanical follow-up (a second set of tags off the same versions.env and Containerfiles) once an arm64 runner/host exists.

## Container runner

A repo whose **runner** is `container` (repo settings → Runner) runs each session's pane command as `podman run -it --rm …` — rootless podman + crun, tmux still host-side owning liveness/attach/capture — per [ADR-0052](adr/0052-container-runner.md). `host` (the default) keeps today's pane — the provider CLI directly on the host under the prlimit nofile cap — as break-glass, labeled "unsandboxed — full host access" in the UI. Flipping a repo to `container` requires host provisioning; the startup **preflight** verifies all of it and refuses container spawns with actionable errors until the host passes.

**What the host must provide** for `repos.runner = container`:

**On NixOS the module provisions all of it by default** through `services.lab.container.*` ([option table](#nixos-module-recommended) above): since ADR-0054, `services.lab.enable` alone makes the host container-ready — `container.enable` defaults `true` and `toolsImages` defaults to the published agent-tools images pinned by the source's own `versions.env`, so a plain deployment needs **no** container config at all. The deploy pipeline (`deploy.yml`) refuses to bump the host pin until those default refs resolve in the registry, so a rev never deploys ahead of the images it names — since issue #230 that skopeo probe covers `container.defaultImage`'s pinned `buildpack-deps:stable-scm` ref too (ADR-0056), which knowingly adds `docker.io` to the probe's dependencies alongside the agent-tools registry. Enabling turns on rootless podman, puts podman + passt on the unit PATH, enables lingering for the service user (the `user@` manager that owns container cgroup placement, ADR-0060), and adds the service user's subuid/subgid ranges; the bullets below are what that provisioning *amounts to* (and what a non-NixOS host assembles by hand). Overrides:

```nix
services.lab.container = {
  # host-only deployment (opt OUT — the unit reverts to the byte-identical
  # pre-container output):
  # enable = false;

  # hand-pinned tools refs instead of the versions.env-tagged defaults:
  # toolsImages."claude-code" = "ghcr.io/cloonar/agent-tools@sha256:…";

  # per-repo-only deployment (opt OUT of the pinned buildpack-deps default —
  # every container repo carries its own Dev image, ADR-0053/ADR-0056):
  # defaultImage = null;
};
```

Preflight (below) stays the runtime authority on host readiness regardless of distro; the module provisions exactly what it checks.

- **podman >= 4, crun, and passt `2025_04_15` or newer** on the service PATH. Preflight probes `podman` (the `--podman` flag names the binary) and `pasta` (the network backend the argv pins — shipped by the `passt` package); crun is podman's default OCI runtime on current distros and is not probed separately. The `pasta` check is a *capability* probe, not just a `LookPath`: the pane argv pins `--network=pasta:--map-guest-addr,none` so podman's default mapping of `host.containers.internal` onto the host's *global* address is gone (ADR-0052 as amended by #216 — that default left every host service bound to a wildcard address, lab's own `--addr :8080` included, reachable from inside every container), and because dropping the mapping only makes podman fall back to the first *other* global-unicast host address it can find, the argv also pins `--add-host host.containers.internal:127.0.0.1` and `--add-host host.docker.internal:127.0.0.1` — user `--add-host` entries are written first and suppress podman's automatic one for the same names, so both resolve to the container's own loopback and refuse instantly. Egress is untouched, so a wildcard-bound host service on a *non-shadowed* host address (a bridge, a second NIC, a VPN) stays reachable **by raw IP** from inside a container: bind co-located host services to `127.0.0.1` — on a multi-address host that is the control, not defense in depth. `--map-guest-addr none` needs passt `2025_04_15` (`2024_08_21` added the option, but every release through `2025_03_20` rejects the value `none`) and podman's backwards-compat retry covers only the default it appends itself — so whenever pasta is on PATH preflight runs the exact option and value the argv uses, `pasta --map-guest-addr none --version`, and fails the same `pasta` check with an upgrade-passt hint on a non-zero exit, instead of trusting a version parse or a `pasta --help` grep (a host in that window passes both, then fails every container at start). Upgrade the `passt` package (NixOS: it comes with `container.enable`; by hand, whatever your distro calls passt/pasta) if that check fires. NixOS: `container.enable` turns on `virtualisation.podman.enable` (which pulls in crun plus the `policy.json` / `registries.conf` podman resolves against) and puts podman + passt on the unit PATH — the exact PATH preflight probes and the pane argv resolves against.
- **subuid/subgid ranges for the service user** — rootless podman cannot build the user namespace `--userns=keep-id` needs without them. NixOS: `container.subIdRange` (default `{ start = 100000; count = 65536; }`) provisions them, and because NixOS merges user attrs it covers an operator-brought `user` too; `null` opts out. By hand: `usermod --add-subuids 100000-165535 --add-subgids 100000-165535 lab` (or the equivalent `lab:100000:65536` lines in `/etc/subuid` and `/etc/subgid`).
- **cgroup v2 with a lingering user manager ([ADR-0060](adr/0060-containers-as-user-manager-scopes.md), superseding [ADR-0059](adr/0059-payload-cgroup-holder-unit.md) and [ADR-0058](adr/0058-restart-safe-container-cgroup-layout.md)).** Container panes run `podman --cgroup-manager=systemd run …`: each container becomes its own transient `libpod-<id>.scope` under the lab user's `user@<uid>.service` manager, and the `--memory`/`--pids-limit` caps bind as per-scope `MemoryMax`/`TasksMax` on the scope's own cgroup. lab performs **no cgroup placement of its own** — cgroup v2 gates a PID migration on write access to the *common ancestor* of source and destination, which is why every unprivileged layout failed (ADR-0058's in-unit split: restart-`EBUSY`; ADR-0059's `lab-payload.service` holder: spawn-`EACCES` on every `podman create`, never having worked in production); the user manager forwards each attach to PID 1, so the ancestor rule never applies. `lab.service` is plain — lab, the tmux server, and every host process in its ordinary unit cgroup; no `Delegate=`, no holder unit, no hygiene oneshot, no per-spawn guard. Each old failure mode is gone structurally rather than pinned: no unit lab manages delegates, so a lab restart can never `EBUSY`; containers survive lab restarts by construction (independent scope units); and `user@lab` is systemd's own machinery, never restarted by nixos-rebuild, with no survivor-`EBUSY` mode either (stopping it kills its whole subtree). Preflight's **`user-manager`** check probes that `/run/user/<uid>` is writable and fails with the linger hint (`users.users.lab.linger = true`; by hand `loginctl enable-linger lab`) — lab retries it on its own, because logind brings `user@` up asynchronously at boot and lab cannot order on it. The **`spawn-probe`** check then proves the pipeline end-to-end with a real spawn: `podman create` + `podman init` a probe container named `lab-preflight-probe` (deliberately outside the `labrun-` namespace so the boot orphan sweep never races it) off the first resolvable agent-tools image with `--memory 64m --pids-limit 16` and command `/bin/labctl` (`init` pauses before exec, so nothing in the image ever runs), assert it landed in a `libpod-*.scope` under the user manager with the scope cgroup's `memory.max`/`pids.max` reading exactly `67108864`/`16`, then rm. The old preflight verified controllers and ownership but never performed the PID migration that actually fails; the spawn probe preserves #205's no-false-green property for real.
- **The user manager's runtime dir — no unit machinery.** Lingering gives the service user `/run/user/<uid>`, up from boot and independent of `lab.service`'s lifecycle; rootless podman's runroot/tmpdir live there, so container runtime state survives lab restarts with nothing to preserve. lab sets `XDG_RUNTIME_DIR=/run/user/<uid>` and `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus` process-wide at its own startup (the uid is unknown at nix eval time, so the unit cannot carry them); the tmux env baseline forwards both into every pane, and on adopting a surviving pre-ADR-0060 tmux server its server-global env is re-pinned to the current baseline so stale `/run/lab` values cannot leak into new panes. The former `RuntimeDirectory=lab` + `XDG_RUNTIME_DIR=/run/lab` + `RuntimeDirectoryPreserve=yes` machinery (ADR-0057) is retired — there is nothing to configure by hand beyond the linger.
- **The image knobs** ([module table](#nixos-module-recommended) / flags table above): `container.defaultImage` (`--container-image`) is the global **default** dev image sessions run in — the module now ships it digest-pinned to `buildpack-deps:stable-scm` (issue #230, ADR-0056), so a plain deployment spawns out of the box with no container config at all. A repo's own **Dev image** ref (`repos.image_ref`, **Dev image expectations** below) still overrides it, and explicit `null` remains a valid opt-out for a per-repo-only deployment (ADR-0053) — preflight still doesn't refuse on it (a spawn with no effective image — repo ref blank *and* the global explicitly `null` — is refused at spawn instead, naming both knobs). `container.toolsImages` (`--container-tools-image provider=ref[,provider=ref…]`) carries the agent-tools refs from the release job ([Agent-tools images](#agent-tools-images), ADR-0051) and — unlike the dev-image knob — *is* preflight-checked: preflight **pulls every configured tools ref on each boot** (pull-first so a moved tag reaches the host, ADR-0054; an up-to-date tag makes it a cheap manifest check) and falls back to a locally cached image with a logged warning when the registry is unreachable — only a pull failure with no cache refuses container spawns. On NixOS, `container.enable` with `toolsImages` explicitly emptied fails `nixos-rebuild` at eval, before preflight ever runs.

**Migration from the `/run/lab` layout.** Hosts that ran the pre-ADR-0060 layout (`XDG_RUNTIME_DIR=/run/lab` with the `lab-payload.service` holder) trip podman's local db on first use under the new runtime dir: the db records the old runroot/tmpdir, and podman refuses with `database configuration mismatch`. The one-time fix is `podman system reset --force` as the service user — it wipes local containers and images, none of which is worth keeping: preflight re-pulls the tools images each boot and dev images re-pull at spawn. Survivor containers under the old cgroup locations keep running **uncapped until natural exit** (the same accepted stance as the 0058/0059 migrations). Cleanup: prune the leftover empty `libpod-*` dirs under the old holder's `payload/`; the holder unit itself disappears from the config on deploy, and its cgroup dies with its last survivor.

**Resource limits.** Global settings rows `container_memory` / `container_pids` / `container_nofile` (seeded `8g` / `4096` / `16384`; Settings UI or `PATCH /api/v1/settings`) feed `--memory`, `--pids-limit`, and `--ulimit nofile` on every container pane; each repo can override any of the three on its Runner settings card (blank = inherit the global). The host prlimit wrapper applies to host-runner panes only — for container panes the nofile cap moves inside the container.

**Preflight and refusals.** Preflight runs at server startup and collects *every* failure — podman missing/too old, pasta missing **or too old** (it rejects `--map-guest-addr none`: upgrade passt to `2025_04_15`+), missing subuid/subgid entry, cgroup v1, an unreachable user manager (`user-manager`), a failed spawn probe (`spawn-probe`), an unset `--container-tools-image`, unresolvable tools refs — into one message, each item paired with the command or config that fixes it. The **dev**-image knob is deliberately *not* preflight-checked any more: `--container-image` is optional now that a repo can carry its own ref (ADR-0053), so the no-effective-image case (repo **Dev image** blank *and* the global unset) is refused *at spawn* — where the repo, and therefore its effective image, is known — naming both knobs, rather than failing startup. While any preflight check fails, container spawns are refused with that message (host-runner repos are unaffected); an AFK spawn is refused *before* the issue is claimed, so an unready host never parks an issue. Fix the host, restart lab, and the refusal clears — except the **`user-manager` check and unresolvable tools refs, which lab retries on its own**. The `user-manager` check is retryable by design (ADR-0060): logind brings `user@<uid>` up asynchronously at boot and lab cannot order on it (the uid is unknown at nix eval time), so preflight re-runs until the manager is reachable. For tools refs (ADR-0054): the deploy pipeline already refuses to bump the host pin until the default refs resolve, but a registry blip or a `workflow_dispatch` re-release can still leave a window where only time heals the pull, so preflight re-runs while a pull failure is the blocker and container spawns unblock the moment the registry serves the ref, no restart needed. (A host that has the image cached never refuses at all — it degrades to the cache with a warning.)

**Socket path on upgrades.** The agent socket moved to `<state>/agent/agent.sock` with this feature (the container runner mounts the socket's *directory*; a socket-file bind would go stale across a lab restart). A back-compat symlink is kept at the old `<state>/agent.sock` — existing `LAB_URL` values and tooling keep working with no operator action (see [Agent socket](#agent-socket)).

**Dev image expectations.** Each container repo picks its dev image in **repo settings → Runner** — the **Dev image** field (`repos.image_ref`, PATCH field `image_ref`), the OCI reference its sessions run in. A blank field inherits the global `--container-image`, which is now the *default* dev image, not the only one (ADR-0053) — on a stock module deployment that global is the pinned `buildpack-deps:stable-scm` default ([module table](#nixos-module-recommended), issue #230, ADR-0056). The image is the operator's choice and needs **no lab-specific contents** — the agent layer (provider CLI + `labctl`) is always injected via the read-only agent-tools mount at `/opt/lab`, and the container PATH puts `/opt/lab/bin` first. What the image must bring is the session's userland: a shell and coreutils, `git` (the agent commits and pushes from inside), and an ssh client for repos with ssh remotes. `buildpack-deps:stable-scm` qualifies as-is — shell, coreutils, `git`, `openssh-client`, plus `curl`/`ca-certificates` — which is why the module now defaults to it; `alpine` qualifies once `git` and `openssh-client` are added on top. Plain `debian:stable-slim` does **not** qualify — it carries neither `git` nor an ssh client.

**Provider login in container mode.** On a host with container wiring (podman + preflight + a tools image for the provider), login sessions **and** every non-interactive provider-CLI invocation — auth status (the spawn gate and login-completion detection), logout, the credential-refresh poke (ADR-0055) — run in containers: the CLI comes from the agent-tools mount, and the machine's master credential store is bind-mounted rw, so a completed login lands exactly where spawns copy credentials from. A container-mode host therefore needs **no** provider CLI on PATH — the agent-tools image is the only CLI distribution (host-runner repos still resolve their CLI from the host PATH, and host-mode login remains only on servers with no container config at all — never a silent fallback). The mount *source* honors the `CLAUDE_CONFIG_DIR` / `CODEX_HOME` operator overrides, making the override officially supported in container mode; the login container runs the global `--container-image` default (login is repo-less — no per-repo resolution; an unset global refuses naming the knob). Each attempt gets a scratch HOME under `<state>/logins/`, wiped at teardown. While preflight fails, login is refused with the same actionable preflight text run spawns get, surfaced in the login UI. Details: [ADR-0057](adr/0057-containerized-provider-login.md).

**Pinned on save.** A ref is resolved to a digest when the operator saves it, and stored pinned — so what runs is exactly what was reviewed, and a same-tag re-push upstream never silently swaps a repo's image out. A non-digest ref (`docker.io/library/debian:bookworm`) is resolved tag→digest against the registry's v2 API and stored as `host/path:tag@sha256:…` — the tag kept for readability, the digest the thing that runs (podman ignores the tag once a digest is present). An already-`@sha256`-pinned ref is stored as-is. Refs must be **fully qualified**: the docker.io `library/` namespace may be omitted (`docker.io/debian` normalizes to `docker.io/library/debian`, `index.docker.io` to `docker.io`), but a ref whose first segment is not a registry host — a bare `debian:bookworm`, podman-style short-name — is rejected on save. Resolution is **anonymous and HTTPS-only** — no http fallback, and a token realm the registry points at must itself be https; private registries that need auth to pull are out of scope for now (public / anonymous-pull registries only). A resolution failure fails the save with the registry's own error shown in the settings form, so a bad ref never reaches a spawn. Updating a repo's image is therefore an explicit re-save of the ref.

**Pull-if-missing at spawn.** Before a container run claims anything, lab runs `podman image exists <ref>` and, on a miss, one `podman pull`. A failed pull refuses the spawn with an actionable error surfaced in the UI — and, for AFK work, that refusal lands *before* the issue is claimed, so an unpullable image never parks an issue. The ref is digest-pinned, so a successful pull is the permanent fix; but the first spawn on a freshly-set image can block on the pull.

**Image storage counts toward `stateDir`.** Rootless podman keeps its image store under `$HOME/.local/share/containers`, and the lab user's HOME *is* the state dir — so every dev image and agent-tools image lab pulls (per-repo `image_ref`s, the global `--container-image` default, and each configured `--container-tools-image` ref) lands inside `stateDir` and counts toward its disk sizing. A handful of distro dev images plus the agent-tools images is easily several GB; size the state volume for it. This location follows rootless podman's HOME by design and is **not** separately configurable — point `stateDir` at a volume with room to spare rather than trying to relocate the store.

**Everything lab injects is read-only.** The agent-tools mount at `/opt/lab` arrives read-only today, and future lab features will import additional content into the container the same read-only way. An image must therefore treat lab's mount points as **reserved**: never bake content at them, never depend on writing to them. The image owns its own userland; what lab owns, it mounts.

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
