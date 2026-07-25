# Restart-safe container cgroup layout: DelegateSubgroup=main + a lab-owned payload/ subtree replaces --cgroups=split

On 2026-07-25 the first `lab.service` restart after any container had run failed with `Failed to spawn executor: Device or resource busy` (result `resources`) and stayed down until manual cgroup surgery on dev-new. The trap was structural, not a flake: `--cgroups=split` (ADR-0052) nested every container's payload cgroup *inside lab's own* cgroup, so podman had to enable `memory`/`pids` in that cgroup's `subtree_control` for the `--memory`/`--pids-limit` caps to bind. cgroup v2's no-internal-processes rule forbids processes directly in a cgroup that delegates controllers — and systemd attaches the service's new main PID *directly into that cgroup* on every start. With `KillMode=process` + `RuntimeDirectoryPreserve` deliberately keeping containers (and conmon, passt, the rootless pause processes) alive across restarts, the delegation could never clear, so the next attach died with EBUSY. Worse, podman's own workaround for the same rule — moving every process it finds into a nested `runtime/` subgroup so it can enable controllers — stacked one level per service generation (`runtime/runtime/runtime/…` observed four deep) and armed the trap even harder. Three intentional features (in-subtree caps, restart-surviving sessions, systemd's root attach) were mutually inconsistent.

The fix restructures the unit's cgroup tree so the cgroup systemd attaches into never delegates and the cgroup that delegates never holds processes:

```
lab.service/          delegated root (Delegate=yes) — no processes;
│                     subtree_control carries +memory +pids
├── main/             lab + tmux + every host process
│                     (systemd's DelegateSubgroup=main attach point)
└── payload/          every container, via --cgroup-parent
    └── libpod-*/     no processes directly in payload/ itself
```

The pins, decided (grilled 2026-07-25; architecture A of the incident analysis, chosen over the podman-canonical linger + `user@` manager alternative):

- **`DelegateSubgroup=main` on the unit (systemd ≥ 254).** systemd spawns lab (and its control processes) into the `main/` subgroup instead of the unit cgroup root, so the root stays process-free and can carry the payload controllers across any restart. An older systemd ignores the directive; lab then finds itself at the delegation root and the preflight refuses container spawns with an actionable `cgroup-layout` failure — degraded loudly, never the armed trap.

- **`--cgroup-parent=<unit cgroup>/payload` + `--cgroup-manager=cgroupfs` replace `--cgroups=split` in every shape** (run pane, login pane, non-interactive CLI poke — one renderer, `podmanx.RunArgv`). The payload subtree is proc-free at its own level, so podman can enable controllers on it without the move-everything workaround, and the caps land in a subtree lab owns and preflight actually verified — preserving #205's no-false-green property that motivated split in the first place. The cgroupfs manager is pinned because a system service has no systemd user session for podman's default manager; the choice must not depend on podman's environment sniffing. An empty `RunSpec.CgroupParent` (unreachable behind a green preflight) renders no cgroup flags at all: podman's default parent is then unwritable for the rootless service user, so a miswired spawn fails loudly instead of landing uncapped in `user.slice`.

- **Lab establishes the layout at preflight** (`podmanx.setupCgroups`): adopt any process still sitting at the delegated root into its own cgroup (a tmux server surviving from a pre-ADR-0058 deploy — the root must be process-free before controllers can be enabled on it), enable `+memory +pids` on the root's `subtree_control`, create `payload/`, and confirm the controllers actually arrived in it. All within the delegated authority `Delegate=yes` grants; no new privileges.

- **A restart-safety guard runs at preflight and again before EVERY container spawn, hard-failing.** It re-checks the two invariants whose violation wedges the next restart: `main/` delegates nothing, the root holds no processes. This is the commit-directly trade's insurance — if a podman upgrade resurrects the move-into-`runtime/` heuristic and dirties `main/` after boot, the very next spawn refuses with the actionable text instead of the next deploy failing at 3am. Refusal shapes mirror each surface's contract: `*BadRequestError` for run spawns, `provider.ErrLoginUnavailable` for login panes, plain could-not-run for CLI pokes.

- **A root `lab-cgroup-hygiene` oneshot, ordered before every lab start, keeps restarts unconditionally safe.** It clears any controller delegation found inside `main/` bottom-up (a parent's `subtree_control` cannot drop a controller while a child still delegates it — the exact ordering the manual recovery tripped over) and prunes emptied leftover child cgroups (the legacy `runtime/` chains). Wants+Before wiring, not Requires: a failed cleanup must not veto the start — lab's own preflight re-checks the layout anyway. Scope is deliberately `main/` only: the root's and `payload/`'s delegation carries the containers' caps and must survive.

- **Migration is the fix itself — no separate migration step.** On the first boot after upgrade, systemd attaches lab into the freshly created `main/` (legal even under a legacy root whose `subtree_control` is still enabled), setupCgroups adopts legacy root strays, and the legacy `runtime/` chains die with their processes. Survivor containers from a pre-fix deploy keep running under the old chains, outside the caps' verified subtree, until they exit naturally — accepted: they were already uncapped by the incident recovery, and killing sessions to relabel them would invert the feature's whole point.

## Status

Accepted. Diagnosed and grilled live on 2026-07-25 (the dev-new outage; decisions: in-subtree architecture, commit without a podman-behavior spike, guard + hard-fail, hygiene-unit migration). Amends ADR-0052 (the `--cgroups=split` choice this replaces — everything else about the runner split stands) and the ADR-0057 shapes (login/CLI argv pick up the same flags); the module invariants live next to `Delegate`/`KillMode=process` in `nix/module.nix`, the layout mechanics in `internal/podmanx/cgroup.go`. Fallback if podman's heuristics prove unmanageable in-subtree despite the guard: rootless podman under a lingering `user@lab` manager (containers in transient scopes outside the unit) — re-opens ADR-0057's runtime-dir decisions and the preflight's verification story, documented here so nobody re-grills the option space from scratch.

## Considered options

- **Keep the layout, clear the delegation on every start (hygiene alone).** Rejected as the whole fix: clearing `subtree_control` strips the running containers' memory/pids caps on every restart — the silent-uncapped state #205 exists to prevent — and leaves podman re-arming the trap between restarts. As a last line of defense behind a layout that makes clearing normally a no-op, it earns its place.

- **Podman-canonical: `users.users.lab.linger = true` and a real `user@` manager.** Podman's most-tested path, zero custom cgroup surgery, restart survival free (containers live in a different unit entirely). Rejected for now: a permanent user manager for a system user, containers leave lab's delegated subtree (killing the preflight's verify-the-exact-subtree property and `Delegate`'s purpose), and it re-opens ADR-0057's `XDG_RUNTIME_DIR=/run/lab` + `RuntimeDirectoryPreserve` machinery. Kept as the documented fallback above.

- **Drop container restart-survival.** Rejected outright: since ADR-0057 the provider CLI *is* the session — killing containers on deploy breaks the D4-class sessions-survive-deploys invariant the whole unit design (KillMode=process, preserved runtime dir) exists to uphold.

- **`ExecStartPre` cleanup instead of a separate oneshot.** Rejected on mechanism: ExecStartPre processes are spawned into the unit's own cgroup, so an armed trap kills the cleanup the same way it kills the main attach. Only a unit outside `lab.service`'s cgroup can run before the attach.
