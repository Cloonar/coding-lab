#!/usr/bin/env bash
# build.sh — build the full dev image locally with podman (ADR-0069).
#
# The workflow calls this as `containers/dev-image/build.sh` from the repo
# root, but it is cwd-independent: it resolves its own directory, which is
# also the whole build context (unlike the agent-tools images, nothing here
# is compiled from the repo's Go tree). Every pin comes from versions.env and
# is passed as --build-arg, so a missing pin fails the build.
#
# Expect a long first build and an image of several gigabytes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "${SCRIPT_DIR}"

# shellcheck source=./versions.env
. ./versions.env

PODMAN="${PODMAN:-podman}"

image_ref="localhost/dev-image:full"

"${PODMAN}" build \
  -f Containerfile \
  --build-arg "DEBIAN_RELEASE=${DEBIAN_RELEASE}" \
  --build-arg "GO_VERSION=${GO_VERSION}" \
  --build-arg "GOLANGCI_LINT_VERSION=${GOLANGCI_LINT_VERSION}" \
  --build-arg "NODE_MAJOR=${NODE_MAJOR}" \
  --build-arg "PHP_VERSIONS=${PHP_VERSIONS}" \
  --build-arg "PHP_DEFAULT=${PHP_DEFAULT}" \
  -t "${image_ref}" \
  .

# Last stdout line: the image ref this build produced (smoke-test.sh uses it).
echo "${image_ref}"
