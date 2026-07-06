#!/usr/bin/env bash
# Playwright webServer: build the SPA, embed it into a fresh `lab` binary
# (-tags ui) and run it on a throwaway state dir. The state dir (vault
# master key, DB, binary) lives OUTSIDE the repo tree via mktemp: inside
# web/e2e/ it would be swept into the world-readable nix store by any
# `nix flake check "path:$PWD"` on the tree. Playwright kills the process
# tree on teardown; the OS tmp reaper sweeps the dir.
set -euo pipefail

cd "$(dirname "$0")/.." # web/
root=$(cd .. && pwd)    # repo root
rm -rf "$PWD/e2e/.run"  # sweep the in-tree location earlier revisions used
run_dir=$(mktemp -d "${TMPDIR:-/tmp}/lab-e2e.XXXXXXXX")
mkdir -p "$run_dir/state"
echo "lab e2e state dir: $run_dir" >&2

npm run build
rm -rf "$root/internal/webui/dist"
cp -r dist "$root/internal/webui/dist"
(cd "$root" && CGO_ENABLED=0 go build -tags ui -o "$run_dir/lab" ./cmd/lab)

# -claude-config points inside the throwaway state dir so the smoke never
# reads the operator's real ~/.claude.json.
exec "$run_dir/lab" \
  -addr "${E2E_ADDR:-127.0.0.1:8378}" \
  -state-dir "$run_dir/state" \
  -claude-config "$run_dir/state/claude.json"
