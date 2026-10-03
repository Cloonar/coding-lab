# An opt-in full dev image: lab publishes one batteries-included dev image beside the agent-tools images — toolchains, a browser, database servers, apt-only sudo — that a repo or a host selects explicitly, while the module's default dev image stays the stock scm image

ADR-0052 gave container sessions an operator-owned userland and ADR-0056 made the module ship a working default for it: a digest-pinned `buildpack-deps:stable-scm`, which is git, an ssh client and curl. ADR-0056 considered a Cloonar-built image for that default and rejected it — owning a Containerfile and a publish pipeline for a *fallback* was not worth it — with a named revisit condition: "only if agents routinely need a richer userland the stock image lacks."

That condition is met, and not for the reason ADR-0056 guessed (`rg`/`jq`). A container session is structurally unable to help itself: it runs as an unprivileged user (`--userns=keep-id`), so it cannot install a package; its HOME is a fresh directory every run, so nothing it downloads survives; and it has no Nix, which is how a host-runner session reaches a per-project toolchain (ADR-0033). A session in the default image therefore has no Go, no Node, no Python, no PHP, no browser and no database, and no way to get them. The only answer was for the operator to build, publish and re-pin a dev image per repo — work that is identical across most repos and that nobody wants to do for the first one.

This ADR adds the image that removes that work, without touching the default.

The pins, decided (with the maintainer, 2026-10-03):

- **Opt-in, never a default.** `ghcr.io/cloonar/dev-image:full` is selected per repo through the Dev image field, or host-wide by setting `container.defaultImage` to it. The module default stays ADR-0056's stock image, and no deploy probe depends on this image: ADR-0056's rejection of a Cloonar-built *default* stands. What changes is ADR-0053's aside that the dev image is "never one lab builds" — lab now builds one, and a deployment may ignore it entirely.

- **It is an ordinary dev image.** Nothing in lab's Go code knows it exists. It is resolved and digest-pinned on save, pulled if missing before the claim, and carries nothing lab-specific, exactly as ADR-0053 requires of any dev image; `/opt/lab` stays reserved for the agent-tools mount. Built on `buildpack-deps` (the full variant of the default's own family), so the dev-image contract — shell, coreutils, git, ssh client — holds by construction.

- **What it carries.** Go with `gopls` and `golangci-lint`; Node LTS with npm, pnpm, yarn and corepack; Python 3 with pip, venv, pipx and uv; PHP 8.2, 8.3 and 8.4 side by side with the common extensions and Composer; Chromium plus Playwright with its own browser build, and fonts; PostgreSQL, MariaDB and Redis servers and clients; ripgrep, fd, jq, yq, shellcheck and similar; ImageMagick, GraphicsMagick, Ghostscript, poppler. Deliberately not Rust, Java or .NET until a repo needs them, and not Nix, which needs a store and a daemon an image run as an arbitrary uid cannot provide cleanly.

- **The container runner dictates how it is assembled.** Three facts of `podmanx.RunArgv` shape the Containerfile, and its header records them. *PATH is lab's*: the runner passes one fixed PATH, so an image `ENV PATH` never reaches a session and every tool must resolve through `/usr/local/bin` or `/usr/bin` — Go, npm and the PostgreSQL server binaries are symlinked there. *HOME is mounted over*: everything installs system-wide, and the Playwright browser lives at `/opt/playwright` (world-writable, so a project pinning another Playwright version can add its build). *Sessions are unprivileged*: the packaged database data directories are root-owned and useless, so the main PostgreSQL cluster is never created and three helpers — `start-postgres`, `start-mariadb`, `start-redis` — initialize and start a throwaway server per session under `/tmp/devdb`.

- **Database servers are baked in because nothing else can supply them.** A session cannot run a nested container, so the usual answer — a service container, `ddev`, `docker compose` — is unavailable. A test suite that needs a real database gets one only if the server binaries are in the image.

- **Passwordless sudo, for the package manager only.** `sudo` is allowed without a password for `apt-get`, `apt` and `dpkg`, so a session can install the long tail this image lacks. This costs the host nothing: under rootless `--userns=keep-id`, root in the container is a subordinate uid on the host, gains no access the session's own uid did not already have to the writable mounts, cannot remount the read-only ones (no `CAP_SYS_ADMIN`), stays inside the same memory and pid caps, and leaves nothing behind in a `--rm` container. The restriction is a guard rail against an accident, not a security boundary — a package manager can be made to run arbitrary commands. The accident is file ownership: ADR-0052 chose keep-id so that everything a session writes into the worktree and HOME is owned by the lab service user host-side "with no chown dance", and a file written there as root is instead owned by a subordinate uid lab can neither diff nor remove. A stray `sudo npm install` in the worktree would do exactly that; `sudo apt-get install` cannot.

- **Chromium runs with its sandbox off and `/dev/shm` unused.** Chromium's sandbox needs to create user namespaces, which the container's seccomp profile denies an unprivileged process, and the runner sets no `--shm-size`, leaving podman's 64 MB default. `/etc/chromium.d/lab-container` adds `--no-sandbox --disable-dev-shm-usage --disable-gpu` to every launch; the container is the sandbox. Puppeteer and Karma are pointed at the system Chromium by environment, because a Chrome they downloaded into HOME would start without those flags and fail.

