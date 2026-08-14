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
# Container runner (ADR-0052/ADR-0053/ADR-0054/ADR-0060; docs/ops.md
# "Container runner"):
#
#   services.lab.container.enable provisions everything the startup
#   preflight verifies for `repos.runner = container`: rootless podman +
#   passt on the unit PATH, lingering for the service user (its permanent
#   user@<uid>.service manager is the cgroup authority containers run under
#   as transient libpod-<id>.scope scopes — ADR-0060, which retires the
#   ADR-0059 lab-payload holder / lab-cgroup-hygiene layout the kernel could
#   never make work), subuid/subgid ranges for the service user, and the
#   --container-image / --container-tools-image flags. Every host mutation
#   gates on the enable switch — never on option non-emptiness — so a
#   half-filled container block changes nothing on a host that has opted out.
#
#   enable defaults to TRUE (ADR-0054): enabling lab means container-ready
#   provisioning, and toolsImages defaults to the CLI-version tags from
#   containers/agent-tools/versions.env — the committed pin the publish
#   job (.github/workflows/agent-tools.yml, path-gated) builds from, so
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
  ++ lib.optionals (cfg.sessionCookieDomain != null) [
    "--session-cookie-domain"
    cfg.sessionCookieDomain
  ]
  ++ lib.optionals (cfg.onecli.url != null) [
    "--onecli-url"
    cfg.onecli.url
  ]
  ++ lib.optionals (cfg.onecli.apiKeyFile != null) [
    "--onecli-api-key-file"
    cfg.onecli.apiKeyFile
  ]
  ++ lib.optionals (cfg.onecli.gatewayUrl != null) [
    "--onecli-gateway-url"
    cfg.onecli.gatewayUrl
  ]
  ++ lib.optionals (cfg.onecli.caFile != null) [
    "--onecli-ca-file"
    cfg.onecli.caFile
  ]
  # Emitted only when non-"off": --onecli-dashboard off would be harmless
  # (lab's own default) but noisy, and this matches how every other optional
  # flag above only renders when it says something the default doesn't.
  ++ lib.optionals (cfg.onecli.dashboard != "off") [
    "--onecli-dashboard"
    cfg.onecli.dashboard
  ]
  ++ lib.optionals (cfg.onecli.dashboardAddr != null) [
    "--onecli-dashboard-addr"
    cfg.onecli.dashboardAddr
  ]
  ++ lib.optionals (cfg.onecli.dashboardUrl != null) [
    "--onecli-dashboard-url"
    cfg.onecli.dashboardUrl
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

    sessionCookieDomain = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "example.com";
      description = ''
        `Domain` attribute on lab's session cookie (--session-cookie-domain).
        `null` (the default) omits `Domain` entirely, which makes the cookie
        host-only — correct for every topology except
        {option}`onecli.dashboard` = `"subdomain"`, which puts the dashboard
        at a DIFFERENT host, `onecli.<domain>`, fronted by the operator's own
        reverse proxy that delegates auth back to lab via forward-auth
        (checking lab's session cookie): a host-only cookie minted for the
        main domain would never reach that other host. Set this to the
        PARENT domain (e.g. `example.com`, not `onecli.example.com`) only
        when using that mode — widening a cookie's reach is a real trade, not
        the default posture, so this stays `null` unless
        {option}`onecli.dashboard` or {option}`onecli.dashboardUrl` names a
        reason. A bare domain is required: no scheme, port, or path (those
        aren't part of the cookie `Domain` attribute); a single leading dot
        is accepted like any modern browser accepts it, but is not required.
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

    onecli = {
      url = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "http://127.0.0.1:10254";
        description = ''
          Base URL of the OneCLI sidecar's REST API (--onecli-url) — its
          dashboard/API port, default 10254 — as lab itself reaches it,
          typically loopback. `null` (the default) keeps the OneCLI
          integration off. Must be set together with {option}`apiKeyFile`;
          lab refuses to start with only one of the two set. Deliberately
          separate from {option}`gatewayUrl`: the address lab itself uses to
          reach the REST API and the address a container must use to reach
          the gateway proxy are two different addresses, not the same one at
          a different path.
        '';
      };

      apiKeyFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "/run/secrets/lab-onecli-api-key";
        description = ''
          Path to a file holding the OneCLI API key (--onecli-api-key-file).
          Carries {option}`masterKeyFile`'s enforcement contract — 0600 or
          stricter, lab refuses to start on looser permissions — see the
          header comment for the sops-nix / LoadCredential patterns. Unlike
          {option}`masterKeyFile`, lab never auto-generates this file: the
          key is minted in OneCLI's own dashboard. Must be set together with
          {option}`url`; lab refuses to start with only one of the two set.
        '';
      };

      gatewayUrl = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "http://10.88.0.1:10255";
        description = ''
          OneCLI sidecar's gateway proxy URL (--onecli-gateway-url) — its
          default port 10255 — injected into runs as `HTTPS_PROXY`.
          Deliberately separate from {option}`url`: lab itself dials the REST
          API on loopback, but a `container`-runner run cannot reach
          loopback — lab's container argv pins `host.containers.internal`
          and `host.docker.internal` to 127.0.0.1 (ADR-0052 / issue #216),
          so a containerized run needs a host address routable from the
          container's netns instead. Consequently the sidecar's gateway port
          must not be bound strictly to loopback on hosts that run
          {option}`container.enable`. Independently settable from
          {option}`url` / {option}`apiKeyFile` — no pairing requirement.
        '';
      };

      caFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "/var/lib/lab/onecli-ca.pem";
        description = ''
          Path to the PEM file holding the OneCLI gateway's interception CA
          certificate on the host (--onecli-ca-file), composed with the
          host's system CA bundle into a per-run trust bundle (issue #24)
          rather than replacing the system roots outright — a bare
          interception CA would break every direct HTTPS call a run makes
          off the gateway (git push to the forge, api.anthropic.com). The
          same host file reaches a `container`-runner run through the
          per-run runtime dir's existing host-identical bind mount, so this
          is one setting rather than a separate host and container path.
          `null` (the default) is fine when {option}`gatewayUrl` is also
          unset; with {option}`gatewayUrl` set and this left `null`, lab
          refuses to spawn a run rather than start one whose HTTPS is
          broken. Independently settable from {option}`url` /
          {option}`apiKeyFile` — no pairing requirement with those two.
        '';
      };

      dashboard = lib.mkOption {
        type = lib.types.enum [
          "off"
          "port"
          "subdomain"
        ];
        default = "off";
        example = "port";
        description = ''
          How the OneCLI dashboard — OneCLI's own Next.js app on its
          dashboard/API port — is exposed through lab's own auth
          (--onecli-dashboard). `off` (the default) exposes nothing.

          `port` has lab open a SECOND listener
          ({option}`dashboardAddr`) that reverse-proxies the whole {option}`url`
          origin behind a lab-session gate; it needs
          {option}`services.lab.baseUrl` set too, because an unauthenticated
          browser hitting that listener is bounced to lab's login there. No
          cookie configuration is needed in this mode — lab's session cookie
          is host-scoped, not port-scoped, so it already works on the second
          port.

          `subdomain` instead has the OPERATOR's own reverse proxy front
          `onecli.<domain>` and delegate auth back to lab via forward-auth
          against `GET /api/v1/auth/check`; it needs
          {option}`services.lab.sessionCookieDomain` set to the parent domain
          so lab's cookie reaches that other host, and {option}`dashboardUrl`
          so lab can report the browser-facing origin to its own UI.

          A fourth mode — proxying the dashboard under a path prefix on lab's
          own origin — was researched and REJECTED in ADR-0067 (no basePath
          support upstream, and both apps claim `/settings`), so there is no
          such mode here.

          Either non-`off` mode also requires the OneCLI integration itself
          ({option}`url` + {option}`apiKeyFile`) — lab refuses to start
          otherwise, same as every other refusal described above.
        '';
      };

      dashboardAddr = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = ":8443";
        description = ''
          Listen address for lab's SECOND listener in `dashboard = "port"`
          mode (--onecli-dashboard-addr) — distinct from
          {option}`services.lab.listenAddr` (lab's main listener) and from
          {option}`url` (the proxy's upstream target in this mode: OneCLI's
          own dashboard/API port). Required when {option}`dashboard` is
          `"port"`; lab refuses to start otherwise. Meaningless — and
          rejected by lab at startup — outside that mode.
        '';
      };

      dashboardUrl = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "https://onecli.example.com";
        description = ''
          Browser-facing dashboard origin (--onecli-dashboard-url). Required
          when {option}`dashboard` is `"subdomain"`, where lab has no
          listener of its own to derive an origin from — the operator's
          reverse proxy fronts the dashboard on a domain only the operator
          knows, and delegates auth back to lab (forward-auth), so lab must
          be told what that origin is. Optional in `"port"` mode, where lab
          would otherwise derive the browser-facing origin from
          {option}`services.lab.baseUrl`'s host plus {option}`dashboardAddr`'s
          port; setting this OVERRIDES that derivation, for an operator who
          terminates TLS for the second port on a different external port
          than the one lab listens on internally (e.g. behind a load
          balancer that remaps ports). Rejected by lab at startup when
          {option}`dashboard` is `"off"` — nothing is exposed for it to name.
        '';
      };
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
          unit PATH, lingering for the service user (its user@<uid>.service
          manager is the cgroup authority containers run under as transient
          libpod-<id>.scope scopes, ADR-0060), subuid/subgid ranges, and the
          container image flags. On by default (ADR-0054): enabling lab means
          the host can serve `repos.runner = container` out of the box, with
          {option}`toolsImages` defaulting to the published agent-tools
          images pinned by this source's `versions.env`. Set to `false` for
          a host-only deployment — the unit is then byte-identical to the
          pre-container output (no podman, no lingering, no subid ranges,
          no flags).
        '';
      };

      toolsImageRepo = lib.mkOption {
        type = lib.types.nonEmptyStr;
        default = "ghcr.io/cloonar/agent-tools";
        description = ''
          OCI repository (registry/namespace/name, no tag) the default
          {option}`toolsImages` refs point into — where
          `.github/workflows/agent-tools.yml` publishes. The lab host pulls
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
        example = lib.literalExpression ''{ "claude-code" = "ghcr.io/cloonar/agent-tools:claude-code@sha256:…"; }'';
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
      # The four services.lab.onecli.dashboard assertions below mirror lab's
      # own startup refusals (see the option's description) — an eval-time
      # failure here instead of a crash-looping unit after the first restart.
      {
        assertion = cfg.onecli.dashboard != "off" -> cfg.onecli.url != null;
        message = "services.lab.onecli.dashboard is ${cfg.onecli.dashboard} but services.lab.onecli.url is unset — every non-\"off\" dashboard mode requires the OneCLI integration itself (services.lab.onecli.url and services.lab.onecli.apiKeyFile). lab itself refuses to start on this mismatch.";
      }
      {
        assertion = cfg.onecli.dashboard == "port" -> cfg.onecli.dashboardAddr != null;
        message = "services.lab.onecli.dashboard is \"port\" but services.lab.onecli.dashboardAddr is unset — port mode needs a listen address for lab's second, authenticated listener. lab itself refuses to start on this mismatch.";
      }
      {
        assertion = cfg.onecli.dashboard == "port" -> cfg.baseUrl != null;
        message = "services.lab.onecli.dashboard is \"port\" but services.lab.baseUrl is unset — port mode bounces unauthenticated browsers on the second listener to services.lab.baseUrl for login. lab itself refuses to start on this mismatch.";
      }
      {
        assertion = cfg.onecli.dashboard == "subdomain" -> cfg.onecli.dashboardUrl != null;
        message = "services.lab.onecli.dashboard is \"subdomain\" but services.lab.onecli.dashboardUrl is unset — subdomain mode has no listener of its own to derive a browser-facing origin from, so it must be told the origin explicitly. lab itself refuses to start on this mismatch.";
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
      # Lingering for the service user (ADR-0060) — the container cgroup
      # authority. loginctl enable-linger gives the user a permanent
      # user@<uid>.service manager started at boot with NO login session, and
      # rootless podman runs --cgroup-manager=systemd against it: every
      # container becomes its own transient libpod-<id>.scope scope and
      # systemd itself performs all cgroup placement, so nothing lab manages
      # ever delegates controllers. That is what clears the ADR-0058/0059
      # EBUSY/EACCES wall — the kernel's cgroup-v2 common-ancestor rule lets a
      # non-root process migrate PIDs only into a delegated subtree it already
      # lives in, so the cross-unit attach from lab.service into the old
      # holder EACCESed on every podman create (the 2026-07-26 dev-new
      # incident). Linger also keeps /run/user/<uid> — the runtime dir + user
      # bus lab's runtime env points podman at — alive independent of
      # lab.service, which is what lets containers and podman's runtime state
      # survive a lab restart by construction (each scope is independent of the
      # unit). Deliberately a separate mkMerge piece keyed on ${cfg.user}, like
      # the subuid ranges above, so it lands on operator-brought users too.
      (lib.mkIf cfg.container.enable { ${cfg.user}.linger = true; })
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
      # No container-machinery ordering (ADR-0060): containers are transient
      # scopes under the service user's user@<uid>.service manager, and
      # lab.service deliberately does NOT order After= it — the uid is unknown
      # at eval time, so preflight treats an unreachable user manager as a
      # retryable failure instead of letting systemd veto the whole start.
      after = [ "network.target" ];

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

      serviceConfig = {
        Type = "simple";
        User = cfg.user;
        Group = cfg.group;

        # LOAD-BEARING (design §9 / D4): only the lab process dies on
        # restart; the tmux server and its sessions survive and are
        # re-adopted. Do not change. Under the container runner it also keeps
        # the pane podman clients alive across a restart, while conmon and the
        # containers themselves sit OUTSIDE this unit entirely — they live as
        # transient libpod-<id>.scope scopes under the service user's
        # user@<uid>.service manager (ADR-0060), so lab.service carries no
        # cgroup-delegation or runtime-dir machinery for them.
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
