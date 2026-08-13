{
  description = "lab — phone-first control panel for Claude Code agent sessions (server `lab` + agent CLI `labctl`)";

  inputs = {
    # Must ship go_1_26 (go.mod declares `go 1.26`, design §13 M1 gate).
    # nixos-unstable does; record pin changes in docs/ops.md.
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      eachSystem = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # Stamped into `main.version` (both binaries) via ldflags.
      rev = self.rev or self.dirtyRev or "unknown";

      labPkgsFor = lib.genAttrs systems (
        system: nixpkgs.legacyPackages.${system}.callPackage ./nix/package.nix { inherit rev; }
      );
    in
    {
      packages = eachSystem (pkgs: {
        inherit (labPkgsFor.${pkgs.stdenv.hostPlatform.system}) lab labctl web;
        default = labPkgsFor.${pkgs.stdenv.hostPlatform.system}.lab;
      });

      nixosModules = {
        lab = import ./nix/module.nix self;
        default = self.nixosModules.lab;
      };

      devShells = eachSystem (
        pkgs:
        let
          # The docs site's entire toolchain (ADR-0066), pinned by flake.lock
          # like everything else — which is the whole reason there is no
          # requirements.txt beside mkdocs.yml to keep in sync with it.
          # `python3.withPackages` is what puts `mkdocs` on PATH: nixpkgs'
          # mkdocs-material installs no console script of its own and only
          # propagates mkdocs, so the binary belongs to the environment rather
          # than to either package.
          mkdocsEnv = pkgs.python3.withPackages (ps: [ ps.mkdocs-material ]);
        in
        {
          default = pkgs.mkShell {
            packages =
              (with pkgs; [
                go_1_26
                gopls
                golangci-lint
                nodejs
                tmux
                util-linux # prlimit
                sqlite
                git
              ])
              ++ [ mkdocsEnv ]; # `mkdocs serve` without leaving the dev shell
          };

          # What the docs gate enters (.github/workflows/docs.yml). The same
          # mkdocs, without realizing the Go toolchain, node and sqlite to
          # render markdown — closure the docs job has no use for and would
          # pay for on every run.
          docs = pkgs.mkShell { packages = [ mkdocsEnv ]; };
        }
      );

      # `nix flake check` is the CI truth (design §10): package builds carry
      # the go test suite (real git/tmux/prlimit in nativeCheckInputs) and the
      # SPA vitest suite; golangci-lint runs over the same source with the
      # build's vendored module cache; the nixosModule is eval-proven against
      # a dummy nixosSystem, and nixos-container-closure builds a
      # container-enabled dummy's full system closure (full VM test is M8).
      checks = eachSystem (
        pkgs:
        let
          system = pkgs.stdenv.hostPlatform.system;
          labPkgs = labPkgsFor.${system};

          # Shared settings for every dummy nixosSystem (nixos-module and
          # nixos-container-closure below). The db value keeps the ExecStart
          # '%'/'$' escaping regression (module.nix); the per-dummy
          # agentPackages/claudePackage overrides layer on top.
          common = {
            nixpkgs.hostPlatform = system;
            system.stateVersion = "25.11";
            services.lab = {
              enable = true;
              baseUrl = "https://lab.example.com";
              # Regression (module.nix ExecStart): '%' and '$' in a flag
              # value must survive systemd's specifier/env expansion.
              db = "postgres://lab:p%40ss$word@db.example.com/lab?sslmode=disable";
              environmentFile = "/run/secrets/lab.env";
              openFirewall = true;
              proxyAuth = {
                enable = true;
                trustedProxies = [ "10.0.0.0/8" ];
              };
            };
          };
          mkDummy = extra: lib.nixosSystem { modules = [ self.nixosModules.lab common extra ]; };

          # Container text-level dummy (issue #218): its unit text is grepped
          # by nixos-module AND its full toplevel is built by
          # nixos-container-closure, so both agent packages are nulled — no
          # real/unfree agent CLIs may enter either closure. Two toolsImages
          # entries pin the name-sorted comma-join; subIdRange stays at its
          # default so the check pins the default range.
          containerDummy = mkDummy {
            services.lab = {
              agentPackages."claude-code" = null;
              agentPackages.codex = null;
              container = {
                enable = true;
                toolsImages = {
                  "claude-code" = "example.com/lab/agent-tools:claude";
                  codex = "example.com/lab/agent-tools:codex";
                };
                defaultImage = "docker.io/library/debian:stable-slim";
              };
            };
            # The closure build insists on a root filesystem and a bootloader;
            # the eval-only dummies skip these and carry the (filtered-out)
            # failed assertions instead.
            fileSystems."/" = {
              device = "none";
              fsType = "tmpfs";
            };
            boot.loader.grub.enable = false;
          };
        in
        {
          inherit (labPkgs) lab labctl web;

          golangci-lint =
            pkgs.runCommand "lab-golangci-lint"
              {
                nativeBuildInputs = [
                  pkgs.go_1_26
                  pkgs.golangci-lint
                ];
              }
              ''
                export HOME=$TMPDIR
                export GOCACHE=$TMPDIR/go-cache
                export GOPATH=$TMPDIR/go
                export GOLANGCI_LINT_CACHE=$TMPDIR/golangci-cache
                export GOPROXY=off
                export GOFLAGS=-mod=vendor
                # Same as the package builds — and typechecking net/http with
                # cgo on needs a C compiler the sandbox doesn't have.
                export CGO_ENABLED=0

                cp -r ${labPkgs.goSrc} source
                chmod -R u+w source
                cd source
                ln -s ${labPkgs.lab.goModules} vendor

                # Untagged lint pass: embed_ui.go (build tag `ui`) needs the
                # copied SPA dist to typecheck its go:embed; the placeholder
                # variant covers the package here, and the `ui` file is
                # compiled + vetted inside packages.lab's checkPhase.
                golangci-lint run ./...
                touch $out
              '';

          nixos-module =
            let
              # Text-level dummy: its unit text IS realized by the runCommand,
              # so it must carry NO real agent CLIs. It exercises the deprecated
              # claudePackage alias with pkgs.hello (a cheap free stand-in whose
              # bin/ lands on PATH) and nulls the codex default to keep codex
              # out of the check's build closure.
              dummy = mkDummy {
                services.lab = {
                  claudePackage = pkgs.hello;
                  agentPackages.codex = null;
                };
              };

              # Second text-level dummy (its unit text is also realized by the
              # runCommand, so the same no-real-agent-CLIs rule applies): pins
              # that an explicitly-set agentUrl still serializes as --agent-url
              # now that the module default is null / no flag (#201).
              agentUrlDummy = mkDummy {
                services.lab = {
                  agentPackages."claude-code" = null;
                  agentPackages.codex = null;
                  agentUrl = "unix:///run/lab/agent.sock";
                };
              };

              # Text-level dummy (realized, same no-real-agent-CLIs rule):
              # pins that all three OneCLI settings serialize, and — via the
              # default unit's negative grep below — that an unconfigured host
              # emits none of them, which is what "integration entirely off
              # when unset" means at the unit level (issue #23). gatewayUrl is
              # deliberately a non-loopback address: lab's container argv pins
              # host.containers.internal to 127.0.0.1 (ADR-0052 / #216), so
              # loopback here would pin the one value a container run cannot
              # reach.
              oneCLIDummy = mkDummy {
                services.lab = {
                  agentPackages."claude-code" = null;
                  agentPackages.codex = null;
                  onecli = {
                    url = "http://127.0.0.1:10254";
                    apiKeyFile = "/run/secrets/lab-onecli-api-key";
                    gatewayUrl = "http://10.88.0.1:10255";
                  };
                };
              };

              # Third text-level dummy (realized): container mode explicitly
              # OFF. Since #220 flipped container.enable's default to true,
              # the opt-OUT is the non-default path — this dummy's unit text
              # carries the zero-diff guard below (a host-only deployment
              # must get the byte-identical pre-container unit).
              containerOffDummy = mkDummy {
                services.lab = {
                  agentPackages."claude-code" = null;
                  agentPackages.codex = null;
                  container.enable = false;
                };
              };

              # Eval-only dummies: the asserts below read package *names* off
              # config.systemd.services.lab.path. lib.getName / pname access
              # does not force outPaths, so the unfree claude-code default
              # evaluates without allowUnfree and nothing here gets built — the
              # real agent CLIs stay out of the check's build closure.
              defaultsDummy = mkDummy { services.lab.extraPackages = [ pkgs.hello ]; }; # defaults + additivity
              claudeNullDummy = mkDummy { services.lab.agentPackages."claude-code" = null; }; # per-key opt-out
              conflictDummy = mkDummy {
                services.lab = {
                  claudePackage = pkgs.hello;
                  agentPackages."claude-code" = pkgs.hello;
                };
              }; # must fail the new claudePackage/agentPackages conflict assertion

              # Eval-only container dummies (issue #218) — config-inspected,
              # never built, so like the other eval-only dummies they skip the
              # fileSystems/bootloader extras and the asserts below filter the
              # resulting unrelated assertion noise.
              # enable is NOT set here (#220): with the default now true, an
              # explicitly-emptied toolsImages alone must trip the assertion.
              containerEmptyDummy = mkDummy { services.lab.container.toolsImages = { }; };
              containerSubIdDummy = mkDummy {
                services.lab.container = {
                  enable = true;
                  toolsImages.codex = "example.com/lab/agent-tools:codex";
                  subIdRange = {
                    start = 200000;
                    count = 131072;
                  };
                };
              }; # custom range lands verbatim
              containerNoSubIdDummy = mkDummy {
                services.lab.container = {
                  enable = true;
                  toolsImages.codex = "example.com/lab/agent-tools:codex";
                  subIdRange = null;
                };
              }; # null opts out of subuid/subgid provisioning
              # Explicit opt-out (issue #230 only changed the *default*; the
              # ADR-0053 demotion of defaultImage to optional stands, so
              # setting it to null explicitly must still be a valid
              # deployment — per-repo image refs cover the rest).
              containerNullImageDummy = mkDummy {
                services.lab.container = {
                  enable = true;
                  toolsImages.codex = "example.com/lab/agent-tools:codex";
                  defaultImage = null;
                };
              };

              unitPathNames = d: map lib.getName d.config.systemd.services.lab.path;
              baselineNames = [
                # bash is load-bearing for Claude Code's Bash tool (no /bin/bash
                # on NixOS); since per-run HOMEs (#202) the unit PATH is a
                # session's only tool source, so the shell-work basics are
                # pinned here too (module.nix documents each).
                "bash-interactive"
                "coreutils"
                "findutils"
                "gnugrep"
                "gnused"
                "diffutils"
                "which"
                "gawk"
                "gnutar"
                "gzip"
                "xz"
                "zstd"
                "unzip"
                "curl"
                "jq"
                "file"
                "patch"
                "procps"
                "ripgrep"
                "nix"
              ];
              hasAll = want: names: lib.all (n: lib.elem n names) want;
              # conflictDummy asserts on config.assertions directly: a failed
              # assertion only throws at toplevel build (config.system.build.*),
              # so we inspect the merged assertion list instead of building it.
              failedAssertionMessages =
                d: map (a: a.message) (lib.filter (a: !a.assertion) d.config.assertions);
              # subIdRange asserts read the provisioned ranges off the service
              # user (every dummy keeps the `lab` default user).
              subUids = d: d.config.users.users.lab.subUidRanges;
              subGids = d: d.config.users.users.lab.subGidRanges;
            in
            assert lib.assertMsg (hasAll (baselineNames ++ [ "claude-code" "codex" "hello" ]) (unitPathNames defaultsDummy))
              "nixos-module check: defaults must put claude-code + codex + the tools baseline on the unit PATH, and extraPackages must append";
            assert lib.assertMsg (!(lib.elem "claude-code" (unitPathNames claudeNullDummy)) && hasAll (baselineNames ++ [ "codex" ]) (unitPathNames claudeNullDummy))
              "nixos-module check: agentPackages.\"claude-code\" = null must drop claude while the codex default and baseline survive (per-key merge)";
            assert lib.assertMsg (lib.any (m: lib.hasInfix "claudePackage" m && lib.hasInfix "agentPackages" m) (failedAssertionMessages conflictDummy))
              "nixos-module check: claudePackage + explicit agentPackages.\"claude-code\" must fail eval with a message naming both options";
            assert lib.assertMsg (lib.any (lib.hasInfix "claudePackage") dummy.config.warnings)
              "nixos-module check: setting the deprecated claudePackage must emit a deprecation warning";
            assert lib.assertMsg (!lib.any (lib.hasInfix "claudePackage") defaultsDummy.config.warnings)
              "nixos-module check: the defaults must not warn";
            assert lib.assertMsg (lib.hasInfix "unfree" defaultsDummy.options.services.lab.agentPackages.description)
              "nixos-module check: the agentPackages description must document the unfree claude-code default";
            # Container surface (issue #218). Filter for the container message
            # rather than expecting a singleton: eval-only dummies also fail
            # the unrelated root-fs/bootloader assertions.
            assert lib.assertMsg
              (lib.any (m: lib.hasInfix "services.lab.container.enable" m && lib.hasInfix "services.lab.container.toolsImages" m) (failedAssertionMessages containerEmptyDummy))
              "nixos-module check: container.enable with empty toolsImages must fail eval with a message naming both options";
            # ...while the fully-specified containerDummy (tmpfs root + grub
            # disabled) must be assertion-clean — this is what lets
            # nixos-container-closure build its toplevel at all.
            assert lib.assertMsg (failedAssertionMessages containerDummy == [ ])
              "nixos-module check: the fully-specified containerDummy must carry no failed assertions";
            # subIdRange semantics: default range, custom range, null opt-out,
            # and nothing at all while container mode is off.
            assert lib.assertMsg
              (subUids containerDummy == [ { startUid = 100000; count = 65536; } ] && subGids containerDummy == [ { startGid = 100000; count = 65536; } ])
              "nixos-module check: the default container.subIdRange must provision exactly start=100000 count=65536 subuid/subgid ranges for the service user";
            assert lib.assertMsg
              (subUids containerSubIdDummy == [ { startUid = 200000; count = 131072; } ] && subGids containerSubIdDummy == [ { startGid = 200000; count = 131072; } ])
              "nixos-module check: a custom container.subIdRange must land verbatim on the service user";
            assert lib.assertMsg (subUids containerNoSubIdDummy == [ ] && subGids containerNoSubIdDummy == [ ])
              "nixos-module check: container.subIdRange = null must provision no subuid/subgid ranges (operator-managed /etc/subuid+/etc/subgid)";
            # Default-on semantics (#220): a config that never mentions
            # container.* provisions the default range; the explicit opt-out
            # provisions nothing.
            assert lib.assertMsg
              (subUids defaultsDummy == [ { startUid = 100000; count = 65536; } ] && subGids defaultsDummy == [ { startGid = 100000; count = 65536; } ])
              "nixos-module check: container mode is on by default (#220), so a config with no container.* settings must still provision the default subuid/subgid range";
            assert lib.assertMsg (subUids containerOffDummy == [ ] && subGids containerOffDummy == [ ])
              "nixos-module check: with container.enable = false no subuid/subgid ranges may be provisioned";
            # Default toolsImages (#220): the CLI-version tags from
            # versions.env for both shipped providers (prefix-grepped so a
            # version bump never touches this check).
            assert lib.assertMsg
              (
                lib.attrNames defaultsDummy.config.services.lab.container.toolsImages == [
                  "claude-code"
                  "codex"
                ]
                && lib.hasPrefix "ghcr.io/cloonar/agent-tools:claude-" defaultsDummy.config.services.lab.container.toolsImages."claude-code"
                && lib.hasPrefix "ghcr.io/cloonar/agent-tools:codex-" defaultsDummy.config.services.lab.container.toolsImages.codex
              )
              "nixos-module check: the default toolsImages must carry claude-code + codex refs into ghcr.io/cloonar/agent-tools with claude-/codex- tag prefixes (#220)";
            # Explicit defaultImage = null (per-repo image refs, ADR-0053):
            # the tools flag renders, the image flag does not. Reads only the
            # ExecStart string — never forces the dummy's unfree agent
            # package defaults.
            assert lib.assertMsg
              (
                lib.hasInfix "--container-tools-image" containerNullImageDummy.config.systemd.services.lab.serviceConfig.ExecStart
                && !lib.hasInfix "--container-image" containerNullImageDummy.config.systemd.services.lab.serviceConfig.ExecStart
              )
              "nixos-module check: with explicit defaultImage = null the unit must pass --container-tools-image but no --container-image";
            # The default (issue #230): a config that never mentions
            # container.defaultImage now gets the pinned stock scm image
            # rather than null — re-pin command lives beside the option in
            # module.nix.
            assert lib.assertMsg
              (
                defaultsDummy.config.services.lab.container.defaultImage
                == "docker.io/library/buildpack-deps:stable-scm@sha256:07554a82a7a29ce00a048e0b29d18f454b5721b41940d43ee3be1ef59d55b114"
              )
              "nixos-module check: services.lab.container.defaultImage must default to the digest-pinned buildpack-deps:stable-scm ref (issue #230)";
            assert lib.assertMsg (defaultsDummy.config.virtualisation.podman.enable && !containerOffDummy.config.virtualisation.podman.enable)
              "nixos-module check: virtualisation.podman must be on by default (#220) and off under container.enable = false";
            # ADR-0060 retires the ADR-0059 cgroup machinery: containers are
            # now transient scopes under the service user's user@ manager, so
            # lab manages no auxiliary cgroup units — neither lab-payload nor
            # lab-cgroup-hygiene may exist in systemd.services in EITHER the
            # container-on or the container-off dummy.
            assert lib.assertMsg
              (
                !(containerDummy.config.systemd.services ? "lab-payload")
                && !(containerDummy.config.systemd.services ? "lab-cgroup-hygiene")
                && !(containerOffDummy.config.systemd.services ? "lab-payload")
                && !(containerOffDummy.config.systemd.services ? "lab-cgroup-hygiene")
              )
              "nixos-module check: ADR-0060 retires the ADR-0059 cgroup machinery — neither lab-payload nor lab-cgroup-hygiene may exist in systemd.services (container-on or -off)";
            # Lingering is the ADR-0060 container cgroup authority: on for the
            # service user under container.enable (its user@<uid>.service
            # manager is what containers run under as transient scopes), and
            # left at the option default — null, lingering unmanaged — when
            # the runner is off. Deliberately NOT false: linger is a
            # tri-state (nullOr bool), and false would have the module
            # actively disable lingering for a possibly operator-brought
            # user; opting out of the runner must simply stop managing it.
            assert lib.assertMsg (containerDummy.config.users.users.lab.linger == true)
              "nixos-module check: container.enable must enable lingering for the service user (ADR-0060 — the user@ manager containers run under as transient scopes)";
            assert lib.assertMsg (containerOffDummy.config.users.users.lab.linger == null)
              "nixos-module check: with container.enable = false the service user's linger must stay at the unmanaged default (null) — the module must neither enable nor force-disable lingering for a runner-less host";
            pkgs.runCommand "lab-nixos-module-eval"
              {
                unit = dummy.config.systemd.units."lab.service".text;
                agentUrlUnit = agentUrlDummy.config.systemd.units."lab.service".text;
                oneCLIUnit = oneCLIDummy.config.systemd.units."lab.service".text;
                containerUnit = containerDummy.config.systemd.units."lab.service".text;
                containerOffUnit = containerOffDummy.config.systemd.units."lab.service".text;
                passAsFile = [
                  "unit"
                  "agentUrlUnit"
                  "oneCLIUnit"
                  "containerUnit"
                  "containerOffUnit"
                ];
                # PATH-serialization greps below match against these store paths:
                # the alias (hello), a baseline tool, ripgrep, and nix — plus
                # podman/passt for the container-enabled unit.
                hello = pkgs.hello;
                gawk = pkgs.gawk;
                ripgrep = pkgs.ripgrep;
                nixPkg = dummy.config.nix.package;
                podman = pkgs.podman;
                passt = pkgs.passt;
              }
              ''
                # Load-bearing unit invariants (design §9, adrs-nix port spec).
                grep -q '^KillMode=process$' "$unitPath"
                grep -q '^Restart=on-failure$' "$unitPath"
                grep -q 'openssh' "$unitPath"
                grep -q 'util-linux' "$unitPath"

                # Regression (issue #28): the unit must export a real POSIX
                # $SHELL, not the lab system user's nologin passwd shell. Claude
                # Code resolves its Bash tool's shell from $SHELL, so a leaked
                # nologin bricks every spawned agent's first Bash call.
                grep -q 'Environment="SHELL=[^"]*/bin/bash"' "$unitPath"
                if grep '^Environment="SHELL=' "$unitPath" | grep -qF nologin; then
                  echo "unit SHELL leaks a nologin shell (issue #28 regression)" >&2
                  exit 1
                fi

                # Regression: ExecStart must use systemd escaping, not shell
                # quoting — systemd expands '%' specifiers and '$' env vars
                # regardless of quotes, so escapeSystemdExecArgs must double
                # them ('%%', '$$'). A raw '%40' would fail unit load
                # ("Invalid slot"); a raw '$' would be silently expanded.
                grep '^ExecStart=' "$unitPath" | grep -qF 'p%%40ss$$word@db.example.com'
                if grep '^ExecStart=' "$unitPath" | grep -qF 'p%40ss'; then
                  echo "ExecStart leaks an unescaped '%' (shell-style escaping?)" >&2
                  exit 1
                fi
                if grep '^ExecStart=' "$unitPath" | grep -qF "'"; then
                  echo "ExecStart contains shell single-quotes (escapeShellArgs?)" >&2
                  exit 1
                fi

                # Regression (issues #30, #201): machine traffic must default
                # to lab's own agent socket, never the network. agentUrl
                # defaults to null and the module emits no --agent-url flag, so
                # lab falls back internally to unix://<state-dir>/agent/agent.sock
                # (relocated into its own dir by issue #205) — even with the
                # external https baseUrl set above, labctl's LAB_URL cannot
                # hairpin out through the SSO/auth proxy (#30's failure mode).
                if grep '^ExecStart=' "$unitPath" | grep -qF -- '--agent-url'; then
                  echo "default unit must not pass --agent-url (socket default, issue #201)" >&2
                  exit 1
                fi
                # ...while an explicitly-set agentUrl must still serialize.
                grep '^ExecStart=' "$agentUrlUnitPath" | grep -qF -- '"--agent-url" "unix:///run/lab/agent.sock"'

                # OneCLI credential gateway (issue #23, ADR-0067): all three
                # settings are optional and default to null, so an operator who
                # has not opted in must get a unit with no --onecli-* flag at
                # all — "integration entirely off when unset" is a property of
                # the rendered argv, not just of the Go default.
                if grep '^ExecStart=' "$unitPath" | grep -qF -- '--onecli-'; then
                  echo "default unit must not pass any --onecli-* flag (integration off when unset, issue #23)" >&2
                  exit 1
                fi
                # ...while each explicitly-set OneCLI option must serialize.
                grep '^ExecStart=' "$oneCLIUnitPath" | grep -qF -- '"--onecli-url" "http://127.0.0.1:10254"'
                grep '^ExecStart=' "$oneCLIUnitPath" | grep -qF -- '"--onecli-api-key-file" "/run/secrets/lab-onecli-api-key"'
                grep '^ExecStart=' "$oneCLIUnitPath" | grep -qF -- '"--onecli-gateway-url" "http://10.88.0.1:10255"'

                # Text-level PATH serialization (issue #74): prove the path list
                # actually lands on the unit's Environment=PATH line — the
                # claudePackage alias (hello), a baseline tool (gawk), ripgrep,
                # and nix. The eval-time asserts above cover the attrset merge
                # semantics; these prove the wiring reaches the real unit text.
                grep -q "$hello/bin" "$unitPath"
                grep -q "$gawk/bin" "$unitPath"
                grep -q "$ripgrep/bin" "$unitPath"
                grep -q "$nixPkg/bin" "$unitPath"

                # Default-on container provisioning (#220): a unit with NO
                # container.* settings must come up container-ready — the
                # versions.env-pinned default tools refs (prefix-grepped so
                # a version bump never touches this check), the pinned stock
                # scm --container-image default (issue #230 — no more null, so
                # a repo-less deployment can still spawn), and podman + passt
                # on the unit PATH. Under ADR-0060 the unit carries NO runtime
                # dir and NO cgroup delegation — the negative guards below are
                # the invariant; systemd (the service user's user@ manager)
                # places every container in its own transient scope.
                grep '^ExecStart=' "$unitPath" | grep -qF -- '--container-tools-image'
                grep '^ExecStart=' "$unitPath" | grep -qF 'ghcr.io/cloonar/agent-tools:claude-'
                grep '^ExecStart=' "$unitPath" | grep -qF 'ghcr.io/cloonar/agent-tools:codex-'
                grep '^ExecStart=' "$unitPath" | grep -qF -- '"--container-image" "docker.io/library/buildpack-deps:stable-scm@sha256:07554a82a7a29ce00a048e0b29d18f454b5721b41940d43ee3be1ef59d55b114"'
                # lab.service must NEVER delegate — under ADR-0060 systemd (the
                # user@ manager) performs all cgroup placement, so lab.service
                # holds no Delegate= at all. Now unconditionally true; kept as a
                # regression pin against delegation ever creeping back on.
                if grep -q '^Delegate' "$unitPath"; then
                  echo "lab.service must never delegate — containers are user@ transient scopes (ADR-0060)" >&2
                  exit 1
                fi
                # ...and no runtime-dir / XDG_RUNTIME_DIR machinery either:
                # ADR-0060 retires /run/lab, and podman's runroot follows lab's
                # runtime env to /run/user/<uid>, whose lifetime linger owns.
                if grep -q '^RuntimeDirectory' "$unitPath"; then
                  echo "lab.service must carry no RuntimeDirectory under ADR-0060 (containers are user@ scopes)" >&2
                  exit 1
                fi
                if grep -qF 'XDG_RUNTIME_DIR' "$unitPath"; then
                  echo "lab.service must set no XDG_RUNTIME_DIR under ADR-0060 (lab sets the runtime env itself)" >&2
                  exit 1
                fi
                grep -q "$podman/bin" "$unitPath"
                grep -q "$passt/bin" "$unitPath"
                grep -qF '/run/wrappers/bin' "$unitPath"

                # Zero-diff guard (issue #218; opt-OUT since #220 flipped the
                # default): with container.enable = false NOTHING
                # container-shaped may reach the unit — no flags, no cgroup
                # delegation, no runtime dir (the bare ^RuntimeDirectory also
                # catches Preserve/Mode), no podman env. (The retired ADR-0059
                # lab-payload / lab-cgroup-hygiene units are asserted out of
                # existence at eval level above, for both dummies.) Container
                # config leaking into a disabled unit fails here forever after.
                if grep '^ExecStart=' "$containerOffUnitPath" | grep -qF -- '--container'; then
                  echo "container flags leaked into a container-disabled unit" >&2
                  exit 1
                fi
                if grep -q '^Delegate' "$containerOffUnitPath"; then
                  echo "Delegate= leaked into a container-disabled unit" >&2
                  exit 1
                fi
                if grep -q '^RuntimeDirectory' "$containerOffUnitPath"; then
                  echo "RuntimeDirectory* leaked into a container-disabled unit" >&2
                  exit 1
                fi
                if grep -qF 'XDG_RUNTIME_DIR' "$containerOffUnitPath"; then
                  echo "XDG_RUNTIME_DIR leaked into a container-disabled unit" >&2
                  exit 1
                fi
                if grep -q "$podman/bin" "$containerOffUnitPath"; then
                  echo "podman leaked onto a container-disabled unit PATH" >&2
                  exit 1
                fi
                if grep -qF '/run/wrappers/bin' "$containerOffUnitPath"; then
                  echo "/run/wrappers leaked onto a container-disabled unit PATH" >&2
                  exit 1
                fi

                # Container-enabled unit (issue #218): the image flags render
                # with systemd escaping, the tools flag comma-joined and
                # name-sorted (claude-code before codex) — pinning the
                # deterministic provider=ref serialization. containerDummy's
                # explicit defaultImage also proves a repo-level/operator
                # override still WINS over the module's new pinned default
                # (issue #230): the debian:stable-slim ref renders and the
                # buildpack-deps default does not leak in alongside it.
                grep '^ExecStart=' "$containerUnitPath" | grep -qF -- '"--container-tools-image" "claude-code=example.com/lab/agent-tools:claude,codex=example.com/lab/agent-tools:codex"'
                grep '^ExecStart=' "$containerUnitPath" | grep -qF -- '"--container-image" "docker.io/library/debian:stable-slim"'
                if grep '^ExecStart=' "$containerUnitPath" | grep -qF 'buildpack-deps'; then
                  echo "explicit defaultImage override must replace the module default, not join it (issue #230)" >&2
                  exit 1
                fi

                # ADR-0060: lab.service carries NO runtime-dir or XDG_RUNTIME_DIR
                # machinery — /run/lab is retired, and podman's runroot follows
                # lab's runtime env to /run/user/<uid>, whose lifetime linger
                # owns. And no delegation and no ordering on any auxiliary
                # cgroup unit: systemd (the service user's user@ manager) does
                # all cgroup placement, so lab.service holds no Delegate= and
                # the ADR-0059 lab-payload / lab-cgroup-hygiene units are gone
                # (asserted out of existence at eval level above).
                if grep -q '^RuntimeDirectory' "$containerUnitPath"; then
                  echo "container unit must carry no RuntimeDirectory under ADR-0060" >&2
                  exit 1
                fi
                if grep -qF 'XDG_RUNTIME_DIR' "$containerUnitPath"; then
                  echo "container unit must set no XDG_RUNTIME_DIR under ADR-0060" >&2
                  exit 1
                fi
                if grep -q '^Delegate' "$containerUnitPath"; then
                  echo "lab.service must never delegate — containers are user@ transient scopes (ADR-0060)" >&2
                  exit 1
                fi
                if grep -qE 'lab-payload|lab-cgroup-hygiene' "$containerUnitPath"; then
                  echo "container unit must not reference the retired ADR-0059 cgroup units (ADR-0060)" >&2
                  exit 1
                fi

                # ...and podman + passt (pasta) on the unit PATH line, where
                # preflight's lookup and the container pane argv resolve them —
                # plus /run/wrappers/bin, where rootless podman's setuid
                # newuidmap/newgidmap live on NixOS (issue #228).
                grep -q "$podman/bin" "$containerUnitPath"
                grep -q "$passt/bin" "$containerUnitPath"
                grep -qF '/run/wrappers/bin' "$containerUnitPath"

                cp "$unitPath" $out
              '';

          # Issue #218's headline criterion: a container-enabled NixOS system
          # (dummy image refs, no real agent CLIs) assembles as a FULL system
          # closure — option typos, module regressions, and failed assertions
          # break CI here without booting anything. Its own attr, deliberately
          # not folded into nixos-module: the eval/grep pass above stays
          # cheap, while this one builds the whole toplevel (substituted from
          # cache.nixos.org in CI).
          nixos-container-closure = containerDummy.config.system.build.toplevel;
        }
      );
    };
}