- **A short pin list; everything else floats, and a monthly rebuild delivers it.** `containers/dev-image/versions.env` pins the Debian release, the Go version, the Node major, the PHP versions and default, and golangci-lint (matched to `ci.yml`). Debian packages, the browser, and the `@latest` tools move with each build. `.github/workflows/dev-image.yml` rebuilds on a change to `containers/dev-image/**`, on the first of each month, and on dispatch. This is safe to float because of pin-on-save: no repo's running image changes until its ref is re-saved.

- **Tags `full` and `full-<YYYYMMDD>`, published once.** The dated tag is pushed and the moving tag is then pointed at the same manifest registry-side, so the two always name one digest. The flavor is in the tag, not the repository name, which leaves room for a second flavor without a second package.

- **PHP comes from `packages.sury.org`.** Debian ships one PHP version per release; side-by-side versions need this archive, maintained by Debian's own PHP maintainer. Its keyring is fetched over HTTPS as the archive's README prescribes. That is a third-party supply-chain dependency this image accepts and the default image does not have.

- **The smoke test reproduces the runner, and is the proof.** `smoke-test.sh` runs the image under `--userns=keep-id` with an empty HOME bind-mounted at `/home/agent` and lab's exact container PATH, then checks every tool resolves, builds and runs a Go program, checks each PHP version's extensions and the default, proves sudo works for apt and is refused otherwise, takes a screenshot with both Chromium and Playwright, and starts each database and connects to it from PHP. A plain `podman run` as root would pass with half of this broken.

- **The agent-tools workflow's path gate narrows to `containers/agent-tools/**`.** It was `containers/**` while that directory held nothing else; left alone, every change to the dev image would re-release the agent-tools images under unchanged tags.

## Status

Accepted. Settled with the maintainer on 2026-10-03. Builds on ADR-0052 (the container runner whose argv shapes the image), ADR-0053 (per-repo dev images, pin-on-save — the mechanism this image is selected through) and ADR-0051/ADR-0064 (the ghcr publish pattern it copies). Amends ADR-0053 only in that lab now publishes a dev image of its own; the contract is unchanged. Takes up ADR-0056's named revisit condition for a Cloonar-built image while leaving that ADR's decision intact: the module default is still the stock `buildpack-deps:stable-scm`.

## Considered options

- **Making it the module default.** Rejected: it is several gigabytes, so every fresh deployment's first spawn and every containerized provider login would wait on that pull, the deploy probe would gain a dependency on this image's publish, and deployments that want a small audited userland would have to opt out. A default should be the smallest thing that works; this is the opposite, on purpose.

- **One image per language.** Rejected for now: real repos mix them (this one is Go, Node and Python), the browser and the databases are wanted across all of them, and each additional image is another pipeline and another ref to explain. The `full` tag leaves room for a slimmer flavor if the size becomes the complaint.

- **Running sessions as root so they can install what they need.** Rejected: it would abandon ADR-0052's keep-id ownership invariant for every file a session writes, to solve a problem a prebuilt image solves better — a per-run install repeats on every spawn and spends the run's budget clock on package downloads.

- **Unrestricted passwordless sudo.** Rejected: it adds nothing the apt rule does not already allow a determined agent, and it removes the one thing the restriction is for — keeping an ordinary `sudo <build command>` from leaving root-owned files in the worktree.

- **Putting Nix in the image.** Rejected: a usable Nix needs either a daemon or a store owned by the session's uid, and that uid is not known when the image is built. Repos whose checks need Nix stay on the host runner.

- **Playwright's browser only, or the system Chromium only.** Rejected in both directions: a plain `chromium` command with the container flags already applied is the shortest path to a screenshot and what Puppeteer and Karma can be pointed at, while Playwright only drives the browser build it was released with. Both are installed; the cost is disk.

- **Digest-pinning the base and builder images.** Rejected: the monthly rebuild exists to pick up security updates, which a pinned digest would freeze. Pin-on-save already makes each consumer's image a reviewed, stable fact.

## Consequences

- **A container repo can have toolchains, a browser and a database by saving one ref.** No per-repo Containerfile, registry or pipeline.
- **A session cannot discover the image it runs in.** The helpers and the versioned PHP binaries are only useful if the repo's agent instructions mention them; `docs/ops.md` says what to write.
- **Updating is an explicit re-save**, as for every dev image; a host-wide `defaultImage` is re-pinned by hand.
- **The first spawn on it is slow, and it counts toward `stateDir`.** Several gigabytes, pulled synchronously before the claim (ADR-0053).
- **One manual step on first publish**: the ghcr package must be made public, because lab resolves and pulls dev images anonymously. The publish job's last step fails with the click path until it is.
- **x86_64 only**, like the agent-tools images.
- **`go install`ed binaries are not on PATH**, because lab's PATH is fixed and `~/go/bin` is not in it. Extending the container PATH is a lab change, not an image one, and is not part of this ADR.
- **The scheduled build can break without a diff** — an upstream package rename, a rotated archive key. A failed scheduled run leaves the previous tags in place; nothing that is running depends on the rebuild succeeding.
