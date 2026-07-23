#!/usr/bin/env bash
# build.sh <claude|codex> — build one agent-tools OCI image locally with podman.
#
# The workflow calls this as `containers/agent-tools/build.sh claude` from the
# repo root, but it is cwd-independent: it resolves the repo root from its own
# path so it works from anywhere. Versions and checksums come from versions.env
# (the single source of truth) and are passed as --build-arg, so a missing pin
# fails the build rather than silently fetching something else.
#
# The build context is the REPO ROOT, not this directory: the image's labctl
# stage COPYs the Go working tree and compiles labctl from it. .containerignore
# at the repo root keeps that context lean (drops .git, build outputs, the SPA
# node_modules/dist that labctl's untagged build never needs).
set -euo pipefail

# Resolve the repo root from this script's own location, then run everything
# relative to it so the build context and file paths are stable regardless of cwd.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

# shellcheck source=containers/agent-tools/versions.env
. containers/agent-tools/versions.env

PODMAN="${PODMAN:-podman}"

ARTIFACTS_DIR="containers/agent-tools/artifacts"

# fetch_artifact <dest> <url> <sha256> — reuse dest when it already matches the
# pinned sha256, else (re)download and verify. The fetch lives HERE, not in a
# Containerfile RUN step: a context file can be cached across CI runs
# (actions/cache keyed on versions.env) and reused by local rebuilds, where an
# in-build fetch re-downloads on every build — repeated pulls of the claude
# binary tripped per-IP throttling on downloads.claude.ai (CI runs 242–248).
# The Containerfiles re-verify the sha256 in-stage, so artifact provenance
# stays pinned to versions.env either way. Slow-transfer aborts + retries
# because a degraded CDN path otherwise rides a crawling connection forever.
fetch_artifact() {
  dest="$1"; url="$2"; sha="$3"
  if [ -f "${dest}" ] && echo "${sha}  ${dest}" | sha256sum -c - >/dev/null 2>&1; then
    echo "artifact cached: ${dest}"
    return 0
  fi
  echo "fetching: ${url}"
  curl -fsSL --create-dirs \
    --connect-timeout 15 --speed-limit 65536 --speed-time 60 \
    --retry 6 --retry-delay 2 --retry-all-errors \
    -o "${dest}.tmp" "${url}"
  echo "${sha}  ${dest}.tmp" | sha256sum -c -
  mv "${dest}.tmp" "${dest}"
}

provider="${1:-}"
case "${provider}" in
  claude)
    version="${CLAUDE_CODE_VERSION}"
    build_args=(
      --build-arg "CLAUDE_CODE_VERSION=${CLAUDE_CODE_VERSION}"
      --build-arg "CLAUDE_CODE_SHA256_X64_MUSL=${CLAUDE_CODE_SHA256_X64_MUSL}"
    )
    fetch_artifact \
      "${ARTIFACTS_DIR}/claude-${CLAUDE_CODE_VERSION}-linux-x64-musl" \
      "https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION}/linux-x64-musl/claude" \
      "${CLAUDE_CODE_SHA256_X64_MUSL}"
    ;;
  codex)
    version="${CODEX_VERSION}"
    build_args=(
      --build-arg "CODEX_VERSION=${CODEX_VERSION}"
      --build-arg "CODEX_SHA256_X64_MUSL_TGZ=${CODEX_SHA256_X64_MUSL_TGZ}"
    )
    fetch_artifact \
      "${ARTIFACTS_DIR}/codex-${CODEX_VERSION}-x86_64-linux-musl.tar.gz" \
      "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-x86_64-unknown-linux-musl.tar.gz" \
      "${CODEX_SHA256_X64_MUSL_TGZ}"
    ;;
  *)
    echo "usage: $0 <claude|codex>" >&2
    exit 2
    ;;
esac

image_ref="localhost/agent-tools:${provider}-${version}"

# Build context is the repo root (final positional `.`); labctl is compiled from
# the working tree in the image's labctl stage.
"${PODMAN}" build \
  -f "containers/agent-tools/Containerfile.${provider}" \
  "${build_args[@]}" \
  -t "${image_ref}" \
  .

# Last stdout line: the image ref this build produced (smoke-test.sh reads it).
echo "${image_ref}"
