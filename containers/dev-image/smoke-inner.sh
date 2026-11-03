#!/bin/sh
# smoke-inner.sh — runs INSIDE the full dev image, started by smoke-test.sh
# under lab's session conditions (unprivileged uid, empty HOME, lab's PATH).
# Expects PHP_VERSIONS, PHP_DEFAULT, GO_VERSION, NODE_MAJOR, BUILDX_VERSION
# and DDEV_VERSION from versions.env in the environment.
set -eu

step() { echo ">>> $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

step "session conditions"
[ "$(id -u)" != 0 ] || fail "running as root — the test must mirror lab's unprivileged session user"
[ -z "$(ls -A "${HOME}")" ] || fail "HOME is not empty"
# /opt/lab is lab's mount point for the agent-tools image; the image must not
# bake anything there.
if [ -e /opt/lab ] && [ -n "$(ls -A /opt/lab)" ]; then fail "/opt/lab has baked content"; fi
# lab's per-run ssh config Includes this file, and OpenSSH refuses an include
# that is not root-owned or is group/world-writable (docs/ops.md).
[ "$(stat -c '%U %a' /etc/ssh/ssh_config)" = "root 644" ] || fail "/etc/ssh/ssh_config is not root:644"

step "every tool resolves on lab's PATH"
for c in \
  git git-lfs ssh scp sftp curl wget rsync make gcc g++ \
  go gofmt gopls golangci-lint \
  node npm npx corepack pnpm yarn playwright \
  python python3 pip3 pipx uv uvx \
  php composer \
  rg fd jq yq shellcheck sqlite3 zip unzip tree less file patch \
  psql initdb pg_ctl mariadb mariadbd mariadb-install-db mariadb-admin \
  redis-server redis-cli start-postgres start-mariadb start-redis \
  chromium gm gs pdftotext sudo \
  podman crun conmon catatonit pasta nft newuidmap newgidmap getcap \
  docker ddev ddev-hostname mkcert start-nested-podman
do
  command -v "${c}" >/dev/null || fail "${c} not found on PATH (${PATH})"
done
command -v magick >/dev/null || command -v convert >/dev/null || fail "ImageMagick not found"

step "Go"
go version | grep -q "go${GO_VERSION}" || fail "go is not ${GO_VERSION}: $(go version)"
mkdir -p "${HOME}/gosmoke"
cat > "${HOME}/gosmoke/main.go" <<'GO'
package main

import "fmt"

func main() { fmt.Println("go-ok") }
GO
(cd "${HOME}/gosmoke" && go mod init gosmoke >/dev/null 2>&1 && [ "$(go run .)" = "go-ok" ]) || fail "go run"
gopls version >/dev/null || fail "gopls"
golangci-lint --version >/dev/null || fail "golangci-lint"

step "Node"
node --version | grep -q "^v${NODE_MAJOR}\." || fail "node is not v${NODE_MAJOR}: $(node --version)"
[ "$(node -e 'console.log(6*7)')" = "42" ] || fail "node -e"
npm --version >/dev/null || fail "npm"
pnpm --version >/dev/null || fail "pnpm"
yarn --version >/dev/null || fail "yarn"
corepack --version >/dev/null || fail "corepack"

step "Python"
python3 -m venv "${HOME}/venv" || fail "python3 -m venv"
"${HOME}/venv/bin/python" -c 'import ssl, sqlite3' || fail "python stdlib"
uv --version >/dev/null || fail "uv"

step "PHP"
php_ext="curl mbstring xml zip intl gd mysqli pdo_mysql pdo_pgsql pdo_sqlite bcmath soap readline redis apcu imagick"
for v in $(echo "${PHP_VERSIONS}" | tr ',' ' '); do
  got="$("php${v}" -r 'echo PHP_MAJOR_VERSION, ".", PHP_MINOR_VERSION;')"
  [ "${got}" = "${v}" ] || fail "php${v} reports ${got}"
  mods="$("php${v}" -m)"
  for e in ${php_ext}; do
    echo "${mods}" | grep -qix "${e}" || fail "php${v} lacks extension ${e}"
  done
  echo "${mods}" | grep -qi "opcache" || fail "php${v} lacks opcache"
done
got="$(php -r 'echo PHP_MAJOR_VERSION, ".", PHP_MINOR_VERSION;')"
[ "${got}" = "${PHP_DEFAULT}" ] || fail "bare php is ${got}, want ${PHP_DEFAULT}"
composer --version >/dev/null || fail "composer"

step "sudo is passwordless for apt, and for nothing else"
sudo -n apt-get --version >/dev/null || fail "sudo apt-get refused"
if sudo -n true 2>/dev/null; then fail "sudo ran a non-apt command"; fi

step "browser screenshots"
cat > "${HOME}/page.html" <<'HTML'
<!doctype html><html><body style="background:#fff"><h1>dev-image smoke</h1><p>äöü ✓ 😀</p></body></html>
HTML
is_png() { [ -s "$1" ] && file -b "$1" | grep -q '^PNG image data'; }
timeout 120 chromium --headless --screenshot="${HOME}/chromium.png" --window-size=1280,800 \
  "file://${HOME}/page.html" >/dev/null 2>&1 || fail "chromium --headless --screenshot"
is_png "${HOME}/chromium.png" || fail "chromium produced no PNG"
timeout 120 playwright screenshot --browser chromium "file://${HOME}/page.html" "${HOME}/playwright.png" \
  >/dev/null 2>&1 || fail "playwright screenshot"
is_png "${HOME}/playwright.png" || fail "playwright produced no PNG"

step "PostgreSQL"
export PGPORT=55432
start-postgres >/dev/null || fail "start-postgres"
start-postgres >/dev/null || fail "start-postgres is not re-runnable"
[ "$(psql -h 127.0.0.1 -U postgres -Atc 'select 6*7')" = "42" ] || fail "psql"
php -r 'new PDO("pgsql:host=127.0.0.1;port=" . getenv("PGPORT") . ";dbname=postgres", "postgres");' || fail "php pdo_pgsql connect"
pg_ctl -D /tmp/devdb/postgres -m fast stop >/dev/null || fail "pg_ctl stop"

step "MariaDB"
export MYSQL_TCP_PORT=53306
start-mariadb >/dev/null || fail "start-mariadb"
start-mariadb >/dev/null || fail "start-mariadb is not re-runnable"
[ "$(mariadb -h 127.0.0.1 -P "${MYSQL_TCP_PORT}" -uroot -N -e 'select 6*7' 2>/dev/null)" = "42" ] || fail "mariadb client"
php -r 'new PDO("mysql:host=127.0.0.1;port=" . getenv("MYSQL_TCP_PORT"), "root", "");' || fail "php pdo_mysql connect"
mariadb-admin --no-defaults --socket=/tmp/devdb/mariadb/mariadb.sock -uroot shutdown || fail "mariadb shutdown"

step "Redis"
export REDIS_PORT=56379
start-redis >/dev/null || fail "start-redis"
start-redis >/dev/null || fail "start-redis is not re-runnable"
[ "$(redis-cli -p "${REDIS_PORT}" ping)" = "PONG" ] || fail "redis-cli ping"
redis-cli -p "${REDIS_PORT}" shutdown nosave >/dev/null 2>&1 || true

# Nested containers (ADR-0086). Only what works without lab's nested argv:
# this container has no /dev/net/tun, a masked /proc, podman's default seccomp
# profile and no subuid line, so podman runs with a single-uid mapping (and
# says so in a warning) and no nested container is started here.
step "nested containers: file capabilities, no subid files, podman's helpers"
# Debian installs newuidmap/newgidmap setuid root, which fails inside a run;
# the image swaps that for file capabilities (see the Containerfile). getcap
# may append a [rootid=...] note for a namespaced capability, so match the
# capability itself rather than the whole line.
for m in uid:setuid gid:setgid; do
  b="/usr/bin/new${m%%:*}map"
  cap="cap_${m#*:}=ep"
  case "$(getcap "${b}")" in
    *" ${cap}"*) ;;
    *) fail "${b} lacks ${cap}: '$(getcap "${b}")'" ;;
  esac
  [ ! -u "${b}" ] || fail "${b} is still setuid"
