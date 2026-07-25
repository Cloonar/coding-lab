# nixosModules.lab — the lab server as a systemd service.
#
# Unit invariants ported from v0 (docs/reference/lab-v0/default.nix; see
# docs/ops.md and the adrs-nix port spec):
#
#   - KillMode=process is LOAD-BEARING: the tmux server lab spawns (and every
#     agent session under it) lives in this unit's cgroup. KillMode=process
#     makes a restart/deploy kill only the lab process; lab re-adopts the
#     surviving tmux server on start. A config switch never drops a session.
#   - openssh on the unit PATH is load-bearing: origins are SSH remotes and
#     git forks `ssh` off PATH — without it every fetch dies with
#     "cannot run ssh: No such file or directory".
#   - util-linux provides prlimit, the per-session RLIMIT_NOFILE wrapper; it
#     runs as the inner tmux pane command and resolves against this PATH.
#   - bashInteractive is load-bearing for the agent harness: Claude Code's
#     Bash tool shells out through bash and NixOS has no /bin/bash. Since
#     per-run instance HOMEs (issue #202) the unit PATH is a session's ONLY
#     tool source — no user dotfiles or settings env.PATH apply — so every
#     tool an agent needs must be here (or in extraPackages).
#
# Secret provisioning (design §9, D9):
#
#   masterKeyFile — lab itself enforces the key file: auto-generates 0600 on
#   first start, refuses to start on loose permissions or malformed content
#   (no eval-time assertion can check runtime file perms). Point it at a
#   sops-nix path:
#
#     sops.secrets."lab/master.key" = { owner = config.services.lab.user; mode = "0600"; };
#     services.lab.masterKeyFile = config.sops.secrets."lab/master.key".path;
#
#   or use systemd LoadCredential:
#
#     systemd.services.lab.serviceConfig.LoadCredential = "master.key:/run/secrets/lab-master.key";
#     services.lab.masterKeyFile = "/run/credentials/lab.service/master.key";
#
#   vapidKeyFile — the Web Push VAPID keypair (issue #98) follows the exact
#   same contract (auto-generate 0600, refuse loose perms/malformed content,
#   never overwritten) and takes the same sops-nix / LoadCredential patterns
#   as masterKeyFile above. Rotating or deleting it strands every push
#   subscription — each device must re-enable from its settings page.
#
#   Postgres DSN with a password: put LAB_DB=postgres://… into
#   `environmentFile` and leave `db` at null — lab's precedence is
#   flag > env > default, so a --db flag would shadow LAB_DB.
#
# Container runner (ADR-0052/ADR-0053/ADR-0054; docs/ops.md "Container
# runner"):
#
#   services.lab.container.enable provisions everything the startup
#   preflight verifies for `repos.runner = container`: rootless podman +
#   passt on the unit PATH, the ADR-0059 cgroup layout (delegation lives in
#   the never-restarted lab-payload.service holder — lab.service itself stays
#   undelegated — and the lab-cgroup-hygiene pre-start oneshot keeps it that
#   way), a preserved /run/lab runtime dir, subuid/subgid ranges for the
#   service user, and the --container-image / --container-tools-image flags.
#   Every host mutation gates on the enable switch — never on option
#   non-emptiness — so a half-filled container block changes nothing on a host
#   that has opted out.
#
#   enable defaults to TRUE (ADR-0054): enabling lab means container-ready
#   provisioning, and toolsImages defaults to the CLI-version tags from
#   containers/agent-tools/versions.env — the committed pin the publish
#   job (.forgejo/workflows/agent-tools.yml, path-gated) builds from, so
#   the default refs only move when the agent-tools inputs change and
#   always name an already-published image. A host that must not run
#   containers sets container.enable = false and gets the byte-identical
#   pre-container unit (zero-diff checks pin this).
self:
{
  config,
  options,
  lib,
  pkgs,
  utils,
  ...
}:
let
  cfg = config.services.lab;

  # Port for openFirewall, from the listen address (":8080", "0.0.0.0:8080",
  # "[::]:8080" all work — the port is the piece after the last colon).
  listenPort = lib.toInt (lib.last (lib.splitString ":" cfg.listenAddr));

  args = [
    "--addr"
    cfg.listenAddr
    "--state-dir"
    cfg.stateDir
    "--master-key-file"
    cfg.masterKeyFile
    "--vapid-key-file"
    cfg.vapidKeyFile
    "--max-instances"
    (toString cfg.maxInstances)
    "--session-nofile"
    (toString cfg.sessionNofile)
  ]
  ++ lib.optionals (cfg.db != null) [
    "--db"
    cfg.db
  ]
  ++ lib.optionals (cfg.baseUrl != null) [
    "--base-url"
    cfg.baseUrl
  ]
  ++ lib.optionals (cfg.agentUrl != null) [
    "--agent-url"
    cfg.agentUrl
  ]
  ++ lib.optionals (cfg.seedUser != null) [
    "--seed-user"
    cfg.seedUser
  ]
  ++ lib.optionals (cfg.seedPasswordHash != null) [
    "--seed-password-hash"
    cfg.seedPasswordHash
  ]
  ++ lib.optionals (cfg.seedPasswordHashFile != null) [
    "--seed-password-hash-file"
    cfg.seedPasswordHashFile
  ]
  ++ lib.optionals cfg.proxyAuth.enable [
    "--proxy-auth"
    "--proxy-auth-header"
    cfg.proxyAuth.header
  ]
  # trustedProxies is deliberately NOT gated on proxyAuth.enable: the server
  # also uses --trusted-proxies without proxy auth (X-Forwarded-Proto trust
  # for Secure-cookie detection behind a TLS-terminating proxy with lab's
  # own login).
  ++ lib.optionals (cfg.proxyAuth.trustedProxies != [ ]) [
    "--trusted-proxies"
    (lib.concatStringsSep "," cfg.proxyAuth.trustedProxies)
  ]
  # Container flags gate on container.enable, never on option non-emptiness:
  # with enable = false they never render regardless of the option values.
  # Attrset iteration is name-sorted, so the provider=ref string is
  # deterministic across evals.
  ++ lib.optionals (cfg.container.enable && cfg.container.toolsImages != { }) [
    "--container-tools-image"
    (lib.concatStringsSep "," (lib.mapAttrsToList (id: ref: "${id}=${ref}") cfg.container.toolsImages))
  ]
  ++ lib.optionals (cfg.container.enable && cfg.container.defaultImage != null) [
    "--container-image"
    cfg.container.defaultImage
  ]
  # extraFlags stays LAST: an operator's hand-rolled container flags (today's
  # pre-module workaround) must still win during migration — the Go flag
  # parser takes the last occurrence of a flag.
  ++ cfg.extraFlags;

  # HOME for the unit (and thus every spawned session): the agent CLIs and git
  # both need a writable HOME. Prefer the configured user's home; fall back to
  # the state dir for system users without a real one.
  home =
    let
      u = config.users.users.${cfg.user} or null;
      h = if u == null then "" else (u.home or "");
    in
    if h != "" && h != "/homeless-shelter" then h else cfg.stateDir;

  # Operative agentPackages defaults, injected per-key at mkOptionDefault
  # priority (see the config section) so a consumer's definition of ONE key
  # merges per-key with the rest instead of replacing the whole attrset. The
  # option's own `default` is documentation only — it would replace-not-merge.
  agentPackageDefaults = {
    "claude-code" = pkgs.claude-code;
    codex = pkgs.codex;
  };

  # An agentPackages."claude-code" definition is "explicit" when it beats the
  # injected mkOptionDefault priority — that is what the claudePackage
  # conflict assertion keys on. Reads only per-attr priorities, never the
  # values, so it cannot force the (unfree) default packages to evaluate.
  agentPackagesPrio = lib.modules.mergeAttrDefinitionsWithPrio options.services.lab.agentPackages;
  claudeCodeExplicitlySet =
    agentPackagesPrio ? "claude-code"
    && agentPackagesPrio."claude-code".highestPrio < (lib.mkOptionDefault null).priority;

  # Deprecated-alias overlay: the claudePackage value populates
  # agentPackages."claude-code". The conflict assertion below guarantees no
  # explicit agentPackages."claude-code" is being silently overridden here.
  effectiveAgentPackages =
    cfg.agentPackages
    // lib.optionalAttrs (cfg.claudePackage != null) { "claude-code" = cfg.claudePackage; };

  # Count of hash sources set, for the seedUser assertion below: lab itself
  # only requires "at least one"; the assertion is stricter (exactly one) so
  # a typo'd deploy (e.g. both options set, or neither) fails at eval time
  # instead of at service start.
  seedHashSourceCount = lib.count (x: x != null) [
    cfg.seedPasswordHash
    cfg.seedPasswordHashFile
  ];

  # Default agent-tools refs, derived from the committed pin (ADR-0054):
  # containers/agent-tools/versions.env is the single source of truth the
  # build scripts and the publish job also read, and the publish leg pushes
  # exactly these `<prefix>-<cli-version>` tags — only when the agent-tools
  # inputs change, which is also the only time these defaults change. The
  # deploy pipeline bumps the consuming host pin only after the refs
  # resolve in the registry, and preflight re-pulls the tags each boot, so
  # a same-tag re-push (a labctl refresh via workflow_dispatch) reaches
  # hosts on their next restart.
  agentToolsVersion =
    key:
    let
      lines = lib.splitString "\n" (builtins.readFile (self + "/containers/agent-tools/versions.env"));
    in
    lib.removePrefix "${key}=" (lib.head (lib.filter (l: lib.hasPrefix "${key}=" l) lines));
  agentToolsDefault =
    tagPrefix: versionKey:
    "${cfg.container.toolsImageRepo}:${tagPrefix}-${agentToolsVersion versionKey}";
