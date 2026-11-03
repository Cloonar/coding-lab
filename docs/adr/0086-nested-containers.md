# Nested containers: a per-repo opt-in run shape under the container runner lets a session run rootless podman containers inside its own run — ddev, `docker compose`, buildx — with the stack baked into `dev-image:full`, a lab-shipped entry program as PID 1, and every nested container bounded by the run's cgroup

ADR-0069 put database servers into the full dev image on one premise: "A session cannot run a nested container, so the usual answer — a service container, `ddev`, `docker compose` — is unavailable." For a repo whose development setup *is* a container stack, that leaves the agent working around the project instead of in it. A ddev project, a `docker compose` test environment and a `docker buildx build` all fail the same way, and the baked servers cover only the database part of what they provide.

The premise did not survive a probe. On 2026-11-02 the maintainer ran a lab-shaped run on the dev host with a handful of extra flags on the outer `podman run` and the stack below installed into `full` by hand. ddev 1.25.5 built its web image with buildx and started web, db and router as nested rootless podman containers inside the run. The router served PHP 8.4 with HTTP 200, and MariaDB 11.8 answered queries. The outer run's memory and pid caps bounded the nested containers, because they are children of the run's cgroup ([ADR-0060](0060-containers-as-user-manager-scopes.md)). What the probe had to add, and why each piece is needed, is the substance of this ADR.