done
# The entry program writes the run's one subuid/subgid line at run start; a
# baked-in file would add a second, overlapping extent.
for f in /etc/subuid /etc/subgid; do
  [ ! -e "${f}" ] || fail "${f} exists in the image"
done
# netavark and aardvark-dns live off PATH, where podman looks for them.
for b in /usr/lib/podman/netavark /usr/lib/podman/aardvark-dns; do
  [ -x "${b}" ] || fail "${b} missing"
done
nft --version >/dev/null || fail "nft --version"

step "docker buildx and compose"
got="$(docker buildx version)" || fail "docker buildx version"
v="$(echo "${got}" | awk '{print $2}')"
v="${v#v}"
[ "${v}" = "${BUILDX_VERSION}" ] || fail "buildx is ${v}, want ${BUILDX_VERSION}: ${got}"
[ "$(printf '%s\n' 0.17.0 "${v}" | sort -V | head -n 1)" = "0.17.0" ] \
  || fail "buildx ${v} is older than 0.17, which ddev refuses"
docker compose version >/dev/null || fail "docker compose version"

step "start-nested-podman, twice, without image archives"
unset LAB_IMAGE_ARCHIVES
rt="${XDG_RUNTIME_DIR:-/tmp/lab-nested/run}"
want="export DOCKER_HOST=unix://${rt}/podman.sock"
conf="${HOME}/.config/containers/storage.conf"
err="${HOME}/start-nested-podman.err"
# On failure, show the helper's stderr and the service log: both die with
# this container.
svc_fail() {
  cat "${err}" >&2 2>/dev/null || true
  cat "${rt}/podman-service.log" >&2 2>/dev/null || true
  fail "$*"
}
# exports_only <stdout>: the helper printed exactly the two export lines,
# DOCKER_HOST last — everything else must have gone to stderr.
exports_only() {
  [ "$1" = "$(printf 'export XDG_RUNTIME_DIR=%s\n%s' "${rt}" "${want}")" ] \
    || svc_fail "start-nested-podman's stdout is not exactly the two export lines ending in '${want}': ${1}"
}
# The storage config is the overlay driver alone: no additional image store.
driver_only() {
  [ "$(grep -v '^#' "${conf}")" = "$(printf '[storage]\ndriver = "overlay"')" ] \
    || fail "storage.conf is not the overlay driver alone: $(cat "${conf}")"
}
out="$(start-nested-podman 2>"${err}")" || svc_fail "start-nested-podman"
exports_only "${out}"
grep -qF "LAB_IMAGE_ARCHIVES unset — no image archives imported" "${err}" \
  || svc_fail "start-nested-podman did not say that LAB_IMAGE_ARCHIVES is unset"
