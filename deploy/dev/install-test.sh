#!/usr/bin/env bash
# End-to-end test of deploy/install.sh on two systemd containers:
# "panel" (install.sh panel) and "node1" (the one-line command from the panel).
set -euo pipefail
cd "$(dirname "$0")/../.."
ARCH=$(docker version --format '{{.Server.Arch}}')
make build-linux >/dev/null
docker build -q -t basalt-systemd -f deploy/dev/systemd.Dockerfile deploy/dev >/dev/null

cleanup() { docker rm -f basalt-panel basalt-node1 >/dev/null 2>&1 || true; docker network rm basalt-net >/dev/null 2>&1 || true; }
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
sleep 3

echo "### install panel"
docker exec basalt-panel sh /deploy/install.sh panel --binary /dist/basalt-linux-$ARCH

echo "### services on panel"
docker exec basalt-panel systemctl is-active basalt-server basalt-proxy basalt-agent
echo "### panel UI through the proxy"
PANELIP=$(docker inspect -f '{{(index .NetworkSettings.Networks "basalt-net").IPAddress}}' basalt-panel)
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
docker exec basalt-node1 sh -c "$CMD --with-php --with-mariadb --with-docker"
docker exec basalt-node1 systemctl is-active basalt-proxy basalt-agent
docker exec basalt-node1 sh -c 'git --version && docker buildx version | cut -d" " -f1-2' 
docker exec basalt-node1 ps -o user=,comm= -C basalt | sort | uniq -c

for i in $(seq 30); do
  $API http://127.0.0.1:8080/api/v1/servers | grep -q '"name":"node1","status":"online"' && break; sleep 1
done
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; [print(" ", s["name"], s["status"], "php:", s["php_versions"], "isolated:", s["isolated"], "docker:", s["docker"], s.get("docker_security")) for s in json.load(sys.stdin)["servers"]]'
echo "### restic for backups on every server"
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; l=json.load(sys.stdin)["servers"]; [print(" ", s["name"], "restic", s["restic"]) for s in l]; sys.exit(0 if all(s["restic"] for s in l) else "restic missing")'
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
for i in $(seq 60); do
  $API http://127.0.0.1:8080/api/v1/servers | grep -q '"name":"node1","status":"online"' && break; sleep 1
done
$API http://127.0.0.1:8080/api/v1/servers | python3 -c 'import sys,json; l=json.load(sys.stdin)["servers"]; [print("  after recovery:", s["name"], s["status"]) for s in l]; sys.exit(0 if all(s["status"]=="online" for s in l) else "an agent did not reconnect")'
echo "### done"
