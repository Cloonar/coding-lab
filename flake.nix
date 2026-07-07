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
              dummy = lib.nixosSystem {
                modules = [
                  self.nixosModules.lab
                  {
                    nixpkgs.hostPlatform = system;
                    system.stateVersion = "25.11";
                    services.lab = {
                      enable = true;
                      claudePackage = pkgs.hello; # stand-in: only its bin/ lands on PATH
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
                  }
                ];
              };
            in
            pkgs.runCommand "lab-nixos-module-eval"
              {
                unit = dummy.config.systemd.units."lab.service".text;
                passAsFile = [ "unit" ];
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

                # Regression (issue #30): machine traffic must default to
                # loopback. Even with the external https baseUrl set above, the
                # module defaults agentUrl to a loopback URL derived from
                # listenAddr and passes it as --agent-url, so labctl's LAB_URL
                # never hairpins out through the SSO/auth proxy.
                grep '^ExecStart=' "$unitPath" | grep -qF -- '"--agent-url" "http://127.0.0.1:8080"'

                cp "$unitPath" $out
              '';
        }
      );
    };
}