in
{
  options.services.lab = {
    enable = lib.mkEnableOption "lab, the phone-first control panel for Claude Code agent sessions";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.lab;
      defaultText = lib.literalExpression "coding-lab.packages.\${system}.lab";
      description = "The lab package (contains both `lab` and `labctl`; both land on the unit PATH).";
    };

    agentPackages = lib.mkOption {
      type = lib.types.attrsOf (lib.types.nullOr lib.types.package);
      default = {
        "claude-code" = pkgs.claude-code;
        codex = pkgs.codex;
      };
      defaultText = lib.literalExpression ''{ "claude-code" = pkgs.claude-code; codex = pkgs.codex; }'';
      description = ''
        Agent CLI packages, keyed by lab provider ID — the same strings the
        provider registry, the DB `provider` column, and the API use. Every
        non-null value's `bin/` lands on the unit PATH, and thus on the PATH of
        every session spawned under the unit.

        Setting one key to `null` disables that CLI while the other defaults
        survive: the declared defaults are injected per-key at
        `lib.mkOptionDefault` priority, so a definition of one key merges
        with — rather than replaces — the rest.

        `claude-code` is **unfree** in nixpkgs. Hosts that forbid unfree
        software set `agentPackages."claude-code" = null` (dropping the Claude
        CLI while keeping the codex default) or scope
        `nixpkgs.config.allowUnfreePredicate` to allow it.
      '';
    };

    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      description = ''
        Extra packages appended to the unit PATH, and thus to the PATH of every
        spawned session. Purely additive — never affects
        {option}`agentPackages` or the fixed tools baseline.

        Since per-run instance HOMEs (issue #202) the unit PATH is a session's
        only tool source — user dotfiles and `~/.claude/settings.json`
        `env.PATH` no longer apply — so host-specific tools agents rely on
        (e.g. `ddev`, `docker`) must be listed here; `environment.systemPackages`
        does not reach sessions.
      '';
    };

    claudePackage = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = null;
      description = ''
        Deprecated alias for {option}`agentPackages."claude-code"`. When
        non-null it populates `agentPackages."claude-code"` (its bin/ lands on
        the unit PATH) and emits a deprecation warning. Setting it together
        with an explicit {option}`agentPackages."claude-code"` definition fails
        eval — set `agentPackages."claude-code"` directly instead.
      '';
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "lab";
      description = ''
        User the service (and every agent session) runs as. The default
        creates a `lab` system user with its home at {option}`stateDir`;
        each agent CLI's own auth/config state lives under that HOME.
      '';
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "lab";
      description = "Group the service runs as (created when left at the default).";
    };

    stateDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/lab";
      description = ''
        State root: lab.db, master.key, repos/ (bare reference clones),
        worktrees/, runtime/. Managed via systemd StateDirectory when left at
        the default; otherwise the operator provides the directory (lab
        creates missing children itself, 0700).
      '';
    };

    listenAddr = lib.mkOption {
      type = lib.types.str;
      default = ":8080";
      description = "Listen address, passed as --addr.";
    };

    baseUrl = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "https://lab.example.com";
      description = ''
        External base URL (--base-url). Drives Secure-cookie detection and
        the CSRF Origin check — set it whenever lab sits behind TLS.
      '';
    };

    agentUrl = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      defaultText = lib.literalExpression ''null (lab's own default: unix://''${config.services.lab.stateDir}/agent/agent.sock)'';
      example = "http://lab-host.internal:8080";
      description = ''
        Session-facing base URL (--agent-url) handed to labctl as `LAB_URL`.
        Leave at `null` (the default) and lab hands every spawned session
        `unix://<state-dir>/agent/agent.sock` — the agent API's own unix socket
        under {option}`stateDir`, mode 0700, always present, never touching
        the network or any proxy in front of {option}`baseUrl`. Set this
        option only when sessions run off-host and must reach lab over TCP
        (a `unix:///abs/path` value naming a different socket is also
        accepted); never point it at the external/SSO-fronted origin — that
        was issue #30's failure mode (agent traffic hairpinning through the
        auth proxy).
      '';
    };

    db = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      defaultText = lib.literalExpression ''"sqlite:''${config.services.lab.stateDir}/lab.db" (lab's own default, derived from --state-dir)'';
      example = "postgres://lab@10.0.0.5/lab";
      description = ''
        Database DSN (--db): `sqlite:<path>` or `postgres://…`. null keeps
        lab's derived sqlite default AND lets a LAB_DB entry in
        {option}`environmentFile` take effect (flag > env > default).
      '';
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = "/run/secrets/lab.env";
      description = ''
        systemd EnvironmentFile= for secret env vars (LAB_DB with a
        password-bearing postgres DSN, etc.). LoadCredential-friendly.
      '';
    };

    masterKeyFile = lib.mkOption {
      type = lib.types.str;
      default = "${cfg.stateDir}/master.key";
      defaultText = lib.literalExpression ''"''${config.services.lab.stateDir}/master.key"'';
      description = ''
        Vault master key file (--master-key-file). lab auto-generates it 0600
        when absent and refuses to start on loose permissions; see the header
        comment for the sops-nix / LoadCredential patterns.
      '';
    };

    vapidKeyFile = lib.mkOption {
      type = lib.types.str;
      default = "${cfg.stateDir}/vapid.key";
      defaultText = lib.literalExpression ''"''${config.services.lab.stateDir}/vapid.key"'';
      description = ''
        Web Push VAPID key file (--vapid-key-file). lab auto-generates it 0600
        when absent and refuses to start on loose permissions; same
        load-or-generate/refuse contract as {option}`masterKeyFile` (see the
        header comment for the sops-nix / LoadCredential patterns). Rotating
        or deleting it strands every push subscription — each device must
        re-enable from its settings page.
      '';
    };

    seedUser = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "admin";
      description = ''
        Username of the initial operator user (--seed-user), reconciled on
        every boot: the config, not the database, is this credential's source
        of truth. On an empty database lab creates the user; on every later
        boot it compares the stored hash against {option}`seedPasswordHash` /
        {option}`seedPasswordHashFile` and updates it when they differ, so
        changing the configured hash rotates the password on next restart
        without logging out existing sessions. If the database already has
        users and none of them is this one, lab refuses to start. Requires
        exactly one of {option}`seedPasswordHash` or
        {option}`seedPasswordHashFile` to be set.
      '';
    };

    seedPasswordHash = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$...";
      description = ''
        PHC-encoded argon2id hash for {option}`seedUser` (--seed-password-hash),
        generated with `lab hash-password`. Passed inline: a password hash is
        deliberately safe to keep in world-readable config or the nix store —
        that's the entire point of hashing the password rather than seeding it
        as plaintext (issue #137). {option}`seedPasswordHashFile` wins over
        this option when both are set.
      '';
    };

    seedPasswordHashFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = "/run/secrets/lab-seed-password-hash";
      description = ''
        File containing a PHC-encoded argon2id hash for {option}`seedUser`
        (--seed-password-hash-file), generated with `lab hash-password`. Wins
        over {option}`seedPasswordHash` when both are set — LoadCredential-
        friendly, same contract as {option}`environmentFile`.
      '';
    };

    maxInstances = lib.mkOption {
      type = lib.types.ints.positive;
      default = 6;
      description = ''
        Global live-instance cap (--max-instances). Seeds the settings row on
        first start; thereafter the in-app setting wins.
      '';
    };

    sessionNofile = lib.mkOption {
      type = lib.types.ints.unsigned;
      default = 16384;
      description = ''
        RLIMIT_NOFILE prlimit cap per spawned session (--session-nofile);
        0 disables. A runaway session hits its own EMFILE and dies alone.
      '';
    };

    proxyAuth = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Trust a reverse-proxy auth header (e.g. from Authelia) as the
          authenticated username — only from {option}`trustedProxies` peers.
        '';
      };

      header = lib.mkOption {
        type = lib.types.str;
        default = "Remote-User";
        description = "Header carrying the proxy-authenticated username (--proxy-auth-header).";
      };

      trustedProxies = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [ "10.0.0.0/8" ];
        description = ''
          CIDRs of trusted reverse proxies (--trusted-proxies). Passed
          whenever non-empty, independent of {option}`enable`: without proxy
          auth the list still gates X-Forwarded-Proto trust (Secure-cookie
          detection behind a TLS-terminating proxy with lab's own login).
        '';
      };
    };

    # Container runner (ADR-0052/ADR-0053/ADR-0054): host provisioning +
    # image flags for rootless-podman container panes. Everything below only
    # takes effect under container.enable — see the header comment.
    container = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Container-runner host provisioning: rootless podman + passt on the
          unit PATH, the lab-payload.service cgroup holder (ADR-0059), a
          preserved /run/lab runtime dir, subuid/subgid ranges, and the
          container image flags. On by default (ADR-0054): enabling lab means
          the host can serve `repos.runner = container` out of the box, with
          {option}`toolsImages` defaulting to the published agent-tools
          images pinned by this source's `versions.env`. Set to `false` for
          a host-only deployment — the unit is then byte-identical to the
          pre-container output (no podman, no holder unit, no subid ranges,
          no flags).
        '';
      };

      toolsImageRepo = lib.mkOption {
        type = lib.types.nonEmptyStr;
        default = "git.cloonar.com/cloonar/agent-tools";
        description = ''
          OCI repository (registry/namespace/name, no tag) the default
          {option}`toolsImages` refs point into — where
          `.forgejo/workflows/agent-tools.yml` publishes. The lab host pulls
          from it anonymously (the module provisions no registry
          credentials), which the publish job verifies after every push.
          Only consulted by the {option}`toolsImages` default; an explicit
          `toolsImages` ignores it.
        '';
      };

      toolsImages = lib.mkOption {
        type = lib.types.attrsOf lib.types.nonEmptyStr;
        default = {
          "claude-code" = agentToolsDefault "claude" "CLAUDE_CODE_VERSION";
          codex = agentToolsDefault "codex" "CODEX_VERSION";
        };
        defaultText = lib.literalExpression ''{ "claude-code" = "''${toolsImageRepo}:claude-''${CLAUDE_CODE_VERSION}"; codex = "''${toolsImageRepo}:codex-''${CODEX_VERSION}"; } — the CLI versions from containers/agent-tools/versions.env'';
        example = lib.literalExpression ''{ "claude-code" = "git.cloonar.com/cloonar/agent-tools:claude-code@sha256:…"; }'';
        description = ''
          Agent-tools OCI image refs (--container-tools-image), keyed by lab
          provider ID — the same strings the provider registry, the DB
          `provider` column, and the API use. Each value names the read-only
          agent-tools image (provider CLI + labctl) mounted at `/opt/lab` in
          that provider's container panes (ADR-0051).

          The default follows the committed pin (ADR-0054): the CLI-version
          tags from `containers/agent-tools/versions.env`, which the publish
          job builds and pushes exactly when the agent-tools inputs change —
          so the default refs only move together with the images they name,
          and preflight re-pulls the tags each boot so a same-tag re-push (a
          labctl refresh) reaches hosts on their next restart. `@sha256`-
          pinned refs are the strict contract and remain recommended for
          hand-set values, but are deliberately not enforced: a local tag is
          fine during bring-up.

          Keys are deliberately NOT validated against provider IDs at eval
          time: the server's boot error names the registered IDs, and a
          nix-side list would drift as providers land (e.g. #126). Preflight
          remains the runtime authority on host readiness — it resolves every
          configured ref at startup (retrying pulls that race the publish
          job) and refuses container spawns until all of them do.
        '';
      };

      defaultImage = lib.mkOption {
        type = lib.types.nullOr lib.types.nonEmptyStr;
        # Digest-pinned stock scm image (issue #230): satisfies the dev-image
        # contract as-is with no operator-supplied image, so a stock
        # deployment stops refusing container spawns for repos that never
        # set their own image_ref.
        # Re-pin (manual PR, no automation):
        #   skopeo inspect --format '{{.Digest}}' docker://docker.io/library/buildpack-deps:stable-scm
        default = "docker.io/library/buildpack-deps:stable-scm@sha256:07554a82a7a29ce00a048e0b29d18f454b5721b41940d43ee3be1ef59d55b114";
        description = ''
          Global default dev image (--container-image) container sessions run
          in when their repo's own **Dev image** field is blank. Defaults to
          a digest-pinned stock scm image (`buildpack-deps:stable-scm`) that
          satisfies the dev-image contract as-is — shell, coreutils, git,
          openssh-client, curl, ca-certificates — and pulls anonymously from
          docker.io (issue #230). Explicit `null` remains a valid opt-out:
          each repo can instead carry its own ref (`repos.image_ref`,
          ADR-0053), and a spawn with no effective image — repo field blank
          AND this null — is refused at spawn, naming both knobs. A repo's
          own `repos.image_ref` always overrides this default when set.
        '';
      };

      subIdRange = lib.mkOption {
        type = lib.types.nullOr (
          lib.types.submodule {
            options = {
              start = lib.mkOption {
                type = lib.types.ints.unsigned;
                default = 100000;
                description = "First subordinate UID/GID of the range.";
              };
              count = lib.mkOption {
                type = lib.types.ints.positive;
                default = 65536;
                description = "Number of subordinate IDs in the range.";
              };
            };
          }
        );
        default = {
          start = 100000;
          count = 65536;
        };
        description = ''
          subuid/subgid range provisioned for {option}`user` — rootless
          podman cannot build the user namespace `--userns=keep-id` needs
          without one. NixOS merges user attrs, so the range lands on
          whatever {option}`user` names — operator-brought users included,
          not just the module-created `lab` default. Override
          `start`/`count` when the default collides with ranges already
          allocated on the host — with {option}`enable` on by default this
          deserves a look on any host whose `/etc/subuid` already has
          entries (100000 is also the range most tools hand the first user;
          neither NixOS nor lab detects the overlap). Set the whole option
          to `null` to opt out entirely and manage `/etc/subuid` /
          `/etc/subgid` yourself.
        '';
      };
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Open the firewall for the port in {option}`listenAddr`.";
    };

    extraFlags = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [
        "--provider-bin"
        "claude-code=/run/current-system/sw/bin/claude"
      ];
      description = "Extra command-line flags appended to ExecStart.";
    };
  };

  config = lib.mkIf cfg.enable {
    # Operative agentPackages defaults, injected per-key at mkOptionDefault
    # priority so a consumer defining ONE key (or nulling it) keeps the rest.
    services.lab.agentPackages = lib.mapAttrs (_: lib.mkOptionDefault) agentPackageDefaults;

    assertions = [
      {
        assertion = cfg.proxyAuth.enable -> cfg.proxyAuth.trustedProxies != [ ];
        message = "services.lab.proxyAuth.enable requires at least one entry in services.lab.proxyAuth.trustedProxies (the header is only ever trusted from those peers).";
      }
      {
        assertion = !(cfg.claudePackage != null && claudeCodeExplicitlySet);
        message = "services.lab.claudePackage (deprecated) and an explicit services.lab.agentPackages.\"claude-code\" are both set — the alias would silently override the explicit definition. Drop the deprecated services.lab.claudePackage and keep services.lab.agentPackages.\"claude-code\".";
      }
      {
        assertion = (cfg.seedUser != null) == (seedHashSourceCount == 1);
        message = "services.lab.seedUser must be set together with exactly one of services.lab.seedPasswordHash or services.lab.seedPasswordHashFile — not both, not neither. lab itself refuses to start on this mismatch; this assertion catches a typo'd deploy at `nixos-rebuild` eval instead of after the service has already restarted.";
      }
      # Deliberately no assertion on container.defaultImage (null + per-repo
      # image refs is a valid deployment, ADR-0053), no key validation, no
      # digest-pin enforcement — see the toolsImages description.
      {
        assertion = cfg.container.enable -> cfg.container.toolsImages != { };
        message = "services.lab.container.enable is on (the default) but services.lab.container.toolsImages is empty — without agent-tools refs, preflight would refuse every container spawn anyway. Restore the versions.env-pinned default (or set explicit refs), or opt out of the container runner entirely with services.lab.container.enable = false.";
      }
    ];

    warnings = lib.optional (cfg.claudePackage != null) "services.lab.claudePackage is deprecated and will be removed; set services.lab.agentPackages.\"claude-code\" instead (it now lands on the unit PATH as agentPackages.\"claude-code\").";

    users.users = lib.mkMerge [
      (lib.mkIf (cfg.user == "lab") {
        lab = {
          isSystemUser = true;
          group = cfg.group;
          home = cfg.stateDir;
          description = "lab service user";
        };
      })
      # subuid/subgid ranges for rootless podman's user namespace. Deliberately
      # a separate mkMerge piece keyed on ${cfg.user}, NOT folded into the
      # `lab` default above: NixOS merges user attrs, so the ranges land on
      # operator-brought users too. subIdRange = null opts out (the operator
      # manages /etc/subuid+/etc/subgid); custom start/count overrides a
      # collision with ranges already allocated on the host.
      (lib.mkIf (cfg.container.enable && cfg.container.subIdRange != null) {
        ${cfg.user} = {
          subUidRanges = [
            {
              startUid = cfg.container.subIdRange.start;
              count = cfg.container.subIdRange.count;
            }
          ];
          subGidRanges = [
            {
              startGid = cfg.container.subIdRange.start;
              count = cfg.container.subIdRange.count;
            }
          ];
        };
      })
    ];
    users.groups = lib.mkIf (cfg.group == "lab") { lab = { }; };

    # Pulls in the container-host baseline (policy.json, registries.conf, crun
    # via virtualisation.containers). A plain `true`, not mkDefault: an
    # operator force-disabling podman while container mode is on should
    # surface as an option conflict at eval, not silently break preflight.
    virtualisation.podman.enable = lib.mkIf cfg.container.enable true;

    # Operators debug sessions with `tmux attach` (v0 parity).
    environment.systemPackages = [ pkgs.tmux ];

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ listenPort ];

    systemd.services.lab = {
      description = "lab — phone-first control panel for Claude Code agent sessions";
      wantedBy = [ "multi-user.target" ];
      # Both container-machinery units must be up before every lab start
      # (ADR-0059, issue #237): the hygiene oneshot (below) guarantees
      # lab.service's own cgroup delegates nothing so the executor spawn never
      # EBUSYs, and the lab-payload.service holder establishes the delegated
      # payload root that lab's preflight enables controllers on. Wants, not
      # Requires — if the holder is missing lab still starts and its preflight
      # reports the actionable cgroup-holder failure (naming lab-payload.service)
      # instead of systemd vetoing the whole service; a failed hygiene cleanup
      # likewise must not veto the start, lab's preflight re-checks the layout.
      after = [ "network.target" ]
      ++ lib.optionals cfg.container.enable [
        "lab-cgroup-hygiene.service"
        "lab-payload.service"
      ];
      wants = lib.optionals cfg.container.enable [
        "lab-cgroup-hygiene.service"
        "lab-payload.service"
      ];

      # Everything a run needs, resolved from the unit PATH: git (+ssh),
      # tmux, prlimit — and labctl for agent sessions (cfg.package ships it).
      # Since per-run instance HOMEs (issue #202), this PATH is the ONLY tool
      # source a session has: the agent's HOME is a fresh private dir, so no
      # user dotfile, home-manager session-var, or ~/.claude/settings.json
      # env.PATH can smuggle tools in anymore. What is not on the unit PATH
      # does not exist for the agent.
      path = [
        cfg.package
        pkgs.git
        pkgs.tmux
        pkgs.openssh
        pkgs.util-linux
        # bash is load-bearing for the agent harness itself: Claude Code's
        # Bash tool executes through bash, and NixOS ships no /bin/bash — an
        # agent on a unit PATH without it has no working shell tool at all
        # ("no bash access", diagnosed live 2026-07-23 on the #202 rollout).
        pkgs.bashInteractive
      ]
      # Fixed baseline of tools every session can assume — deliberately not an
      # option, deliberately no language toolchains (those come from each
      # project's flake via nix). config.nix.package puts `nix` on PATH so
      # sessions can `nix develop`/`nix shell` for per-project toolchains.
      # (issue #74 / ADR-0033)
      #
      # coreutils/findutils/grep/sed are listed explicitly even though NixOS
      # appends them to every unit PATH: the baseline documents the full
      # session contract in one place rather than leaning on a NixOS default
      # the reader has to know about. diffutils and which round out what
      # agent shell work assumes everywhere (`diff`, `which go`).
      ++ [
        pkgs.coreutils
        pkgs.findutils
        pkgs.gnugrep
        pkgs.gnused
        pkgs.diffutils
        pkgs.which
        pkgs.gawk
        pkgs.gnutar
        pkgs.gzip
        pkgs.xz
        pkgs.zstd
        pkgs.unzip
        pkgs.curl
        pkgs.jq
        pkgs.file
        pkgs.patch
        pkgs.procps
        pkgs.ripgrep
        config.nix.package
      ]
      # Container runner (enable-gated): preflight's PATH lookup probes
      # `podman` and `pasta` (shipped by the passt package), and the container
      # pane argv resolves against this PATH. crun is podman's default OCI
      # runtime and comes in via virtualisation.podman below.
      #
      # /run/wrappers is load-bearing (issue #228): rootless podman execs
      # setuid newuidmap/newgidmap to map the subordinate ID range, and on
      # NixOS those only exist as security wrappers under /run/wrappers/bin —
      # the nix store forbids setuid bits, so no package can provide them and
      # the wrapper dir must be on the unit PATH (makeBinPath appends /bin).
      ++ lib.optionals cfg.container.enable [
        pkgs.podman
        pkgs.passt
        "/run/wrappers"
      ]
      # Agent CLIs (non-null agentPackages values, claudePackage alias folded
      # in) then the additive extraPackages.
      ++ lib.filter (p: p != null) (lib.attrValues effectiveAgentPackages)
      ++ cfg.extraPackages;

      environment.HOME = home;

      # SHELL for the unit (and thus the tmux server and every session spawned
      # under it): Claude Code resolves its Bash tool's shell from $SHELL, so
      # the lab system user's nologin passwd entry would otherwise leak through
      # and brick every spawned agent's first Bash call ("No suitable shell
      # found"). Same class of fix as HOME above — pin a real POSIX shell in the
      # unit environment ONLY; the account stays non-login in passwd
      # (isSystemUser). A tmux server that survives an upgrade keeps its old
      # env, so a running deploy picks this up only after that server restarts.
      environment.SHELL = "${pkgs.bashInteractive}/bin/bash";

      # Rootless podman's runtime root — matches RuntimeDirectory=lab below
      # (see the RuntimeDirectoryPreserve comment there for why the dir must
      # survive a service stop).
      environment.XDG_RUNTIME_DIR = lib.mkIf cfg.container.enable "/run/lab";

      serviceConfig = {
        Type = "simple";
        User = cfg.user;
        Group = cfg.group;

        # LOAD-BEARING (design §9 / D4): only the lab process dies on
        # restart; the tmux server and its sessions survive and are
        # re-adopted. Do not change.
        KillMode = "process";
        Restart = "on-failure";
        RestartSec = 5;

        # systemd does NOT use shell quoting for ExecStart: '%' specifier
        # expansion and '$' env expansion run regardless of (shell-style)
        # quotes, so escapeShellArgs would brick the unit on a DSN like
        # postgres://lab:p%40ss@… and silently corrupt any '$' in a value.
        # escapeSystemdExecArgs is the systemd-correct escaper (% -> %%,
        # $ -> $$, backslash-escaped double quoting).
        ExecStart = utils.escapeSystemdExecArgs ([ (lib.getExe cfg.package) ] ++ args);

        EnvironmentFile = lib.mkIf (cfg.environmentFile != null) [ cfg.environmentFile ];
      }
      // lib.optionalAttrs (cfg.stateDir == "/var/lib/lab") {
        StateDirectory = "lab";
        StateDirectoryMode = "0700";
      }
      // lib.optionalAttrs cfg.container.enable {
        # LOAD-BEARING: lab.service must NEVER delegate — payload delegation
        # lives in lab-payload.service (ADR-0059, issue #237). systemd attaches
        # the new main PID directly into THIS unit's cgroup root on every start
        # (DelegateSubgroup= does not apply to the main-process spawn), while
        # KillMode=process survivors keep that cgroup populated and armed, so
        # any controller ever enabled in its subtree_control makes the next
        # restart fail with "Failed to spawn executor: Device or resource busy"
        # — the exact #237 incident (the 2026-07-26 dev-new outage that
        # restart-looped past counter 700). No Delegate=, no DelegateSubgroup=
        # here, ever; the lab-cgroup-hygiene oneshot enforces the empty subtree.

        # Rootless podman needs XDG_RUNTIME_DIR and a system service gets
        # none. RuntimeDirectoryPreserve is LOAD-BEARING next to
        # KillMode=process: podman-attached containers stay alive across a
        # lab restart/deploy, and a plain RuntimeDirectory would be wiped
        # underneath them on service stop, destroying rootless podman's
        # runtime state (pause-process refs, netns bookkeeping) — Preserve is
        # what makes restart-survival work.
        RuntimeDirectory = "lab";
        RuntimeDirectoryPreserve = "yes";
        RuntimeDirectoryMode = "0700";
      };
    };

    # lab-payload.service — the "holder" unit that carries the delegated
    # payload cgroup root (ADR-0059, issue #237). ADR-0058 kept delegation on
    # lab.service's own unit cgroup; that is structurally broken, because
    # systemd's DelegateSubgroup= does NOT apply to the main-process spawn, so
    # every lab.service restart clone3(CLONE_INTO_CGROUP)s the new main PID into
    # the delegating unit root and the kernel's no-internal-process rule EBUSYs
    # it (the 2026-07-26 outage). Delegation therefore moves OUT of lab.service
    # into this never-restarted holder: its own cgroup root delegates the
    # controllers, lab's rootless preflight enables +memory +pids on the
    # holder's subtree_control and pins every container under a payload/ sibling
    # via --cgroup-parent, and because this unit is never restarted the
    # executor-spawn EBUSY trap (a delegating root + a populated cgroup at
    # spawn time) can never fire for it.
    systemd.services.lab-payload = lib.mkIf cfg.container.enable {
      description = "lab payload cgroup holder — delegated root for container caps (ADR-0059)";
      wantedBy = [ "multi-user.target" ];

      # LOAD-BEARING: deploys must NEVER restart this unit. Its whole purpose is
      # that the executor-spawn EBUSY trap can never fire for it — but a restart
      # with surviving containers keeping the delegated root populated WOULD
      # fire exactly that trap (the #237 incident, re-armed). restartIfChanged =
      # false makes `nixos-rebuild switch` leave a running holder untouched even
      # when its unit text changes.
      restartIfChanged = false;

      serviceConfig = {
        # Type=exec: the start job completes only after exec(), i.e. after
        # sd-executor has already migrated the process into the main/ subgroup
        # (DelegateSubgroup below) — so lab's After= ordering can never observe
        # the sleep still sitting at the delegated root.
        Type = "exec";

        # A trivial, immortal main process: the holder exists only to own the
        # delegated cgroup; the sleep never does anything else.
        ExecStart = "${lib.getExe' pkgs.coreutils "sleep"} infinity";

        User = cfg.user;
        Group = cfg.group;

        # Delegate=yes + User= is the whole mechanism: it chowns the delegated
        # cgroup dir (plus cgroup.procs / cgroup.subtree_control) to the lab
        # user, which is exactly what lets lab's rootless preflight enable
        # +memory +pids on the holder's subtree_control and rootless podman
        # create the libpod-* children under payload/. The Go preflight
        # hardcodes this holder's cgroup path (/system.slice/lab-payload.service)
        # and checks it is writable by the service user as the delegation
        # signal — so DO NOT set Slice= or otherwise rename/re-slice this unit
        # without a matching podmanx change.
        Delegate = true;

        # systemd >= 254: the holder's own trivial main process must sit in a
        # subgroup so the delegated root itself may carry controllers (cgroup
        # v2's no-internal-process rule). On older systemd the directive is
        # ignored, the sleep sits at the root, and lab's preflight adopts it
        # into main/ before enabling controllers.
        DelegateSubgroup = "main";

        # Stopping the holder must NEVER take the containers down: they live in
        # payload/ children of this unit's cgroup and survive across a holder
        # stop the same way sessions survive a lab restart.
        KillMode = "process";
      };
    };

    # lab-cgroup-hygiene — ADR-0059's guarantee that lab.service's OWN cgroup
    # subtree delegates NOTHING before every lab start, run as root (a oneshot
    # without RemainAfterExit deactivates after each run, so each lab start job
    # pulls a fresh one). It is two things at once:
    #
    #   (a) the MIGRATION path off the ADR-0058 layout. On the first
    #       post-upgrade start lab.service's own cgroup root still carries
    #       +memory +pids (ADR-0058 delegated it) while surviving processes
    #       (tmux, and containers under the old payload/) keep the cgroup alive
    #       — the armed trap — and an undelegated lab.service can never clear
    #       its own root from inside. Only this root-run pre-start oneshot can.
    #   (b) standing insurance against anything ever re-arming delegation under
    #       lab.service after boot.
    #
    # Accepted migration cost: pre-0059 survivor containers under the old
    # lab.service/payload/ lose their --memory/--pids-limit caps when this
    # clears the root's subtree_control — they are already outside the new
    # verified holder subtree, the same accepted-uncapped-survivors posture as
    # ADR-0058's migration, and killing sessions to relabel them would invert
    # the whole restart-survival feature.
    systemd.services.lab-cgroup-hygiene = lib.mkIf cfg.container.enable {
      description = "clear all controller delegation from lab.service's own cgroup subtree";
      before = [ "lab.service" ];
      serviceConfig.Type = "oneshot";
      path = [
        pkgs.coreutils
        pkgs.findutils
      ];
      script = ''
        root=/sys/fs/cgroup/system.slice/lab.service
        [ -d "$root" ] || exit 0
        # Clear every cgroup.subtree_control under lab.service INCLUDING the
        # root's own file: `find "$root" -name cgroup.subtree_control` already
        # matches "$root/cgroup.subtree_control", so the root's delegation is
        # cleared too (the old main/-scoped sweep missed exactly that file).
        # Bottom-up (deepest first): a parent's subtree_control cannot drop a
        # controller while a child still delegates it.
        find "$root" -name cgroup.subtree_control -printf '%d %p\n' \
          | sort -rn | cut -d' ' -f2- | while read -r f; do
          for c in $(cat "$f"); do echo "-$c" > "$f" || true; done
        done
        # Prune emptied leftover child cgroups (legacy payload/ / runtime/ chains).
        find "$root" -mindepth 1 -depth -type d \
          -exec rmdir --ignore-fail-on-non-empty {} + 2>/dev/null || true
      '';
    };
  };
}
