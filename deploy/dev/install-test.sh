#!/usr/bin/env bash
# End-to-end test of deploy/install.sh on two systemd containers:
# "panel" (install.sh panel) and "node1" (the one-line command from the panel).
set -euo pipefail
cd "$(dirname "$0")/../.."
ARCH=$(docker version --format '{{.Server.Arch}}')
make build-linux >/dev/null
# A newer "release" signed with a test key, for the update test.
RT=bin/release-test
sum256() { if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }
rm -rf "$RT"; mkdir -p "$RT/latest/download" "$RT/download/v9.9.9"
GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/wiefix/basalt/internal/buildinfo.Version=v9.9.9" \
  -o "$RT/download/v9.9.9/basalt-linux-$ARCH" ./cmd/basalt
echo v9.9.9 >"$RT/latest/download/version.txt"
(cd "$RT/latest/download" && cp "../../download/v9.9.9/basalt-linux-$ARCH" . && sum256 "basalt-linux-$ARCH" version.txt >SHA256SUMS && rm "basalt-linux-$ARCH")
ssh-keygen -q -t ed25519 -N '' -C test-release -f "$RT/key"
ssh-keygen -q -Y sign -f "$RT/key" -n basalt-release "$RT/latest/download/SHA256SUMS"
RELKEY=$(cat "$RT/key.pub")
docker build -q -t basalt-systemd -f deploy/dev/systemd.Dockerfile deploy/dev >/dev/null

cleanup() { docker rm -f basalt-panel basalt-node1 basalt-standby basalt-releases >/dev/null 2>&1 || true; docker network rm basalt-net >/dev/null 2>&1 || true; }
cleanup
[ "${KEEP:-}" = 1 ] || trap cleanup EXIT
docker network create basalt-net >/dev/null
run() { # name
  docker run -d --name "basalt-$1" --hostname "$1" --network basalt-net --privileged --cgroupns=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock \
    -v "$PWD/bin":/dist:ro -v "$PWD/deploy":/deploy:ro basalt-systemd >/dev/null
}
run panel
run node1
run standby
sleep 3

PANELIP=$(docker inspect -f '{{(index .NetworkSettings.Networks "basalt-net").IPAddress}}' basalt-panel)
STANDBYIP=$(docker inspect -f '{{(index .NetworkSettings.Networks "basalt-net").IPAddress}}' basalt-standby)
echo "### guided install of the panel (answers typed into a terminal; agents connect to its IP)"
# Domain: none; agents: the IP; Docker: no; PHP, MariaDB: yes (the panel's
# server takes a copy of a site); DNS: no; install: yes.
OUT=$(printf '\n%s\nn\ny\ny\nn\ny\n' "$PANELIP" |
  docker exec -i basalt-panel script -qec "sh /deploy/install.sh --binary /dist/basalt-linux-$ARCH" /dev/null 2>&1) || { echo "$OUT"; exit 1; }
echo "$OUT"
case "$OUT" in *"Ready to install"*"Agents:  $PANELIP:8443"*"Basalt is running"*) ;; *) echo "the guided install did not ask or finish"; exit 6 ;; esac
# The installer's service starts the agent: no advice to start it by hand.
case "$OUT" in *"start the agent with"*) echo "the installer tells to start the agent by hand"; exit 6 ;; esac
echo "### a second run without --agent-host keeps it"
docker exec basalt-panel sh /deploy/install.sh panel --binary /dist/basalt-linux-$ARCH >/dev/null
docker exec basalt-panel grep -q -- "--agent-url https://$PANELIP:8443 " /etc/systemd/system/basalt-server.service
docker exec basalt-panel sh /deploy/install.sh panel --agent-host 'a;b' >/dev/null 2>&1 && exit 5

echo "### services on panel"
docker exec basalt-panel systemctl is-active basalt-server basalt-proxy basalt-agent
echo "### panel UI through the proxy"
for i in $(seq 30); do
  docker exec basalt-panel curl -sk "https://$PANELIP/" | grep -q "<title>Basalt</title>" && break; sleep 1
done
docker exec basalt-panel curl -sk "https://$PANELIP/" | grep -o "<title>[^<]*</title>"

TOKEN=$(docker exec basalt-panel cat /var/lib/basalt/server/setup-token)
API="docker exec basalt-panel curl -sS -b /tmp/j -c /tmp/j -H Content-Type:application/json"
$API -d "{\"setup_token\":\"$TOKEN\",\"email\":\"admin@example.test\",\"name\":\"Admin\",\"password\":\"installer test password\"}" http://127.0.0.1:8080/api/v1/setup >/dev/null
echo "### servers after install (local agent auto-enrolled)"
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; [print(" ", s["name"], s["status"], "isolated" if s["isolated"] else "") for s in json.load(sys.stdin)["servers"]]'

