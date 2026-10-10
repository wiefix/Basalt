#!/bin/sh
# Basalt installer for Debian and Ubuntu (amd64, arm64).
#
#   Panel host:  sh install.sh panel [--domain panel.example.com] [--binary ./basalt] [--with-php] [--with-mariadb] [--with-docker]
#   New server:  the one-line command shown in the panel ("Add server"), which runs
#                sh install.sh agent --panel https://HOST:8443 --token TOKEN --ca-sha256 FINGERPRINT
#
# Running it again upgrades the binary and restarts the services; existing
# data and enrollment are kept.
set -eu

BIN=/usr/local/bin/basalt
LIB=/var/lib/basalt
SITES=/srv/basalt/sites
UNITS=/etc/systemd/system
RELEASES=https://github.com/wiefix/Basalt/releases/latest/download

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
	sed -n '2,10p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'
	exit 2
}

MODE=${1:-}
[ -n "$MODE" ] || usage
shift
DOMAIN="" BINARY="" PANEL="" TOKEN="" CA_SHA="" WITH_PHP=0 WITH_DB=0 WITH_DOCKER=0
while [ $# -gt 0 ]; do
	case "$1" in
	--domain) DOMAIN=$2; shift 2 ;;
	--binary) BINARY=$2; shift 2 ;;
	--panel) PANEL=$2; shift 2 ;;
	--token) TOKEN=$2; shift 2 ;;
	--ca-sha256) CA_SHA=$2; shift 2 ;;
	--with-php) WITH_PHP=1; shift ;;
	--with-mariadb) WITH_DB=1; shift ;;
	--with-docker) WITH_DOCKER=1; shift ;;
	-h | --help) usage ;;
	*) die "unknown option $1" ;;
	esac
done

# ---------- checks ----------

[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh install.sh ...)"
[ -r /etc/os-release ] || die "cannot detect the operating system"
. /etc/os-release
case " ${ID:-} ${ID_LIKE:-} " in
*" debian "* | *" ubuntu "*) ;;
*) die "Debian or Ubuntu is required (found ${PRETTY_NAME:-unknown})" ;;
esac
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported CPU architecture $(uname -m)" ;;
esac
case "$MODE" in
panel) ;;
agent) [ -n "$PANEL" ] && [ -n "$TOKEN" ] && [ -n "$CA_SHA" ] || die "agent mode needs --panel, --token and --ca-sha256" ;;
*) usage ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ---------- packages ----------

PKGS="ca-certificates curl"
[ "$WITH_PHP" -eq 1 ] && PKGS="$PKGS php-fpm php-mysql php-curl php-gd php-intl php-mbstring php-xml php-zip"
[ "$WITH_DB" -eq 1 ] && PKGS="$PKGS mariadb-server"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
if [ "$WITH_DOCKER" -eq 1 ]; then
	# git and ssh clone repositories of apps built from source; buildx gives
	# the docker command BuildKit (older releases lack the package).
	PKGS="$PKGS docker.io git openssh-client"
	apt-cache show docker-buildx >/dev/null 2>&1 && PKGS="$PKGS docker-buildx"
fi
say "Installing packages: $PKGS"
# shellcheck disable=SC2086
apt-get install -y -qq $PKGS >/dev/null
# Packages do not always start their services (e.g. policy-rc.d in images).
if [ "$WITH_PHP" -eq 1 ]; then
	for unit in /lib/systemd/system/php*-fpm.service; do
		[ -e "$unit" ] && systemctl enable --now "$(basename "$unit")" >/dev/null 2>&1
	done
fi
[ "$WITH_DB" -eq 1 ] && systemctl enable --now mariadb.service >/dev/null 2>&1
[ "$WITH_DOCKER" -eq 1 ] && systemctl enable --now docker.service >/dev/null 2>&1

# ---------- binary ----------