The design touches the image, a new entry program and lab itself, so it lands as three issues. This ADR lands with the first (issue #113, the image). The image's new tools are inert until the third starts runs in the nested shape.

The pins, decided (with the maintainer, 2026-11-02):

- **A per-repo Nested containers knob, NULL inheriting a global default seeded off, meaningful only under the container runner.** Every flag a nested run adds widens the run's surface, so only the repos that need it pay for it. The knob follows the inheritance shape of the Runner and Dev image settings ([ADR-0071](0071-global-runner-and-dev-image-defaults.md)): a repo pins on or off, or inherits the global default, which is off on a fresh deployment. A repo whose effective runner is `host` ignores it. It is not on for every container run, and it is not derived from a label on the image (both rejected below).

- **A writable cgroup through podman's systemd mode, and a lab-shipped entry program as PID 1.** A nested podman needs a cgroup it may write, and `--systemd=always` gives the run one. The outer container starts as container root running `labctl container-entry` (issue 2). The entry program writes the subuid and subgid line, chowns the run's cgroup to the lab uid and drops to the lab uid irreversibly. It then spawns the provider, forwards signals to it and reaps orphans. Podman refuses `--init` together with `--systemd=always`, so in a nested run the reaper duty of [ADR-0074](0074-container-init-reaps-orphans.md) moves from catatonit into the entry program. A shell script in the argv and a chown from the host through `podman exec` were both rejected (below).

- **The podman API service and the healthcheck loop belong to a helper in the image, not to lab's Go code.** ADR-0069's stance holds: nothing in lab's Go code knows the full image exists. The helper is `start-nested-podman` in `rootfs/usr/local/bin`, beside the `start-*` database helpers (its contract is below). Lab's part ends at the run's shape: the argv, the entry program and the mounts.

- **The stack lives in `full`, not in a second flavor.** ADR-0069 rejected one image per language for the cost of another pipeline and another ref to explain, and a nested flavor would cost the same. The knob and the image have to agree either way: a nested run in an image without the stack has nothing to run, and the stack in an image whose run is not nested is inert.

- **The entry program writes the one subuid line at run start.** It is a per-run fact (the uid is not known when the image is built) and the entry program is the code path that holds the privilege to write it, as container root before the drop. The image therefore ships no `/etc/subuid` or `/etc/subgid` (image facts, below).

- **Lab embeds containers/common's seccomp profile and patches it in code to allow `sethostname` and `setdomainname` unconditionally.** The default profile allows the two syscalls only with CAP_SYS_ADMIN, and the run holds no CAP_SYS_ADMIN, so every nested `crun` fails with `sethostname: Operation not permitted`. The kernel still requires CAP_SYS_ADMIN over the UTS namespace, so the change grants nothing outside namespaces the run itself created. Lab writes the patched profile to a file and passes it with `--security-opt seccomp=`. Importing containers/common and shipping the file from the Nix module were rejected (below).

- **Preflight gains static tun-device checks and a nested spawn probe.** The probe runs the entry program's self-test in a nested-shaped container. Both are gated on nested being enabled for any repo or as the global default, so a host on which nobody uses nested containers is never refused for one.

- **A shared, read-only image store, populated by lab, best effort.** Lab pulls the refs in a global `nested_image_cache` list into a store it mounts read-only into nested runs at its host-identical path, and names that path to the image in the environment variable `LAB_SHARED_STORE`. The helper lists it under `additionalimagestores`, so a nested `ddev start` finds ddev's images without pulling them. A miss never refuses a spawn: the nested podman pulls what the store lacks into the run's own storage. A per-run store only (with a capped buildx cache), an operator-managed directory and mounting lab's own image store were rejected (below).

- **Previews: `labctl expose web 8080` plus `additional_fqdns`, no Host rewriting in the tunnel.** ddev's router routes by `Host`, and a preview arrives as `web.<preview-domain>` ([ADR-0084](0084-previews-reverse-tunnel.md), [ADR-0085](0085-preview-tls-front-door.md)). The worktree notes teach the agent to expose the router's HTTP port and to add the preview hostname to `additional_fqdns` in ddev's gitignored `.ddev/config.local.yaml`, so the committed ddev config stays untouched. The tunnel keeps forwarding the browser's `Host` unchanged.

- **The concept is called Nested containers.** It is the name in the glossary, the settings and the notes. "ddev" names one consumer of the feature, not the feature; "Docker-in-Docker" and "dind" name a different technique (a Docker daemon in a privileged container) and imply a Docker daemon this design does not have; "privileged" is exactly what the outer run is not.

- **Run panes only, manual and AFK.** A provider login pane and the non-interactive CLI pokes (auth status, logout, the credential-refresh poke, [ADR-0057](0057-containerized-provider-login.md)) keep today's argv. They never need a nested container, and they run without a repo, so a per-repo knob has nothing to say about them.

- **The outer argv of a nested run** (implemented in issue 3): `--security-opt unmask=ALL`, `--security-opt seccomp=<lab-generated file>`, `--device /dev/net/tun`, `-v /sys:/sys`, `--systemd=always`, an explicit `--stop-signal`, no `--init`, no `/dev/fuse`, started as container root with the entry program. Each was measured in the probe:
  - `unmask=ALL`: the masked `/proc` and `/sys` paths of the default argv make the kernel refuse a fresh procfs or sysfs mount in any child namespace, so no nested container could start.
  - `-v /sys:/sys`: the outer read-only sysfs is flag-locked, so buildx's privileged buildkit container cannot mount sysfs read-write unless the outer `/sys` is a read-write bind.
  - `--device /dev/net/tun`: pasta, the nested containers' network, needs it.
  - No `/dev/fuse`: native overlay inside a user namespace works on the pinned kernel, so FUSE stays closed.
  - `--systemd=always`, no `--init`, an explicit `--stop-signal`: the writable cgroup above, the init podman refuses with it, and a stop signal the entry program forwards instead of the one systemd mode would pick.

- **Three issues, in order: image and ADR, entry program, lab feature.** Issue 1 (#113) is the image and this ADR. Issue 2 is `labctl container-entry`, the entry program. Issue 3 is the lab feature: the knob, the argv, the seccomp file, preflight, the shared store and the worktree notes. Each lands on its own, and nothing is reachable from a run until the third does.

The image, decided with it (issue #113):

- **The Debian packages are `podman uidmap passt crun conmon netavark aardvark-dns catatonit nftables`, each named explicitly.** The image installs with `--no-install-recommends`, and each of `nftables`, `catatonit` and `aardvark-dns` was missing in the probe or is needed by netavark or pasta. `nftables` is required: without it netavark fails with `unable to execute nft`. The Debian packages float with the monthly rebuild, as ADR-0069 decided for every package.

- **`newuidmap` and `newgidmap` carry file capabilities, not setuid** (`chmod u-s`, `setcap cap_setuid+ep /usr/bin/newuidmap`, `setcap cap_setgid+ep /usr/bin/newgidmap`). Debian's setuid-root binaries fail inside the run with `write to uid_map failed: Operation not permitted`: the kernel's `map_write` demands CAP_SYS_ADMIN over the child namespace from a root writer, and container root has none. With file capabilities the binary keeps the caller's euid, the namespace-owner rule applies, and the mapping succeeds with no extra capability. This is Fedora's layout and what `quay.io/podman/stable` ships. A v2 capability xattr written at build time is honored in every nested namespace.

- **`/etc/containers/containers.conf` sets `[containers] cgroups = "enabled"`, `[engine] cgroup_manager = "cgroupfs"` and `events_logger = "file"`.** There is no systemd inside a run, so cgroupfs is the only manager there is. Cgroups must stay enabled: buildx's buildkit container asks for a cgroup parent, and podman refuses that with `cannot set cgroup parent if not creating cgroups` when they are disabled.

- **`/etc/containers/registries.conf` sets `unqualified-search-registries = ["docker.io"]`.** Short image names otherwise fail to resolve, and ddev and most compose files use them.

- **The Docker CLI, with buildx 0.17 or newer, pinned in `versions.env`.** ddev 1.25.1 and later refuse an older buildx (`compose build requires buildx 0.17.0 or later`), and Debian trixie ships 0.13. The CLI is Debian's `docker-cli`, with Debian's `docker-compose` (Compose v2) for the `docker compose` plugin; buildx is the upstream release binary. The CLI talks to podman's API socket; there is no Docker daemon.

- **ddev, pinned in `versions.env`.** The release asset is `ddev_linux-amd64.<tag>.tar.gz` under `releases/download/<tag>/`; there is no unversioned `latest/download` asset. `ddev version` must not run at build time, because ddev refuses to run as root.

- **ddev's usage-statistics prompt is off** (`DDEV_NO_INSTRUMENTATION=true` in the image). ddev asks on the first start after an upgrade, and every run's HOME is fresh, so it would ask in every run.

- **mkcert.** ddev warns without it. HTTPS inside the run is not needed, since lab's preview front door terminates TLS (ADR-0085), but the warning is noise in every `ddev start`.

- **No `/etc/subuid` or `/etc/subgid` in the image.** The uid is unknown at build time, and podman sums every line that matches the user by name *or* by numeric uid into one map. Two lines for one user therefore produce overlapping extents and `newuidmap: write to uid_map failed: Invalid argument`. The one correct line is the entry program's (above).

- **`start-nested-podman` prepares one run, and does nothing else.** Run as the session user inside a nested run, it is idempotent and:
  - creates the runtime dir, `${XDG_RUNTIME_DIR:-/tmp/lab-nested/run}`, which podman needs and the outer argv may or may not set;
  - writes `~/.config/containers/storage.conf` with the overlay driver and, when `LAB_SHARED_STORE` names an existing directory, that directory under `additionalimagestores`;
  - starts `podman system service --time=0 unix://$XDG_RUNTIME_DIR/podman.sock` detached from the terminal, unless it is already listening;
  - starts a detached healthcheck runner that runs `podman healthcheck run` every second for every running container that defines a healthcheck. Podman schedules healthchecks only through systemd timers, and there is no systemd in the run, so without this loop every container stays `starting` forever and ddev's "waiting for containers" times out after 60 s. The loop survives a Ctrl-C in the agent's shell: the probe once lost it to a job-control stop;
  - writes ddev's global config: `router_http_port` 8080 and `router_https_port` 8443, because an unprivileged uid cannot bind 80 or 443, and the system buildx (`ddev config global --docker-buildx-version=system`);
  - prints `export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman.sock` as its last stdout line, so an agent can `eval` it. An `export XDG_RUNTIME_DIR=…` line precedes it, so a plain `podman` in the agent's shell shares the service's runtime dir.

  It does not start containers and does not require ddev: `docker compose` and `buildx` users get the same socket and the same loop.

- **`LAB_SHARED_STORE` is the contract between lab and the image.** Lab (issue 3) sets it to the host-identical path at which it mounts the shared store; the helper reads it and nothing else does. Unset or naming a missing directory, the helper writes a storage.conf without an additional store.

- **The smoke test proves what it can without lab's side.** Under the existing keep-id conditions it checks that every new binary resolves on lab's container PATH, the two file capabilities and the absence of the setuid bit, the absence of `/etc/subuid` and `/etc/subgid`, `nft --version`, buildx at 0.17 or newer, `ddev version` as the session user, `podman info` reporting `cgroupfs` and `netavark`, and `start-nested-podman` run twice. Starting a nested container needs the outer flags, so the end-to-end proof is the maintainer's on the dev host once issue 3 lands.

## Status

Accepted 2026-11-03. Decided with the maintainer in a grill session on 2026-11-02; issue #113's agent brief is the contract. Lands with issue #113 (the image, the helper and this ADR); the entry program (issue 2) and the lab feature (issue 3) implement the remaining pins.

Implemented 2026-11-03 by issue #114 (the entry program, `labctl container-entry`) and issue #115, the lab feature, which implements the remaining pins: the per-repo knob `repos.nested_containers` with the global `nested_containers_default`, the nested run argv, the seccomp file at `<state>/containers/nested-seccomp.json`, the `nested-tun` and `nested-probe` preflight checks, the shared store at `<state>/containers/shared-store` filled from `nested_image_cache`, and the worktree notes' `## Nested containers` section.

- **[ADR-0069](0069-opt-in-full-dev-image.md):** amended. Its sentence "A session cannot run a nested container, so the usual answer — a service container, `ddev`, `docker compose` — is unavailable" is superseded: a session in a nested run can. The baked database servers and their `start-*` helpers stay. The stack is additive, and a repo that is not nested still has only them.
- **[ADR-0074](0074-container-init-reaps-orphans.md):** unchanged for every run that is not nested. In a nested run podman refuses `--init`, and the entry program reaps instead.
- **[ADR-0052](0052-container-runner.md):** the default argv is unchanged; a nested run adds the flags above. The run still reaches lab only through the agent socket.
- **[ADR-0060](0060-containers-as-user-manager-scopes.md):** unchanged. A nested run is still one `libpod-<id>.scope` with the run's `MemoryMax` and `TasksMax`, and its nested containers live beneath it.
- **[ADR-0065](0065-tagged-release-static-binaries.md):** unchanged. The seccomp profile is embedded as data, so lab stays a static binary.
- **[ADR-0084](0084-previews-reverse-tunnel.md):** unchanged. A nested run's preview is an ordinary exposure of the ddev router's port.

## Considered options

- **Nested containers always on for every container run.** Rejected: each flag widens the run's surface, and most repos never start a container.
- **Deriving nested mode from a label on the image.** Rejected: preflight cannot inspect image contents, and the image's author would decide the shape of lab's sandbox.
- **A shell script in the argv to prepare the run before the provider starts.** Rejected for an entry program lab ships and tests: the argv would carry the logic as an untested string, and something still has to be PID 1 and reap once `--init` is gone.
- **Chowning the run's cgroup from the host with `podman exec`.** Rejected: it races the provider, which is already running when the exec lands.
- **A second image flavor for nested containers.** Rejected for the pipeline-and-ref cost ADR-0069 named.
- **Running the podman API service and the healthchecks from lab's Go code.** Rejected: lab would have to know what the image contains, against ADR-0069.
- **Baking `/etc/subuid` and `/etc/subgid` into the image.** Rejected: the uid is not known at build time, and a second line for the same user overlaps the first.
- **Debian's setuid `newuidmap`/`newgidmap`.** Rejected: they fail inside the run, because a root writer needs CAP_SYS_ADMIN over the child namespace.
- **Importing containers/common for its seccomp profile.** Rejected: it drags libseccomp bindings in, against ADR-0065's static binaries.
- **Shipping the seccomp file from the Nix module.** Rejected: non-NixOS hosts would have no profile, and the file would drift from the lab version that renders the argv.
- **Per-run image storage only, with a capped buildx cache.** Rejected as the only store: every run would pull ddev's ~2 GB again, and the shared store removes that for the common images.
- **An operator-managed image directory.** Rejected: one more thing an operator must fill and keep current by hand, when lab already knows how to pull a list of refs.
- **Mounting lab's own image store into nested runs.** Rejected: it would expose every repo's private dev image to every nested run.
- **Rewriting `Host` in the preview tunnel for ddev's router.** Rejected: `additional_fqdns` in the gitignored local config does it in the project, and the tunnel stays a plain forwarder.
- **`/dev/fuse` and fuse-overlayfs.** Rejected: native overlay works in a user namespace on the pinned kernel, so FUSE stays closed.
- **Disabling cgroups in `containers.conf`.** Rejected: buildkit's container asks for a cgroup parent, which podman refuses with cgroups disabled.
- **Debian's buildx.** Rejected: trixie's 0.13 is older than the 0.17 ddev requires.

## Consequences

- **A repo that opts in can run its own container stack.** `ddev start`, `docker compose up` and `docker buildx build` work in the run as they do on a laptop, once issue 3 lands. Until then the image's new tools are inert: the default argv does not let a nested container start.
- **No new host-level privilege.** The outer run is still rootless podman under the lab service user, with no CAP_SYS_ADMIN on the host. Every flag above widens what the run may do inside namespaces it owns, and the seccomp change grants nothing outside namespaces the run created.
- **A larger kernel attack surface.** A nested run creates user, mount, network and UTS namespaces, mounts procfs and sysfs in them, and starts privileged nested containers (buildx's buildkit). A kernel bug reachable from those paths is reachable from the agent. The per-repo knob, off by default, is the mitigation.
- **Per-run disk of roughly 3–4 GB**, bounded by the instance cap, until the shared cache is populated.
- **Root-owned files in the worktree are possible.** A nested container that writes into a bind-mounted worktree as its own root leaves files owned by a subordinate uid host-side, which lab can neither diff nor remove. It is the same accident class as ADR-0069's sudo rule.
- **Unpinned third-party images run at runtime.** ddev's images and whatever a compose file names are pulled when the agent runs them, not reviewed or pinned by lab: the same trust class as `npm install`.
- **The first `ddev start` pulls ~2 GB and takes about two minutes** without the shared cache, on the run's budget clock.
- **buildx's default GC policy keeps up to 9 GiB of build cache** before it collects, which counts against the run's disk.
- **The image grows** by the stack and its dependencies, and its build gains a download of the pinned ddev release from GitHub.
- **A session still cannot discover its image.** `start-nested-podman` is useful only if the agent knows to run it: issue 3's worktree notes say so in nested runs, and `docs/ops.md` documents it for repos that write their own instructions.
- **Another dev image can join in** by carrying the same stack and an equivalent helper; lab's side does not depend on the full image.
