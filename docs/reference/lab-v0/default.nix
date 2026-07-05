{ config, lib, pkgs, ... }:
let
  # Restrict the source to files the Go build actually reads so unrelated
  # edits in this directory (notes, README, etc.) don't bust the derivation.
  src = lib.cleanSourceWith {
    src = ./.;
    filter = path: type:
      let base = baseNameOf path; in
      base == "go.mod"
      || base == "go.sum"
      || base == "templates"
      || lib.hasSuffix ".go" base
      || lib.hasSuffix ".html" base;
  };

  lab = pkgs.buildGoModule {
    pname = "lab";
    version = "0-unstable-2026-06-10";
    inherit src;
    vendorHash = null; # stdlib only

    # Sessions tests shell out to a real tmux; the nofile-cap integration tests
    # also shell out to prlimit (util-linux). Provide both during checkPhase.
    nativeCheckInputs = [ pkgs.tmux pkgs.util-linux ];

    env.CGO_ENABLED = "0";
    ldflags = [ "-s" "-w" ];

    meta = {
      description = "dev microvm session launcher — lists git projects and toggles claude --remote-control tmux sessions";
      mainProgram = "lab";
    };
  };
in
{
  # tmux is the source of truth for session state; claude-code is the binary
  # lab spawns. Both must be on the system so the user can `tmux attach` when
  # debugging and lab can resolve the spawn command.
  environment.systemPackages = [ pkgs.tmux ];

  # Two ingress paths share this port: LAN-direct via dev.cloonar.com:8080
  # (dnsmasq → 10.x.97.15) is unauthenticated; the public lab.cloonar.com
  # vhost on the web microvm terminates TLS and gates access through Authelia.
  networking.firewall.allowedTCPPorts = [ 8080 ];

  systemd.services.lab = {
    description = "lab — dev session launcher (Cloonar/nixos#19)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    # tmux + claude-code for spawning sessions; git + tea for AFK runs (worktree
    # lifecycle, Forgejo detection, and the issue claim). openssh because project
    # origins are SSH remotes (forgejo@git.cloonar.com:…) and git forks `ssh` off
    # PATH to fetch/push — without it `git fetch origin` dies with "cannot run
    # ssh: No such file or directory". util-linux for prlimit, which lab prepends
    # to each spawned session to cap its RLIMIT_NOFILE; it runs as the inner pane
    # command and so resolves against this service PATH. All on PATH for the
    # service's shellouts.
    path = [ pkgs.tmux pkgs.claude-code pkgs.git pkgs.tea pkgs.openssh pkgs.util-linux ];
    environment.HOME = "/home/dominik";
    serviceConfig = {
      Type = "simple";
      User = "dominik";
      Group = "users";
      # Kill only the lab process on stop/restart, never the whole cgroup. The
      # tmux server lab spawns (and every claude session under it) lives in this
      # unit's cgroup; under the default KillMode=control-group a `nixos-rebuild
      # switch` that restarts lab would take all sessions down with it. lab
      # re-adopts the surviving server on start (tmux is its source of truth),
      # so on this self-managed VM a config switch never drops a session
      # (ADR-0018).
      KillMode = "process";
      # -session-nofile caps each spawned agent's open-file budget well below the
      # VM-wide ceiling (Cloonar/nixos#76): a runaway hits its own EMFILE and dies
      # alone, leaving sshd/nscd/lab their headroom. 16384 errs generous (a busy
      # agent — claude + several LSPs + inotify + ripgrep + the forgejo MCP — can
      # hold low-thousands of fds); containment holds at any value far below the
      # system fs.nr_open, so start generous and tune down.
      ExecStart = "${lib.getExe lab} -addr :8080 -root /home/dominik/projects -session-nofile 16384";
      Restart = "on-failure";
      RestartSec = "5s";
    };
  };
}