echo "### add node1 with the one-line command from the panel"
CMD=$($API -d '{"name":"node1"}' http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; print(json.load(sys.stdin)["enrollment"]["install_command"])')
case "$CMD" in *"--panel https://$PANELIP:8443 "*) ;; *) echo "the command does not use the agent host: $CMD"; exit 5 ;; esac
docker exec basalt-node1 sh -c "$CMD --with-php --with-mariadb --with-docker --with-dns"
docker exec basalt-node1 systemctl is-active basalt-proxy basalt-agent
docker exec basalt-node1 sh -c 'git --version && docker buildx version | cut -d" " -f1-2' 
docker exec basalt-node1 ps -o user=,comm= -C basalt | sort | uniq -c
echo "### a standby panel on its own server, from its one-line command"
SBCMD=$($API -d '{"name":"spare"}' http://127.0.0.1:8080/api/v1/standbys | python3 -c 'import sys,json; print(json.load(sys.stdin)["enrollment"]["install_command"])')
case "$SBCMD" in *"| sh -s -- standby --panel https://$PANELIP:8443 "*"--agent-token "*) ;; *) echo "standby command: $SBCMD"; exit 5 ;; esac
docker exec basalt-standby sh -c "$SBCMD"
docker exec basalt-standby systemctl is-active basalt-server basalt-proxy basalt-agent
standby_ready() {
  $API http://127.0.0.1:8080/api/v1/standbys | python3 -c 'import sys,json; s=json.load(sys.stdin)["standbys"][0]; print("  standby:", s["name"], "connected" if s["connected"] else "not connected", "ready" if s["ready"] else "copying", s.get("error", "")); sys.exit(0 if s["connected"] and s["ready"] else 1)'
}
for i in $(seq 60); do standby_ready >/dev/null && break; sleep 1; done
standby_ready
docker exec basalt-standby curl -fsS http://127.0.0.1:8080/healthz | python3 -c 'import sys,json; s=json.load(sys.stdin); print("  its state:", s); sys.exit(0 if s["ready"] else "the standby says it is not ready")'
[ "$(docker exec basalt-standby stat -c '%U %a' /var/lib/basalt/server/master.key)" = "basalt 600" ] || { echo "no private master key on the standby"; exit 1; }
[ "$(docker exec basalt-standby sha256sum /var/lib/basalt/server/master.key | cut -c1-64)" = "$(docker exec basalt-panel sha256sum /var/lib/basalt/server/master.key | cut -c1-64)" ] || { echo "the standby has another master key"; exit 1; }
docker exec basalt-standby sh -c '! curl -fsS http://127.0.0.1:8080/api/v1/servers >/dev/null 2>&1' || { echo "the standby serves the panel's API"; exit 1; }
echo "### a second standby panel (by hand on node1, its own data directory): it will follow whichever takes over"
SB2=$($API -d '{"name":"spare-2"}' http://127.0.0.1:8080/api/v1/standbys)
SB2TOKEN=$(echo "$SB2" | python3 -c 'import sys,json; print(json.load(sys.stdin)["enrollment"]["token"])')
SB2CA=$(echo "$SB2" | python3 -c 'import sys,json,re; print(re.search(r"--ca-sha256 (\S+)", json.load(sys.stdin)["enrollment"]["install_command"]).group(1))')
# Its host is node1, whose agent is enrolled already: the extra server goes.
$API -X DELETE http://127.0.0.1:8080/api/v1/servers/$(echo "$SB2" | python3 -c 'import sys,json; print(json.load(sys.stdin)["server"]["id"])') >/dev/null
N1IP=$(docker inspect -f '{{(index .NetworkSettings.Networks "basalt-net").IPAddress}}' basalt-node1)
docker exec basalt-node1 basalt server standby-enroll --data-dir /var/lib/basalt/spare2 --panel https://$PANELIP:8443 --token "$SB2TOKEN" --ca-sha256 "$SB2CA"
docker exec basalt-node1 systemd-run --unit basalt-spare2 --quiet /usr/local/bin/basalt server --data-dir /var/lib/basalt/spare2 --listen 127.0.0.1:8090 --agent-url https://$N1IP:8445
spare2() { # field of its state
  docker exec basalt-node1 curl -fsS http://127.0.0.1:8090/healthz 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("'$1'", ""))' 2>/dev/null
}
for i in $(seq 60); do [ "$(spare2 ready)" = True ] && break; sleep 1; done
[ "$(spare2 ready)" = True ] || { docker exec basalt-node1 journalctl -u basalt-spare2 --no-pager | tail -20; echo "the second standby has no copy"; exit 1; }
for i in $(seq 30); do docker exec basalt-node1 grep -q "https://$N1IP:8445" /var/lib/basalt/spare2/standby.json && break; sleep 1; done
echo "  the second standby knows: $(docker exec basalt-node1 python3 -c 'import json; print(json.load(open("/var/lib/basalt/spare2/standby.json"))["panels"])')"
docker exec basalt-node1 grep -q "https://$N1IP:8445" /var/lib/basalt/spare2/standby.json || { echo "the second standby does not know the panel's addresses"; exit 1; }
echo "### systemd exposure scores (lower is safer)"
for u in basalt-agent basalt-proxy; do
  echo "  $u: $(docker exec basalt-node1 systemd-analyze security $u.service 2>/dev/null | tail -1)"
done

for i in $(seq 30); do
  $API http://127.0.0.1:8080/api/v1/servers | grep -q '"name":"node1","status":"online"' && $API http://127.0.0.1:8080/api/v1/servers | grep -q '"name":"spare","status":"online"' && break; sleep 1
done
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; [print(" ", s["name"], s["status"], "php:", s["php_versions"], "isolated:", s["isolated"], "docker:", s["docker"], s.get("docker_security")) for s in json.load(sys.stdin)["servers"]]'
echo "### restic for backups on every server"
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; l=json.load(sys.stdin)["servers"]; [print(" ", s["name"], "restic", s["restic"]) for s in l]; sys.exit(0 if all(s["restic"] for s in l) else "restic missing")'
echo "### the agents' tunnel listens on 8444 everywhere"
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; l=json.load(sys.stdin)["servers"]; [print(" ", s["name"], "tunnel", s["peer_port"], s.get("peer_error", "")) for s in l]; sys.exit(0 if all(s["peer_port"] == 8444 for s in l) else "tunnel not listening")'
echo "### Docker on node1 runs containers in user namespaces"
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; s=[s for s in json.load(sys.stdin)["servers"] if s["name"]=="node1"][0]; sys.exit(0 if (s.get("docker_security") or {}).get("userns") else "userns-remap is off on node1")'
NODE=$($API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; print([s["id"] for s in json.load(sys.stdin)["servers"] if s["name"]=="node1"][0])')
PHPV=$($API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; print([s["php_versions"][0] for s in json.load(sys.stdin)["servers"] if s["name"]=="node1"][0])')

echo "### PHP site on node1 (proxy runs as basalt-proxy, PHP as the site user)"
SITE=$($API -d "{\"server_id\":$NODE,\"name\":\"shop\",\"domains\":[\"shop.test\"],\"tls\":\"internal\",\"php_version\":\"$PHPV\"}" http://127.0.0.1:8080/api/v1/sites | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["site"]["id"]); print(d.get("provision_error",""), file=sys.stderr)')
printf '%s' '<?php echo "php ", PHP_VERSION, " as ", posix_getpwuid(posix_geteuid())["name"];' > /tmp/basalt-index.php
docker cp /tmp/basalt-index.php basalt-panel:/root/index.php
$API -X PUT -H Content-Type:application/octet-stream --data-binary @/root/index.php "http://127.0.0.1:8080/api/v1/sites/$SITE/files/content?path=/public/index.php" >/dev/null
$API -d '{"paths":["/public/index.html"]}' "http://127.0.0.1:8080/api/v1/sites/$SITE/files/delete" >/dev/null
NODEIP=$(docker inspect -f '{{(index .NetworkSettings.Networks "basalt-net").IPAddress}}' basalt-node1)
for i in $(seq 30); do
  docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " && break; sleep 1
done
docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/; echo
echo "### the proxy logs the site's requests"
$API "http://127.0.0.1:8080/api/v1/sites/$SITE/access-log?limit=5" | python3 -c 'import sys,json; l=json.load(sys.stdin)["entries"]; [print("  ", e["at"], e["method"], e["host"], e["uri"], e["status"]) for e in l[:2]]; sys.exit(0 if l and l[0]["host"]=="shop.test" else "no access log")'
echo "### the site's PHP runs as its own systemd service, with CPU and memory limits"
docker exec basalt-node1 systemctl is-active basalt-php-site-$SITE.service
docker exec basalt-node1 sh -c "! ls /etc/php/*/fpm/pool.d/basalt-site-$SITE.conf 2>/dev/null" || { echo "the site's pool is still in the shared PHP-FPM"; exit 1; }
$API -X PUT -d '{"cpu_percent":50,"ram_mb":256}' http://127.0.0.1:8080/api/v1/sites/$SITE/php-settings >/dev/null
LIMITS=$(docker exec basalt-node1 systemctl show -p CPUQuotaPerSecUSec -p MemoryMax --value basalt-php-site-$SITE.service | tr '\n' ' ')
echo "  limits: $LIMITS"
[ "$LIMITS" = "500ms 268435456 " ] || { echo "limits not applied"; exit 1; }
for i in $(seq 30); do
  docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " && break; sleep 1
done
docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " || { echo "site down after setting limits"; exit 1; }
echo "### after an update of PHP, the site's PHP runs the new binary, not the replaced one left in memory"
OLDPID=$(docker exec basalt-node1 systemctl show -p MainPID --value basalt-php-site-$SITE.service)
# As a package update does: a new file renamed over the old one.
docker exec basalt-node1 sh -c "cp -p /usr/sbin/php-fpm$PHPV /usr/sbin/php-fpm$PHPV.dpkg-new && mv /usr/sbin/php-fpm$PHPV.dpkg-new /usr/sbin/php-fpm$PHPV"
docker exec basalt-node1 grep -q "php-fpm$PHPV (deleted)" /proc/$OLDPID/maps || { echo "the replaced binary is not marked deleted"; exit 1; }
for i in $(seq 100); do
  FPMPID=$(docker exec basalt-node1 systemctl show -p MainPID --value basalt-php-site-$SITE.service)
  [ "$FPMPID" != 0 ] && [ "$FPMPID" != "$OLDPID" ] && break; sleep 1
done
[ "$FPMPID" != "$OLDPID" ] || { echo "the site's PHP was not restarted"; exit 1; }
docker exec basalt-node1 sh -c "! grep -q 'php-fpm.*(deleted)' /proc/$FPMPID/maps" || { echo "the site's PHP still runs the replaced binary"; exit 1; }
echo "  restarted after ${i}s"
docker exec basalt-node1 systemctl is-active basalt-php-site-$SITE.service
for i in $(seq 30); do
  docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " && break; sleep 1
done
docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " || { echo "site down after the PHP update"; exit 1; }

echo "### Laravel processes: scheduler and queue workers as sandboxed systemd services of the site's user"
# A stand-in artisan: it records how it runs, then stays up as the real commands do.
cat > /tmp/basalt-artisan <<'PHP'
<?php
$out = __DIR__ . "/logs/" . str_replace(":", "-", $argv[1] ?? "none") . ".txt";
$who = posix_getpwuid(posix_geteuid())["name"];
$read = @file_get_contents("/etc/passwd") === false ? "fenced" : "open";
# A program it starts (allowed here, unlike in pages) is fenced by systemd:
# a folder anyone may write to on the host is read-only.
$write = trim((string) shell_exec("touch /var/lib/php/sessions/basalt-escape 2>/dev/null && echo writable || echo readonly"));
file_put_contents($out, "$who " . ini_get("open_basedir") . " $read $write\n", FILE_APPEND);
echo "started " . ($argv[1] ?? "") . "\n";
fwrite(STDERR, "<3>error from " . ($argv[1] ?? "") . "\n");
sleep(3600);
PHP
docker cp /tmp/basalt-artisan basalt-panel:/root/artisan
$API -X PUT -H Content-Type:application/octet-stream --data-binary @/root/artisan "http://127.0.0.1:8080/api/v1/sites/$SITE/files/content?path=/artisan" >/dev/null
$API -X PUT -d '{"scheduler":true,"workers":2}' http://127.0.0.1:8080/api/v1/sites/$SITE/laravel | python3 -c 'import sys,json; d=json.load(sys.stdin); print("  laravel:", d.get("site",{}).get("laravel"), d.get("error",""))'
for u in schedule queue-1 queue-2; do
  docker exec basalt-node1 systemctl is-active basalt-php-site-$SITE-$u.service || { docker exec basalt-node1 journalctl -u basalt-php-site-$SITE-$u.service --no-pager | tail -20; exit 1; }
done
for i in $(seq 30); do
  docker exec basalt-node1 test -s /srv/basalt/sites/$SITE/logs/queue-work.txt && docker exec basalt-node1 test -s /srv/basalt/sites/$SITE/logs/schedule-work.txt && break; sleep 1
done
for f in schedule-work queue-work; do
  RAN=$(docker exec basalt-node1 head -1 /srv/basalt/sites/$SITE/logs/$f.txt)
  echo "  $f: $RAN"
  [ "$RAN" = "basalt-s$SITE /srv/basalt/sites/$SITE/ fenced readonly" ] || { echo "$f does not run as the site's user in its fence"; exit 1; }
done
LIMITS=$(docker exec basalt-node1 systemctl show -p CPUQuotaPerSecUSec -p MemoryMax --value basalt-sites-laravel$SITE.slice | tr '\n' ' ')
echo "  slice limits: $LIMITS"
[ "$LIMITS" = "500ms 268435456 " ] || { echo "the site's limits are not on the Laravel processes"; exit 1; }
echo "  exposure score: $(docker exec basalt-node1 systemd-analyze security basalt-php-site-$SITE-queue-1.service 2>/dev/null | tail -1)"
# Their state, output and restart in the panel.
$API http://127.0.0.1:8080/api/v1/sites/$SITE/laravel | python3 -c '
import sys, json
p = json.load(sys.stdin)["processes"]
print("  processes:", [(x["name"], x["state"], x["sub_state"]) for x in p])
assert [x["name"] for x in p] == ["schedule", "queue-1", "queue-2"] and all(x["state"] == "active" and x["since"] for x in p), p
' || exit 1
for i in $(seq 15); do
  $API "http://127.0.0.1:8080/api/v1/sites/$SITE/laravel/log?lines=50" > /tmp/basalt-laravel-log.json
  python3 -c '
import sys, json
l = json.load(open("/tmp/basalt-laravel-log.json"))["lines"]
assert any(x["process"] == "schedule" and x["message"] == "started schedule:work" for x in l)
assert any(x["process"] == "queue-2" and x["message"] == "error from queue:work" and x.get("error") for x in l)
' 2>/dev/null && break
  [ $i = 15 ] && { echo "the processes' output is not in the panel:"; cat /tmp/basalt-laravel-log.json; exit 1; }
  sleep 1
done
echo "  output: $(python3 -c 'import json; print(len(json.load(open("/tmp/basalt-laravel-log.json"))["lines"]))') lines"
$API -X POST http://127.0.0.1:8080/api/v1/sites/$SITE/laravel/restart | grep -q '"processes"' || { echo "restart failed"; exit 1; }
for i in $(seq 30); do
  [ "$(docker exec basalt-node1 cat /srv/basalt/sites/$SITE/logs/schedule-work.txt | wc -l)" -ge 2 ] && break
  [ $i = 30 ] && { echo "the scheduler was not restarted"; exit 1; }
  sleep 1
done
echo "  restarted from the panel"
$API -X PUT -d '{"scheduler":false,"workers":1}' http://127.0.0.1:8080/api/v1/sites/$SITE/laravel >/dev/null
docker exec basalt-node1 systemctl is-active basalt-php-site-$SITE-queue-1.service
docker exec basalt-node1 sh -c "! ls /etc/systemd/system/basalt-php-site-$SITE-schedule.service /etc/systemd/system/basalt-php-site-$SITE-queue-2.service 2>/dev/null" || { echo "units left"; exit 1; }
$API -X PUT -d '{"scheduler":false,"workers":0}' http://127.0.0.1:8080/api/v1/sites/$SITE/laravel >/dev/null
docker exec basalt-node1 sh -c "! ls /etc/systemd/system/basalt-php-site-$SITE-* /etc/systemd/system/basalt-sites-laravel$SITE.slice 2>/dev/null" || { echo "units left"; exit 1; }
docker exec basalt-node1 sh -c "! pgrep -u basalt-s$SITE -f artisan" || { echo "Laravel processes still running"; exit 1; }
# A site without artisan cannot have them.
$API -d '{"paths":["/artisan"]}' "http://127.0.0.1:8080/api/v1/sites/$SITE/files/delete" >/dev/null
$API -X PUT -d '{"scheduler":true,"workers":0}' http://127.0.0.1:8080/api/v1/sites/$SITE/laravel | grep -q 'artisan' || { echo "Laravel processes accepted without artisan"; exit 1; }

echo "### app on node1 (container reached by its IP on a private network, no published port)"
APP=$($API -d "{\"server_id\":$NODE,\"name\":\"who\",\"image\":\"traefik/whoami:v1.10\",\"port\":80,\"domains\":[\"who.test\"],\"tls\":\"internal\"}" http://127.0.0.1:8080/api/v1/apps | python3 -c 'import sys,json; print(json.load(sys.stdin)["app"]["id"])')
$API -X PUT -d '{"env":{"WHOAMI_NAME":"basalt-on-node1"}}' http://127.0.0.1:8080/api/v1/apps/$APP/env >/dev/null
DEP=$($API -d '{}' http://127.0.0.1:8080/api/v1/apps/$APP/deploy | python3 -c 'import sys,json; print(json.load(sys.stdin)["deployment"]["id"])')
for i in $(seq 120); do
  ST=$($API http://127.0.0.1:8080/api/v1/apps/$APP/deployments/$DEP | python3 -c 'import sys,json; print(json.load(sys.stdin)["deployment"]["status"])')
  [ "$ST" != running ] && break; sleep 1
done
echo "  deployment: $ST"
[ "$ST" = succeeded ] || { $API http://127.0.0.1:8080/api/v1/apps/$APP/deployments/$DEP; exit 1; }
docker exec basalt-node1 docker ps --format '  {{.Names}} {{.Ports}}' --filter label=basalt.app
for i in $(seq 30); do
  docker exec basalt-panel curl -sk --resolve who.test:443:$NODEIP https://who.test/ | grep -q basalt-on-node1 && break; sleep 1
done
docker exec basalt-panel curl -sk --resolve who.test:443:$NODEIP https://who.test/ | grep -E "^(Name|IP: 172)"
echo "### an app without a Dockerfile, built by Railpack on node1 (BuildKit builder under userns-remap)"
RPV=$(sed -n 's/^RAILPACK_VERSION=//p' deploy/install.sh)
[ "$(docker exec basalt-node1 railpack --version | awk '{print $3}')" = "$RPV" ] || { echo "Railpack $RPV not installed on node1"; exit 1; }
for i in $(seq 30); do
  [ "$($API http://127.0.0.1:8080/api/v1/servers/$NODE | python3 -c 'import sys,json; print(json.load(sys.stdin)["server"]["railpack"])')" = "$RPV" ] && break; sleep 1
done
RPAPP=$($API -d "{\"server_id\":$NODE,\"name\":\"auto\",\"source\":\"git\",\"port\":3000,\"domains\":[\"auto.test\"],\"tls\":\"internal\",\"git\":{\"url\":\"https://github.com/wiefix/Basalt.git\",\"branch\":\"main\",\"context\":\"deploy/dev/railpack-app\",\"builder\":\"railpack\"}}" http://127.0.0.1:8080/api/v1/apps | python3 -c 'import sys,json; print(json.load(sys.stdin)["app"]["id"])')
DEP=$($API -d '{}' http://127.0.0.1:8080/api/v1/apps/$RPAPP/deploy | python3 -c 'import sys,json; print(json.load(sys.stdin)["deployment"]["id"])')
for i in $(seq 900); do
  ST=$($API http://127.0.0.1:8080/api/v1/apps/$RPAPP/deployments/$DEP | python3 -c 'import sys,json; print(json.load(sys.stdin)["deployment"]["status"])')
  [ "$ST" != running ] && break; sleep 1
done
echo "  deployment: $ST after ${i}s"
[ "$ST" = succeeded ] || { $API http://127.0.0.1:8080/api/v1/apps/$RPAPP/deployments/$DEP | python3 -c 'import sys,json; d=json.load(sys.stdin)["deployment"]; print(d.get("error")); print(d.get("log","")[-3000:])'; exit 1; }
for i in $(seq 30); do
  docker exec basalt-panel curl -sk --resolve auto.test:443:$NODEIP https://auto.test/ | grep -q "built by railpack, listening on 3000" && break; sleep 1
done
docker exec basalt-panel curl -sk --resolve auto.test:443:$NODEIP https://auto.test/; echo
docker exec basalt-panel curl -sk --resolve auto.test:443:$NODEIP https://auto.test/ | grep -q "built by railpack" || { echo "the Railpack app does not answer"; exit 1; }
echo "### node1 serves DNS (--with-dns), zones from the panel"
ZONE=$($API -d '{"name":"zone.test"}' http://127.0.0.1:8080/api/v1/dns/zones | python3 -c 'import sys,json; print(json.load(sys.stdin)["zone"]["id"])')
$API -d '{"name":"www","type":"A","value":"192.0.2.44"}' http://127.0.0.1:8080/api/v1/dns/zones/$ZONE/records >/dev/null
for i in $(seq 30); do
  [ "$(docker exec basalt-panel dig +short @$NODEIP www.zone.test A)" = 192.0.2.44 ] && break; sleep 1
done
docker exec basalt-panel dig +noall +answer @$NODEIP www.zone.test A
[ "$(docker exec basalt-panel dig +short +tcp @$NODEIP www.zone.test A)" = 192.0.2.44 ] || { echo "no DNS answer from node1"; exit 1; }
docker exec basalt-panel dig +noall +comments @$NODEIP example.com A | grep -q "status: REFUSED" || { echo "node1 answered for a foreign zone"; exit 1; }
echo "### automatic record for the PHP site's domain (shop.test on node1)"
$API -X PUT -d "{\"ips\":[\"$NODEIP\"]}" http://127.0.0.1:8080/api/v1/servers/$NODE/public-ips >/dev/null
$API -d '{"name":"shop.test"}' http://127.0.0.1:8080/api/v1/dns/zones >/dev/null
for i in $(seq 30); do
  [ "$(docker exec basalt-panel dig +short @$NODEIP shop.test A)" = "$NODEIP" ] && break; sleep 1
done
docker exec basalt-panel dig +noall +answer @$NODEIP shop.test A
[ "$(docker exec basalt-panel dig +short @$NODEIP shop.test A)" = "$NODEIP" ] || { echo "no automatic record"; exit 1; }
echo "### the panel's own domain, in a zone of Basalt, points at the panel's server"
# The containers' addresses are private: given as the servers' public ones.
for srv in $($API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; print(" ".join("%d:%s" % (s["id"], s["name"]) for s in json.load(sys.stdin)["servers"] if s["name"] != "node1"))'); do
  ip=$PANELIP; [ "${srv#*:}" = spare ] && ip=$STANDBYIP
  $API -X PUT -d "{\"ips\":[\"$ip\"]}" http://127.0.0.1:8080/api/v1/servers/${srv%%:*}/public-ips >/dev/null
done
$API -X PUT -d '{"url":"https://panel.zone.test"}' http://127.0.0.1:8080/api/v1/notifications/panel-url >/dev/null
for i in $(seq 30); do
  [ "$(docker exec basalt-panel dig +short @$NODEIP panel.zone.test A)" = "$PANELIP" ] && break; sleep 1
done
docker exec basalt-panel dig +noall +answer @$NODEIP panel.zone.test A
[ "$(docker exec basalt-panel dig +short @$NODEIP panel.zone.test A)" = "$PANELIP" ] || { echo "no record for the panel's domain"; exit 1; }
echo "### DNS-01 relay: the proxy user asks the agent, the panel publishes the TXT record"
docker exec basalt-node1 stat -c '  %n %U %a' /var/lib/basalt/proxy/acme.sock
[ "$(docker exec basalt-node1 stat -c '%U %a' /var/lib/basalt/proxy/acme.sock)" = "basalt-proxy 600" ] || { echo "acme socket not private to the proxy"; exit 1; }
docker exec basalt-node1 runuser -u basalt-proxy -- curl -fsS --unix-socket /var/lib/basalt/proxy/acme.sock \
  -X POST -d '{"name":"_acme-challenge.shop.test","value":"relay-token"}' http://agent/acme-challenge
[ "$(docker exec basalt-panel dig +short @$NODEIP _acme-challenge.shop.test TXT)" = '"relay-token"' ] || { echo "challenge not published"; exit 1; }
echo "  _acme-challenge.shop.test TXT relay-token served by node1"
DENIED=$(docker exec basalt-node1 runuser -u nobody -- curl -sS --unix-socket /var/lib/basalt/proxy/acme.sock \
  -X POST -d '{"name":"_acme-challenge.shop.test","value":"x"}' http://agent/acme-challenge 2>&1 || true)
echo "  as nobody: $DENIED"
case "$DENIED" in *"Couldn't connect"* | *"Permission denied"* | *"Failed to connect"*) ;; *) echo "socket open to other users"; exit 1 ;; esac
docker exec basalt-node1 runuser -u basalt-proxy -- curl -fsS --unix-socket /var/lib/basalt/proxy/acme.sock \
  -X DELETE -d '{"name":"_acme-challenge.shop.test","value":"relay-token"}' http://agent/acme-challenge
echo "### DNSSEC: the signed zone validates (delv with the zone key as trust anchor)"
$API -X PUT -d '{"enabled":true}' http://127.0.0.1:8080/api/v1/dns/zones/$ZONE/dnssec >/dev/null
KEY=$($API http://127.0.0.1:8080/api/v1/dns/zones/$ZONE | python3 -c 'import sys,json; print(json.load(sys.stdin)["zone"]["dnssec"]["dnskey"])')
# shellcheck disable=SC2086
docker exec basalt-panel sh -c "printf 'trust-anchors { zone.test. static-key %s %s %s \"%s\"; };\n' $KEY >/tmp/anchor"
for i in $(seq 30); do
  docker exec basalt-panel dig +dnssec +short @$NODEIP www.zone.test A | grep -q "^A 13 " && break; sleep 1
done
VALID=$(docker exec basalt-panel delv @$NODEIP -a /tmp/anchor +root=zone.test www.zone.test A 2>&1)
echo "$VALID" | grep -E "validated|192.0.2.44" | sed 's/^/  /'
echo "$VALID" | grep -q "; fully validated" || { echo "$VALID"; exit 1; }
NEG=$(docker exec basalt-panel delv @$NODEIP -a /tmp/anchor +root=zone.test nope.zone.test A 2>&1)
echo "$NEG" | grep -E "validated|resolution failed" | sed 's/^/  nope.zone.test: /'
echo "$NEG" | grep -q "negative response, fully validated" || { echo "$NEG"; exit 1; }
echo "### the PHP site with a database on the panel's server too: files, database copy, primary switch"
PSRV=$($API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; print([s["id"] for s in json.load(sys.stdin)["servers"] if s["name"]!="node1"][0])')
$API -X PUT -d "{\"ips\":[\"$PANELIP\"]}" http://127.0.0.1:8080/api/v1/servers/$PSRV/public-ips >/dev/null
read -r DBID DBPW <<<"$($API -d '{"name":"data"}' http://127.0.0.1:8080/api/v1/sites/$SITE/databases | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["database"]["id"], d["credentials"]["password"])')"
DBN="s${SITE}_data"
docker exec basalt-node1 mariadb "$DBN" -e "CREATE TABLE hits (id INT AUTO_INCREMENT PRIMARY KEY, at VARCHAR(16)); INSERT INTO hits (at) VALUES ('node1')"
# A page that writes through "localhost" like WordPress does, and counts.
printf '<?php $m = new mysqli("localhost", "%s", "%s", "%s"); $m->query("INSERT INTO hits (at) VALUES (\\"" . gethostname() . "\\")"); echo "rows ", $m->query("SELECT COUNT(*) FROM hits")->fetch_row()[0];' "$DBN" "$DBPW" "$DBN" > /tmp/basalt-db.php
docker cp /tmp/basalt-db.php basalt-panel:/root/db.php
$API -X PUT -H Content-Type:application/octet-stream --data-binary @/root/db.php "http://127.0.0.1:8080/api/v1/sites/$SITE/files/content?path=/public/db.php" >/dev/null
R=$($API -d "{\"server_id\":$PSRV}" http://127.0.0.1:8080/api/v1/sites/$SITE/replicas)
echo "$R" | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.exit(0 if "placements" in d else d)'
copied() { # replica server: its copy holds the site's database and copies
  $API "http://127.0.0.1:8080/api/v1/sites/$SITE" | python3 -c '
import sys, json
p = [p for p in json.load(sys.stdin)["placements"] if p["server_id"] == '"$1"'][0]
d = p.get("databases") or {}
print("  synced:", p.get("synced_at"), p.get("sync_error", ""), "databases:", d)
sys.exit(0 if p.get("synced_at") and d.get("running") and d.get("copied") == ["'"$DBN"'"] else 1)'
}
for i in $(seq 90); do copied "$PSRV" >/dev/null && break; sleep 2; done
copied "$PSRV" || exit 1
# MariaDB of the panel's server was restarted once, by systemd, to turn its binary log on.
docker exec basalt-panel mariadb -N -e "SELECT @@log_bin, @@server_id, @@gtid_domain_id" | sed 's/^/  binlog, server id, domain: /'
[ "$(docker exec basalt-panel mariadb -N -e 'SELECT @@log_bin')" = 1 ] || { echo "binary log off on the panel"; exit 1; }
for i in $(seq 30); do
  [ "$(docker exec basalt-panel mariadb -N -e "SELECT COUNT(*) FROM $DBN.hits" 2>/dev/null)" = 1 ] && break; sleep 1
done
[ "$(docker exec basalt-panel mariadb -N -e "SELECT COUNT(*) FROM $DBN.hits")" = 1 ] || { echo "rows not copied"; exit 1; }
# The copy's files belong to the site's own user there, its PHP runs as that user.
docker exec basalt-panel stat -c '  copy: %n %U:%G %a' /srv/basalt/sites/$SITE/public/index.php /srv/basalt/sites/$SITE/public
[ "$(docker exec basalt-panel stat -c %U /srv/basalt/sites/$SITE/public/db.php)" = "basalt-s$SITE" ] || { echo "the copy's files are not the site user's"; exit 1; }
for i in $(seq 30); do
  docker exec basalt-panel curl -sk --resolve shop.test:443:$PANELIP https://shop.test/ | grep -q "as basalt-s$SITE" && break; sleep 1
done
echo "  copy serves: $(docker exec basalt-panel curl -sk --resolve shop.test:443:$PANELIP https://shop.test/)"
docker exec basalt-panel curl -sk --resolve shop.test:443:$PANELIP https://shop.test/ | grep -q "as basalt-s$SITE" || exit 1
# The copy writes into the primary's database through the agents' tunnel.
W=$(docker exec basalt-panel curl -sk --resolve shop.test:443:$PANELIP https://shop.test/db.php)
echo "  copy writes: $W"
[ "$W" = "rows 2" ] || exit 1
[ "$(docker exec basalt-node1 mariadb -N -e "SELECT at FROM $DBN.hits ORDER BY id DESC LIMIT 1")" = panel ] || { echo "the copy did not write on the primary"; exit 1; }
# The site's user may only read its local copy (root could write anyway).
docker exec basalt-panel mariadb -u "$DBN" -p"$DBPW" "$DBN" -e "INSERT INTO hits (at) VALUES ('x')" 2>/dev/null && { echo "the copy's database takes writes"; exit 1; }
[ "$(docker exec basalt-panel mariadb -N -u "$DBN" -p"$DBPW" "$DBN" -e 'SELECT COUNT(*) FROM hits')" -ge 1 ] || { echo "the site's user cannot read its copy"; exit 1; }
switch() { # server
  $API -X PUT -d "{\"server_id\":$1}" "http://127.0.0.1:8080/api/v1/sites/$SITE/primary" | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get("site",{}).get("server_id")=='"$1"' else d)'
}
echo "### planned switch to the panel's server and back"
switch "$PSRV"
for i in $(seq 90); do copied "$NODE" >/dev/null && break; sleep 2; done
copied "$NODE" || exit 1
W=$(docker exec basalt-panel curl -sk --resolve shop.test:443:$PANELIP https://shop.test/db.php)
echo "  new primary writes: $W"
[ "$W" = "rows 3" ] || exit 1
for i in $(seq 30); do
  W=$(docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/db.php)
  [ "$W" = "rows 4" ] && break; sleep 1
done
echo "  former primary, now a copy, writes on the new one: $W"
[ "$W" = "rows 4" ] || exit 1
[ "$(docker exec basalt-panel mariadb -N -e "SELECT at FROM $DBN.hits ORDER BY id DESC LIMIT 1")" = node1 ] || { echo "the copy did not write on the new primary"; exit 1; }
docker exec basalt-node1 mariadb -u "$DBN" -p"$DBPW" "$DBN" -e "INSERT INTO hits (at) VALUES ('x')" 2>/dev/null && { echo "the former primary still takes writes"; exit 1; }
switch "$NODE"
for i in $(seq 90); do copied "$PSRV" >/dev/null && break; sleep 2; done
copied "$PSRV" || exit 1
W=$(docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/db.php)
echo "  back on node1: $W"
[ "$W" = "rows 5" ] || exit 1
echo "### node1 fails (its agent hangs: no panel, no tunnel): forced switch, node1 comes back as a copy"
rows() { # host: rows in its copy of the site's database
  docker exec basalt-$1 mariadb -N -e "SELECT COUNT(*) FROM $DBN.hits" 2>/dev/null
}
for i in $(seq 60); do [ "$(rows panel)" = 5 ] && break; sleep 1; done
[ "$(rows panel)" = 5 ] || { echo "the copy is behind before the failure"; exit 1; }
docker exec basalt-node1 systemctl kill -s STOP basalt-agent
online() { # server id
  $API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; sys.exit(0 if [s for s in json.load(sys.stdin)["servers"] if s["id"]=='"$1"'][0]["status"]=="online" else 1)'
}
for i in $(seq 120); do online "$NODE" || break; sleep 1; done
online "$NODE" && { echo "the panel still sees node1 online"; exit 1; }
CODE=$($API -o /tmp/r -w '%{http_code}' -X PUT -d "{\"server_id\":$PSRV}" "http://127.0.0.1:8080/api/v1/sites/$SITE/primary")
[ "$CODE" = 409 ] || { echo "switch from an offline primary without force: $CODE"; exit 1; }
echo "  without force: $($API -X PUT -d "{\"server_id\":$PSRV}" "http://127.0.0.1:8080/api/v1/sites/$SITE/primary" | cut -c1-160)"
$API -X PUT -d "{\"server_id\":$PSRV,\"force\":true}" "http://127.0.0.1:8080/api/v1/sites/$SITE/primary" | python3 -c 'import sys,json; d=json.load(sys.stdin); print("  forced:", d["site"]["server_id"], d.get("warning", "")); sys.exit(0 if d["site"]["server_id"]=='"$PSRV"' else 1)'
for i in $(seq 30); do
  W=$(docker exec basalt-panel curl -sk --resolve shop.test:443:$PANELIP https://shop.test/db.php)
  [ "$W" = "rows 6" ] && break; sleep 1
done
echo "  new primary writes: $W"
[ "$W" = "rows 6" ] || exit 1
# What node1 does alone meanwhile is lost once it is back.
docker exec basalt-node1 mariadb "$DBN" -e "INSERT INTO hits (at) VALUES ('lost')"
docker exec basalt-node1 sh -c "echo lost > /srv/basalt/sites/$SITE/public/lost.txt"
docker exec basalt-node1 systemctl kill -s CONT basalt-agent
for i in $(seq 120); do online "$NODE" && break; sleep 1; done
online "$NODE" || { echo "node1 did not come back"; exit 1; }
for i in $(seq 120); do copied "$NODE" >/dev/null && [ "$(rows node1)" = 6 ] && ! docker exec basalt-node1 test -e /srv/basalt/sites/$SITE/public/lost.txt && break; sleep 2; done
copied "$NODE" || exit 1
echo "  node1, a copy now: rows $(rows node1), lost row: $(docker exec basalt-node1 mariadb -N -e "SELECT COUNT(*) FROM $DBN.hits WHERE at='lost'")"
[ "$(rows node1)" = 6 ] && [ "$(docker exec basalt-node1 mariadb -N -e "SELECT COUNT(*) FROM $DBN.hits WHERE at='lost'")" = 0 ] || { echo "node1's copy differs from the new primary"; exit 1; }
docker exec basalt-node1 test -e /srv/basalt/sites/$SITE/public/lost.txt && { echo "node1 kept a file the primary does not have"; exit 1; }
docker exec basalt-node1 mariadb -u "$DBN" -p"$DBPW" "$DBN" -e "INSERT INTO hits (at) VALUES ('x')" 2>/dev/null && { echo "node1's copy takes writes"; exit 1; }
switch "$NODE"
for i in $(seq 90); do copied "$PSRV" >/dev/null && break; sleep 2; done
copied "$PSRV" || exit 1
W=$(docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/db.php)
echo "  back on node1: $W"
[ "$W" = "rows 7" ] || exit 1
$API -X DELETE "http://127.0.0.1:8080/api/v1/sites/$SITE/replicas/$PSRV" >/dev/null
for i in $(seq 30); do
  [ -z "$(docker exec basalt-panel mariadb -N -e "SHOW DATABASES LIKE '$DBN'")" ] && break; sleep 1
done
[ -z "$(docker exec basalt-panel mariadb -N -e "SHOW DATABASES LIKE '$DBN'")" ] || { echo "the copy's database stays after removing the replica"; exit 1; }
$API -X DELETE "http://127.0.0.1:8080/api/v1/sites/$SITE/databases/$DBID" >/dev/null
echo "  replica removed, copy dropped"
echo "### mail server on node1: domain, mailbox, SMTP in, IMAPS out"
MAIL=$($API -d "{\"server_id\":$NODE,\"hostname\":\"mail.shop.test\",\"memory_mb\":1024}" http://127.0.0.1:8080/api/v1/mail | python3 -c 'import sys,json; print(json.load(sys.stdin)["server"]["id"])')
for i in $(seq 180); do
  MST=$($API http://127.0.0.1:8080/api/v1/apps/$MAIL/deployments | python3 -c 'import sys,json; print(json.load(sys.stdin)["deployments"][0]["status"])')
  [ "$MST" != running ] && break; sleep 2
done
echo "  mail deployment: $MST"
[ "$MST" = succeeded ] || { $API http://127.0.0.1:8080/api/v1/apps/$MAIL/deployments | head -c 3000; exit 1; }
for i in $(seq 30); do
  MD=$($API -d '{"name":"shop.test"}' http://127.0.0.1:8080/api/v1/mail/domains | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("domain",{}).get("id",""))' 2>/dev/null)
  [ -n "$MD" ] && break; sleep 2
done
[ -n "$MD" ] || { echo "mail domain not created"; exit 1; }
$API -d '{"local":"alice","password":"installer mail pass"}' http://127.0.0.1:8080/api/v1/mail/domains/$MD/mailboxes >/dev/null
for i in $(seq 30); do
  [ "$(docker exec basalt-panel dig +short @$NODEIP shop.test MX)" = "10 mail.shop.test." ] && break; sleep 1
done
echo "  MX: $(docker exec basalt-panel dig +short @$NODEIP shop.test MX)"
printf 'From: tester@example.net\r\nTo: alice@shop.test\r\nSubject: installer test\r\n\r\nHello.\r\n' | docker exec -i basalt-panel sh -c 'cat >/tmp/mail.txt'
for i in $(seq 30); do
  docker exec basalt-panel curl -sS --url "smtp://$NODEIP:25/client.example.net" --mail-from tester@example.net --mail-rcpt alice@shop.test -T /tmp/mail.txt && break; sleep 2
done
# A message from an unknown sender may land in Junk Mail: look in both.
for i in $(seq 30); do
  for BOX in INBOX Junk%20Mail; do
    MSGS=$(docker exec basalt-panel curl -sSk --resolve mail.shop.test:993:$NODEIP -u 'alice@shop.test:installer mail pass' "imaps://mail.shop.test:993/$BOX" -X 'SEARCH SUBJECT "installer test"' 2>&1)
    echo "$MSGS" | grep -q "SEARCH [0-9]" && break 2
  done
  sleep 2
done
echo "  folder: $BOX"
echo "  IMAPS search: $MSGS"
echo "$MSGS" | grep -q "SEARCH [0-9]" || { echo "mail not delivered"; exit 1; }
# Deliverability checks run on node1 with the host's resolvers (the
# test zones are not public, so only their completeness is checked).
$API -d '{}' http://127.0.0.1:8080/api/v1/mail/checks | python3 -c '
import sys, json
c = json.load(sys.stdin)["checks"]
assert not c.get("error"), c["error"]
for r in c["results"]:
    print("  check %-14s %-8s %s" % (r.get("domain", "") + " " + r["check"], r["status"], r["detail"][:90]))
got = {r.get("domain", "") + " " + r["check"] for r in c["results"]}
want = {" outbound_smtp", " hostname", " ptr", " blocklist"} | {"shop.test " + k for k in ("mx", "spf", "dkim", "dmarc")}
assert got == want, got
'
echo "### panel backup, then recovery from it"
ST=$($API -d '{"name":"local","kind":"local","path":"/var/backups/basalt"}' http://127.0.0.1:8080/api/v1/backup-storages | python3 -c 'import sys,json; print(json.load(sys.stdin)["storage"]["id"])')
$API -X PUT -d "{\"storage_id\":$ST,\"schedule\":\"daily\",\"at_minute\":0,\"keep_last\":3}" http://127.0.0.1:8080/api/v1/panel-backup/policy >/dev/null
$API -X PUT -d '{"password":"installer test password","passphrase":"orange ladder of quiet mornings"}' http://127.0.0.1:8080/api/v1/panel-backup/recovery >/dev/null
BK=$($API -d '{}' http://127.0.0.1:8080/api/v1/panel-backup/run | python3 -c 'import sys,json; print(json.load(sys.stdin)["backup"]["id"])')
for i in $(seq 60); do
  ST2=$($API http://127.0.0.1:8080/api/v1/panel-backup/runs/$BK | python3 -c 'import sys,json; print(json.load(sys.stdin)["backup"]["status"])')
  [ "$ST2" != running ] && break; sleep 1
done
echo "  panel backup: $ST2"
[ "$ST2" = succeeded ] || { $API http://127.0.0.1:8080/api/v1/panel-backup/runs/$BK; exit 1; }
PW=$($API http://127.0.0.1:8080/api/v1/backup-storages/$ST/password | python3 -c 'import sys,json; print(json.load(sys.stdin)["password"])')
docker exec basalt-panel systemctl stop basalt-server
docker exec -e RESTIC_PASSWORD="$PW" basalt-panel restic -r /var/backups/basalt/panel restore latest --target /root/restore >/dev/null
docker exec -e BASALT_RECOVERY_PASSPHRASE="orange ladder of quiet mornings" basalt-panel basalt server recover --from /root/restore --force
docker exec basalt-panel systemctl start basalt-server
for i in $(seq 60); do docker exec basalt-panel curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done
# Agents retry with a backoff of up to a minute: wait for both.
for i in $(seq 120); do
  [ "$($API http://127.0.0.1:8080/api/v1/servers | grep -o '"status":"online"' | wc -l)" -ge 3 ] && break; sleep 1
done
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; l=json.load(sys.stdin)["servers"]; [print("  after recovery:", s["name"], s["status"]) for s in l]; sys.exit(0 if all(s["status"]=="online" for s in l) else "an agent did not reconnect")'
# The standby follows the recovered panel (a new snapshot).
for i in $(seq 60); do standby_ready >/dev/null && break; sleep 1; done
standby_ready
echo "### update from the panel: a signed newer release reaches both servers through systemd"
docker run -d --name basalt-releases --network basalt-net -v "$PWD/$RT":/www:ro busybox:1.37 httpd -f -p 80 -h /www >/dev/null
for host in panel node1 standby; do
  docker exec basalt-$host sh -c "mkdir -p /etc/systemd/system/basalt-agent.service.d && printf '[Service]\nEnvironment=\"BASALT_RELEASE_KEY=%s\"\n' '$RELKEY' >/etc/systemd/system/basalt-agent.service.d/release.conf && systemctl daemon-reload && systemctl restart basalt-agent"
done
docker exec basalt-panel sh -c "mkdir -p /etc/systemd/system/basalt-server.service.d && printf '[Service]\nEnvironment=BASALT_RELEASE_URL=http://basalt-releases BASALT_RELEASE_ALLOW_PRIVATE=1 \"BASALT_RELEASE_KEY=%s\"\n' '$RELKEY' >/etc/systemd/system/basalt-server.service.d/release.conf && systemctl daemon-reload && systemctl restart basalt-server"
for i in $(seq 60); do docker exec basalt-panel curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done
for i in $(seq 120); do
  [ "$($API http://127.0.0.1:8080/api/v1/servers | grep -o '"status":"online"' | wc -l)" -ge 3 ] && break; sleep 1
done
$API -d '{}' http://127.0.0.1:8080/api/v1/updates/check | python3 -c 'import sys,json; u=json.load(sys.stdin); print("  latest:", u["latest"], "available:", u["available"], u["error"]); sys.exit(0 if u["latest"]=="v9.9.9" and u["available"] else "no update found")'
$API -d '{}' http://127.0.0.1:8080/api/v1/updates/apply >/dev/null
for i in $(seq 180); do
  V=$($API http://127.0.0.1:8080/api/v1/updates 2>/dev/null | python3 -c 'import sys,json; u=json.load(sys.stdin); print(" ".join(sorted(s["version"] for s in u["servers"] if s["online"])), u["current"])' 2>/dev/null || true)
  [ "$V" = "v9.9.9 v9.9.9 v9.9.9 v9.9.9" ] && break; sleep 1
done
echo "  versions (servers, panel): $V"
[ "$V" = "v9.9.9 v9.9.9 v9.9.9 v9.9.9" ] || { $API http://127.0.0.1:8080/api/v1/updates; exit 1; }
for host in panel node1 standby; do
  docker exec basalt-$host basalt version | grep -q " v9.9.9 " || { echo "$host runs another binary"; exit 1; }
  docker exec basalt-$host systemctl is-active -q basalt-proxy basalt-agent || { echo "$host services down after the update"; exit 1; }
done
for i in $(seq 30); do
  docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " && break; sleep 1
done
docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " || { echo "site down after the update"; exit 1; }
for i in $(seq 60); do standby_ready >/dev/null && break; sleep 1; done
standby_ready
echo "### the panel fails: the standby is promoted and the servers move to it by themselves"
agentcfg() { # host field
  docker exec basalt-$1 cat /var/lib/basalt/agent/agent.json | python3 -c 'import sys,json; print(json.load(sys.stdin).get("'$2'", ""))'
}
echo "  node1's agent knows: $(agentcfg node1 panels)"
[ "$(agentcfg node1 panels | grep -o https | wc -l | tr -d " ")" = 3 ] || { echo "the agents do not know the standbys"; exit 1; }
docker exec basalt-standby basalt server promote >/dev/null 2>&1 && { echo "promoted while the standby runs"; exit 1; }
# The standby takes over by itself after a minute without the panel, if
# node1 (the only server besides the panel's and its own) agrees.
SBID=$($API http://127.0.0.1:8080/api/v1/standbys | python3 -c 'import sys,json; print([s["id"] for s in json.load(sys.stdin)["standbys"] if s["name"]=="spare"][0])')
$API -X PUT -d "{\"standby_id\":$SBID,\"after_s\":60}" http://127.0.0.1:8080/api/v1/standbys/failover | python3 -c 'import sys,json; f=json.load(sys.stdin)["failover"]; print("  automatic takeover:", f); sys.exit(0 if [w["name"] for w in f["witnesses"]]==["node1"] else "the witnesses should be node1 only")'
for i in $(seq 30); do docker exec basalt-standby curl -fsS http://127.0.0.1:8080/healthz | grep -q '"auto_failover":true' && break; sleep 1; done
docker exec basalt-standby curl -fsS http://127.0.0.1:8080/healthz | grep -q '"auto_failover":true' || { echo "the standby does not know it takes over"; exit 1; }
docker exec basalt-panel systemctl stop basalt-server
echo "  panel stopped at $(date -u +%T); waiting for the standby to take over"
for i in $(seq 150); do
  docker exec basalt-standby curl -fsS http://127.0.0.1:8080/api/v1/standby >/dev/null 2>&1 || { docker exec basalt-standby curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; }
  sleep 1
done
echo "  took over at $(date -u +%T)"
docker exec basalt-standby journalctl -u basalt-server --no-pager -o cat | grep -m1 "taking over the panel" | cut -c1-200
docker exec basalt-standby test ! -e /var/lib/basalt/server/standby.json || { echo "the standby did not take over"; exit 1; }
docker exec basalt-standby systemctl is-active -q basalt-server || { echo "the panel is not running there"; exit 1; }
OLD=$API
API="docker exec basalt-standby curl -sS -b /tmp/j -c /tmp/j -H Content-Type:application/json"
$API -d '{"email":"admin@example.test","password":"installer test password"}' http://127.0.0.1:8080/api/v1/auth/login >/dev/null
for i in $(seq 30); do $API "http://127.0.0.1:8080/api/v1/audit?action=panel.promoted" | grep -q '"auto":true' && break; sleep 1; done
$API "http://127.0.0.1:8080/api/v1/audit?action=panel.promoted" | python3 -c 'import sys,json; e=json.load(sys.stdin)["entries"]; print("  audit:", [(x["action"], x["details"]) for x in e]); sys.exit(0 if e and e[0]["details"].get("auto") else "no record of the automatic takeover")'
for i in $(seq 180); do
  [ "$(agentcfg node1 epoch)" = 1 ] && [ "$(agentcfg panel epoch)" = 1 ] && [ "$(agentcfg standby epoch)" = 1 ] && break
  sleep 1
done
for host in node1 panel standby; do
  echo "  $host's agent: $(agentcfg $host panel_url), epoch $(agentcfg $host epoch)"
  [ "$(agentcfg $host epoch)" = 1 ] || { echo "$host's agent is not on the new panel"; exit 1; }
done
$API http://127.0.0.1:8080/api/v1/panel | python3 -c 'import sys,json; p=json.load(sys.stdin)["panel"]; print("  new panel:", p); sys.exit(0 if p["epoch"]==1 and not p["superseded"] else 1)'
$API http://127.0.0.1:8080/api/v1/sites | python3 -c 'import sys,json; l=json.load(sys.stdin)["sites"]; print("  its sites:", [s["name"] for s in l]); sys.exit(0 if any(s["name"]=="shop" for s in l) else "data missing")'
docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " || { echo "site down after the takeover"; exit 1; }
ping_node1() { $API -X POST http://127.0.0.1:8080/api/v1/servers/$NODE/ping | python3 -c 'import sys,json; print("  ping node1 from the new panel:", json.load(sys.stdin)["rtt_ms"], "ms")'; }
ping_node1
for i in $(seq 60); do
  [ "$(docker exec basalt-panel dig +short @$NODEIP panel.zone.test A)" = "$STANDBYIP" ] && break; sleep 1
done
echo "  the panel's domain now: $(docker exec basalt-panel dig +short @$NODEIP panel.zone.test A)"
[ "$(docker exec basalt-panel dig +short @$NODEIP panel.zone.test A)" = "$STANDBYIP" ] || { echo "the panel's domain did not follow the takeover"; exit 1; }
for i in $(seq 120); do [ "$(spare2 panel)" = "https://$STANDBYIP:8443" ] && [ "$(spare2 connected)" = True ] && [ "$(spare2 ready)" = True ] && break; sleep 1; done
echo "  the second standby follows: $(spare2 panel), epoch $(spare2 epoch), ready $(spare2 ready)"
[ "$(spare2 panel)" = "https://$STANDBYIP:8443" ] && [ "$(spare2 epoch)" = 1 ] && [ "$(spare2 ready)" = True ] || { echo "the second standby did not follow the new panel"; exit 1; }
new_standbys() {
  $API http://127.0.0.1:8080/api/v1/standbys | python3 -c 'import sys,json; l=json.load(sys.stdin)["standbys"]; print("  its standbys:", [(s["name"], s["connected"], s["ready"]) for s in l]); sys.exit(0 if [s["name"] for s in l]==["spare-2"] and l[0]["connected"] and l[0]["ready"] else 1)'
}
for i in $(seq 30); do new_standbys >/dev/null && break; sleep 1; done
new_standbys
echo "### the former panel comes back: no server returns to it, it shows it was replaced"
docker exec basalt-panel systemctl start basalt-server
for i in $(seq 60); do docker exec basalt-panel curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done
for i in $(seq 60); do
  $OLD http://127.0.0.1:8080/api/v1/panel | grep -q '"superseded":{' && break; sleep 1
done
$OLD http://127.0.0.1:8080/api/v1/panel | python3 -c 'import sys,json; p=json.load(sys.stdin)["panel"]; print("  former panel:", p); sys.exit(0 if p["superseded"] and p["superseded"]["epoch"]==1 else "it does not know")'
ping_node1
# The second standby stays with the new panel.
sleep 5
[ "$(spare2 panel)" = "https://$STANDBYIP:8443" ] && [ "$(spare2 connected)" = True ] || { echo "the second standby left the new panel"; exit 1; }
docker exec basalt-node1 systemctl stop basalt-spare2
echo "### node1 removed from the panel: its agent stops connecting, sites keep running"
$API -d '{"name":"main"}' http://127.0.0.1:8080/api/v1/sites/$SITE/databases >/dev/null
docker exec basalt-node1 mariadb -N -e "SHOW DATABASES LIKE 's${SITE}\_%'" | grep -q main || { echo "site database missing"; exit 1; }
$API -X DELETE http://127.0.0.1:8080/api/v1/servers/$NODE >/dev/null
for i in $(seq 90); do docker exec basalt-node1 test -f /var/lib/basalt/agent/removed && break; sleep 1; done
docker exec basalt-node1 test -f /var/lib/basalt/agent/removed || { echo "agent did not record its removal"; exit 1; }
docker exec basalt-node1 journalctl -u basalt-agent --no-pager -o cat | grep -m1 "removed from the panel" | cut -c1-160
docker exec basalt-panel curl -sk --resolve shop.test:443:$NODEIP https://shop.test/ | grep -q "php " || { echo "site stopped with the panel's removal"; exit 1; }
echo "### uninstall keeps the data; --purge deletes it"
docker exec basalt-node1 sh /deploy/install.sh uninstall
docker exec basalt-node1 sh -c '! systemctl is-active -q basalt-agent basalt-proxy && ! test -e /usr/local/bin/basalt && test -d /srv/basalt/sites && id basalt-s'$SITE' >/dev/null' || { echo "plain uninstall removed too much or too little"; exit 1; }
docker exec basalt-node1 sh /deploy/install.sh uninstall --purge --yes
docker exec basalt-node1 sh -c '
  ! test -e /srv/basalt && ! test -e /var/lib/basalt || exit 1
  ! id basalt-s'$SITE' 2>/dev/null && ! id basalt 2>/dev/null && ! id basalt-proxy 2>/dev/null || exit 2
  ! ls /etc/php/*/fpm/pool.d/basalt-site-* /etc/systemd/system/basalt-php-site-* 2>/dev/null && ! test -e /etc/basalt || exit 3
  ! systemctl list-units --all --plain --no-legend "basalt-php-site-*" | grep -q . || exit 7
  [ -z "$(docker ps -aq --filter label=basalt.app)" ] && [ -z "$(docker volume ls -q --filter label=basalt.app)" ] && [ -z "$(docker network ls -q --filter label=basalt.app)" ] || exit 4
  [ -z "$(mariadb -N -e "SHOW DATABASES LIKE '"'"'s%\_%'"'"'")" ] || exit 5
  [ -z "$(mariadb -N -e "SELECT user FROM mysql.user WHERE user LIKE '"'"'s%\_%'"'"'")" ] || exit 6
' || { echo "purge left something behind (code $?)"; exit 1; }
echo "  node1 is clean"
echo "### done"
