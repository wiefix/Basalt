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
