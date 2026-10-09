#!/usr/bin/env bash
# Runs the Linux integration tests (real system users, PHP-FPM) in a
# disposable Debian container. Requires Docker. Usage: deploy/dev/linux-test.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
GOMODCACHE="$(go env GOMODCACHE)"
exec docker run --rm \
  -v "$PWD":/src -w /src \
  -v "$GOMODCACHE":/go/pkg/mod \
  -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 \
  golang:1.27-trixie bash -euo pipefail -c '
    apt-get update -qq >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq php-fpm php-mysql mariadb-server >/dev/null
    V=$(ls /etc/php | sort -V | tail -1)
    mkdir -p /run/php /run/mysqld && chown mysql:mysql /run/mysqld
    php-fpm$V --daemonize
    (mariadbd --user=mysql >/tmp/mariadb.log 2>&1 &)
    for i in $(seq 50); do [ -S /run/mysqld/mysqld.sock ] && break; sleep 0.2; done
    echo "PHP-FPM $V and $(mariadb -N -e "SELECT VERSION()") started"
    go test -count=1 -tags linux_integration -run "TestLinux" -v ./internal/e2e/ 2>&1 | grep -vE "^\{\"level\""
  '
