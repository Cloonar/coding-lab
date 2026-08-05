# lab's package set: the SPA (web), the server with embedded UI + labctl
# (lab), and a slim agent-only labctl. Called from flake.nix via callPackage;
# returns an attrset (plus goSrc, consumed by the golangci-lint check).
{
  lib,
  buildGoModule,
  go_1_26,
  buildNpmPackage,
  importNpmLock,
  git,
  tmux,
  util-linux,
  rev ? "unknown",
}:

let
  version = "0.1.0";

  # `go 1.26` in go.mod is a design §13 M1 gate — build with go 1.26
  # explicitly rather than trusting the nixpkgs default.
  buildGoModule' = buildGoModule.override { go = go_1_26; };

  # Go source: everything the module build and `go test ./...` need, and
  # nothing else (web/ excluded — the SPA is its own derivation; a stray
  # locally-built internal/webui/dist must never leak into the src hash).
  goSrc = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.difference (lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../.golangci.yml
      ../cmd
      ../internal
      ../migrations
      ../assets
    ]) (lib.fileset.maybeMissing ../internal/webui/dist);
  };

  # SPA source without local build/install artifacts or Playwright e2e
  # residue (all gitignored, but dev trees have them; test-results and the
  # HTML report would otherwise change the src hash after every e2e run).
  webSrc = lib.fileset.toSource {
    root = ../web;
    fileset = lib.fileset.difference ../web (
      lib.fileset.unions [
        (lib.fileset.maybeMissing ../web/node_modules)
        (lib.fileset.maybeMissing ../web/dist)
        (lib.fileset.maybeMissing ../web/e2e/.run)
        (lib.fileset.maybeMissing ../web/test-results)
        (lib.fileset.maybeMissing ../web/playwright-report)
      ]
    );
  };

  # Both Go derivations share src + vendorHash (one goModules fetch).
  vendorHash = "sha256-hOPrF9pvuxt4r29yoUo/Uy6G7FbCOyxmFPqDDXuEbnA=";

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}+${rev}"
  ];
in
rec {
  inherit goSrc;

  # SPA build. importNpmLock derives per-package fetches from
  # web/package-lock.json directly — no npmDepsHash to keep in sync.
  web = buildNpmPackage {
    pname = "lab-web";
    inherit version;
    src = webSrc;

    npmDeps = importNpmLock { npmRoot = webSrc; };
    npmConfigHook = importNpmLock.npmConfigHook;

    # `npm run build` (tsc --noEmit && vite build) is the default build hook.
    # Design §9 pins the SPA gate as vitest AND eslint; prettier --check
    # keeps formatting in the same gate. All three run from the
    # devDependencies importNpmLock already put in node_modules.
    doCheck = true;
    checkPhase = ''
      runHook preCheck
      npm run lint
      npm run format:check
      npm test
      runHook postCheck
    '';

    # The artifact is the dist tree itself (copied to internal/webui/dist by
    # lab's preBuild below and by `make build-ui`), not an npm package.
    installPhase = ''
      runHook preInstall
      cp -r dist $out
      runHook postInstall
    '';

    meta = {
      description = "lab operator SPA (SolidJS)";
      platforms = lib.platforms.linux;
    };
  };

  # Server binary with the embedded UI, plus labctl — one derivation, both
  # binaries (design §9). Runs the full go test suite as its checkPhase:
  # real git, tmux (tests use private sockets), and prlimit (util-linux) are
  # the D17 production bar inside `nix flake check`.
  lab = buildGoModule' {
    pname = "lab";
    inherit version vendorHash ldflags;
    src = goSrc;

    env.CGO_ENABLED = "0";
    tags = [ "ui" ];
    subPackages = [
      "cmd/lab"
      "cmd/labctl"
    ];

    # go:embed cannot reach outside the package dir: the SPA dist is copied
    # to internal/webui/dist, same as `make build-ui` (design §8).
    preBuild = ''
      cp -r ${web} internal/webui/dist
      chmod -R u+w internal/webui/dist
    '';

    doCheck = true;
    nativeCheckInputs = [
      git
      tmux
      util-linux
    ];
    # Default checkPhase honors subPackages (would test only cmd/*);
    # buildGoDir keeps tags/checkFlags handling from buildGoModule.
    checkPhase = ''
      runHook preCheck
      export GOFLAGS=''${GOFLAGS//-trimpath/}
      buildGoDir test ./...
      runHook postCheck
    '';

    meta = {
      description = "Phone-first control panel for Claude Code agent sessions";
      homepage = "https://github.com/Cloonar/coding-lab";
      mainProgram = "lab";
      platforms = lib.platforms.linux;
    };
  };

  # Slim agent-side CLI: no `ui` tag, no web dependency — for seeding into
  # worktrees/hosts that only need the agent surface. The full test suite
  # already runs in `lab` (same module, same src), so skip it here.
  labctl = buildGoModule' {
    pname = "labctl";
    inherit version vendorHash ldflags;
    src = goSrc;

    env.CGO_ENABLED = "0";
    subPackages = [ "cmd/labctl" ];

    doCheck = false;

    meta = {
      description = "Agent-side CLI for lab (run-token API client)";
      homepage = "https://github.com/Cloonar/coding-lab";
      mainProgram = "labctl";
      platforms = lib.platforms.linux;
    };
  };
}