# fetch_from_panel PATH OUT: download from the panel's agent channel, trusting
# only its CA, which is verified against the fingerprint from the command.
PANEL_HOSTPORT=""
fetch_from_panel() {
	curl -fsS --retry 3 --cacert "$TMP/ca.pem" \
		--connect-to "basalt-panel:443:$PANEL_HOSTPORT" \
		"https://basalt-panel$1" -o "$2"
}

if [ "$MODE" = agent ]; then
	PANEL_HOSTPORT=${PANEL#https://}
	PANEL_HOSTPORT=${PANEL_HOSTPORT%%/*}
	case "$PANEL_HOSTPORT" in *:*) ;; *) PANEL_HOSTPORT="$PANEL_HOSTPORT:443" ;; esac
	say "Verifying the panel's certificate authority"
	curl -fsSk --retry 3 "https://$PANEL_HOSTPORT/agent/v1/ca.der" -o "$TMP/ca.der"
	echo "$CA_SHA  $TMP/ca.der" | sha256sum -c --quiet - || die "the panel's CA does not match the fingerprint; refusing to continue"
	{
		echo "-----BEGIN CERTIFICATE-----"
		base64 -w 64 "$TMP/ca.der"
		echo "-----END CERTIFICATE-----"
	} >"$TMP/ca.pem"
	[ -n "$BINARY" ] || { say "Downloading basalt from the panel"; fetch_from_panel "/agent/v1/binary/linux-$ARCH" "$TMP/basalt"; BINARY="$TMP/basalt"; }
elif [ -z "$BINARY" ]; then
	say "Downloading basalt for linux-$ARCH"
	curl -fsSL --retry 3 "$RELEASES/basalt-linux-$ARCH" -o "$TMP/basalt"
	curl -fsSL --retry 3 "$RELEASES/SHA256SUMS" -o "$TMP/SHA256SUMS"
	(cd "$TMP" && grep " basalt-linux-$ARCH\$" SHA256SUMS | sed "s/basalt-linux-$ARCH/basalt/" | sha256sum -c --quiet -) ||
		die "checksum mismatch for the downloaded binary"
	BINARY="$TMP/basalt"
fi
[ -f "$BINARY" ] || die "binary $BINARY not found"
[ -x "$BINARY" ] || chmod 0755 "$BINARY"
"$BINARY" version >/dev/null 2>&1 || die "$BINARY does not run on this machine (wrong architecture?)"
install -m 0755 "$BINARY" "$BIN.new"
mv -f "$BIN.new" "$BIN"
say "Installed $($BIN version)"

# ---------- users and directories ----------

ensure_user() { # name home
	id "$1" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --home-dir "$2" --shell /usr/sbin/nologin "$1"
}
getent group basalt-www >/dev/null || groupadd --system basalt-www
ensure_user basalt-proxy "$LIB/proxy"
usermod --append --groups basalt-www basalt-proxy
install -d -m 0755 "$LIB" /srv/basalt "$SITES"
install -d -m 0700 -o root -g root "$LIB/agent"
install -d -m 0700 -o basalt-proxy -g basalt-proxy "$LIB/proxy"
if [ "$MODE" = panel ]; then
	ensure_user basalt "$LIB/server"
	install -d -m 0700 -o basalt -g basalt "$LIB/server"
fi

# ---------- addresses ----------

public_ip() {
	ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit }}' ||
		hostname -I | awk '{print $1}'
}
ADDR=${DOMAIN:-$(public_ip)}
[ -n "$ADDR" ] || ADDR=$(hostname)

# ---------- systemd units ----------

HARDEN='NoNewPrivileges=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native'

if [ "$MODE" = panel ]; then
	cat >"$UNITS/basalt-server.service" <<EOF
[Unit]
Description=Basalt panel
After=network-online.target
Wants=network-online.target

[Service]
User=basalt
Group=basalt
ExecStart=$BIN server --data-dir $LIB/server --listen 127.0.0.1:8080 --agent-listen :8443 --agent-url https://$ADDR:8443 --trusted-proxies 127.0.0.1,::1 --log-format json
Restart=always
RestartSec=2
UMask=0077
ProtectSystem=strict
ReadWritePaths=$LIB/server
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
SystemCallFilter=@system-service
$HARDEN

[Install]
WantedBy=multi-user.target
EOF
fi

cat >"$UNITS/basalt-proxy.service" <<EOF
[Unit]
Description=Basalt web proxy
After=network-online.target
Wants=network-online.target

[Service]
User=basalt-proxy
Group=basalt-proxy
SupplementaryGroups=basalt-www
ExecStart=$BIN proxy --data-dir $LIB/proxy --log-format json
Restart=always
RestartSec=2
Environment=XDG_DATA_HOME=$LIB/proxy XDG_CONFIG_HOME=$LIB/proxy
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ReadWritePaths=$LIB/proxy
ReadOnlyPaths=/srv/basalt
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
SystemCallFilter=@system-service
LimitNOFILE=1048576
$HARDEN

[Install]
WantedBy=multi-user.target
EOF

PANEL_FLAGS=""
[ "$MODE" = panel ] && PANEL_FLAGS="--panel-domain $ADDR --panel-upstream 127.0.0.1:8080"
cat >"$UNITS/basalt-agent.service" <<EOF
[Unit]
Description=Basalt agent
After=network-online.target basalt-proxy.service
Wants=network-online.target basalt-proxy.service

[Service]
Environment=HOME=$LIB/agent
ExecStart=$BIN agent --data-dir $LIB/agent --proxy-dir $LIB/proxy --sites-root $SITES --proxy-user basalt-proxy $PANEL_FLAGS --log-format json
Restart=always
RestartSec=2
RuntimeDirectory=basalt
RuntimeDirectoryPreserve=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
LockPersonality=yes
RestrictRealtime=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload

# ---------- start ----------

if [ "$MODE" = panel ]; then
	systemctl enable --now basalt-server.service >/dev/null 2>&1
	systemctl restart basalt-server.service
	say "Waiting for the panel"
	i=0
	until curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -lt 60 ] || die "the panel did not start; see: journalctl -u basalt-server"
		sleep 0.5
	done
fi

if [ ! -f "$LIB/agent/agent.json" ]; then
	if [ "$MODE" = panel ]; then
		say "Registering this server with the panel"
		ENROLL=$(runuser -u basalt -- "$BIN" server local-enroll --data-dir "$LIB/server" --agent-url https://127.0.0.1:8443 --name "$(hostname -s)")
	else
		say "Registering this server with the panel"
		ENROLL="--panel $PANEL --token $TOKEN --ca-sha256 $CA_SHA"
	fi
	# shellcheck disable=SC2086
	"$BIN" agent enroll --data-dir "$LIB/agent" $ENROLL
fi

systemctl enable --now basalt-proxy.service basalt-agent.service >/dev/null 2>&1
systemctl restart basalt-proxy.service basalt-agent.service

# ---------- done ----------

echo
if [ "$MODE" = panel ]; then
	say "Basalt is running"
	echo
	echo "  Open:         https://$ADDR/"
	if [ -f "$LIB/server/setup-token" ]; then
		echo "  Setup token:  $(cat "$LIB/server/setup-token")"
	fi
	case "$ADDR" in
	*[!0-9.]*) ;;
	*) echo "  (No domain given: the browser will warn about the certificate. Re-run with --domain to get Let's Encrypt.)" ;;
	esac
	echo
	echo "  Open these ports: 80/tcp, 443/tcp+udp (sites), 8443/tcp (other servers' agents), 2222/tcp (SFTP)"
else
	say "This server is connected. Manage it in the panel."
	echo "  Open these ports: 80/tcp, 443/tcp+udp (sites), 2222/tcp (SFTP)"
fi
