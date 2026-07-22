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
  "${PODMAN}" run --rm \
    --mount "type=image,source=${image_ref},destination=/opt/lab" \
    "${base}" \
    sh -c "export PATH=/opt/lab/bin:\$PATH; ${cli} --version && labctl --help >/dev/null && echo \"OK (${cli} + labctl on \$(uname -m))\""
done

echo "ALL GREEN: ${provider} (${cli} + labctl) injected cleanly on both glibc and musl bases"
