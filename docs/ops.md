# lab — operations

Deployment, configuration, state, backup, CI, and observability for the `lab` server. The product contract is [`agent-brief.md`](agent-brief.md); design decisions are in [`adr/`](adr/).

## Deployment

### NixOS module (recommended)

Import `nixosModules.lab` from this repo's flake. Options (authoritative defaults in [`nix/module.nix`](../nix/module.nix)):

| Option | Default | Meaning |
|---|---|---|
| `enable` | `false` | Enable the service. |
| `package` | this flake's `packages.lab` | The lab package; ships both `lab` and `labctl`, both land on the unit PATH. |
| `claudePackage` | `null` | Claude Code package whose `bin/` is added to the unit PATH. When null, `claude` must reach the unit PATH some other way (e.g. `systemd.services.lab.path`) or spawns will fail. |
| `user` / `group` | `lab` / `lab` | Service identity. The default creates a `lab` system user with its home at `stateDir`; claude's own auth/config state lives under that HOME. |
| `stateDir` | `/var/lib/lab` | State root (layout below). Managed via systemd `StateDirectory` when left at the default; otherwise the operator provides the directory (lab creates missing children itself, 0700). |
| `listenAddr` | `":8080"` | Passed as `--addr`. |
| `baseUrl` | `null` | Passed as `--base-url`. Drives Secure-cookie detection and the CSRF Origin check — set it whenever lab sits behind TLS. |
| `db` | `null` | Passed as `--db` (`sqlite:<path>` or `postgres://…`). `null` keeps lab's derived sqlite default **and** lets a `LAB_DB` entry in `environmentFile` take effect (precedence is flag > env > default — a `--db` flag would shadow `LAB_DB`). |
| `environmentFile` | `null` | systemd `EnvironmentFile=` for secret env vars (`LAB_DB` with a password-bearing postgres DSN, etc.). `LoadCredential`-friendly. |
| `masterKeyFile` | `"${stateDir}/master.key"` | Passed as `--master-key-file`. lab auto-generates it 0600 when absent and refuses to start on loose permissions or malformed content. |
| `maxInstances` | `6` | Passed as `--max-instances`. Seeds the `max_instances` settings row on first start; thereafter the in-app setting wins. |
| `sessionNofile` | `16384` | Passed as `--session-nofile`; RLIMIT_NOFILE prlimit cap per spawned session, 0 disables. A runaway session hits its own EMFILE and dies alone. |
| `proxyAuth.enable` | `false` | Trust a reverse-proxy auth header as the authenticated username — only from `trustedProxies` peers. The module asserts that enabling it requires at least one trusted proxy. |
| `proxyAuth.header` | `"Remote-User"` | Passed as `--proxy-auth-header`. |
| `proxyAuth.trustedProxies` | `[ ]` | CIDRs, passed as `--trusted-proxies` whenever non-empty — also without `proxyAuth.enable`: the list gates X-Forwarded-Proto trust (Secure-cookie detection behind a TLS-terminating proxy with lab's own login). |
| `openFirewall` | `false` | Open the firewall for the port in `listenAddr`. |
| `extraFlags` | `[ ]` | Extra flags appended to `ExecStart` (e.g. `[ "--claude" "/run/current-system/sw/bin/claude" ]`). |

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

  services.lab = {
    enable = true;
    claudePackage = pkgs.claude-code;
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
- Unit PATH includes git, tmux, **openssh**, util-linux, `package` (for `labctl`), and `claudePackage`. openssh is load-bearing: origins are SSH remotes and git forks `ssh` off PATH — without it every fetch dies with "cannot run ssh".
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
| `--claude` | — | `claude` (PATH lookup) | Claude Code binary. |
| `--claude-config` | `LAB_CLAUDE_CONFIG` | `~/.claude.json` (from HOME) | Claude's global config file, the folder-trust seeding target. When unresolvable (no HOME, no value), instance/AFK features stay unmounted and lab serves the rest with a loud warning. |
| `--tmux` | — | `tmux` (PATH lookup) | tmux binary. |
| `--git` | — | `git` (PATH lookup) | git binary. |
| `--prlimit` | — | `prlimit` (PATH lookup) | prlimit binary (session NOFILE cap). |
| `--max-instances` | — | `6` | Global live-instance cap. Seeds the `max_instances` settings row on **first start only**; thereafter the runtime setting wins. Must be ≥ 1. |
| `--session-nofile` | — | `16384` | RLIMIT_NOFILE (soft+hard) for spawned sessions via prlimit; `0` disables the cap. |
| `--proxy-auth` | — | off | Accept the proxy auth header from trusted proxies. |
| `--proxy-auth-header` | — | `Remote-User` | Header carrying the proxy-authenticated username. |
| `--trusted-proxies` | — | (empty) | Comma-separated CIDRs of trusted reverse proxies (also gates `X-Forwarded-Proto` / `X-Forwarded-For` trust). |
| `--base-url` | `LAB_BASE_URL` | (empty) | Absolute http(s) external URL. Drives Secure cookies, the CSRF Origin check, and the `LAB_URL` handed to sessions (falls back to `http://127.0.0.1:<port>`). |

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

## State directory layout

```
<state>/
  lab.db                     sqlite database (WAL mode; absent on postgres)
  master.key                 vault master key (64 hex chars, 0600)
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

The Forgejo Actions workflow ([`.forgejo/workflows/ci.yml`](../.forgejo/workflows/ci.yml)) runs one gate on PRs and main: `nix flake check` — identical to the local command, so local green == CI green. Runners need:

- nix with flakes enabled (`experimental-features = nix-command flakes`).
- Egress (or mirrors/substituters) for `proxy.golang.org`, `registry.npmjs.org`, `cache.nixos.org`, and the flake inputs (github.com for nixpkgs).
- Enough disk for the nix store: the check builds the Go toolchain, node_modules (via `importNpmLock`), and runs the Go suite against real git/tmux/prlimit inside the sandbox.
- The workflow's `runs-on: nix` label is a **placeholder** — adjust it to the labels your Forgejo instance's runners actually advertise.

The store suite additionally runs against a real Postgres wherever `LAB_TEST_POSTGRES_DSN` is set; `ci.yml` carries a ready-made `store-postgres` job as a commented template (service container + DSN export) — uncomment it once your runners support service containers.

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
