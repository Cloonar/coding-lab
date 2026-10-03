#!/usr/bin/env bash
# smoke-test.sh — prove the full dev image works the way lab runs it.
#
# The image is only useful if its tools work under the container runner's
# conditions, which differ from a plain `podman run` in three ways this test
# reproduces (ADR-0069, podmanx.RunArgv):
#
#   * --userns=keep-id: the session is an unprivileged user with the caller's
#     uid, not root.
#   * HOME is an empty directory bind-mounted at /home/agent.
#   * PATH is lab's fixed container PATH, not the image's.
#
# Inside that container smoke-inner.sh checks every toolchain, takes real
# browser screenshots, exercises the apt-only sudo rule, and starts each
# database server. Requires the image build.sh produces.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "${SCRIPT_DIR}"

# shellcheck source=./versions.env
. ./versions.env

PODMAN="${PODMAN:-podman}"

image_ref="localhost/dev-image:full"

# The container-side PATH lab passes to every container session — keep in
# step with podmanx.PATH (internal/podmanx/podmanx.go).
lab_path="/opt/lab/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# SMOKE_EXTRA_RUN_ARGS: optional extra `podman run` arguments, split on
# whitespace. An escape hatch for hand-running on an unusual podman host —
# CI leaves it unset.
extra_run_args=()
if [ -n "${SMOKE_EXTRA_RUN_ARGS:-}" ]; then
  read -r -a extra_run_args <<<"${SMOKE_EXTRA_RUN_ARGS}"
fi

if ! "${PODMAN}" image exists "${image_ref}"; then
  echo "error: image ${image_ref} not found locally — run build.sh first:" >&2
  echo "         containers/dev-image/build.sh" >&2
  exit 1
fi

home_dir="$(mktemp -d)"
trap 'rm -rf "${home_dir}"' EXIT

echo ">>> smoke: ${image_ref} as uid $(id -u), empty HOME, lab's PATH"
# --network=host + --cgroups=disabled: the same choice as the agent-tools
# smoke test — the checks need neither a private network namespace nor
# resource limits, and skipping both keeps this runnable inside a nested CI
# container. The database checks use high ports so host networking cannot
# collide with a service on the build machine.
"${PODMAN}" run --rm \
  --userns=keep-id \
  --network=host --cgroups=disabled \
  "${extra_run_args[@]}" \
  -v "${home_dir}:/home/agent" \
  -v "${SCRIPT_DIR}/smoke-inner.sh:/smoke-inner.sh:ro" \
  -w /home/agent \
  --env HOME=/home/agent \
  --env "PATH=${lab_path}" \
  --env "PHP_VERSIONS=${PHP_VERSIONS}" \
  --env "PHP_DEFAULT=${PHP_DEFAULT}" \
  --env "GO_VERSION=${GO_VERSION}" \
  --env "NODE_MAJOR=${NODE_MAJOR}" \
  "${image_ref}" \
  sh /smoke-inner.sh

echo "ALL GREEN: ${image_ref}"
