#!/usr/bin/env bash
# smoke-test.sh <claude|codex> — the injection smoke test for issue #203.
#
# It mounts the locally-built agent-tools image (from build.sh) read-only into
# two stock base images — debian:stable-slim (glibc) and alpine:latest (musl) —
# and proves the provider CLI and labctl both run from /opt/lab/bin on each.
# This is the whole point of the images: the agent surface travels into an
# operator-chosen container without touching the base's libc (claude via its
# bundled musl loader + baked PT_INTERP; codex static-pie; labctl static Go).
#
# cwd-independent: resolves the repo root from its own path and sources
# versions.env for the version tag, same as build.sh. Requires the image
# build.sh produces (localhost/agent-tools:<provider>-<version>); if it is
# missing it tells you to run build.sh first.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

# shellcheck source=containers/agent-tools/versions.env
. containers/agent-tools/versions.env

PODMAN="${PODMAN:-podman}"

provider="${1:-}"
case "${provider}" in
  claude) version="${CLAUDE_CODE_VERSION}"; cli="claude" ;;
  codex)  version="${CODEX_VERSION}";       cli="codex"  ;;
  *)
    echo "usage: $0 <claude|codex>" >&2
    exit 2
    ;;
esac

image_ref="localhost/agent-tools:${provider}-${version}"

# SMOKE_EXTRA_RUN_ARGS: optional extra `podman run` arguments, split on
# whitespace (so no arg may contain spaces). An escape hatch for hand-running
# the smoke test on an unusual podman host — nothing sets it: CI leaves it
# unset, and a real host needs it empty.
extra_run_args=()
if [ -n "${SMOKE_EXTRA_RUN_ARGS:-}" ]; then
  read -r -a extra_run_args <<<"${SMOKE_EXTRA_RUN_ARGS}"
fi

if ! "${PODMAN}" image exists "${image_ref}"; then
  echo "error: image ${image_ref} not found locally — run build.sh first:" >&2
  echo "         containers/agent-tools/build.sh ${provider}" >&2
  exit 1
fi

# Both a glibc base and a musl base, to prove the libc mechanism works on either.
bases="docker.io/library/debian:stable-slim docker.io/library/alpine:latest"

for base in ${bases}; do
  echo ">>> smoke: ${provider} injected into ${base}"
  # `--mount type=image` is read-only by default (the image is pure payload the
  # container must never mutate). The inner script runs in the BASE image's
  # shell: $PATH and $(uname -m) are intentionally left unexpanded here so they
  # evaluate inside the container.
  #
  # --network=host + --cgroups=disabled: the probe needs neither a network
  # namespace nor resource limits, and skipping both is what lets it run
  # inside CI's nested job container (no netavark privileges, no writable
  # cgroup tree there) — with no change to what the test proves.
  "${PODMAN}" run --rm \
    --network=host --cgroups=disabled \
    "${extra_run_args[@]}" \
    --mount "type=image,source=${image_ref},destination=/opt/lab" \
    "${base}" \
    sh -c "export PATH=/opt/lab/bin:\$PATH; ${cli} --version && labctl --help >/dev/null && labctl container-entry --help >/dev/null && echo \"OK (${cli} + labctl on \$(uname -m))\""

  # container-entry --self-test as a non-root user: the smoke environment has
  # no container root with a writable cgroup, so the setup must refuse — and
  # still speak the self-test's stdout contract (one `fail: setup: ...` line,
  # exit 1) that the preflight probe parses.
  echo ">>> smoke: labctl container-entry --self-test refuses non-root on ${base}"
  # shellcheck disable=SC2016 # the script is expanded by the container's shell
  "${PODMAN}" run --rm \
    --network=host --cgroups=disabled --user 65534:65534 \
    "${extra_run_args[@]}" \
    --mount "type=image,source=${image_ref},destination=/opt/lab" \
    "${base}" \
    sh -c 'out=$(/opt/lab/bin/labctl container-entry --self-test --uid 1000 --gid 1000 2>/dev/null); rc=$?
           case "${rc}:${out}" in
             "1:fail: setup: must run as uid 0"*) echo "OK (self-test refused non-root: ${out})" ;;
             *) echo "FAIL: self-test as non-root: exit ${rc}, stdout: ${out}" >&2; exit 1 ;;
           esac'
done

# SMOKE_NESTED_SELF_TEST=1: additionally run the real self-test in a
# nested-shaped container (ADR-0086's outer flags, started as container root
# under keep-id), on a host that can run one — CI's nested job container
# cannot, so nothing sets it there. seccomp=unconfined stands in for lab's
# patched profile, which only lab itself renders. Every line must be `ok: …`
# except newuidmap, which reports on the image (the stock base has no stack).
if [ "${SMOKE_NESTED_SELF_TEST:-}" = "1" ]; then
  base="docker.io/library/debian:stable-slim"
  echo ">>> smoke: labctl container-entry --self-test in a nested-shaped ${base}"
  "${PODMAN}" run --rm \
    --userns=keep-id --user 0 --systemd=always \
    --security-opt unmask=ALL --security-opt seccomp=unconfined \
    --device /dev/net/tun -v /sys:/sys \
    "${extra_run_args[@]}" \
    --mount "type=image,source=${image_ref},destination=/opt/lab" \
    "${base}" \
    /opt/lab/bin/labctl container-entry --self-test --uid "$(id -u)" --gid "$(id -g)"
fi

echo "ALL GREEN: ${provider} (${cli} + labctl) injected cleanly on both glibc and musl bases"
