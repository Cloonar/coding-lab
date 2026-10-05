# lab — operations

How to deploy, configure, back up, and monitor the `lab` server. New here? Start with [`getting-started.md`](getting-started.md). This page is a how-to and reference; the *why* behind the design lives in [`adr/`](adr/).

What's here, in the order you'll need it:

- [Deployment](#deployment) — NixOS module, reverse proxy, bare metal
- [Configuration reference](#configuration-reference) — every flag, env var, and runtime setting
- [Seeding the initial operator user](#seeding-the-initial-operator-user) · [Forge credentials](#forge-credentials)
- [OneCLI credential gateway](#onecli-credential-gateway) — repo secrets for agent runs
- [Warpgate SSH bastion](#warpgate-ssh-bastion) — SSH targets for agent runs
- [State directory layout](#state-directory-layout) · [Backup & restore](#backup-restore)
- [CI runner prerequisites](#ci-runner-prerequisites) · [Agent-tools images](#agent-tools-images) · [Container runner](#container-runner)
- [Observability](#observability) · [Metrics](#metrics) · [Push notifications](#push-notifications) · [Incogni mode](#incogni-mode)

## Deployment

### NixOS module (recommended)

Import `nixosModules.lab` from this repo's flake. Options (authoritative defaults in [`nix/module.nix`](../nix/module.nix)):

| Option | Default | Meaning |
|---|---|---|
| `enable` | `false` | Enable the service. |
| `package` | this flake's `packages.lab` | The lab package; ships both `lab` and `labctl`, both land on the unit PATH. |
| `agentPackages` | `{ "claude-code" = pkgs.claude-code; codex = pkgs.codex; }` | Agent-CLI packages whose `bin/` is added to the unit PATH, keyed by **lab provider ID**. Defaults merge per key: `agentPackages."claude-code" = null` drops claude while the codex default survives. `claude-code` is unfree in nixpkgs — allow it (`nixpkgs.config.allowUnfreePredicate = p: lib.getName p == "claude-code";`) or set the key to `null`. On a container-mode host every value may be `null`: provider CLIs come from the agent-tools images there (ADR-0057). |
| `extraPackages` | `[ ]` | Extra packages appended to the unit PATH — the knob for a host-specific tool a session needs. |
| `claudePackage` | `null` | **Deprecated alias** for `agentPackages."claude-code"`; setting both fails eval. Prefer `agentPackages`. |
| `user` / `group` | `lab` / `lab` | Service identity. The default creates a `lab` system user with its home at `stateDir`. |
| `stateDir` | `/var/lib/lab` | State root ([layout](#state-directory-layout)). Managed via systemd `StateDirectory` at the default; otherwise you provide the directory (lab creates missing children, 0700). |
| `listenAddr` | `":8080"` | Passed as `--addr`. |
| `baseUrl` | `null` | Passed as `--base-url`. Drives Secure-cookie detection and the CSRF Origin check — set it whenever lab sits behind TLS. |
| `agentUrl` | `null` | Passed as `--agent-url`. `null` hands every session `unix://<state-dir>/agent/agent.sock` ([Agent socket](#agent-socket)) — no network, no SSO proxy in the way. Set only for off-host sessions that must reach lab over TCP; **never** point it at the SSO-fronted public origin. Container runs always get the unix socket. |
| `onecli.url` | `null` | Passed as `--onecli-url`. OneCLI REST API base as lab reaches it (typically loopback). Set together with `onecli.apiKeyFile`; `null` leaves the integration off ([OneCLI credential gateway](#onecli-credential-gateway)). |
| `onecli.apiKeyFile` | `null` | Passed as `--onecli-api-key-file`. File holding the OneCLI API key — 0600 or stricter (startup refuses otherwise); never auto-generated, the key comes from OneCLI's dashboard. |
| `onecli.gatewayUrl` | `null` | Passed as `--onecli-gateway-url`. Gateway proxy URL injected into runs as `HTTPS_PROXY` — usually a container-reachable address, not the loopback address `onecli.url` uses. Independent of the other `onecli.*` options. |
| `onecli.caFile` | `null` | Passed as `--onecli-ca-file`. PEM file with the gateway's interception CA, composed with the system CA bundle into a per-run trust bundle. With `gatewayUrl` set and this `null`, spawns refuse rather than start with broken HTTPS. |
| `onecli.dashboard` | `"off"` | Passed as `--onecli-dashboard`: `"off"`, `"port"` (lab proxies the dashboard on a second authenticated listener), or `"subdomain"` (your reverse proxy fronts it, lab answers auth). See [Dashboard exposure](#dashboard-exposure). |
| `onecli.dashboardAddr` | `null` | Passed as `--onecli-dashboard-addr`. Listen address for the second listener, e.g. `":8443"`. Required in `port` mode; rejected in any other mode. |
| `onecli.dashboardUrl` | `null` | Passed as `--onecli-dashboard-url`. The **browser-facing** dashboard origin (e.g. `"https://onecli.example.com"`). Required in `subdomain` mode; optional override in `port` mode. |
| `sessionCookieDomain` | `null` | Passed as `--session-cookie-domain`. `Domain` attribute on the session cookie; `null` keeps it host-only. Needed **only** for `onecli.dashboard = "subdomain"` — read the warning in [Dashboard exposure](#dashboard-exposure) first. |
| `warpgate.url` | `null` | Passed as `--warpgate-url`. Warpgate admin API base as lab reaches it (`"https://localhost:8888"`). Set together with `warpgate.adminTokenFile`; `null` leaves the integration off ([Warpgate SSH bastion](#warpgate-ssh-bastion)). The module never deploys Warpgate itself. |
| `warpgate.adminTokenFile` | `null` | Passed as `--warpgate-admin-token-file`. File holding Warpgate's admin token — 0600 or stricter (startup refuses otherwise); never auto-generated, the token is minted when you deploy Warpgate. |
| `warpgate.sshAddr` | `null` | Passed as `--warpgate-ssh-addr`. `host:port` runs dial for Warpgate's SSH listener — usually a container-reachable address such as `"10.88.0.1:2222"`, not loopback. Independent of the other `warpgate.*` options. |
| `warpgate.caFile` | `null` | Passed as `--warpgate-ca-file`. PEM file lab trusts — exclusively — for the admin API: Warpgate's generated self-signed certificate. `null` uses the system roots. |
| `db` | `null` | Passed as `--db` (`sqlite:<path>` or `postgres://…`). `null` keeps the sqlite default **and** lets `LAB_DB` from `environmentFile` take effect (flag > env > default). |
| `environmentFile` | `null` | systemd `EnvironmentFile=` for secret env vars (e.g. `LAB_DB` with a password-bearing DSN). `LoadCredential`-friendly. |
| `masterKeyFile` | `"${stateDir}/master.key"` | Passed as `--master-key-file`. Auto-generated 0600 when absent; loose permissions or malformed content refuse startup. |
| `vapidKeyFile` | `"${stateDir}/vapid.key"` | Passed as `--vapid-key-file`. P-256 VAPID key for [Web Push](#push-notifications); same auto-generate/refuse contract as `masterKeyFile`. |
| `seedUser` | `null` | Passed as `--seed-user`. Initial operator username, reconciled on every boot ([Seeding the initial operator user](#seeding-the-initial-operator-user)). Requires exactly one of the two hash options. |
| `seedPasswordHash` | `null` | Passed as `--seed-password-hash`. PHC argon2id hash from `lab hash-password`; a hash is safe in world-readable config. `seedPasswordHashFile` wins when both are set. |
| `seedPasswordHashFile` | `null` | Passed as `--seed-password-hash-file`. File containing the hash; `LoadCredential`-friendly. |
| `maxInstances` | `6` | Passed as `--max-instances`. Seeds the `max_instances` setting on first start; thereafter the in-app setting wins. |
| `sessionNofile` | `16384` | Passed as `--session-nofile`; per-session RLIMIT_NOFILE cap, `0` disables. |
| `proxyAuth.enable` | `false` | Trust a reverse-proxy auth header as the authenticated username — only from `trustedProxies` peers (asserted non-empty). |
| `proxyAuth.header` | `"Remote-User"` | Passed as `--proxy-auth-header`. |
| `proxyAuth.trustedProxies` | `[ ]` | CIDRs, passed as `--trusted-proxies` whenever non-empty — also without `proxyAuth.enable` (gates `X-Forwarded-Proto` trust for Secure cookies behind TLS proxies). |
| `openFirewall` | `false` | Open the firewall for `listenAddr`'s port. |
| `extraFlags` | `[ ]` | Extra flags appended to `ExecStart`; rendered after the container flags, so a hand-rolled container flag here wins during a migration. |
| `container.enable` | `true` | Master switch for container-runner host provisioning ([Container runner](#container-runner)): rootless podman + passt on the unit PATH, lingering for the service user, subuid/subgid ranges. **On by default** (ADR-0054) — enabling lab makes the host container-ready. Set `false` for a host-only deployment (the unit reverts to the byte-identical pre-container output). Forcing `virtualisation.podman.enable = false` while this is on conflicts at eval. |
| `container.toolsImageRepo` | `"ghcr.io/cloonar/agent-tools"` | OCI repository the default `toolsImages` refs point into; pulled **anonymously** (the package must stay publicly readable — [Agent-tools images](#agent-tools-images)). |
| `container.toolsImages` | versions.env tags | Per-provider agent-tools image refs, keyed by lab provider ID, rendered into `--container-tools-image`. Defaults to `<toolsImageRepo>:claude-<ver>` / `:codex-<ver>` with versions from `containers/agent-tools/versions.env` — the committed pin that moves only when the images are rebuilt. Explicitly emptying it with `container.enable` on fails eval. |
| `container.defaultImage` | `docker.io/library/buildpack-deps:stable-scm@sha256:07554a82…` | Deployed fallback dev image (`--container-image`): the last layer for a container run, used when neither the repo's own **Dev image** nor the global default dev image (Settings → Runner → Dev image) is set — and the only image provider login runs in. Digest-pinned so a fresh deployment spawns out of the box (ADR-0056). Explicit `null` opts out when every container run gets its image from one of the other two layers; provider login is then refused, naming `--container-image`. |
| `container.subIdRange` | `{ start = 100000; count = 65536; }` | subuid/subgid range for the service user — required by rootless podman's `--userns=keep-id`. Adjust `start`/`count` to dodge collisions with other subid consumers (nothing detects an overlap); `null` opts out (bring your own `/etc/subuid`/`/etc/subgid` entries). |

Full example — sops-provided master key, Postgres DSN via `environmentFile`:

```nix
{
  inputs.coding-lab.url = "github:Cloonar/coding-lab";

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

- `KillMode=process` is **load-bearing**: a restart/deploy kills only the lab process; the tmux server and every agent session under it survive and are re-adopted on start.
- `Type=simple`, `Restart=on-failure`, `RestartSec=5`.
- Unit PATH carries the session tool contract: git, tmux, **openssh** (without it every ssh fetch dies), util-linux, `bashInteractive` (NixOS has no `/bin/bash` and agent shell tools need a real bash), `coreutils`, `findutils`, `gnugrep`, `gnused`, `diffutils`, `which`, `labctl`, a fixed baseline (`gawk`, `gnutar`, `gzip`, `xz`, `zstd`, `unzip`, `curl`, `jq`, `file`, `patch`, `procps`, `ripgrep`, and `nix` — so sessions can enter a project flake's devshell), every non-null `agentPackages` value, and `extraPackages`. Language toolchains are deliberately excluded — they come from each project's flake; `extraPackages` is the additive knob.
- `ExecStart` uses systemd escaping, not shell quoting — `%` and `$` in a DSN survive.

**nixpkgs pin**: the flake input must ship `go_1_26`. Currently `github:NixOS/nixpkgs/nixos-unstable`, locked at `148bab9c1c3c53136ecb44a6ea356a0ed5b39b06`. Record pin changes here.

**Debugging sessions**: the module installs tmux system-wide; attach to a live agent session with `sudo -u lab tmux attach -t '<repo>~<label>'` (detach with `C-b d` — never kill the pane; Stop from the UI so the guarded teardown runs).

### Agent socket

Alongside its TCP listener (`--addr`, web UI / human auth only), lab always serves the agent API (`/agent/v1`, run-token auth) on a unix socket at `<state-dir>/agent/agent.sock` — mode 0700, recreated on every boot. This is what `LAB_URL` resolves to for every spawned session by default, and what the [container runner](#container-runner) bind-mounts (the directory, so the socket survives lab restarts inside containers). `labctl` accepts `LAB_URL=unix:///abs/path` in every subcommand.

The socket used to live at `<state-dir>/agent.sock`; a back-compat symlink is kept there, so old `LAB_URL` values and tooling keep working — no action needed on upgrade (issue #205 / ADR-0052).

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

- The proxy header is trusted **only** when the TCP peer (never X-Forwarded-For) is inside `--trusted-proxies` **and** the header value equals the admin username; on mismatch lab falls through to its own auth and logs once per distinct value.
- Set `--base-url` to the public https URL — it drives the CSRF Origin check and Secure cookies. If lab can never detect TLS (no https `--base-url`, no trusted `X-Forwarded-Proto: https`), it logs a prominent warning at startup.
- **Do not route agent traffic through the proxy.** `labctl` authenticates with a run token, not an SSO session; pointed at the external origin it would get 302'd to the login portal and fail. Out of the box this can't happen — sessions get the unix socket ([Agent socket](#agent-socket)). It only matters if you set `--agent-url` for off-host sessions: point it at a host lab is reachable on directly, never at `baseUrl`'s public origin. `labctl` failing with an HTML/redirect error means `LAB_URL` is aimed at the proxy.
- Keep `proxy_buffering off` for `/api/v1/events` or the SSE stream stalls.

### Bare metal

Any Linux host works:

1. Build or download `lab` and `labctl` (static binaries, CGO-free): `make lab labctl`, `nix build .#lab`, or the prebuilt release binaries ([README § Download a release binary](../README.md#download-a-release-binary)).
2. Ensure on PATH: `git`, `tmux`, `ssh` (openssh), `prlimit` (util-linux) — and `labctl`. Provider CLIs (`claude`, `codex`) are needed only for host-runner repos; a container-mode host needs none ([Container runner](#container-runner)).
3. Run `lab` (defaults: `--state-dir ~/.local/state/lab`, sqlite). Migrations apply on startup; the master key is auto-generated 0600 on first start.

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

## Configuration reference

Precedence: **flag > env > default**. Env overrides exist only where listed.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--addr` | `LAB_ADDR` | `:8080` | Listen address. |
| `--state-dir` | `LAB_STATE_DIR` | `~/.local/state/lab` | State root ([layout](#state-directory-layout)). With no HOME and no value, lab refuses to start. |
| `--db` | `LAB_DB` | `sqlite:<state-dir>/lab.db` | DSN. `sqlite:<path>` or `postgres://…` / `postgresql://…` switches backend. |
| `--master-key-file` | `LAB_MASTER_KEY_FILE` | `<state-dir>/master.key` | Vault master key: 64 hex chars, 0600. Auto-generated when absent; loose perms or malformed content refuse startup. |
| `--vapid-key-file` | `LAB_VAPID_KEY_FILE` | `<state-dir>/vapid.key` | P-256 VAPID key for Web Push; same contract as the master key. |
| `--seed-user` | `LAB_SEED_USER` | (empty) | Initial operator username, reconciled every boot ([below](#seeding-the-initial-operator-user)). Requires a hash source. |
| `--seed-password-hash` | `LAB_SEED_PASSWORD_HASH` | (empty) | PHC argon2id hash from `lab hash-password`, inline. |
| `--seed-password-hash-file` | `LAB_SEED_PASSWORD_HASH_FILE` | (empty) | File holding the hash (one trailing newline stripped). Wins over the inline hash. |
| `--provider-bin` | `LAB_PROVIDER_BIN_<ID>` | adapter default | Per-provider agent binary, repeatable as `id=path` (`--provider-bin claude-code=/usr/bin/claude`). Env `<ID>` = provider ID uppercased, dashes → underscores. Unknown flag IDs are boot errors naming the registered IDs. |
| `--provider-config` | `LAB_PROVIDER_CONFIG_<ID>` | adapter default | Per-provider global config file, repeatable as `id=path`. claude-code's is the folder-trust seeding target; with no HOME and no configured path, instance/AFK features stay unmounted (loud warning). |
| `--claude` | — | — | **Deprecated alias** for `--provider-bin claude-code=<path>`. |
| `--claude-config` | `LAB_CLAUDE_CONFIG` | — | **Deprecated alias** for `--provider-config claude-code=<path>`. |
| `--tmux` / `--git` / `--prlimit` / `--podman` | — | PATH lookup | Tool binaries. |
| `--container-image` | `LAB_CONTAINER_IMAGE` | (empty) | Deployed fallback dev image: the last layer of a container run's dev image, after the repo's own **Dev image** and the global default dev image (Settings → Runner). Provider login runs this image only. Empty is allowed — a container spawn with none of the three set is refused at spawn, naming all three ([Container runner](#container-runner)). |
| `--container-tools-image` | `LAB_CONTAINER_TOOLS_IMAGE` | (empty) | Agent-tools refs as `provider=ref[,provider=ref…]` (unknown IDs / duplicate keys are boot errors; prefer `@sha256`-pinned refs). Empty = container spawns refused by preflight. |
| `--max-instances` | — | `6` | Global live-instance cap; seeds the setting on first start only. Must be ≥ 1. |
| `--session-nofile` | — | `16384` | RLIMIT_NOFILE for spawned sessions via prlimit; `0` disables. |
| `--proxy-auth` | — | off | Accept the proxy auth header from trusted proxies. |
| `--proxy-auth-header` | — | `Remote-User` | Header carrying the proxy-authenticated username. |
| `--trusted-proxies` | — | (empty) | Comma-separated CIDRs of trusted reverse proxies (also gates `X-Forwarded-Proto` / `X-Forwarded-For` trust). |
| `--base-url` | `LAB_BASE_URL` | (empty) | Absolute external http(s) URL. Drives Secure cookies and CSRF. Never feeds `LAB_URL`. |
| `--agent-url` | `LAB_AGENT_URL` | (empty) | Session-facing URL handed to `labctl` as `LAB_URL` — http(s), or `unix:///abs/path`. Unset = `unix://<state-dir>/agent/agent.sock` ([Agent socket](#agent-socket)). Set only for off-host TCP sessions or a custom socket path. |
| `--onecli-url` | `LAB_ONECLI_URL` | (empty) | OneCLI REST API base as lab reaches it, e.g. `http://127.0.0.1:10254`. Set together with `--onecli-api-key-file`; unset = integration off. |
| `--onecli-api-key-file` | `LAB_ONECLI_API_KEY_FILE` | (empty) | File with the OneCLI API key, 0600 or stricter. Never auto-generated. |
| `--onecli-gateway-url` | `LAB_ONECLI_GATEWAY_URL` | (empty) | Gateway proxy URL runs get as `HTTPS_PROXY`, e.g. `http://10.88.0.1:10255` — a container-reachable address, independent of `--onecli-url`. |
| `--onecli-ca-file` | `LAB_ONECLI_CA_FILE` | (empty) | PEM file with the gateway's interception CA. With `--onecli-gateway-url` set and this unset, spawns refuse. |
| `--onecli-dashboard` | `LAB_ONECLI_DASHBOARD` | `off` | Dashboard exposure mode: `off`, `port`, `subdomain` ([Dashboard exposure](#dashboard-exposure)). Non-`off` requires `--onecli-url`. |
| `--onecli-dashboard-addr` | `LAB_ONECLI_DASHBOARD_ADDR` | (empty) | Second listener address for `port` mode, e.g. `:8443`. Required in `port` mode; rejected otherwise. |
| `--onecli-dashboard-url` | `LAB_ONECLI_DASHBOARD_URL` | (empty) | Browser-facing dashboard origin. Required in `subdomain` mode; optional port-remap override in `port` mode; refused in `off`. |
| `--session-cookie-domain` | `LAB_SESSION_COOKIE_DOMAIN` | (empty) | Cookie `Domain` attribute, bare domain only. Empty keeps the cookie host-only — right everywhere except `subdomain` mode ([Dashboard exposure](#dashboard-exposure)). |
| `--warpgate-url` | `LAB_WARPGATE_URL` | (empty) | Warpgate admin API base as lab reaches it, e.g. `https://localhost:8888`. Set together with `--warpgate-admin-token-file`; unset = integration off ([Warpgate SSH bastion](#warpgate-ssh-bastion)). |
| `--warpgate-admin-token-file` | `LAB_WARPGATE_ADMIN_TOKEN_FILE` | (empty) | File with Warpgate's admin token, 0600 or stricter, one line. Never auto-generated; read at startup. |
| `--warpgate-ssh-addr` | `LAB_WARPGATE_SSH_ADDR` | (empty) | `host:port` runs dial for Warpgate's SSH listener, e.g. `10.88.0.1:2222` — a container-reachable address, independent of `--warpgate-url`. |
| `--warpgate-ca-file` | `LAB_WARPGATE_CA_FILE` | (empty) | PEM file lab trusts — and only it — for the admin API, e.g. Warpgate's generated `tls.certificate.pem`. Unset = the system roots. |

Per-provider host settings resolve per provider entry, highest wins: **generic flag > generic env > alias flag > alias env** (ADR-0034).

Runtime-mutable knobs live in the `settings` table (Settings UI / `PATCH /api/v1/settings`), not flags:

| Key | Default | Meaning |
|---|---|---|
| `spawn_model_default` | `opus[1m]` | Default model for spawns (per-repo and per-spawn overrides exist). |
| `spawn_effort_default` | `max` | Default effort. |
| `spawn_model_default_lander` / `spawn_effort_default_lander` | (unset) | Model and effort for Autoland lander and escalate runs. Unset inherits the spawn default; a repo's `lander_model` / `lander_effort` wins over it. Independent of the AFK defaults. |
| `max_instances` | seeded from `--max-instances` | Global live-instance cap (login session excluded). |
| `afk_budget_minutes` | `120` | AFK budget clock; per-repo override on the repo row. |
| `afk_tick_seconds` | `30` | Reaper loop interval. |
| `afk_schedule_seconds` | `45` | Scheduler loop interval. |
| `sweep_interval_minutes` | `10` | Throttled merged-sweep + runtime-credential sweep cadence. |
| `git_author_name` / `git_author_email` | (blank) | Global git identity fallback for sessions and CR merges; per-repo overrides on the repo row. |
| `container_memory` | `8g` | `--memory` for container panes (podman grammar); per-repo override. Ignored by host-runner repos. |
| `container_pids` | `4096` | `--pids-limit` for container panes; per-repo override. |
| `container_nofile` | `16384` | `--ulimit nofile` inside the container — replaces the host prlimit cap there; per-repo override. |
| `runner_default` | `host` | Global runner default: the **Runner** of every repo set to *Inherit global default*. `host` or `container`; anything else is rejected. A change applies to each inheriting repo's next spawn, never to a live run ([Container runner](#container-runner)). |
| `dev_image_default` | (unset) | Global default dev image for container runs whose repo has no **Dev image** of its own; blank falls through to `--container-image`. Pinned on save like a repo's Dev image. |

**Settings → Runner** edits `runner_default`, `dev_image_default` and the three `container_*` limits. `GET /api/v1/settings` also returns `dev_image_fallback` — the `--container-image` value, read-only (a PATCH cannot change it).

## Seeding the initial operator user

`--seed-user` + `--seed-password-hash`/`--seed-password-hash-file` provision the operator account from config instead of the browser first-run wizard. Only a hash is accepted — there is no plaintext seed-password flag.

Generate the hash with `lab hash-password`: it reads the password from stdin or prompts with echo off — never as an argument (shell history and `ps` would leak it) — enforces the same ≥ 8 character rule as the setup page, and prints the PHC `$argon2id$…` string.

```console
$ lab hash-password
Password: ********
$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$eB1n6H...

# services.lab.seedUser = "admin"; services.lab.seedPasswordHash = "$argon2id$...";
# rotating later: edit the hash (or the file it points at) and restart the service.
```

Boot semantics — reconciled on **every** boot, before the listener opens; config, not the database, is the credential's source of truth:

- Zero users: the seeded user is created. The SPA lands on the login page, not the setup wizard.
- The user exists with a different stored hash: updated. Sessions stay valid — rotating the password is edit config + restart.
- Users exist but none matches the seeded username: lab refuses to start, naming the existing username.
- Malformed hash or unreadable hash file: lab refuses to start.
- No `--seed-user` set: nothing changes — the first-run setup page behaves as before.

## Forge credentials

A repo whose **tracker binding** is `forge` reads and writes issues/PRs through a **forge token** credential — server-side only, never materialized into a session. The credential carries a **flavor** and an **API host** (ADR-0015):

| Flavor | API host to enter | PAT scopes |
|---|---|---|
| **Forgejo** | the instance host, bare (`git.cloonar.com`) — lab appends `/api/v1` | a token with issue + pull-request read/write on the repo |
| **GitHub** | `api.github.com` for github.com; a GitHub Enterprise instance's real API root verbatim (`ghe.example.com/api/v3`, or `api.ghe.example.com` under subdomain isolation) | **fine-grained PAT**: Issues (RW), Pull requests (RW), Metadata (R). **classic PAT**: `repo`. |

The flavor is the routing authority: an unrecognized host still binds `forge` when you select it, resolving from the credential alone; a remote whose host contradicts the credential's flavor is refused as a configuration conflict. Git push auth is a **separate** git credential (SSH key or HTTPS token). GitHub calls count against the account's hourly rate limit (~5000 req/h; a polling repo spends ~480/h at 30s ticks) — a throttled repo logs and skips the tick, and the AFK loop self-heals when the window resets.

## OneCLI credential gateway

[OneCLI](https://onecli.sh) (`github.com/onecli/onecli`) is a sidecar credential gateway: a run's outbound HTTPS is routed through its proxy, which injects the real credential at the network layer — the run's environment never holds a secret value. Scope is **repo secrets only**: git credentials (the vault) and provider auth are untouched, and LLM traffic is not routed through it. The integration is entirely off unless `--onecli-url` and `--onecli-api-key-file` are both set. Design rationale: [ADR-0067](adr/0067-onecli-credential-gateway.md).

### Deploying the sidecar

OneCLI's community edition is a Docker Compose stack (Postgres + app):

```console
$ git clone https://github.com/onecli/onecli && cd onecli
$ docker compose -f docker/docker-compose.yml up -d --wait
```

It exposes two ports — **10254** (dashboard/API) and **10255** (gateway proxy) — both bound to `${ONECLI_BIND_HOST:-127.0.0.1}` by default. The two ports usually need *different* interfaces (see below), and `ONECLI_BIND_HOST` is a single variable, so override the `ports:` list instead:

```yaml
# docker-compose.override.yml — bind each OneCLI port to its own interface.
services:
  onecli:
    ports: !override
      - "127.0.0.1:10254:10254"     # dashboard/API: lab and the operator only
      - "10.88.0.1:10255:10255"     # gateway: must be reachable from container runs
```

(The `!override` tag matters: compose otherwise *appends* `ports:` lists across files, publishing four ports instead of two.)

> **The gateway port cannot be loopback-only when the `container` runner is in use.** Containerized runs deliberately cannot reach loopback-bound host services — lab pins `host.containers.internal`/`host.docker.internal` to the container's own loopback ([Container runner](#container-runner)) — so a gateway on `127.0.0.1` is unreachable from them, and those hostnames don't work around it. Bind 10255 to an address container runs can route to (a bridge address, a second NIC, the host's LAN IP) and pass that as `--onecli-gateway-url`. This is the deliberate exception to "bind co-located host services to loopback": scope the interface as narrowly as you can, and treat the per-agent `Proxy-Authorization` token — not the bind address — as the authorization boundary. The `host` runner has no such constraint; loopback is fine there.

### Wiring lab to it

1. Mint an API key in the OneCLI dashboard (Settings → API Keys) — project keys look like `oc_proj_*`.
2. Write it to a 0600 file (lab refuses looser permissions and never auto-generates this file):

   ```console
   $ install -m 0600 /dev/stdin /var/lib/lab/onecli-api-key <<< "oc_proj_xxxxxxxxxxxx"
   ```

3. Set the flags:

   ```console
   $ lab --onecli-url http://127.0.0.1:10254 \
         --onecli-api-key-file /var/lib/lab/onecli-api-key \
         --onecli-gateway-url http://10.88.0.1:10255 \
         ...
   ```

   NixOS:

   ```nix
   services.lab.onecli = {
     url = "http://127.0.0.1:10254";
     apiKeyFile = "/run/secrets/lab-onecli-api-key";     # sops/LoadCredential, 0600
     gatewayUrl = "http://10.88.0.1:10255";
   };
   ```

`--onecli-url` and `--onecli-api-key-file` must be set together. `--onecli-gateway-url` is independent — the REST pair alone is a valid deployment (health surface only, runs untouched).

`--onecli-ca-file` names the gateway's interception CA (PEM). Lab composes it with the host's system CA bundle into a per-run trust bundle — never the interception CA alone, which would break the run's direct HTTPS (git push, the model API) — and points `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, and `GIT_SSL_CAINFO` at it. With `--onecli-gateway-url` set and this unset, spawns refuse with an actionable error.

### What a gateway-wired run gets

A run is **gateway-wired** only when `--onecli-url`, `--onecli-api-key-file` **and** `--onecli-gateway-url` are all set. A wired run's environment gains exactly these entries:

| Variable | Value | Visible in the `podman` argv? |
|---|---|---|
| `HTTPS_PROXY`, `https_proxy` | `--onecli-gateway-url` with this repo's **agent identity** access token as userinfo | **No** — it is a credential, forwarded into the container by name only, never in argv, `ps`, or podman logs. |
| `NO_PROXY`, `no_proxy` | the lab host, the repo's forge host, and the provider's declared direct API hosts | Yes — hostnames are public. |
| `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO` | `<state>/instances/<runID>/runtime/onecli-ca-bundle.pem` (0644) | Yes. |

Both case spellings are set (different HTTP stacks read different ones). `HTTP_PROXY` is deliberately **not** set — plaintext http is not routed. `NO_PROXY` protects the run's lifelines from a gateway outage: `labctl`/the agent API (a `unix://` `LAB_URL` needs no entry), forge traffic (authenticated by the vault's git credential, never a grant), and the model stream (`claude-code` declares `api.anthropic.com`; **`codex` declares nothing yet**, so a wired codex run's LLM traffic does ride the proxy — a known gap worth weighing when picking a provider).

**The trust bundle** is written per run at spawn: the host's system CA roots plus `--onecli-ca-file`. Container runs reach it through the runtime dir's existing bind mount — same absolute path inside and out.

**The run's context file changes too.** A wired run's generated context file renders a **Secrets** section teaching the gateway model (call the granted service's API normally, send a placeholder where the credential goes) plus the repo's granted-service inventory, read best-effort at spawn — a grant attached mid-run works at the gateway immediately but shows up in the *next* run's context file. An unwired run still renders the legacy `labctl secret exec` section.

**Known caveat.** A tool that ignores proxy env vars bypasses the gateway and gets 401/403 from the real API — a tool problem, not an access problem, and not a leak (the environment never held a key). curl, git, node, and python-requests all honor the variables.

### When a spawn refuses

Every gateway refusal lands **before the claim** — no worktree, no branch, no parked AFK issue. All surface as a `400` quoting the text below:

| Message (abridged) | What it means | Fix |
|---|---|---|
| `--onecli-gateway-url is set but --onecli-ca-file is not …` | The proxy terminates TLS and the run would trust nothing it presents. | Set `--onecli-ca-file` to the sidecar's interception CA (PEM). |
| `refusing to spawn … onecli gateway probe: dialing <host:port> …` | Nothing is listening at `--onecli-gateway-url` (fail-closed pin). | Bring the sidecar up or fix its 10255 bind ([Deploying the sidecar](#deploying-the-sidecar)). `GET /api/v1/onecli/health` reports the same fact. |
| `onecli gateway: resolving the agent identity for repo <repo> …` | The REST API rejected or could not serve ensure-agent. | Re-check `--onecli-url` / `--onecli-api-key-file` against the dashboard. |
| `… carries no access token in OneCLI's agent listing …` | A OneCLI build whose wire shape drifted from the verified one. | Check the sidecar's version against the deployment's pin; wire drift is corrected in `internal/onecli/wire.go`. |
| `… the gateway URL must include a host …` / `must be an http(s) URL …` | `--onecli-gateway-url` is malformed. | Full `http(s)://host:port`, container-routable — not `127.0.0.1`, not `host.containers.internal`. |
| `… reading the gateway CA file <path> …` | CA file missing or unreadable by the lab user. | Fix the path or permissions. |
| `… carries no "-----BEGIN CERTIFICATE-----" block …` | The file is not a PEM certificate (DER, an HTML error page, a key, empty). | Re-export the CA in PEM. |
| `… no system CA bundle found on this host …` | The host has no CA store; lab won't build a trust bundle from the gateway CA alone. | Install the distro's CA certificates package. |

Listing the agent identity's grants deliberately does **not** refuse — the inventory is documentation; a failure logs a warning and the run starts.

**The token is stable.** Lab reads the repo's agent access token at every spawn and never regenerates it — lab restarts are harmless to running runs. Regenerating the token **in OneCLI's dashboard** while wired runs are live makes their proxied calls fail (with everything on `NO_PROXY` unaffected) until the runs are restarted.

### Checking it works

`GET /api/v1/onecli/health` (authenticated) always answers 200 with `state`: `off` (unconfigured — not an error), `ok`, `degraded`, or `unreachable`, plus per-component detail:

```console
$ curl -s --cookie "lab_session=$TOKEN" https://lab.example.com/api/v1/onecli/health
{"state":"ok","api":{"configured":true,"reachable":true,"url":"http://127.0.0.1:10254","status":"ok"},"gateway":{"configured":true,"reachable":true,"url":"http://10.88.0.1:10255"}}
```

That proves the sidecar answers; it deliberately does not prove injection (a `CONNECT` probe would spend an agent's credential). Prove injection once, live, from inside a **container-runner** run — the path where the trust bundle crosses a mount and the proxy value crosses the tmux/podman split:

```console
$ tmux attach -t myrepo~1
```

```console
# 1. The wiring is present. Print the gateway ADDRESS only — never the whole
#    value, which carries this run's proxy token.
$ echo "${HTTPS_PROXY:+set}, gateway ${HTTPS_PROXY##*@}"
set, gateway 10.88.0.1:10255

# 2. The trust bundle is the system roots PLUS one, not the gateway CA alone.
#    A count in the hundreds is right; a count of 1 means something is wrong.
$ grep -c 'BEGIN CERTIFICATE' "$SSL_CERT_FILE"
152

# 3. The acceptance test: an HTTPS call to a GRANTED service, sending a
#    placeholder where the credential goes.
$ curl -sS -o /dev/null -w '%{http_code}\n' \
       -H 'Authorization: Bearer placeholder' \
       https://api.github.com/user
200
```

A `200` with real data is the whole criterion. Reading the failures:

- **`401` / `403`** — the request reached the real API un-injected: the service isn't granted to this repo (attach the grant — it takes effect immediately), or the tool ignored `HTTPS_PROXY` (retry with `curl` to tell the two apart).
- **A proxy-auth rejection** (`407` or similar) — the gateway rejected this run's token; most often it was regenerated in OneCLI's dashboard after the run started. Restart the run.
- **`curl: (60)`, Node's `UNABLE_TO_VERIFY_LEAF_SIGNATURE`, Go's `x509: unknown authority`** — the trust bundle isn't in play. Check `--onecli-ca-file` is really the *interception* CA and the tool reads one of the four CA variables.
- **`curl: (7) Failed to connect`** to the gateway — the proxy port isn't reachable from the container's network namespace ([Deploying the sidecar](#deploying-the-sidecar)).
- **`git push` or `labctl` failing at the same time** — the problem is *not* the gateway (both are on `NO_PROXY`). Look at the forge credential or the agent socket.

### Dashboard exposure

The **dashboard** is OneCLI's own web app on the sidecar's 10254 port — the only place secret and connection *values* are created and edited (lab has no value-CRUD screen by design; its UI works one level up, on grants). `--onecli-dashboard` picks how operators reach it. A path prefix under lab's own origin is not offered — OneCLI's app ships no `basePath` ([ADR-0067](adr/0067-onecli-credential-gateway.md)).

| Mode | What is exposed | What you run | Who authenticates |
|---|---|---|---|
| `off` (default) | Nothing. 10254 stays on loopback. | An SSH tunnel when you need the dashboard. | Your SSH access. |
| `port` | Lab's own **second listener**, whole-origin proxy onto `--onecli-url`. | Lab, with one more listen address. | Lab, on its own session cookie / PAT / trusted-proxy identity. |
| `subdomain` | `onecli.example.com`, fronted by **your** reverse proxy. | Your nginx/caddy, delegating auth to lab. | Lab, via `GET /api/v1/auth/check` forward-auth. |

Every non-`off` mode requires `--onecli-url`. The SSH tunnel keeps working in every mode — `port`/`subdomain` add an authenticated front door, they don't remove the loopback one.

**`off` — the SSH tunnel.** OneCLI's dashboard has no login of its own ([Operational notes](#operational-notes)), so its port stays on loopback and the way in is:

```console
$ ssh -N -L 10254:127.0.0.1:10254 operator@lab.example.com
```

Browse `http://127.0.0.1:10254` while that `ssh` lives. No lab configuration needed.

**`port` — lab is the way in.** Lab opens a second HTTP server on `--onecli-dashboard-addr` and serves an authenticated whole-origin proxy onto `--onecli-url`:

```console
$ lab --onecli-url http://127.0.0.1:10254 \
      --onecli-api-key-file /var/lib/lab/onecli-api-key \
      --onecli-dashboard port \
      --onecli-dashboard-addr :8443 \
      --base-url https://lab.example.com \
      ...
```

NixOS:

```nix
services.lab = {
  baseUrl = "https://lab.example.com";          # required by port mode
  onecli = {
    url = "http://127.0.0.1:10254";
    apiKeyFile = "/run/secrets/lab-onecli-api-key";
    dashboard = "port";
    dashboardAddr = ":8443";
  };
};
```

How the listener behaves:

- A request with a valid lab identity (session, PAT, trusted-proxy header) is proxied verbatim; lab strips only its own session cookie on the way through.
- No identity + a browser navigation (`GET`/`HEAD` accepting `text/html`) → `302` to `<base-url>/login?next=onecli-dashboard&path=…`, and the SPA returns you to the dashboard after login (the bounce origin comes from lab's own config — structurally not an open redirect; this is why `--base-url` is required here).
- No identity + anything else (XHR, POST, asset fetch) → `401`.

**No cookie configuration is needed in this mode**: cookies are host-scoped, not port-scoped, so the session cookie set on `lab.example.com` is sent to `lab.example.com:8443` unchanged. Leave `--session-cookie-domain` empty; `SameSite=Strict` survives too.

Two caveats: lab terminates no TLS on this listener (front it with your existing TLS proxy, or accept plain HTTP on a trusted network — there is no `--onecli-dashboard-cert`); and non-443 ports are blocked on plenty of corporate and mobile networks, which is the honest argument for `subdomain` for a phone-first operator. `--onecli-dashboard-url` is optional here — lab derives the browser-facing origin from `--base-url`'s host plus the listener port; set it only when something in front remaps the port.

**`subdomain` — your reverse proxy fronts it, lab answers the auth question.** The dashboard gets its own host and your TLS-terminating proxy fronts it. Lab contributes three load-bearing pieces:

1. **`GET /api/v1/auth/check`** — the forward-auth probe: `204` (empty body) with any valid lab identity, `401` without.
2. **`--session-cookie-domain example.com`** — lab's cookie is host-only by default and would never reach `onecli.example.com`; without this, every visit 401s.
3. **`--onecli-dashboard-url https://onecli.example.com`** — the origin the web UI's link-out reads (lab has no listener here to derive one from).

```console
$ lab --onecli-url http://127.0.0.1:10254 \
      --onecli-api-key-file /var/lib/lab/onecli-api-key \
      --onecli-dashboard subdomain \
      --onecli-dashboard-url https://onecli.example.com \
      --session-cookie-domain example.com \
      --base-url https://lab.example.com \
      ...
```

NixOS:

```nix
services.lab = {
  baseUrl = "https://lab.example.com";
  sessionCookieDomain = "example.com";
  onecli = {
    url = "http://127.0.0.1:10254";
    apiKeyFile = "/run/secrets/lab-onecli-api-key";
    dashboard = "subdomain";
    dashboardUrl = "https://onecli.example.com";
  };
};
```

Keep `SameSite=Strict` — sibling subdomains are same-site, and a guide telling you to drop to `Lax` for a subdomain is giving up a real CSRF property for nothing.

> **Widening the cookie `Domain` widens it for everything under that domain.** `--session-cookie-domain example.com` sends lab's session cookie — a credential for lab's entire API — to *every* host under `example.com`, including hosts you don't operate and whatever answers a stale DNS record. Only set it on a domain whose host inventory you control. Lab never derives it from other flags for exactly this reason.

**nginx.** `lab.example.com` is lab itself, unchanged from [Reverse proxy / Authelia](#reverse-proxy-authelia); this is the dashboard's block beside it:

```nginx
# Belongs at http {} level — it is what lets a proxied WebSocket upgrade
# through while leaving ordinary requests alone.
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name onecli.example.com;

    ssl_certificate     /etc/ssl/certs/onecli.example.com.pem;
    ssl_certificate_key /etc/ssl/private/onecli.example.com.key;

    # The forward-auth subrequest. lab answers 204 for a valid identity and
    # 401 for none; nginx reads the status and discards the body.
    location = /lab-auth-check {
        internal;
        proxy_pass              http://127.0.0.1:8080/api/v1/auth/check;
        proxy_pass_request_body off;
        proxy_set_header        Content-Length "";
        proxy_set_header        Host $host;
        # Load-bearing: the session cookie IS the credential being checked.
        proxy_set_header        Cookie $http_cookie;
    }

    # A 401 becomes lab's login page rather than a bare error.
    error_page 401 = @lab_login;
    location @lab_login {
        return 302 https://lab.example.com/login?next=onecli-dashboard&path=$request_uri;
    }

    location / {
        auth_request /lab-auth-check;

        proxy_pass http://127.0.0.1:10254;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host  $host;

        proxy_http_version 1.1;
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
    }
}
```

**caddy.** Same topology:

```caddyfile
# lab's own site is served exactly as it already is
# (`reverse_proxy 127.0.0.1:8080`); this is the dashboard's site beside it.
onecli.example.com {
	forward_auth 127.0.0.1:8080 {
		uri /api/v1/auth/check

		# Send an unauthenticated operator to lab's login instead of
		# copying lab's 401 JSON to the browser.
		@denied status 401
		handle_response @denied {
			redir * https://lab.example.com/login?next=onecli-dashboard&path={http.request.uri}
		}
	}

	reverse_proxy 127.0.0.1:10254
}
```

**Checking the exposure.** The forward-auth probe, without and with a session:

```console
$ curl -s -o /dev/null -w '%{http_code}\n' https://lab.example.com/api/v1/auth/check
401
$ curl -s -o /dev/null -w '%{http_code}\n' \
       --cookie "lab_session=$TOKEN" https://lab.example.com/api/v1/auth/check
204
```

If it 401s while your browser is logged in, the cookie is not reaching lab — check `--session-cookie-domain` and that you copied a live session token. The resolved exposure (what the web UI reads to render its link-out):

```console
$ curl -s --cookie "lab_session=$TOKEN" https://lab.example.com/api/v1/onecli/dashboard
{"mode":"subdomain","url":"https://onecli.example.com"}
```

`url` is omitted when the mode is `off`. This endpoint is static config, never a sidecar probe — a dead sidecar can't take the link with it. In `port` mode the login bounce is visible from the command line too:

```console
$ curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' \
       -H 'Accept: text/html' https://lab.example.com:8443/
302 https://lab.example.com/login?next=onecli-dashboard&path=%2F
```

(Drop the `Accept` header and the same request answers `401` — the non-navigation branch, not a fault.)

**When lab refuses to start.** Every misconfiguration is caught at boot, before the listener opens:

| Message | What it means | Fix |
|---|---|---|
| `--onecli-dashboard "…": want one of off, port, subdomain` | Unknown mode word. | Spell it `off`, `port`, or `subdomain` (empty reads as `off`). |
| `--onecli-dashboard-addr is set but --onecli-dashboard is "…"` | Dead config — nothing listens on it outside `port` mode. | Drop the flag or switch to `port`. |
| `--onecli-dashboard-url is set but --onecli-dashboard is off` | Dead config — an origin for a dashboard nobody can reach. | Drop the flag or pick a mode. |
| `--onecli-dashboard=<mode> requires the OneCLI integration …` | No OneCLI configured, nothing to expose. | Wire the REST pair first ([Wiring lab to it](#wiring-lab-to-it)). |
| `--onecli-dashboard=port requires --onecli-dashboard-addr …` | Port mode with no port. | Add `--onecli-dashboard-addr :8443`. |
| `--onecli-dashboard=port requires --base-url …` | The login bounce needs a destination. | Set `--base-url` to lab's public https URL. |
| `--onecli-dashboard=subdomain requires --onecli-dashboard-url …` | Lab owns no listener in this mode. | Give it the origin your proxy publishes. |
| `--onecli-dashboard-url "…": want an absolute http(s) URL` | Bare `host:port`, scheme typo, or path fragment. | Full origin, scheme included; a path prefix is the one shape not supported. |
| `--session-cookie-domain "…": want a bare domain like example.com …` | A cookie Domain is a bare domain — no scheme, port, or path. | `example.com` (a leading dot is accepted). |

### Grant picker

Secrets and connections are *created* in OneCLI's dashboard; lab's per-repo settings pick, per repo, which of them that repo's runs may use. A pick becomes a **grant** on the repo's **agent identity** — the grant set *is* the assignment (ADR-0067). In the UI: repo settings → Secrets → **Credential gateway** card. The API behind it:

```console
$ curl -s --cookie "lab_session=$TOKEN" https://lab.example.com/api/v1/onecli/pool
{"configured":true,"secrets":[{"id":"sec_1","name":"DEPLOY_TOKEN","provider":"generic"}],"connections":[{"id":"con_1","name":"GitHub","provider":"github"}]}

$ curl -s --cookie "lab_session=$TOKEN" \
       https://lab.example.com/api/v1/repos/repo_4b1f0c7a9d3e42a8b6c5d0e1f2a3b4c5/onecli/grants
{"configured":true,"grants":[{"kind":"secrets","id":"sec_1","name":"DEPLOY_TOKEN"}]}

$ curl -s -o /dev/null -w '%{http_code}\n' -X PUT \
       --cookie "lab_session=$TOKEN" \
       https://lab.example.com/api/v1/repos/repo_4b1f0c7a9d3e42a8b6c5d0e1f2a3b4c5/onecli/grants/connections/con_1
204
```

- `GET /api/v1/onecli/pool` — everything grantable, metadata only (never a value).
- `GET /api/v1/repos/{id}/onecli/grants` — what one repo already has.
- `PUT`/`DELETE /api/v1/repos/{id}/onecli/grants/{kind}/{resourceId}` — attach/detach one `secrets` or `connections` resource. The attach path creates the repo's agent identity lazily on first grant; the reads never do.

Unconfigured is not unhealthy: with no OneCLI set, the GETs answer `200` with `configured:false` and empty arrays, the mutations answer `409`, and none of the routes ever 404. A configured-but-unreachable OneCLI answers `502` with the underlying error. OneCLI stays the single source of truth — lab mirrors nothing, so a value created, rotated, or deleted in the dashboard is reflected on the next read with no reconciliation; the agent identity's proxy token never appears in any response.

### Operational notes

- The sidecar is a second process to run, back up, and upgrade. Its Postgres volume holds the encrypted secret store — back it up with the same seriousness as `lab.db` / `master.key` ([Backup & restore](#backup-restore)).
- Pin `ONECLI_VERSION` in `.env` rather than tracking `latest`.
- The dashboard runs in local single-user mode with **no login** — which is exactly why its port stays bound to loopback, never widened the way 10255 sometimes must be.
- Dashboard exposure through lab's auth is configuration: `--onecli-dashboard` = `off` | `port` | `subdomain` ([Dashboard exposure](#dashboard-exposure)).

## Warpgate SSH bastion

[Warpgate](https://github.com/warp-tech/warpgate) is an open-source SSH bastion: a client logs in to Warpgate, names the host it wants in its SSH username, and Warpgate opens the onward connection with a credential it keeps in its own database. Wired into lab, it lets a run reach operator-defined **SSH targets** — a staging box, a build host, a machine to deploy to — as plain `ssh <target>`, without ever holding the target's key or password. What the run does hold, its **run key**, opens only Warpgate, only its repo's targets, and only until the run is wiped. It is the SSH sibling of the [credential gateway](#onecli-credential-gateway), which fronts outbound HTTPS. Git is untouched: clone, fetch, and push stay on the repo's deploy key from the vault. The integration is entirely off unless `--warpgate-url` and `--warpgate-admin-token-file` are both set. Design rationale: [ADR-0068](adr/0068-warpgate-ssh-bastion.md).

### Deploying Warpgate

Warpgate ships as one container image; lab is verified against **0.29.1**, so pin that tag. It keeps everything — config, database, TLS certificate, recordings — in one `/data` volume, and exposes two ports: **8888** (admin UI and admin API, HTTPS) and **2222** (the SSH listener). As with OneCLI, the two ports want *different* interfaces:

```yaml
# /srv/warpgate/compose.yaml
services:
  warpgate:
    image: ghcr.io/warp-tech/warpgate:0.29.1
    # The image's default command is a plain `run`. Lab authenticates with
    # Warpgate's static admin token, which only `--enable-admin-token` turns on;
    # the token itself comes from WARPGATE_ADMIN_TOKEN in the env file.
    command: ["run", "--enable-admin-token"]
    env_file: warpgate.env          # WARPGATE_ADMIN_TOKEN=…, 0600 (step 1)
    volumes:
      - warpgate-data:/data
    ports:
      - "127.0.0.1:8888:8888"       # admin UI/API: lab and the operator only
      - "10.88.0.1:2222:2222"       # SSH: must be reachable from container runs
    restart: unless-stopped

volumes:
  warpgate-data:
```

> **The SSH port cannot be loopback-only when the `container` runner is in use** — for the same reason the OneCLI gateway port cannot ([Deploying the sidecar](#deploying-the-sidecar)): containerized runs deliberately cannot reach loopback-bound host services. Bind 2222 to an address container runs can route to (a bridge address, a second NIC, the host's LAN IP) and pass that address as `--warpgate-ssh-addr`. This is the second deliberate exception to "bind co-located host services to loopback": scope the interface as narrowly as you can, and treat Warpgate's own login — which admits a repo's user only with a key registered on it — not the bind address, as the authorization boundary. Lab itself scans the same address to check Warpgate's host key, so it must be reachable from the lab host too (a bridge address on the host is). The `host` runner has no such constraint; `127.0.0.1:2222` is fine there. The admin port gets no exception in any deployment: it stays on loopback.

1. **Mint the admin token** and write it twice — once for the container, once for lab. Lab refuses a token file looser than 0600 and never generates one:

   ```console
   $ cd /srv/warpgate
   $ token=$(openssl rand -hex 32)
   $ install -m 0600 /dev/stdin warpgate.env <<< "WARPGATE_ADMIN_TOKEN=$token"
   $ sudo install -m 0600 -o lab -g lab /dev/stdin /var/lib/lab/warpgate-admin-token <<< "$token"
   $ unset token
   ```

2. **Run Warpgate's one-time setup.** It writes `/data/warpgate.yaml`, creates the database and the `admin` user, generates the SSH host keys and a self-signed TLS certificate, and refuses to run a second time once the config exists:

   ```console
   $ read -rsp 'Warpgate admin password: ' WARPGATE_ADMIN_PASSWORD; echo
   $ export WARPGATE_ADMIN_PASSWORD
   $ docker compose run --rm -e WARPGATE_ADMIN_PASSWORD warpgate \
         unattended-setup --data-path /data --http-port 8888 --ssh-port 2222 --record-sessions
   $ unset WARPGATE_ADMIN_PASSWORD
   ```

   The password is yours, for logging in to the admin UI as `admin`; lab never uses it. `--ssh-port` is what turns the SSH listener on — without it Warpgate serves HTTPS only. `--record-sessions` keeps a recording of every session in the volume; leave it out if you don't want recordings.

3. **Start it, and copy out its certificate** for lab to trust:

   ```console
   $ docker compose up -d
   $ docker compose exec -T warpgate cat /data/tls.certificate.pem \
       | sudo install -m 0644 -o lab -g lab /dev/stdin /var/lib/lab/warpgate-ca.pem
   ```

   The generated certificate names only `localhost` and `warpgate.local` — which is why lab dials `https://localhost:8888` below, never `https://127.0.0.1:8888`.

A Warpgate set up earlier without `--ssh-port` has its SSH listener off. Turn it on in `/data/warpgate.yaml` and restart the container:

```yaml
ssh:
  enable: true
  listen: "[::]:2222"
```

### Wiring lab to Warpgate

```console
$ lab --warpgate-url https://localhost:8888 \
      --warpgate-admin-token-file /var/lib/lab/warpgate-admin-token \
      --warpgate-ca-file /var/lib/lab/warpgate-ca.pem \
      --warpgate-ssh-addr 10.88.0.1:2222 \
      --warpgate-ssh-host-key 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI…' \
      ...
```

NixOS:

```nix
services.lab.warpgate = {
  url = "https://localhost:8888";
  adminTokenFile = "/run/secrets/lab-warpgate-admin-token";   # sops/LoadCredential, 0600
  caFile = "/var/lib/lab/warpgate-ca.pem";
  sshAddr = "10.88.0.1:2222";
  sshHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI…";      # optional; see Host key pinning
};
```

- `--warpgate-url` and `--warpgate-admin-token-file` must be set together; lab refuses to start with only one. The token file follows the master key's rule — 0600 or stricter, one line — and is read once at startup, so a new token means a lab restart.
- `--warpgate-ca-file` makes lab trust **only** that certificate for the admin API. Unset, lab uses the system roots — right for a Warpgate you have given a publicly issued certificate. With the generated certificate the URL must say `localhost`: it carries no IP address, so `https://127.0.0.1:8888` fails verification.
- `--warpgate-ssh-addr` is independent, like `--onecli-gateway-url`: it is the `host:port` runs dial — container-routable, never `host.containers.internal`, and not `127.0.0.1` while any repo uses the container runner.
- `--warpgate-ssh-host-key` is optional and needs `--warpgate-ssh-addr`: the public host key of Warpgate's SSH listener that runs trust. Unset, runs trust whatever the listener presents — see [Host key pinning](#host-key-pinning) for how to read the key and why to set it.
- The REST pair alone is a valid deployment: health, and each repo's Warpgate user and role kept in step with lab, with every spawn untouched. A run is **bastion-wired** only when all three of URL, token file, and SSH address are set — and then only if its repo has an SSH target.

**What lab keeps in Warpgate.** Every lab repo gets one Warpgate user and one Warpgate role, both named `repo-<32 hex>` after the repo's ID and described with the repo's name: created with the repo (and at startup for repos that predate the configuration), deleted with the repo. Leave them to lab. A description edited in Warpgate's UI is overwritten at the next check, and a deleted role takes the repo's target assignments with it — lab re-creates the role, empty. Lab's startup pass never deletes anything in Warpgate.

### Defining targets and granting them to a repo

Targets — and the password or key Warpgate uses to reach each one — are created in Warpgate's own admin UI; lab never creates, reads, or edits one. The admin port is loopback-only, so reach it through an SSH tunnel from your workstation:

```console
$ ssh -N -L 8888:127.0.0.1:8888 operator@lab.example.com
```

Browse `https://localhost:8888/@warpgate/admin` while that `ssh` lives, accept the self-signed certificate, and log in as `admin`. (Reaching this UI through lab's own auth, the way `--onecli-dashboard` does for OneCLI, is issue #47; until then the tunnel is the way in.)

1. **Config → Targets → Add a target**, kind SSH: the host, the port, the user on the target, and how Warpgate authenticates there — a password, or a key: add one of Warpgate's own client keys (**Config → SSH keys**) to the target's `authorized_keys`.
2. **Name it the way a run should type it.** The target's name becomes the run's `ssh` alias, so keep to letters, digits, `.`, `_`, and `-`, starting with a letter or digit, and don't reuse the repo's forge host. A target whose name doesn't qualify is skipped at spawn with a warning in lab's log; rename it in Warpgate.
3. **Trust the target's host key in Warpgate.** On the target's page: **Check host key**, then **Trust**. Warpgate's default policy asks about an unknown target key interactively, and a run's `ssh <target> <command>`, `scp`, or `rsync` has no terminal to answer on — Warpgate drops the connection instead.
4. **Assign it to repos in lab:** repo settings → Secrets → **SSH targets** card. It lists every SSH target in Warpgate, and each toggle (**Not assigned** / **✓ Assigned**) adds or removes the repo's Warpgate role on that target. Warpgate enforces the change from the next login; a running run's aliases were written at its spawn, so a newly assigned target reaches the *next* run.

Assigning the repo's role on the target's own page in Warpgate (**Allow access for roles**) works too, but lab only learns of it the next time it reads that repo's targets: when the SSH targets card is opened, at a lab restart, or — for a repo that already has a target — at its next spawn. For a repo getting its first target that way, opening its SSH targets card once is enough.

The API behind the card:

```console
$ curl -s --cookie "lab_session=$TOKEN" \
       https://lab.example.com/api/v1/repos/repo_4b1f0c7a9d3e42a8b6c5d0e1f2a3b4c5/warpgate/targets
{"configured":true,"targets":[{"id":"8c0e4f5a-…","name":"staging","description":"Staging web host","assigned":true}]}
```

- `GET /api/v1/repos/{id}/warpgate/targets` — every SSH target in Warpgate, each flagged with whether this repo's role carries it. Metadata only, never a credential.
- `PUT`/`DELETE /api/v1/repos/{id}/warpgate/targets/{targetId}` — assign/unassign one target, `204`. The assign creates the repo's Warpgate user and role if they are missing; an id that is not an SSH target answers `404`.

With the REST pair unset, the GET answers `200` with `configured:false` and an empty list, and the mutations answer `409`. A configured-but-unreachable Warpgate answers `502` with the underlying error, which the card shows with a **Retry** — a listing failure blocks nothing else.

### What a bastion-wired run gets

A bastion-wired run of a repo with SSH targets reaches each one by name — no flags, no key handling, nothing to configure:

```console
$ ssh staging                              # interactive shell on the target
$ ssh staging 'systemctl status nginx'
$ scp dist/app.tar.gz staging:/tmp/
$ sftp staging
$ rsync -a -e ssh dist/ staging:/srv/app/
```

Everything lab writes for it lives in the run's runtime dir, `<state>/instances/<runID>/runtime/`, which a container run sees at the same path:

| File | What it is |
|---|---|
| `warpgate-run-key` | The **run key**: a fresh ed25519 private key, 0600. Its public half is registered on the repo's Warpgate user, labelled `lab-run:<runID>`, and removed when the run is wiped. |
| `warpgate-known-hosts` | Warpgate's SSH host key(s), checked strictly: the [trusted key](#host-key-pinning) from `--warpgate-ssh-host-key`, or — with none configured — whatever the listener presented to this spawn's scan. |
| `warpgate-ssh-config` | The run's OpenSSH config: one `Host <target>` block per target, then a `Match all` that restores the normal per-user and system config for every other host. |
| `warpgate-bin/` | `ssh`, `scp`, and `sftp` wrappers, prepended to the run's `PATH`. |

The one exception is the revocation marker, `warpgate-run-key.json` — which Warpgate key to revoke when the run is wiped, no secret — kept one level up at `<state>/instances/<runID>/`. That directory is never mounted into a container, so a run cannot delete or rewrite the record of its own key; it is wiped with the rest of the run.

In the run's HOME, `~/.ssh/config` is a symlink to `warpgate-ssh-config`.

**Why wrappers.** OpenSSH finds `~/.ssh/config` through the user's passwd entry, not `$HOME`, so a config in the run's private HOME alone would be invisible to it. Each wrapper runs the real binary — the next one on `PATH`, the host's or the dev image's — with `-F <runtime>/warpgate-ssh-config` in front of your arguments. `scp` and `sftp` need their own because they start `ssh` by absolute path; `rsync -e ssh`, ansible, and anything else that runs `ssh` by name goes through the `ssh` wrapper. A tool that execs ssh by absolute path and doesn't read `~/.ssh/config` itself sees no aliases: run it through `ssh` by name, or pass it `-F`.

**Git is untouched.** `GIT_SSH_COMMAND` still points at the repo's deploy key, no alias lab writes may match the forge host, and every host that is not an alias — the forge included — resolves exactly as it did without the wrappers. Lab does append Warpgate's pinned host keys to the run's git `known_hosts` (after its usual contents): git checks host keys with `accept-new`, so without them a `git clone staging:app.git` through the wrapper would trust the bastion on first use.

**The system ssh config must pass OpenSSH's permission check.** The run's config pulls in `/etc/ssh/ssh_config` with `Include`, and OpenSSH refuses an included file that is not owned by root (or the run's user) or is group- or world-writable — a check it skips when it reads that file itself. On a dev image with a loose `/etc/ssh/ssh_config`, every ssh in a wired run fails with `Bad owner or permissions on /etc/ssh/ssh_config`, git included; fix the image (`chown root:root`, `chmod 644`).

**The run's context file** gets an **SSH targets** section: the aliases, how to use them, and that no credential for them is in the instance.

**Container runs** get the same files through the runtime dir's existing bind mount and the same `PATH` prefix. The dev image must ship an ssh client (`ssh`, `scp`, `sftp`) — already the rule for ssh remotes; `buildpack-deps:stable-scm` qualifies.

**What stays unwired.** A repo with no SSH target spawns exactly as before — no key, no files, no `PATH` change, and no call to Warpgate. So does a run whose targets all turn out to be gone from Warpgate when the spawn re-reads them, or whose target names are all skipped (one warning names them).

### Host key pinning

A run never answers a host-key prompt and never uses `accept-new`: its `warpgate-known-hosts` is written by lab before the run starts, and checked strictly. Which key goes in it is yours to decide with `--warpgate-ssh-host-key`:

- **Set** — runs trust exactly the key(s) you configured. Every target-bearing spawn scans Warpgate's SSH listener first; if it presents none of the configured keys, health turns `degraded` with `hostKey.state: "mismatch"` (the trusted and the observed fingerprints both listed) and the spawn refuses, until the setting names a key the listener presents. Repos without SSH targets are unaffected. Nothing the listener says can widen what a run trusts.
- **Unset** (the default) — runs trust whatever the listener presents. Each target-bearing spawn scans the listener and writes every key it offered into that run's `warpgate-known-hosts`. Health reports `hostKey.state: "unpinned"` with the observed fingerprints, and lab logs them once at startup. This is trust-on-each-spawn, on the lab host: convenient for a first deployment, but a machine in the middle between lab and the bastion at spawn time would be trusted for that run's lifetime. Pin the key once the sidecar is up.

**Reading the key.** Warpgate generates an ed25519 and an RSA host key at setup and exposes neither through its API, so read them on the Warpgate host itself — never through the network you are about to trust:

```console
$ docker compose exec -T warpgate ssh-keyscan -p 2222 127.0.0.1 2>/dev/null
[127.0.0.1]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI…
[127.0.0.1]:2222 ssh-rsa AAAAB3NzaC1yc2E…
```

(Without `ssh-keyscan` in the image: `ssh-keyscan -p 2222 10.88.0.1` from the lab host, and compare its fingerprints — `ssh-keygen -lf -` — against the ones health lists as `observed`.) Either line, or just its `<type> <base64>` part, is the value; the `[host]:port` prefix and any comment are dropped. One key is enough — OpenSSH prefers the host-key algorithms it already has a key for — and several may be given, separated by commas (or newlines in the environment variable). A fingerprint (`SHA256:…`) or a private key is refused at startup.

**Changing it.** A changed key is expected only when Warpgate's database was replaced: a recreated `warpgate-data` volume, a restore from a different backup, a fresh `unattended-setup`, or a different Warpgate behind `--warpgate-ssh-addr`. If none of that happened, treat a mismatch as an incident, not a prompt. If it did, read the new key as above, put it in `--warpgate-ssh-host-key`, and restart lab. The setting is keys, not an address: moving `--warpgate-ssh-addr` to another address of the same Warpgate needs no change. **Settings → General → SSH bastion** shows the current state — the trusted and observed fingerprints on a mismatch, the observed ones when nothing is pinned — but the key itself is configuration, never something the UI or API changes.

### When a target-bearing spawn refuses

Only a **target-bearing** spawn — a bastion-wired lab, a repo with at least one SSH target — ever waits on Warpgate. When any step of its wiring fails, it refuses **before the claim** (no worktree, no run row, no parked AFK issue) with a `400` quoting the cause:

| The message mentions | What it means | Fix |
|---|---|---|
| `admin token rejected (401)` | Warpgate did not accept the token. | The file must hold exactly the container's `WARPGATE_ADMIN_TOKEN`, and Warpgate must run with `--enable-admin-token` (the image's default command doesn't pass it). Restart lab after changing the file. |
| `admin token lacks a permission lab needs (403)` | A per-user API token whose user is missing an admin permission. | Use the static admin token ([Deploying Warpgate](#deploying-warpgate)). |
| `connection refused`, a timeout, or `no such host` on the admin API | Warpgate is down, or `--warpgate-url` is wrong. | `docker compose ps`; fix the URL. |
| `x509: certificate signed by unknown authority` | Lab doesn't trust Warpgate's certificate. | Point `--warpgate-ca-file` at the copy of `/data/tls.certificate.pem`. |
| `x509: … doesn't contain any IP SANs` or `certificate is valid for …, not …` | The URL's host is not a name on the certificate. | Use `https://localhost:8888`. |
| `cannot verify the SSH host key at <addr>` | Nothing answers SSH at `--warpgate-ssh-addr`. | Check the 2222 bind and that Warpgate's SSH listener is on ([Deploying Warpgate](#deploying-warpgate)). |
| `are not among the trusted keys configured with --warpgate-ssh-host-key` … `refusing to wire SSH targets` | Warpgate presents none of the configured host keys. | [Host key pinning](#host-key-pinning). |

A repo **without** SSH targets never calls Warpgate at spawn and never refuses on its account: a bastion outage leaves it alone.

### Checking the bastion works

`GET /api/v1/warpgate/health` (authenticated) always answers 200 with `state` — the same four words as the credential gateway's — plus per-component detail:

```console
$ curl -s --cookie "lab_session=$TOKEN" https://lab.example.com/api/v1/warpgate/health
{"state":"ok","api":{"configured":true,"reachable":true,"url":"https://localhost:8888","version":"v0.29.1","authenticated":true},"ssh":{"configured":true,"reachable":true,"addr":"10.88.0.1:2222"},"hostKey":{"state":"pinned","pinned":["SHA256:Xq7…"],"observed":["SHA256:Xq7…","SHA256:k2V…"]}}
```

| `state` | When |
|---|---|
| `off` | Nothing configured — not an error. |
| `ok` | Everything configured answers, the token is accepted as admin, and the listener presents a key from `--warpgate-ssh-host-key` (or none is configured). |
| `degraded` | One component unreachable, the token not accepted (`api.authenticated: false`), or `hostKey.state` `"mismatch"`. |
| `unreachable` | Every configured component unreachable. |

`hostKey` is present when `--warpgate-ssh-addr` is set: `pinned` (the listener presents one of the trusted keys; `pinned` lists the trusted fingerprints, `observed` the listener's), `unpinned` (no `--warpgate-ssh-host-key`; runs trust the `observed` keys), `mismatch` (a trusted key is configured and the listener presents none of them), or `unreachable` (the scan failed; `error` says why). The **SSH bastion** card on Settings → General shows the same status, next to the credential gateway's.

That proves the sidecar answers; it does not log in to a target. Prove that once from inside a bastion-wired run — ideally a **container-runner** one, where the files cross a mount and the address crosses a network namespace:

```console
$ tmux attach -t myrepo~1
```

```console
# 1. The wrapper is in front, and the alias resolves to the bastion.
$ command -v ssh
/var/lib/lab/instances/run_…/runtime/warpgate-bin/ssh
$ ssh -G staging | grep -E '^(user|hostname|port) '
user repo-4b1f0c7a9d3e42a8b6c5d0e1f2a3b4c5:staging
hostname 10.88.0.1
port 2222

# 2. The acceptance test: a non-interactive login through Warpgate.
$ ssh staging true && echo ok
ok
```

Reading the failures:

- **`Could not resolve hostname staging`** — the alias isn't in this run's config: the target was assigned after the run started (start a new run), its name was skipped (lab's log names it), or the command bypassed the wrapper (`command -v ssh` doesn't point into `warpgate-bin`).
- **`Permission denied`** — Warpgate refused the login: the target no longer carries the repo's role, or Warpgate requires MFA ([Bastion operational notes](#bastion-operational-notes)).
- **`Host key verification failed`** — Warpgate's key no longer matches the pin this run was spawned with. Check health for a mismatch, accept if it's genuine, and start a new run.
- **The connection drops right after login** — the *target's* host key isn't trusted in Warpgate yet; Warpgate's log (`docker compose logs warpgate`) says `Target host key is not trusted, but there is no active PTY channel`. **Check host key → Trust** on the target's page.
- **`Connection refused` or a timeout to `10.88.0.1:2222`** — the SSH port isn't reachable from the container's network namespace ([Deploying Warpgate](#deploying-warpgate)).
- **`Bad owner or permissions on /etc/ssh/ssh_config`**, from every ssh in the run — the dev image's system ssh config is group/world-writable or not root-owned, which the run's `Include` of it refuses ([What a bastion-wired run gets](#what-a-bastion-wired-run-gets)). Fix the image and start a new run.
- **`git push` failing at the same time** — not the bastion; git never goes through it (unless it is the `Bad owner or permissions` error above).

### Bastion operational notes

- Warpgate is a second process to run, back up, and upgrade. Its `/data` volume holds the target credentials, its SSH host keys, and the recordings — back it up with the same seriousness as `lab.db` / `master.key` ([Backup & restore](#backup-restore)). A recreated or differently restored volume brings new host keys: expect a [mismatch](#host-key-pinning).
- Keep the image pinned (`0.29.1`) rather than tracking `latest`; lab's client is verified against that version. Check health after an upgrade.
- **Run keys are revoked, not left behind.** A run's key is removed from Warpgate whenever the run is wiped — Stop, the AFK reaper, a launch rollback, the orphan sweep — and a startup sweep removes any `lab-run:` key whose run is no longer active and no longer on disk. If Warpgate isn't answering when lab starts, that sweep retries (after 1, 2, 4, 8, then every 15 minutes) until one pass completes. A run that survives a lab restart keeps its key. Keys you add by hand to a repo's Warpgate user are never touched.
- **Recordings stay in Warpgate** (its **Status** pages). Lab neither reads nor stores them; linking a run to its recordings is issue #47. Warpgate logs each session against the key it used, and that key's label names the run while the run is live.
- **Don't require MFA.** Warpgate's **Config → Global parameters → MFA enforcement** set to **Require** demands a second factor on every SSH login, which a run's key login can't give: target-bearing runs start and then fail to authenticate inside the session, where lab can't catch it. **Off** and **Enroll** are fine.
- **The static admin token is full admin over Warpgate and never expires.** Whoever reads it can create a user, give it every target, and log in through the bastion. Keep both copies — `warpgate.env` and lab's token file — at 0600. To rotate, write a new token to both, `docker compose up -d --force-recreate`, and restart lab.
- The admin UI stays on loopback: the SSH tunnel is the way in until issue #47 adds exposure through lab's auth.

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
                             provider credential copy, config, transcripts, and
                             (bastion-wired runs only) .ssh/config, a symlink to
                             runtime/warpgate-ssh-config; created at launch,
                             wiped at stop/rollback, swept at boot
  instances/<runID>/runtime/ 0700 — per-run runtime dir (issue #205): the run's
                             materialized git credential files, known_hosts,
                             dialog spool, --settings file, (gateway-wired
                             runs only) onecli-ca-bundle.pem, 0644, and
                             (bastion-wired runs only) warpgate-run-key,
                             warpgate-known-hosts and warpgate-ssh-config,
                             all 0600, plus warpgate-bin/, 0700 (the
                             ssh/scp/sftp wrappers); same lifecycle as home/,
                             bind-mounted into the run's container at its
                             host-identical path
  instances/<runID>/warpgate-run-key.json
                             0600 — (bastion-wired runs only) which Warpgate
                             key to revoke at wipe; no secret. At the run
                             dir's root, never mounted into a container, so a
                             run cannot tamper with its own revocation
  instances/<runID>/imports/ 0700 — per-run read-only import snapshots
                             (issue #261 / ADR-0063): one .git-less copy per
                             imported repo, taken from origin/<default> at
                             spawn, each with a sibling <name>.commit file;
                             same lifecycle as home/, bind-mounted read-only
                             into the run's container at its host-identical path
  logins/                    0700 — per-attempt scratch HOMEs for containerized
                             provider login (ADR-0057); wiped at login teardown
```

Sessions are named `<repo>~<label>`; `~` never appears in paths.

**Sizing note — container image store.** When any repo uses the [container runner](#container-runner), rootless podman's image store also lives under the state dir, at `<state>/.local/share/containers` (the lab user's HOME is `<state>`). Dev and agent-tools images typically dominate disk usage — several GB and up. Account for it when sizing the `stateDir` volume; it is reconstructible (pull-if-missing at spawn), so it is not part of the backup set.

## Backup & restore

Back up, **consistently together** (one snapshot set):

- `<state>/lab.db` (or the Postgres database) — config, credentials (encrypted), built-in tracker, run history; with the [SSH bastion](#warpgate-ssh-bastion) wired, also Warpgate's pinned host key and each repo's SSH-target cache. (Warpgate's own `/data` volume is backed up separately — [Bastion operational notes](#bastion-operational-notes).)
- `<state>/master.key` (or the sops-managed key file) — without it, credential payloads are unrecoverable.
- `<state>/repos/` — the bare reference clones (claims and parked branches live here as git refs).

Explicitly **excluded** (reconstructible or ephemeral): `<state>/runtime/`, `<state>/instances/`, `<state>/logins/` (all regenerated per operation/run and swept at boot), `<state>/worktrees/` (recreated from branches — resolve parked/dirty worktrees *before* decommissioning a host; a backup cannot carry uncommitted changes safely), `<state>/agent/` and the `agent.sock` symlink (recreated on boot), and `<state>/.local/share/containers/` (digest-pinned images, re-pulled on demand).

Mechanics:

- **SQLite**: the DB runs in WAL mode. Either stop the service and copy `lab.db` (plus `lab.db-wal`/`lab.db-shm` if present), or — with the service running — `sqlite3 <state>/lab.db ".backup '/backups/lab.db'"`. Never copy a live `lab.db` alone.
- **Postgres**: `pg_dump` on any schedule; restore with `pg_restore`/`psql` before starting lab (migrations are idempotent and apply on startup).

Restore procedure:

1. Stop the service.
2. Restore `master.key` **from the same backup set as the database** — it must be the key that encrypted the credentials in the restored DB; a newer or regenerated key decrypts nothing, and lab has no re-key command. If you rotate keys, re-enter credentials through the UI afterwards.
3. Restore `lab.db` (or the Postgres database) and `repos/`.
4. Fix ownership (`chown -R lab:lab <state>`) and perms (`master.key` 0600).
5. Start the service. Startup heals interrupted clones, re-adopts surviving tmux sessions, reconciles worktrees/branches against the restored refs (guarded — nothing dirty or unmerged is destroyed), and sweeps `runtime/`.

## CI runner prerequisites

Pull requests are gated by **GitHub Actions** on hosted `ubuntu-latest` runners (ADR-0023/0064). The workflow files' own header comments are the deep reference; this is what each needs to run:

- **Native gate** ([`.github/workflows/ci.yml`](../.github/workflows/ci.yml), job `native`) — every PR, the **required** check. Go + Node directly on the runner, no nix: SPA eslint/prettier/vitest/`vite build`, the `ui`-tagged Go build + `go test`, untagged golangci-lint. Needs: the `actions/*` actions and GitHub's hosted cache reachable; egress to `proxy.golang.org`, `registry.npmjs.org`, `github.com`/`raw.githubusercontent.com`; `git`/`prlimit` ship on the image and `tmux` is apt-installed in-job.
- **Hermetic gate** ([`.github/workflows/ci-nix.yml`](../.github/workflows/ci-nix.yml), job `flake-check`) — full `nix flake check`, path-gated to nix and Go-dependency changes (`**/*.nix`, `flake.lock`, `go.mod`, `go.sum` — the last two matter because a dep bump stales `vendorHash`, which the native gate can't catch). Needs: root/`sudo` for the Determinate nix installer, egress to `install.determinate.systems` and `cache.nixos.org`, and disk for the store. [`ci-nix-noop.yml`](../.github/workflows/ci-nix-noop.yml) is its always-green twin on the inverse paths, so the required `flake-check` context always reports (a required check that never runs would pin the PR at "Expected" forever).
- **Branch protection**: the required status checks are the job ids **`native`** and **`flake-check`** — kept stable deliberately; branch protection matches them as strings.
- **Agent-tools gate** ([`.github/workflows/agent-tools.yml`](../.github/workflows/agent-tools.yml), see [Agent-tools images](#agent-tools-images)) — path-gated to `containers/agent-tools/**`. Needs podman (preinstalled on the image), egress to `downloads.claude.ai`, `github.com`, `docker.io`, and — release leg only — `ghcr.io`. CLI artifacts are sha256-verified and cached keyed on `versions.env`.
- **Dev-image gate** ([`.github/workflows/dev-image.yml`](../.github/workflows/dev-image.yml), see [The full dev image](#the-full-dev-image)) — path-gated to `containers/dev-image/**`, plus a monthly scheduled rebuild. Needs podman and skopeo (both preinstalled on the image), several gigabytes of free disk (the job reclaims it from unused preinstalled toolchains), egress to `docker.io`, `ghcr.io`, `deb.debian.org`, `packages.sury.org`, `registry.npmjs.org`, `proxy.golang.org`, `raw.githubusercontent.com` and Playwright's browser CDN.
- **Docs gate** ([`.github/workflows/docs.yml`](../.github/workflows/docs.yml), [ADR-0066](adr/0066-docs-site-github-pages.md)) — builds the mkdocs site with `mkdocs build --strict` in `nix develop .#docs`, path-gated to everything that changes the render; on a `v*` tag it publishes to GitHub Pages via OIDC (no operator secret). One-time admin step: enable Pages with source "GitHub Actions" (Settings → Pages) — until then the deploy fails loudly.
- **Forgejo deploy poll** ([`.forgejo/workflows/deploy.yml`](../.forgejo/workflows/deploy.yml)) — the only workflow left on Forgejo (its `NIXOS_PIN_TOKEN` credential must never become a GitHub secret): a 5-minute schedule that bumps the coding-lab pin in the private `Cloonar/nixos` repo. Every fact it acts on is fetched from GitHub's `main`, never the mirror checkout; a no-op poll needs only `github.com` + `git.cloonar.com`, a real bump additionally needs the hermetic gate's prerequisites, KVM for `test-configuration`, and anonymous pulls from `ghcr.io`/`docker.io` for the skopeo probes.

The store suite additionally runs against a real Postgres wherever `LAB_TEST_POSTGRES_DSN` is set; `ci.yml` carries a ready-made `store-postgres` job as a commented template — uncomment it once the postgres store lands.

## Agent-tools images

Per-provider **agent-tools** OCI images carry the agent CLI and a static `labctl` INTO an operator-chosen dev container, so the agent surface travels with lab instead of being baked into the base image. Built by [`.github/workflows/agent-tools.yml`](../.github/workflows/agent-tools.yml) from `containers/agent-tools/`; design rationale in [ADR-0051](adr/0051-agent-tools-oci-images.md). Consumed by the [container runner](#container-runner).

**What the images are.** One per provider, `FROM scratch` (pure payload, never run as a container), tagged `ghcr.io/cloonar/agent-tools:<provider>-<cli-version>`:

| Image | Root filesystem |
|---|---|
| `agent-tools:claude-<ver>` | `/bin/claude` (Claude Code `linux-x64-musl`), `/bin/labctl` (static), `/lib/ld-musl-x86_64.so.1` (musl loader) |
| `agent-tools:codex-<ver>` | `/bin/codex` (upstream static-pie musl binary), `/bin/labctl` |

No shell, no userland — the session's userland is the dev image's business.

**The injection contract.** The container runner mounts the image read-only and prepends `/opt/lab/bin` to PATH:

```
podman run --mount type=image,src=ghcr.io/cloonar/agent-tools@sha256:…,dst=/opt/lab …
```

`/opt/lab` is a **hard contract**: the claude binary's ELF interpreter is rewritten at build time to `/opt/lab/lib/ld-musl-x86_64.so.1` — mounted anywhere else, claude won't start. That bundled musl loader is what lets claude run on both glibc and musl bases.

**Tagging + digest pinning.** The `<provider>-<cli-version>` tags are what the NixOS module's `toolsImages` default derives from `containers/agent-tools/versions.env` — the committed pin; defaults move only together with a publish. A same-tag re-push produces a new digest, so strict hand-set refs pin `agent-tools@sha256:…` (the release job prints the digest-pinned ref in its summary).

**The version catalog.** `versions.env` is the single source of truth for provider CLI versions and artifact checksums, and those versions ARE the repo's compat-record pins (`internal/compat/compat.md` for Claude Code, `internal/compat/codex/compat.md` for codex). **Bump procedure**: re-verify the compat record against the new CLI version FIRST, then bump the version + sha in `versions.env`. The PR runs the injection smoke test; the merge publishes.

**CI legs.** The PR leg (`smoke`) builds both images and mounts them into stock `debian:stable-slim` and `alpine` to run `claude --version` / `codex --version` / `labctl --help` — no secrets, no push, fork-safe. The release leg (`publish`, main + `workflow_dispatch`) re-runs the smoke test, pushes to `ghcr.io` with the built-in `GITHUB_TOKEN`, then verifies the tags **pull back anonymously** — anonymous pull is the deployment contract. `workflow_dispatch` is the manual re-release knob (e.g. to bake a new `labctl` into images at an unchanged CLI version); hosts pick the re-pushed tag up at their next restart.

**One manual step**: make the ghcr package public, once (org **Packages → `agent-tools` → Package settings → Change visibility → Public**). `GITHUB_TOKEN` cannot set visibility, and the module provisions no registry credentials, so a private package fails every host's anonymous pull — the release leg's verify step catches it in CI with that click path in the error.

**x86_64-only today.** Both upstreams publish arm64 artifacts, so an arm64 variant is a mechanical follow-up once an arm64 runner/host exists.

## Container runner

A repo whose effective **runner** is `container` runs each session's pane command as `podman run -it --rm …` — rootless podman + crun, tmux still host-side owning liveness/attach/capture ([ADR-0052](adr/0052-container-runner.md)). `host` runs the provider CLI directly on the host — break-glass: global Settings → Runner labels the option "Host — unsandboxed, full host access", and both Runner settings show "Host runs are unsandboxed — the agent has full host access to the server." whenever the runner they set is `host`. Running repos in containers requires host provisioning; the startup **preflight** verifies all of it and refuses container spawns with actionable errors until the host passes.

**Choosing the runner.** A repo's Runner (repo settings → Runner) is a three-way pick: *Inherited · Container* or *Inherited · Host* (naming what the global runner default currently is), **Container**, or **Host**. A pinned repo's field says *set here* with a *Default:* line naming the runner it would inherit, and **Reset** returns it to inheriting. New repos start inheriting; upgrading changes no existing repo — each keeps its `host` or `container` as a pin. The global runner default lives in **Settings → Runner → Runner** (`runner_default`, `host` out of the box). The card shows how many repos inherit it, and saving a switch to `host` asks you to confirm with that count. A change reaches every inheriting repo at its next spawn — live runs keep the runner they started with — and never touches a pinned repo. Manual, AFK, scheduled, lander and escalate runs all use the same effective runner. Switching the default to `container` is never blocked by preflight state: on a host that isn't ready, the spawn is refused instead (below). If `runner_default` cannot be read or holds anything but `host` or `container`, spawns of inheriting repos are refused before anything is claimed, naming the setting — lab never falls back to `host` ([ADR-0071](adr/0071-global-runner-and-dev-image-defaults.md)).

**On NixOS the module provisions everything by default** ([option table](#nixos-module-recommended)): since ADR-0054, `services.lab.enable` alone makes the host container-ready — rootless podman + passt on the unit PATH, lingering, subuid/subgid ranges, published agent-tools refs, and a digest-pinned default dev image. Overrides:

```nix
services.lab.container = {
  # host-only deployment (opt OUT — the unit reverts to the byte-identical
  # pre-container output):
  # enable = false;

  # hand-pinned tools refs instead of the versions.env-tagged defaults:
  # toolsImages."claude-code" = "ghcr.io/cloonar/agent-tools@sha256:…";

  # no deployed fallback (opt OUT of the pinned buildpack-deps default —
  # every container run gets its image from the repo's Dev image or the
  # global default dev image, ADR-0053/ADR-0056/ADR-0071; provider login
  # runs only this image and is refused without it, ADR-0057):
  # defaultImage = null;
};
```

**What the host must provide** for the `container` runner (what the module provisioning amounts to, and what a non-NixOS host assembles by hand):

- **podman ≥ 4, crun, and passt `2025_04_15`+** on the service PATH. Preflight probes `podman` and `pasta` — the pasta probe actually runs `pasta --map-guest-addr none --version`, because the pane argv pins `--network=pasta:--map-guest-addr,none` and older passt releases (through `2025_03_20`) reject that value while passing version parses and `--help` greps. If the check fires: upgrade the passt package. The argv also pins `host.containers.internal` and `host.docker.internal` to the container's own loopback — deliberate hardening so loopback-bound host services (lab's own listener included) are unreachable from containers. Egress is otherwise untouched: a host service bound to a non-loopback address is reachable by raw IP, so **bind co-located host services to `127.0.0.1`** (the two deliberate exceptions are the OneCLI gateway port and the Warpgate SSH port — see [Deploying the sidecar](#deploying-the-sidecar) and [Deploying Warpgate](#deploying-warpgate)).
- **subuid/subgid ranges for the service user** — required by `--userns=keep-id`. By hand: `usermod --add-subuids 100000-165535 --add-subgids 100000-165535 lab`.
- **cgroup v2 with a lingering user manager** ([ADR-0060](adr/0060-containers-as-user-manager-scopes.md)). Container panes run `podman --cgroup-manager=systemd`: each container is a transient `libpod-<id>.scope` under the lab user's `user@<uid>.service`, with the memory/pids caps as per-scope `MemoryMax`/`TasksMax`. Lab performs no cgroup placement of its own, so containers survive lab restarts by construction. Preflight's **`user-manager`** check probes that `/run/user/<uid>` is writable and fails with the linger hint (`users.users.lab.linger = true`; by hand `loginctl enable-linger lab`) — lab retries this one on its own, since logind brings `user@` up asynchronously at boot. The **`spawn-probe`** check then proves the pipeline with a real `podman create` + `init` of a probe container (`lab-preflight-probe`, `--memory 64m --pids-limit 16`, nothing in the image ever runs), asserting it lands in a scope with exactly those caps, then removes it.
- **The user manager's runtime dir** — lingering gives the service user `/run/user/<uid>`, independent of `lab.service`'s lifecycle; rootless podman's runroot/tmpdir live there, so container runtime state survives lab restarts. Lab sets `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` itself at startup; nothing to configure by hand beyond the linger.
- **The image knobs**: `--container-image` (the deployed fallback dev image — used when neither the repo's own **Dev image** nor the global default dev image is set, and by provider login) and `--container-tools-image` (the agent-tools refs — preflight **pulls every configured ref on each boot**, pull-first so a moved tag reaches the host, falling back to a cached image with a logged warning when the registry is unreachable; only pull-failure-with-no-cache refuses).

**Migration from the `/run/lab` layout** (pre-ADR-0060 hosts): podman's local db records the old runroot and refuses with `database configuration mismatch`. One-time fix: `podman system reset --force` as the service user — it wipes local containers and images, all of which re-pull. Survivor containers under the old cgroup layout keep running uncapped until natural exit; prune the leftover `libpod-*` dirs under the old holder's `payload/` at leisure.

**Resource limits.** Settings rows `container_memory` / `container_pids` / `container_nofile` (defaults `8g` / `4096` / `16384`) feed `--memory`, `--pids-limit`, and `--ulimit nofile` on every container pane; edit them in **Settings → Runner → Container limits**. Each repo can override any of the three in its Runner section: an inherited limit shows the value it resolves to, and **Reset** returns an override to inheriting. While a repo's effective runner is `host`, its limits and Dev image fold into the note "Dev image and container limits apply to container runs only."; **Show anyway** unfolds them to stage a value before switching. The host prlimit cap applies to host-runner panes only.

**Preflight and refusals.** Preflight runs at server startup and collects *every* failure — podman missing/too old, pasta missing or too old, missing subuid/subgid entries, cgroup v1, an unreachable user manager, a failed spawn probe, unset or unresolvable tools refs — into one message, each item paired with the fixing command or config. While any check fails, container spawns are refused with that message (host-runner repos are unaffected); an AFK spawn is refused *before* the issue is claimed, so an unready host never parks an issue. Fix the host and restart — except the `user-manager` check and tools-ref pulls, which lab retries on its own until they clear. The dev image is deliberately *not* preflight-checked: a spawn with no effective image (the repo's Dev image, the global default dev image and `--container-image` all unset) is refused at spawn, where the repo is known, naming all three. A spawn is likewise refused before the claim when `runner_default` (for an inheriting repo) or `dev_image_default` cannot be read.

**Which dev image runs.** A container run uses the first of these that is set: the repo's own **Dev image** (repo settings → Runner, `repos.image_ref`), the global default dev image (**Settings → Runner → Dev image**, `dev_image_default`), and the deployed `--container-image` (`container.defaultImage` on the module — on a stock deployment the pinned `buildpack-deps:stable-scm`). Leave the global field blank to use the deployed image: its hint then names the deployed image, or says none is configured. In repo settings, a Dev image field left to inherit shows the image the repo gets ("Inherits `<ref>`."). A changed global default reaches every container repo without a Dev image of its own at its next spawn.

**Dev image expectations.** The image needs **no lab-specific contents** (the agent layer is injected via the read-only `/opt/lab` mount); what it must bring is the session's userland: a shell and coreutils, `git`, and an ssh client for ssh remotes and [SSH targets](#warpgate-ssh-bastion). `buildpack-deps:stable-scm` qualifies as-is; `alpine` qualifies once `git` + `openssh-client` are added; plain `debian:stable-slim` does **not** (no git, no ssh). For sessions that need language toolchains, a browser or a database without a per-repo image of your own, lab publishes an opt-in one — see [The full dev image](#the-full-dev-image).

**Provider login in container mode.** On a container-wired host, login sessions and every non-interactive provider-CLI invocation (auth status, logout, the credential-refresh poke) run in containers: the CLI comes from the agent-tools mount, and the machine's master credential store is bind-mounted rw so a completed login lands where spawns copy from. Such a host needs **no** provider CLI on PATH. The login container runs the `--container-image` image (login is repo-less), never a repo's Dev image or the global default dev image: with the flag unset, login is refused, naming `--container-image`, even when the global default dev image is set. Each attempt gets a scratch HOME under `<state>/logins/`, wiped at teardown. While preflight fails, login is refused with the same actionable text. Details: [ADR-0057](adr/0057-containerized-provider-login.md).

**Pinned on save.** A dev-image ref, whether a repo's Dev image or the global default dev image, is resolved to a digest when saved and stored pinned (`host/path:tag@sha256:…`) — what runs is exactly what was reviewed; a same-tag re-push upstream never silently swaps a repo's image. Refs must be fully qualified (bare short-names like `debian:bookworm` are rejected; `docker.io/debian` normalizes fine). Resolution is anonymous and HTTPS-only — private registries needing pull auth are out of scope. A resolution failure fails the save with the registry's own error in the settings form. Updating an image is an explicit re-save.

**Pull-if-missing at spawn.** Before a container run claims anything, lab checks `podman image exists` and pulls on a miss. A failed pull refuses the spawn with an actionable error — before the issue is claimed, for AFK work. The first spawn on a freshly-set image can block on the pull.

**Image storage counts toward `stateDir`** — see the [sizing note](#state-directory-layout).

**Everything lab injects is read-only.** The `/opt/lab` mount (and any future lab-injected content) arrives read-only; an image must treat lab's mount points as reserved — never bake content at them, never write to them.

### The full dev image

The default dev image is deliberately bare: git, an ssh client, curl. A session in it has no Go, Node, Python or PHP, no browser and no database, and — running as an unprivileged user — cannot install them. For repos where that is the wrong trade, lab publishes an opt-in image that already carries them: **`ghcr.io/cloonar/dev-image:full`** ([ADR-0069](adr/0069-opt-in-full-dev-image.md)). Nothing uses it until you select it; the module default stays the stock image.

**Selecting it.** Per repo: repo settings → Runner → **Dev image**, enter `ghcr.io/cloonar/dev-image:full` and save — lab pins it to the digest it resolves to. For every container repo that has no Dev image of its own: **Settings → Runner → Dev image**, the same entry, pinned the same way on save — a runtime change, no deploy. To make it the deployed default instead, set the module option, digest-pinned by hand like any other value of it — provider login then runs in it too, since login always uses the deployed default:

```nix
services.lab.container.defaultImage = "ghcr.io/cloonar/dev-image:full@sha256:…";
```

The digest is printed in the publish job's summary; `skopeo inspect --format '{{.Digest}}' docker://ghcr.io/cloonar/dev-image:full` prints the current one.

**What is inside.**

| Area | Contents |
|---|---|
| Base | Debian stable (`buildpack-deps`): git, git-lfs, ssh client, curl, wget, gcc/g++, make, common `-dev` headers |
| Go | Go, `gopls`, `golangci-lint` |
| Node | Node.js LTS, `npm`, `pnpm`, `yarn`, `corepack` |
| Python | Python 3, `pip`, `venv`, `pipx`, `uv` |
| PHP | 8.2, 8.3 and 8.4 side by side as `php8.2` / `php8.3` / `php8.4` (bare `php` is 8.2), each with curl, mbstring, xml, zip, intl, gd, mysql, pgsql, sqlite3, bcmath, soap, opcache, redis, apcu and imagick; Composer 2 |
| Browser | Chromium, Playwright with its own Chromium build, Liberation/DejaVu/Noto fonts with emoji |
| Databases | PostgreSQL, MariaDB and Redis — servers and clients; SQLite |
| Shell tools | `rg`, `fd`, `jq`, `yq`, `shellcheck`, `rsync`, `zip`/`unzip`, `tree`, `less`, `lsof`, `ip`, `nc`, `dig` |
| Media | ImageMagick, GraphicsMagick, Ghostscript, poppler tools |

The exact pins (Go, Node major, PHP versions, golangci-lint) are in [`containers/dev-image/versions.env`](../containers/dev-image/versions.env); everything else is whatever the Debian release currently ships.

**Working in it.** A session has no way to know which dev image it landed in, so tell your agents once — a few lines in the repo's `CLAUDE.md` / `AGENTS.md` naming what applies:

- **A PHP version other than the default**: call it by name — `php8.3 bin/console …`, `php8.3 /usr/local/bin/composer install`.
- **A screenshot**: `chromium --headless --screenshot=out.png --window-size=1280,800 <url>`, or `playwright screenshot <url> out.png`. Chromium starts with the flags a container needs (no sandbox, no `/dev/shm`) already applied; Puppeteer and Karma are pointed at it through `PUPPETEER_EXECUTABLE_PATH` / `CHROME_BIN`. A project that pins its own Playwright version adds that version's browser with `npx playwright install chromium`.
- **A database for tests**: `start-postgres`, `start-mariadb`, `start-redis`. Each starts a throwaway server on `127.0.0.1` at the standard port (override with `PGPORT` / `MYSQL_TCP_PORT` / `REDIS_PORT`), prints its connection URL, and is safe to run twice. PostgreSQL's superuser is `postgres`, MariaDB's is `root`; neither has a password. Data lives under `/tmp/devdb` and ends with the session.
- **A missing package**: `sudo apt-get update && sudo apt-get install -y <pkg>`. `sudo` is passwordless for `apt-get`, `apt` and `dpkg` and refused for everything else. Root inside the container is an unprivileged subordinate uid on the host, so this grants nothing lab protects — but a file written into the worktree or HOME as root would belong to that uid host-side, where lab could neither diff nor remove it. That is the reason the rule stops at the package manager. Installed packages last until the session ends.

**Limits.** x86_64 only. No nested containers: `docker` and `ddev` cannot run inside a session, which is why the database servers are baked in. No Nix — a repo whose checks need `nix` stays on the host runner or brings its own image. Binaries installed with `go install` land in `~/go/bin`, which is not on the session PATH (lab sets PATH itself); run them by path or with `go run`.

**Updates.** The image is rebuilt on every change to `containers/dev-image/` and on the first of each month, so its Debian packages and browser stay current. Each build is also tagged `full-<YYYYMMDD>`. Because a Dev image is pinned on save, a rebuild reaches a repo only when you **re-save its ref** (the global default dev image likewise; a hand-pinned `defaultImage` moves only when you change the digest). It is several gigabytes: the first spawn after selecting or updating it waits for the pull, and it counts toward `stateDir` like any image.

**One manual step**, as for the agent-tools package: make the ghcr package public once (org **Packages → `dev-image` → Package settings → Change visibility → Public**). lab resolves and pulls dev images without credentials, so a private package fails every save; the publish job's last step checks anonymous resolution and prints this click path when it fails.

**Building it yourself.** `containers/dev-image/build.sh` builds `localhost/dev-image:full` with podman, and `containers/dev-image/smoke-test.sh` runs it the way lab does — unprivileged user, empty HOME, lab's PATH — checking every toolchain, taking real screenshots and starting each database. To add or drop a PHP version, edit `PHP_VERSIONS` in `versions.env`.

## Observability

- `GET /healthz` — liveness; 200 `ok` always (no dependencies).
- `GET /readyz` — readiness; 503 `database unavailable` while the DB is unreachable (2s probe timeout), 200 `ok` otherwise.
- `GET /metrics` — Prometheus text format. All three are mounted outside auth and CSRF — probes must work with the DB down.
- Logs: slog JSON on stdout (journald under systemd). Keys: `component`, `repo`, `session`, `run`, `err`. Secrets, tokens, key material, and DSN passwords never appear in logs.
- `GET /api/v1/onecli/health` — authenticated OneCLI gateway health (`off`/`ok`/`degraded`/`unreachable`); see [Checking it works](#checking-it-works).
- `GET /api/v1/onecli/dashboard` — authenticated; the resolved dashboard exposure, `{"mode":"off|port|subdomain","url":"…"}` with `url` omitted when off. Static config, never a sidecar probe. See [Dashboard exposure](#dashboard-exposure).
- `GET /api/v1/onecli/pool`, `GET /api/v1/repos/{id}/onecli/grants`, `PUT`/`DELETE /api/v1/repos/{id}/onecli/grants/{kind}/{resourceId}` — the grant picker's API; see [Grant picker](#grant-picker).
- `GET /api/v1/warpgate/health` — authenticated Warpgate SSH bastion health (`off`/`ok`/`degraded`/`unreachable`, host-key state folded in); see [Checking the bastion works](#checking-the-bastion-works).
- `GET /api/v1/repos/{id}/warpgate/targets`, `PUT`/`DELETE /api/v1/repos/{id}/warpgate/targets/{targetId}` — the SSH targets picker's API; see [Defining targets and granting them to a repo](#defining-targets-and-granting-them-to-a-repo).
- `GET /api/v1/auth/check` — authenticated; `204` (empty body) for any valid lab identity, `401` (standard error body) for none. The forward-auth probe `subdomain` mode is built on; see [Dashboard exposure](#dashboard-exposure).
- `GET /api/v1/repos/{id}/readiness` — authenticated; whether a run can start in that repo right now, `{"state":…,"checks":[…]}`. Not to be confused with `/readyz`. See [Repository readiness](#repository-readiness).

### Repository readiness

Each repo's home opens with a **Readiness** block: whether a run can start in that repo right now. Each check is *passing*, *failing* or *pending*, with one sentence of detail and, when failing, one action — retry the clone, or open the setting that fixes it. The same report is `GET /api/v1/repos/{id}/readiness`, and every repo response carries it as `summary.readiness`, beside `summary.claimable` and `summary.open_issues` (the last known counts, `null` until lab has computed them). The Repositories list's **Needs you** block is built from those summaries.

| Check | What it looks at | Left out when | Fix |
|---|---|---|---|
| Clone | The repo's clone; *pending* while it runs, with its progress. | Never. | **Retry clone**. |
| Git credential | The last fetch lab made with the repo's git credential: a launch, `/pull-base`, a change-request merge, or the clone itself. | The clone is not ready, or no such fetch since lab started under the credential as it is now. | Integrations → Git credential. |
| Tracker | Built-in: always passes. Forge: the binding's configuration, checked locally, then the last definitive answer to an issue or pull listing lab read from the forge: a success, or a 401, 404, or 403 that is not a rate limit. | No definitive answer since lab started under the current forge credential. | Integrations → Forge credential or Tracker binding. |
| Agent | The last known login state of the agent runs here would use, plus the AFK agent while Auto is on and it differs. | That agent's login was never successfully checked since lab started; a check that failed or was cut off does not count. | The Credentials page. |
| Dev image | Container runner only: the same gate a container spawn is refused by (container wiring, preflight, agent-tools image, a resolvable dev image), then the last pull of the dev image. It also fails, on either runner, when the effective runner cannot be resolved. | The gate is open but the image was not checked since lab started. | Runner → Runner or Dev image. |
| Imports | Each read-only import's clone and its last fetch; passes when none are declared, *pending* while a target is still cloning. | An import was not fetched since lab started. | Imports. |

Nothing in the report is fetched when you look at it: opening the list or a repo home makes no forge request, no git network operation, and starts no process. Lab records the outcome of work it does anyway and builds the report from that. Consequences worth knowing:

- **A restart empties the records.** They live in memory, so right after a restart most checks are missing until the next launch, engine pass or fetch records an outcome. A missing check is never shown as passing.
- **A record counts only for the credential it was made with.** Pick another git or forge credential, or rotate or rename the one in use, and its check disappears until the next fetch or read under it.
- **Only fetches made with the repo's own git credential count.** The background sweep fetches without credentials and never marks a credential broken. Nothing that failed without an answer is recorded either, and none of it overwrites a record: a rate limit, a 5xx, an unreachable forge, a timeout, or a fetch that lost a lock in lab's own reference repo.
- `repo.changed` fires when a verdict or a count changes, so an open page follows along.

## Metrics

Every label set is a fixed vocabulary — never run ids, repo ids, or free text — so cardinality stays bounded. Beyond the table, the standard Go and process collectors (`go_*`, `process_*`) are registered.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `lab_http_requests_total` | counter | `route` (the ServeMux pattern that handled the request — API 404s land under the `/api/v1/` catch-all and unknown UI paths under `/`; `unmatched` marks requests a middleware short-circuited before route dispatch, e.g. a CSRF 403), `method` (standard verbs, else `OTHER`), `code` | HTTP requests served. |
| `lab_http_request_duration_seconds` | histogram | `route`, `method` | HTTP request latency. |
| `lab_instances_active` | gauge | `kind` = `manual\|afk_manual\|afk_auto\|lander\|fix\|escalate` | Active runs whose tmux session is live. Evaluated **at scrape time** from the runs table + tmux (a custom collector — it cannot drift). The series is absent when the instance stack is disabled or the snapshot fails; `/metrics` itself stays 200 either way. |
| `lab_afk_runs_total` | counter | `outcome` = `success\|death\|timeout\|stopped\|escalated`, `kind` = `afk_manual\|afk_auto\|lander\|fix\|escalate\|scheduled` | Terminal AFK run outcomes, incremented at every terminal-outcome writer: the reaper, Stop, and the parked-branch Discard kill. Deaths recorded by startup re-adoption (runs that died while lab was down) are **not** counted. |
| `lab_afk_run_duration_seconds` | histogram | `outcome` | AFK run duration, `started_at`→`ended_at`. Buckets 60 s – 4 h (the default budget is 120 min). |
| `lab_tracker_requests_total` | counter | `binding` = `forge\|builtin`, `op` = `ready\|issues\|issue\|comment\|pulls\|pulls_for_head\|pull\|checks\|check_log\|create_pull\|merge_pull\|reviews\|rerequest_review\|comment_pull\|pull_comments\|close\|create_issue\|edit_issue\|label_add\|label_remove\|labels\|label_ensure`, `result` = `ok\|error` | Tracker calls resolved through the registry seam. Any non-nil error counts as `error` — domain conditions (not-found, duplicate PR) included. Error text and token bytes never cross the seam. |
| `lab_clone_jobs_total` | counter | `result` = `ready\|error` | Finished clone jobs. Jobs cancelled by a forced repo delete count neither result. |
| `lab_clones_in_flight` | gauge | — | Clone jobs currently running. |

Alerting suggestions:

- **AFK failures**: `increase(lab_afk_runs_total{outcome=~"death|timeout"}[1h]) > 3` — mirrors the three-strikes pause; a paused repo shows in **Needs you** on Repositories and on its Overview, both with **Reset**, and the run history has each `failure_reason`.
- **Tracker binding broken**: `rate(lab_tracker_requests_total{result="error"}[15m]) / rate(lab_tracker_requests_total[15m]) > 0.5` — a revoked forge token or unreachable forge starves the AFK done-signal (runs then die as timeouts).
- **Stuck instances**: `lab_instances_active` pinned at the cap for hours with no `lab_afk_runs_total` movement.
- **Clone health**: `increase(lab_clone_jobs_total{result="error"}[1h]) > 0`, or `lab_clones_in_flight > 0` sustained longer than your largest repo needs.
- **Scrape source degraded**: `lab_instances_active` absent while the service is up.

## Push notifications

Standards Web Push (RFC 8292 VAPID) — no vendor account, no push-provider registration. lab signs every send with its auto-generated VAPID keypair (`--vapid-key-file`) and talks directly to whichever gateway the browser is subscribed through.

Three requirements, all outside lab's control:

- **The UI must be served over HTTPS** — `PushManager.subscribe()` only exists in a secure context.
- **Outbound HTTPS from the server** to the push gateways: `web.push.apple.com` (Safari/iOS), `fcm.googleapis.com` (Chrome/Edge/Android), Mozilla's autopush (Firefox). No inbound ports.
- **iOS needs 16.4+ with lab added to the Home Screen** and opened from there. Enrolling always requires the enabling click itself — browsers only grant notification permission on a user gesture.

**Enrolling a device**: Settings → Notifications → "Enable notifications on this device". The device list below is device-level, not per-user — a subscription survives logout and is removed only explicitly (per-device Remove) or when a gateway reports the endpoint gone.

**Debugging**: each listed device has a Send test button. Delivery failure is silent in the UI and loud in server logs (`component=push`) — check there first. A 404/410 from the gateway is expected lifecycle (lab reaps the row); anything else logs and is dropped, nothing retries.

**Airgapped / no outbound HTTPS**: sends degrade to error logs.

**Rotation consequence**: `vapid.key` carries the same never-overwrite contract as `master.key`. Replacing or deleting it (an older restore, a manual `rm`) strands **every** subscription — pushes silently stop for all devices until each one re-enables from Settings.

## Incogni mode

Per-repo flag; when set, all seven measures apply:

1. **Attribution off at the source** — every spawn seeds the worktree's `.claude/settings.local.json` with `attribution{commit:"",pr:"",sessionUrl:false}` + `includeCoAuthoredBy:false`.
2. **Seed prompt** — the AFK seed prompt's commit step appends "No AI attribution, no Co-Authored-By, no generated-with footers anywhere."
3. **Server-side body sanitization** — the agent API strips Co-Authored-By/generated-with/Claude-Session lines from **every agent-authored body** (PR/CR, issue create, comment create) before it reaches the tracker.
4. **Neutral branch names** — incogni repos default to `issue-<N>` / `wip/`; claim parsing always uses the repo's configured pattern, never a literal `afk/`.
5. **Real git identity** — sessions and CR merges author as the repo's configured `git_author_name`/`git_author_email` (falling back to the global settings), never a bot identity.
6. **Nothing lab seeds is committed** — `.claude/`, `CLAUDE.local.md` and the seeded settings are listed in `.git/info/exclude`, never `.gitignore`.
7. **Pre-push guard** — a pre-push hook in the bare reference repo rejects pushes whose outgoing commits carry AI attribution or touch lab-seeded files, naming the offending commit. Installed when incogni turns on, removed when it turns off.

**Triage workflows vs incogni**: the triage skill posts a body line disclosing AI-generated triage content — body content, not an attribution trailer, so the sanitizer passes it through by design. Running triage against an incogni repo is an operator-level contradiction: pick one.

**Honesty note**: incogni cannot hide the forge account identity of the token used, nor statistical style/timing signals. It removes explicit AI attribution markers; it does not make the work's origin undetectable.
