# Container panes run under `podman run --init`: a container-init is PID 1, so orphaned processes are reaped instead of filling the pids cap with zombies

On 2026-10-05 container runs on dev-new kept dying the same way: nothing inside could fork any more. Tests, `go vet` and `git push` all failed with thread or process creation errors. The container had reached its `--pids-limit` of 4096 (ADR-0052), and almost every slot was held by a zombie, not by a live process.

The cause is structural. `podmanx.RunArgv` rendered `podman run` without an init, so the provider CLI was PID 1 of the container's PID namespace. The kernel reparents every orphaned process to PID 1, and PID 1 is expected to `wait()` for them. The provider CLIs wait only for the children they started themselves, so an adopted orphan that exits stays a zombie, and a zombie keeps its slot in the pids cgroup until the container exits.

Orphans are routine, and git is the main source. `git commit`, `fetch`, `clone`, `push` and the receive side of a push fork a detached `git maintenance run --auto` in its own session, which outlives the git command that started it. Every such call an agent makes therefore leaks one zombie, and a test suite that drives git thousands of times fills the cap in well under an hour. The children of a killed pipeline (`sleep`, `sh`, `head`, `cat`, `sed`) leak the same way.

Measured on dev-new, zombies parented to each container's PID 1:

| Container age | Zombies | `pids.current` / `pids.max` |
| --- | --- | --- |
| 36 min | 4059 | 4073 / 4096 |
| 28 h | 278 | 298 / 4096 |
| 30 h | 206 | 225 / 4096 |
| 1 min | 7 | 34 / 4096 |

Reproduced in a fresh run container: six `git commit --allow-empty` calls left six defunct `git` entries under PID 1. Host-runner runs never had the problem, because their orphans reparent to systemd, which reaps them.

The decisions, pinned (issue #64):

- **Every container shape runs with `--init`.** `podmanx.RunArgv` renders the flag unconditionally, directly after `--name`, in all three shapes: run pane, login pane and the non-interactive CLI poke. Podman bind-mounts its container-init helper (`catatonit`) from the host and runs it as PID 1 with the provider CLI as its only child. The helper reaps every process reparented to it. A login or a CLI poke is too short-lived to exhaust anything, but ADR-0058's one-renderer rule stands: one argv contract, no per-shape exceptions.

- **Nothing else about the pane changes.** The helper forwards signals to its child, so the teardown path is the same: `tmux kill-session` SIGHUPs the podman client, the client proxies the signal into the container, the provider CLI exits, the helper exits with its status, and `--rm` removes the container. `podman rm --force` stays the backstop. With a tty the helper makes the provider CLI the terminal's foreground process group, so the TUI reads keys and receives window-size changes as before. No lab code addresses the container's PID 1.

- **The helper comes from the host, not from the images.** `--init` uses the binary podman finds in its helper-binaries directory, which must be statically linked because it runs inside an arbitrary dev image. Distribution podman packages ship it; the nixpkgs podman that the NixOS module installs carries a static `catatonit` under `libexec/podman`. Neither the agent-tools images (ADR-0051) nor the dev images (ADR-0053, ADR-0069) change, and the dev-image contract gains no entry.

- **Preflight proves the helper exists.** The spawn probe (ADR-0060) creates its container with `--init` too. Podman resolves the helper at `create`, so a host without one fails the `spawn-probe` check at startup, with a hint that names `catatonit`, instead of failing every spawn later. The probe still never executes anything: `podman init` stops before exec, as before.

## Status

Accepted. Diagnosed and decided 2026-10-05 from the dev-new process-exhaustion incidents (issue #64).

Amends ADR-0052: the pane argv gains `--init`. The mount inventory, the limits and the tmux-owns-the-pane split are unchanged. `--pids-limit` now bounds what it was meant to bound, live processes and threads.

Amends ADR-0060 only in the probe's argv: `podman create` carries `--init`, and a create failure that names the container-init helper gets its own hint. The scope and cap assertions are unchanged.

ADR-0057 is unchanged in substance: the login and CLI containers get the same flag through the shared renderer.

## Considered options

- **Disabling git's auto-maintenance** — `maintenance.auto=false` in each test suite, or injected into every container's git environment. Rejected as the fix: it stops one source of orphans and leaves the rest, and it would change git's behaviour inside the agent's worktree to hide a missing reaper. A repository may still disable it in its own tests for hermeticity, as this one does.
- **Raising `container_pids`.** Rejected: the leak is unbounded, so a higher cap only delays the failure.
- **Shipping an init in the agent-tools image** (`tini` under `/opt/lab`) and prefixing the provider argv with it. Rejected: it does what `--init` does with more moving parts, adds a binary to two images and a release, and leaves a container without the tools mount uncovered.
- **Making the provider CLI reap** or setting a child subreaper from a wrapper script. Rejected: lab does not own the provider CLIs, and a wrapper is a hand-rolled init.
- **`--init` only in the run shape.** Rejected: it would split the one renderer for no gain.

## Consequences

- **Zombies no longer accumulate.** An orphan that exits is reaped by PID 1 at once, so a long run's process count tracks what is alive.
- **The provider CLI is PID 2**, a child of the helper. `ps` inside the container shows one extra process.
- **A new host requirement, already met by the module.** Podman must be able to find a static container-init helper. Preflight checks it; a hand-assembled host installs `catatonit` or sets `engine.init_path` in `containers.conf`.
- **Containers started before the deploy keep the old behaviour.** They have no init and keep accumulating zombies until they are stopped; stop and resume such a run to move it onto a new container. A container already at its cap can be given headroom from the host by raising `pids.max` on its `libpod-<id>.scope` cgroup and on the `container` cgroup beneath it; the existing zombies stay until the container exits.
- **Where it lands.** `podmanx.RunArgv` and its three goldens; the provider-CLI and login goldens; the spawn probe and its fixtures in `internal/podmanx`; the gated real-podman test, which now orphans a process and asserts the pane command is not PID 1 and no zombie remains.
- **Docs.** `docs/ops.md`'s Container runner section names the flag, the host requirement and the stuck-container procedure. No `CONTEXT.md` term changes.
