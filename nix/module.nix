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
#   Postgres DSN with a password: put LAB_DB=postgres://… into
#   `environmentFile` and leave `db` at null — lab's precedence is
#   flag > env > default, so a --db flag would shadow LAB_DB.
self:
{
  config,
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
  ++ cfg.extraFlags;

  # HOME for the unit (and thus every spawned session): claude and git both
  # need a writable HOME. Prefer the configured user's home; fall back to the
  # state dir for system users without a real one.
  home =
    let
      u = config.users.users.${cfg.user} or null;
      h = if u == null then "" else (u.home or "");
    in
    if h != "" && h != "/homeless-shelter" then h else cfg.stateDir;
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

    claudePackage = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = null;
      description = ''
        Claude Code package whose bin/ is added to the unit PATH. When null,
        `claude` must reach the unit PATH some other way (e.g.
        {option}`systemd.services.lab.path`) or spawns will fail.
      '';
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "lab";
      description = ''
        User the service (and every agent session) runs as. The default
        creates a `lab` system user with its home at {option}`stateDir`;
        claude's own auth/config state lives under that HOME.
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
      default = "http://127.0.0.1:${toString listenPort}";
      defaultText = lib.literalExpression ''"http://127.0.0.1:<port from listenAddr>"'';
      example = "http://lab-host.internal:8080";
      description = ''
        Session-facing base URL (--agent-url) handed to labctl as `LAB_URL`.
        Defaults to a loopback URL derived from {option}`listenAddr`, so that
        agent/machine traffic reaches lab directly and never hairpins out
        through {option}`baseUrl`'s external origin and any SSO/auth proxy in
        front of it. Override it only when sessions run off-host and must reach
        lab over the network; set it to `null` to fall back to lab's own
        precedence (baseUrl, else loopback).
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

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Open the firewall for the port in {option}`listenAddr`.";
    };

    extraFlags = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [
        "--claude"
        "/run/current-system/sw/bin/claude"
      ];
      description = "Extra command-line flags appended to ExecStart.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.proxyAuth.enable -> cfg.proxyAuth.trustedProxies != [ ];
        message = "services.lab.proxyAuth.enable requires at least one entry in services.lab.proxyAuth.trustedProxies (the header is only ever trusted from those peers).";
      }
    ];

    users.users = lib.mkIf (cfg.user == "lab") {
      lab = {
        isSystemUser = true;
        group = cfg.group;
        home = cfg.stateDir;
        description = "lab service user";
      };
    };
    users.groups = lib.mkIf (cfg.group == "lab") { lab = { }; };

    # Operators debug sessions with `tmux attach` (v0 parity).
    environment.systemPackages = [ pkgs.tmux ];

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ listenPort ];

    systemd.services.lab = {
      description = "lab — phone-first control panel for Claude Code agent sessions";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];

      # Everything a run needs, resolved from the unit PATH: git (+ssh),
      # tmux, prlimit — and labctl for agent sessions (cfg.package ships it).
      path = [
        cfg.package
        pkgs.git
        pkgs.tmux
        pkgs.openssh
        pkgs.util-linux
      ]
      ++ lib.optional (cfg.claudePackage != null) cfg.claudePackage;

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
      };
    };
  };
}
