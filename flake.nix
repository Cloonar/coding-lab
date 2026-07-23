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

      devShells = eachSystem (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_26
            gopls
            golangci-lint
            nodejs
            tmux
            util-linux # prlimit
            sqlite
            git
          ];
        };
      });

      # `nix flake check` is the CI truth (design §10): package builds carry
      # the go test suite (real git/tmux/prlimit in nativeCheckInputs) and the
      # SPA vitest suite; golangci-lint runs over the same source with the
      # build's vendored module cache; the nixosModule is eval-proven against
      # a dummy nixosSystem (full VM test is M8).
      checks = eachSystem (
        pkgs:
        let
          system = pkgs.stdenv.hostPlatform.system;
          labPkgs = labPkgsFor.${system};
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
              # Shared settings for every dummy nixosSystem. The db value keeps
              # the ExecStart '%'/'$' escaping regression (module.nix); the
              # per-dummy agentPackages/claudePackage overrides layer on top.
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
            pkgs.runCommand "lab-nixos-module-eval"
              {
                unit = dummy.config.systemd.units."lab.service".text;
                agentUrlUnit = agentUrlDummy.config.systemd.units."lab.service".text;
                passAsFile = [
                  "unit"
                  "agentUrlUnit"
                ];
                # PATH-serialization greps below match against these store paths:
                # the alias (hello), a baseline tool, ripgrep, and nix.
                hello = pkgs.hello;
                gawk = pkgs.gawk;
                ripgrep = pkgs.ripgrep;
                nixPkg = dummy.config.nix.package;
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

                # Text-level PATH serialization (issue #74): prove the path list
                # actually lands on the unit's Environment=PATH line — the
                # claudePackage alias (hello), a baseline tool (gawk), ripgrep,
                # and nix. The eval-time asserts above cover the attrset merge
                # semantics; these prove the wiring reaches the real unit text.
                grep -q "$hello/bin" "$unitPath"
                grep -q "$gawk/bin" "$unitPath"
                grep -q "$ripgrep/bin" "$unitPath"
                grep -q "$nixPkg/bin" "$unitPath"

                cp "$unitPath" $out
              '';
        }
      );
    };
}
