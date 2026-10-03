#!/bin/sh
# smoke-inner.sh — runs INSIDE the full dev image, started by smoke-test.sh
# under lab's session conditions (unprivileged uid, empty HOME, lab's PATH).
# Expects PHP_VERSIONS, PHP_DEFAULT, GO_VERSION and NODE_MAJOR from
# versions.env in the environment.
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
  chromium gm gs pdftotext sudo
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

echo "inner smoke: all checks passed"
