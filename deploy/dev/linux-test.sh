#!/usr/bin/env bash
# Runs the Linux integration tests (real system users, PHP-FPM) in a
# disposable Debian container. Requires Docker. Usage: deploy/dev/linux-test.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
GOMODCACHE="$(go env GOMODCACHE)"
exec docker run --rm \
  -v "$PWD":/src -w /src \
  -v "$GOMODCACHE":/go/pkg/mod \
  -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 -e GOTOOLCHAIN=auto \
  golang:1.27-trixie bash -euo pipefail -c '
    apt-get update -qq >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq php-fpm php-mysql mariadb-server restic >/dev/null
    V=$(ls /etc/php | sort -V | tail -1)
    mkdir -p /run/php /run/mysqld && chown mysql:mysql /run/mysqld
    php-fpm$V --daemonize
    # Two MariaDB servers with a binary log (database replication between servers).
    (mariadbd --user=mysql --log-bin=basalt-bin >/tmp/mariadb.log 2>&1 &)
    mkdir -p /run/mysqld2 && chown mysql:mysql /run/mysqld2
    mariadb-install-db --user=mysql --datadir=/var/lib/mysql2 >/dev/null
    (mariadbd --user=mysql --datadir=/var/lib/mysql2 --socket=/run/mysqld2/mysqld.sock --port=3307 --pid-file=/run/mysqld2/mariadb.pid --log-bin=basalt-bin >/tmp/mariadb2.log 2>&1 &)
    for i in $(seq 50); do [ -S /run/mysqld/mysqld.sock ] && [ -S /run/mysqld2/mysqld.sock ] && break; sleep 0.2; done
    echo "PHP-FPM $V and $(mariadb -N -e "SELECT VERSION()") started; $(restic version | cut -d" " -f1-2)"
    go test -count=1 -tags linux_integration -run "TestLinux" -v ./internal/e2e/ ./internal/dns/ 2>&1 | grep -vE "^\{\"level\""
    go test -count=1 -run "TestSnapshotFiles|TestCheck" ./internal/restic/
  '