driver_only
pids="$(cat "${rt}/podman-service.pid" "${rt}/healthcheck.pid")"
out="$(start-nested-podman 2>"${err}")" || svc_fail "start-nested-podman is not re-runnable"
exports_only "${out}"
[ "$(cat "${rt}/podman-service.pid" "${rt}/healthcheck.pid")" = "${pids}" ] \
  || fail "the second run started another service or healthcheck runner"
for p in ${pids}; do kill -0 "${p}" 2>/dev/null || fail "pid ${p} from start-nested-podman is gone"; done
eval "${out}"

step "start-nested-podman imports image archives"
# A one-file image with root-owned files, built offline and saved the way lab
# saves a ref (docker-archive, fixed name), then removed from local storage so
# only the helper's import can bring it back. podman runs here with a single-
# uid mapping (no subuid line), which covers an image owned entirely by root.
# The archives dir lives in the container's /tmp, not in the host-side HOME:
# it is made read-only like lab's mount, and the host must still delete HOME.
tag="docker.io/lab-smoke/archive:1"
archives="/tmp/lab-image-archives"
plog="${HOME}/podman-archive.log"
quiet() { "$@" >>"${plog}" 2>&1 || { cat "${plog}" >&2; fail "$*"; }; }
mkdir -p "${HOME}/archive-src" "${archives}"
echo "lab smoke" >"${HOME}/archive-src/hello"
quiet tar -C "${HOME}/archive-src" --owner=0 --group=0 --numeric-owner -cf "${HOME}/archive-rootfs.tar" hello
quiet podman import "${HOME}/archive-rootfs.tar" "${tag}"
quiet podman save --format docker-archive -o "${archives}/docker.io_lab-smoke_archive_1.tar" "${tag}"
quiet podman rmi "${tag}"
if podman image exists "${tag}"; then fail "${tag} still present before the import"; fi
# Junk named to sort last: a failed load must be logged and skipped.
echo "not an image archive" >"${archives}/zz-broken.tar"
chmod a-w "${archives}" "${archives}"/*
for run in first second; do
  out="$(LAB_IMAGE_ARCHIVES="${archives}" start-nested-podman 2>"${err}")" \
    || svc_fail "start-nested-podman with image archives (${run} run) — a broken archive must not fail it"
  exports_only "${out}"
  podman image exists "${tag}" || svc_fail "${tag} not present after the ${run} import"
  grep -qF "imported ${archives}/docker.io_lab-smoke_archive_1.tar in " "${err}" \
    || svc_fail "the ${run} run did not log the archive's import"
  grep -qF "FAILED to import ${archives}/zz-broken.tar" "${err}" \
    || svc_fail "the ${run} run did not log the broken archive's failure"
  grep -qF "1 imported, 1 failed" "${err}" || svc_fail "the ${run} run's import summary is wrong"
  driver_only
  [ "$(cat "${rt}/podman-service.pid" "${rt}/healthcheck.pid")" = "${pids}" ] \
    || fail "the ${run} archive run started another service or healthcheck runner"
done
out="$(LAB_IMAGE_ARCHIVES=/tmp/no-such-archives start-nested-podman 2>"${err}")" \
  || svc_fail "start-nested-podman with a missing archives directory"
exports_only "${out}"
grep -qF "LAB_IMAGE_ARCHIVES '/tmp/no-such-archives' is not a directory — no image archives imported" "${err}" \
  || svc_fail "start-nested-podman did not say the archives directory is missing"
for p in ${pids}; do kill -0 "${p}" 2>/dev/null || fail "pid ${p} from start-nested-podman is gone"; done
# The import left the image's layer in the session's storage under HOME, and
# rootless podman makes a layer's diff/ directory read-only (0555), so the
# host's plain rm -rf of HOME would fail on it. podman removes it cleanly.
quiet podman rmi "${tag}"

step "podman as the session user"
got="$(podman info --format '{{.Host.CgroupManager}} {{.Host.NetworkBackend}}')" || fail "podman info (output above)"
[ "${got}" = "cgroupfs netavark" ] || fail "podman info: want 'cgroupfs netavark', got '${got}'"

step "ddev"
# Against the helper's API service: ddev version fails without a Docker API.
got="$(ddev version 2>&1)" || fail "ddev version: ${got}"
echo "${got}" | grep -qF "v${DDEV_VERSION}" || fail "ddev is not v${DDEV_VERSION}: ${got}"

# The service and the runner are detached on purpose; stop them so nothing
# outlives the checks.
# shellcheck disable=SC2086 # pids is a whitespace-separated pid list
kill ${pids} 2>/dev/null || true

echo "inner smoke: all checks passed"
