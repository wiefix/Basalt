#!/bin/sh
# Basalt installer for Debian and Ubuntu (amd64, arm64).
#
#   Guided:      curl -fsSL https://github.com/wiefix/Basalt/releases/latest/download/install.sh | sh
#                asks for the panel's domain and what to install, then sets up the panel here.
#   Panel host:  sh install.sh panel [--domain panel.example.com] [--agent-host agents.example.com] [--binary ./basalt] [--with-php] [--with-mariadb] [--with-docker] [--with-dns]
#
#   --agent-host is the name or IP other servers' agents connect to (port
#   8443; default: the domain, else this server's IP). It must reach this
#   server directly: a TLS proxy (Cloudflare's orange cloud) breaks the
#   agents' mutual TLS, so with the panel's domain behind one, give a
#   DNS-only name or the IP. It is kept for later runs.
#   New server:  the one-line command shown in the panel ("Add server"), which runs
#                sh install.sh agent --panel https://HOST:8443 --token TOKEN --ca-sha256 FINGERPRINT
#
#   Standby panel: the one-line command shown in the panel (Servers, "Add standby panel"),
#                which runs sh install.sh standby --panel https://HOST:8443 --token TOKEN
#                --agent-token TOKEN --ca-sha256 FINGERPRINT [--domain panel.example.com]:
#                a copy of the panel, kept up to date, that can take over
#                (basalt server promote); its agent joins the panel as a server.
#                Servers learn its address (--agent-host, default: this server's
#                IP, not the domain, which leads to the panel until it takes over).
#
#   --with-docker turns on user namespaces (userns-remap: root in a container
#   is an unprivileged user on the host) when it installs Docker; for an
#   existing Docker, --docker-userns turns them on (its current images and
#   containers are hidden until turned off), --no-docker-userns leaves them off.
#   --with-dns makes this server an authoritative nameserver (port 53, UDP
#   and TCP); --no-dns turns that off again.
#
#   Remove:      sh install.sh uninstall [--purge] [--yes]
#                stops and removes the Basalt services and binary; sites, apps
#                and data stay. --purge also deletes them: site files, users,
#                PHP pools and services, site databases, app containers and volumes, Basalt's
#                data. Local backups in /var/backups/basalt are always kept.
#
# Running it again upgrades the binary and restarts the services; existing
# data and enrollment are kept.
set -eu

BIN=/usr/local/bin/basalt
LIB=/var/lib/basalt
SITES=/srv/basalt/sites
UNITS=/etc/systemd/system
RELEASES=https://github.com/wiefix/Basalt/releases/latest/download
# Releases are signed: SHA256SUMS.sig is an SSH signature of SHA256SUMS
# (namespace basalt-release) made with this key.
RELEASE_KEY='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBUisbbyEb5bDa9jp6Y2foeJ6cqcja04cPmm/BcLGD4v basalt-release'

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

public_ip() {
	ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit }}' ||
		hostname -I | awk '{print $1}'
}

# ask VAR QUESTION DEFAULT reads an answer from the terminal (the script
# itself may come through a pipe); an empty answer takes the default.
ask() {
	if [ -n "$3" ]; then
		printf '%s [%s]: ' "$2" "$3" >/dev/tty
	else
		printf '%s: ' "$2" >/dev/tty
	fi
	IFS= read -r _answer </dev/tty || _answer=""
	[ -n "$_answer" ] || _answer=$3
	eval "$1=\$_answer"
}

# yesno VAR QUESTION y|n sets VAR to 1 or 0.
yesno() {
	while :; do
		ask _yn "$2 (y/n)" "$3"
		case "$_yn" in
		y | Y | yes) eval "$1=1"; return ;;
		n | N | no) eval "$1=0"; return ;;
		esac
	done
}

host_ok() {
	case "$1" in
	"" | *[!A-Za-z0-9.:\[\]-]*) return 1 ;;
	esac
}

usage() {
	if [ -f "$0" ]; then
		sed -n '2,/^set -eu/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
	else
		echo "usage: sh install.sh [panel | agent | standby | uninstall] [options]; without a mode, in a terminal, it asks what to install"
	fi
	exit 2
}

# No mode (or only options such as --binary): the guided install of a panel.
MODE=${1:-}
case "$MODE" in
"" | -*) MODE=guided ;;
*) shift ;;
esac
DOMAIN="" AGENT_HOST="" BINARY="" PANEL="" TOKEN="" AGENT_TOKEN="" CA_SHA="" WITH_PHP=0 WITH_DB=0 WITH_DOCKER=0 USERNS=auto WITH_DNS=keep PURGE=0 YES=0
while [ $# -gt 0 ]; do
	case "$1" in
	--domain) DOMAIN=$2; shift 2 ;;
	--agent-host) AGENT_HOST=$2; shift 2 ;;
	--binary) BINARY=$2; shift 2 ;;
	--panel) PANEL=$2; shift 2 ;;
	--token) TOKEN=$2; shift 2 ;;
	--agent-token) AGENT_TOKEN=$2; shift 2 ;;
	--ca-sha256) CA_SHA=$2; shift 2 ;;
	--with-php) WITH_PHP=1; shift ;;
	--with-mariadb) WITH_DB=1; shift ;;
	--with-docker) WITH_DOCKER=1; shift ;;
	--with-dns) WITH_DNS=1; shift ;;
	--no-dns) WITH_DNS=0; shift ;;
	--docker-userns) USERNS=on; shift ;;
	--no-docker-userns) USERNS=off; shift ;;
	--purge) PURGE=1; shift ;;
	--yes) YES=1; shift ;;
	-h | --help) usage ;;
	*) die "unknown option $1" ;;
	esac
done

# ---------- checks ----------

[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh install.sh ...)"
[ -z "$AGENT_HOST" ] || host_ok "$AGENT_HOST" || die "--agent-host must be a host name or an IP address"
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
panel | uninstall | guided) ;;
agent) [ -n "$PANEL" ] && [ -n "$TOKEN" ] && [ -n "$CA_SHA" ] || die "agent mode needs --panel, --token and --ca-sha256" ;;
standby) [ -n "$PANEL" ] && [ -n "$TOKEN" ] && [ -n "$AGENT_TOKEN" ] && [ -n "$CA_SHA" ] || die "standby mode needs --panel, --token, --agent-token and --ca-sha256" ;;
*) usage ;;
esac

# ---------- guided install ----------

if [ "$MODE" = guided ]; then
	(exec </dev/tty) 2>/dev/null || usage # no terminal to ask on
	MODE=panel
	IP=$(public_ip)
	echo
	say "Basalt: install the panel on this server"
	echo "  The panel, its proxy and an agent run here. Other servers are added later in the"
	echo "  panel (Servers, Add server), which shows the one command to run on each."
	echo
	while :; do
		ask DOMAIN "Domain of the panel, e.g. panel.example.com (empty: this server's IP $IP)" "$DOMAIN"
		DOMAIN=$(printf '%s' "$DOMAIN" | tr 'A-Z' 'a-z')
		case "$DOMAIN" in
		"") break ;;
		*[!a-z0-9.-]* | .* | *. | *..* | -* | *-) ;;
		*.*) break ;;
		esac
		echo "  That is not a domain name."
		DOMAIN=""
	done
	RESOLVED=""
	if [ -n "$DOMAIN" ] && command -v getent >/dev/null 2>&1; then
		RESOLVED=$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk '{print $1; exit}')
		if [ -z "$RESOLVED" ]; then
			warn "$DOMAIN does not resolve yet. Add an A record $DOMAIN -> $IP; the panel gets its certificate once it does."
		elif [ "$RESOLVED" != "$IP" ]; then
			warn "$DOMAIN points to $RESOLVED, not to this server ($IP). Behind a proxy (Cloudflare's orange cloud) the panel works, but other servers' agents need a direct address below. Otherwise fix the A record."
		else
			echo "  $DOMAIN points to this server: the panel gets a Let's Encrypt certificate."
		fi
	elif [ -z "$DOMAIN" ]; then
		echo "  Without a domain the browser warns about the panel's certificate (re-run with a domain later)."
	fi
	DEFAULT_AGENT=${DOMAIN:-$IP}
	[ -z "$RESOLVED" ] || [ "$RESOLVED" = "$IP" ] || DEFAULT_AGENT=$IP
	[ ! -s "$LIB/server/agent-host" ] || DEFAULT_AGENT=$(cat "$LIB/server/agent-host")
	[ -z "$AGENT_HOST" ] || DEFAULT_AGENT=$AGENT_HOST
	while :; do
		ask AGENT_HOST "Address other servers reach this one at (port 8443, directly, not through a proxy)" "$DEFAULT_AGENT"
		host_ok "$AGENT_HOST" && break
		echo "  Give a host name or an IP address."
	done
	echo
	echo "  What should this server run besides the panel? (Each can be added later by running this again.)"
	yesno WITH_DOCKER "  Docker: apps from images, git and compose, databases, mail, the app catalog" y
	yesno WITH_PHP "  PHP-FPM: PHP sites" y
	yesno WITH_DB "  MariaDB: databases of PHP sites" y
	yesno DNS_ANSWER "  DNS server on port 53: host your domains' DNS here" n
	[ "$DNS_ANSWER" = 0 ] || WITH_DNS=1
	echo
	say "Ready to install"
	echo "  Panel:   https://${DOMAIN:-$IP}/"
	echo "  Agents:  $AGENT_HOST:8443"
	ON=""
	[ "$WITH_DOCKER" = 0 ] || ON="$ON Docker"
	[ "$WITH_PHP" = 0 ] || ON="$ON PHP"
	[ "$WITH_DB" = 0 ] || ON="$ON MariaDB"
	[ "$WITH_DNS" != 1 ] || ON="$ON DNS"
	echo "  Also:   ${ON:- nothing else}"
	yesno GO "Install now" y
	[ "$GO" = 1 ] || { echo "Nothing was changed."; exit 0; }
fi

# ---------- uninstall ----------

if [ "$MODE" = uninstall ]; then
	if [ "$PURGE" -eq 1 ] && [ "$YES" -ne 1 ]; then
		echo "This deletes every site's files and databases, the apps' containers and volumes,"
		echo "and Basalt's data on this server ($LIB, /srv/basalt). Backups in /var/backups/basalt stay."
		printf 'Type "purge" to continue: '
		read -r answer </dev/tty || answer=""
		[ "$answer" = purge ] || die "nothing was changed"
	fi
	say "Stopping the Basalt services"
	for unit in basalt-agent basalt-proxy basalt-server; do
		systemctl disable --now "$unit.service" >/dev/null 2>&1 || true
		rm -f "$UNITS/$unit.service"
	done
	systemctl daemon-reload
	rm -f "$BIN"
	if [ "$PURGE" -eq 0 ]; then
		say "Basalt is removed; its data stays"
		echo "  Kept: $LIB, /srv/basalt, the site users, PHP pools, databases and app containers."
		echo "  Run again with --purge to delete them."
		exit 0
	fi
	if command -v docker >/dev/null 2>&1; then
		say "Removing app containers, networks and volumes"
		ids=$(docker ps -aq --filter label=basalt.app 2>/dev/null || true)
		# shellcheck disable=SC2086
		[ -z "$ids" ] || docker rm -f $ids >/dev/null 2>&1 || true
		ids=$(docker network ls -q --filter label=basalt.app 2>/dev/null || true)
		# shellcheck disable=SC2086
		[ -z "$ids" ] || docker network rm $ids >/dev/null 2>&1 || true
		ids=$(docker volume ls -q --filter label=basalt.app 2>/dev/null || true)
		# shellcheck disable=SC2086
		[ -z "$ids" ] || docker volume rm -f $ids >/dev/null 2>&1 || true
	fi
	say "Removing PHP pools"
	# The sites' PHP and their Laravel processes (with their slices).
	for unit in "$UNITS"/basalt-php-site-*.service; do
		[ -e "$unit" ] || continue
		systemctl disable --now "$(basename "$unit")" >/dev/null 2>&1 || true
		rm -f "$unit"
	done
	rm -f "$UNITS"/basalt-sites-laravel*.slice
	systemctl daemon-reload
	rm -rf /etc/basalt
	reload_php=0
	for pool in /etc/php/*/fpm/pool.d/basalt-site-*.conf; do
		[ -e "$pool" ] || continue
		rm -f "$pool"
		reload_php=1
	done
	if [ "$reload_php" -eq 1 ]; then
		for unit in /lib/systemd/system/php*-fpm.service; do
			[ -e "$unit" ] && systemctl reload-or-restart "$(basename "$unit")" >/dev/null 2>&1 || true
		done
	fi
	if command -v mariadb >/dev/null 2>&1 && mariadb -N -e 'SELECT 1' >/dev/null 2>&1; then
		say "Removing site databases"
		# Basalt names them s<site>_<name>, as their users.
		mariadb -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name REGEXP '^s[0-9]+_[a-z0-9_]+\$'" |
			while read -r db; do mariadb -e "DROP DATABASE \`$db\`"; done
		mariadb -N -e "SELECT user FROM mysql.user WHERE host = 'localhost' AND user REGEXP '^s[0-9]+_[a-z0-9_]+\$'" |
			while read -r u; do mariadb -e "DROP USER '$u'@'localhost'"; done
	fi
	say "Removing site users and Basalt's data"
	for u in $(getent passwd | cut -d: -f1 | grep -E '^basalt-s[0-9]+$' || true); do
		pkill -u "$u" >/dev/null 2>&1 || true
		userdel "$u" >/dev/null 2>&1 || true
	done
	rm -rf "$LIB" /srv/basalt /run/basalt
	userdel basalt >/dev/null 2>&1 || true
	userdel basalt-proxy >/dev/null 2>&1 || true
	groupdel basalt-www >/dev/null 2>&1 || true
	say "Basalt and its data are removed"
	[ -d /var/backups/basalt ] && echo "  Backups kept in /var/backups/basalt."
	exit 0
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ---------- packages ----------

PKGS="ca-certificates curl bzip2 openssh-client"
[ "$WITH_PHP" -eq 1 ] && PKGS="$PKGS php-fpm php-mysql php-curl php-gd php-intl php-mbstring php-xml php-zip"
[ "$WITH_DB" -eq 1 ] && PKGS="$PKGS mariadb-server"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
HAD_DOCKER=0
command -v dockerd >/dev/null 2>&1 && HAD_DOCKER=1
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
if [ "$WITH_DOCKER" -eq 1 ]; then
	# User namespaces: on by default only for a Docker installed now, since
	# switching them on hides the images and containers that exist.
	[ "$USERNS" = auto ] && { [ "$HAD_DOCKER" -eq 1 ] && USERNS=off || USERNS=on; }
	if [ "$USERNS" = on ]; then
		if [ ! -s /etc/docker/daemon.json ]; then
			say "Enabling user namespaces for Docker containers"
			mkdir -p /etc/docker
			printf '{\n  "userns-remap": "default"\n}\n' >/etc/docker/daemon.json
			systemctl restart docker.service >/dev/null 2>&1 || true
		elif ! grep -q '"userns-remap"' /etc/docker/daemon.json; then
			say "Note: add \"userns-remap\": \"default\" to /etc/docker/daemon.json and restart Docker to enable user namespaces"
		fi
	fi
	systemctl enable --now docker.service >/dev/null 2>&1
fi

# ---------- restic (backups) ----------

# The distribution's restic when it is recent enough (0.17+), otherwise the
# official release, checked against these checksums.
RESTIC_VERSION=0.19.1
RESTIC_SHA256_amd64=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c
RESTIC_SHA256_arm64=a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465
restic_ok() { # version
	[ -n "$1" ] && dpkg --compare-versions "$1" ge 0.17.0 2>/dev/null
}
installed=$(restic version 2>/dev/null | awk '{print $2}')
if ! restic_ok "$installed"; then
	candidate=$(apt-cache policy restic 2>/dev/null | awk '/Candidate:/ {print $2}')
	if restic_ok "${candidate%%-*}"; then
		say "Installing restic $candidate"
		apt-get install -y -qq restic >/dev/null
	else
		say "Downloading restic $RESTIC_VERSION"
		eval "sum=\$RESTIC_SHA256_$ARCH"
		curl -fsSL --retry 3 -o "$TMP/restic.bz2" \
			"https://github.com/restic/restic/releases/download/v$RESTIC_VERSION/restic_${RESTIC_VERSION}_linux_$ARCH.bz2"
		echo "$sum  $TMP/restic.bz2" | sha256sum -c - >/dev/null 2>&1 || die "the restic download does not match its checksum"
		bunzip2 -c "$TMP/restic.bz2" >"$TMP/restic"
		install -m 0755 "$TMP/restic" /usr/local/bin/restic
	fi
fi

# ---------- railpack (apps without a Dockerfile) ----------

# The release Basalt's agents expect (its BuildKit frontend is pinned in the
# agent), checked against the checksum here.
RAILPACK_VERSION=0.43.0
RAILPACK_SHA256_amd64=248a8f60f36854aabed6124113631078a802de3461660855fc4de9c26065c1fb
RAILPACK_SHA256_arm64=732ece14eb4f4676a43ebba19431f954edf7b130acb76159ef0f2ff86ac42ac9
if [ "$WITH_DOCKER" -eq 1 ] && [ "$(railpack --version 2>/dev/null | awk '{print $3}')" != "$RAILPACK_VERSION" ]; then
	say "Downloading Railpack $RAILPACK_VERSION"
	case "$ARCH" in amd64) rp_arch=x86_64 ;; *) rp_arch=arm64 ;; esac
	eval "sum=\$RAILPACK_SHA256_$ARCH"
	curl -fsSL --retry 3 -o "$TMP/railpack.tar.gz" \
		"https://github.com/railwayapp/railpack/releases/download/v$RAILPACK_VERSION/railpack-v$RAILPACK_VERSION-$rp_arch-unknown-linux-musl.tar.gz"
	echo "$sum  $TMP/railpack.tar.gz" | sha256sum -c - >/dev/null 2>&1 || die "the Railpack download does not match its checksum"
	mkdir -p "$TMP/railpack"
	tar -xzf "$TMP/railpack.tar.gz" -C "$TMP/railpack" railpack
	install -m 0755 "$TMP/railpack/railpack" /usr/local/bin/railpack
fi

# ---------- binary ----------

# fetch_from_panel PATH OUT: download from the panel's agent channel, trusting
# only its CA, which is verified against the fingerprint from the command.
PANEL_HOSTPORT=""
fetch_from_panel() {
	curl -fsS --retry 3 --cacert "$TMP/ca.pem" \
		--connect-to "basalt-panel:443:$PANEL_HOSTPORT" \
		"https://basalt-panel$1" -o "$2"
}

# A panel's host, or a standby panel's: it runs the panel's service.
PANEL_HOST=0
case "$MODE" in panel | standby) PANEL_HOST=1 ;; esac

if [ "$MODE" = agent ] || [ "$MODE" = standby ]; then
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
	curl -fsSL --retry 3 "$RELEASES/SHA256SUMS.sig" -o "$TMP/SHA256SUMS.sig"
	echo "basalt-release $RELEASE_KEY" >"$TMP/allowed_signers"
	ssh-keygen -Y verify -f "$TMP/allowed_signers" -I basalt-release -n basalt-release \
		-s "$TMP/SHA256SUMS.sig" <"$TMP/SHA256SUMS" >/dev/null 2>&1 ||
		die "the release is not signed by the Basalt release key; refusing to install it"
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
if [ "$PANEL_HOST" -eq 1 ]; then
	ensure_user basalt "$LIB/server"
	install -d -m 0700 -o basalt -g basalt "$LIB/server"
fi

# ---------- addresses ----------

ADDR=${DOMAIN:-$(public_ip)}
[ -n "$ADDR" ] || ADDR=$(hostname)

# Where other servers' agents reach the panel: given once, remembered (a
# standby's agents reach it there once it takes over).
if [ "$PANEL_HOST" -eq 1 ]; then
	if [ -n "$AGENT_HOST" ]; then
		printf '%s\n' "$AGENT_HOST" >"$LIB/server/agent-host"
	elif [ -s "$LIB/server/agent-host" ]; then
		AGENT_HOST=$(cat "$LIB/server/agent-host")
	fi
	# A standby's domain still leads to the panel it copies: agents learn
	# its own address, to find it once it takes over.
	[ "$MODE" != standby ] || [ -n "$AGENT_HOST" ] || AGENT_HOST=$(public_ip)
	AGENT_HOST=${AGENT_HOST:-$ADDR}
fi

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

if [ "$PANEL_HOST" -eq 1 ]; then
	# Local backup storages default to /var/backups/basalt: agents (root)
	# write server-<id>/ there, the panel its own panel/.
	install -d -m 0711 /var/backups/basalt
	install -d -o basalt -g basalt -m 0700 /var/backups/basalt/panel
	cat >"$UNITS/basalt-server.service" <<EOF
[Unit]
Description=Basalt panel
After=network-online.target
Wants=network-online.target

[Service]
User=basalt
Group=basalt
ExecStart=$BIN server --data-dir $LIB/server --listen 127.0.0.1:8080 --agent-listen :8443 --agent-url https://$AGENT_HOST:8443 --trusted-proxies 127.0.0.1,::1 --log-format json
Restart=always
RestartSec=2
UMask=0077
ProtectSystem=strict
ReadWritePaths=$LIB/server -/var/backups/basalt/panel
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

AGENT_FLAGS=""
[ "$PANEL_HOST" -eq 1 ] && AGENT_FLAGS="--panel-domain $ADDR --panel-upstream 127.0.0.1:8080"
# An upgrade keeps DNS as it was unless --with-dns or --no-dns says otherwise.
if [ "$WITH_DNS" = keep ]; then
	WITH_DNS=0
	grep -q -- '--dns-listen' "$UNITS/basalt-agent.service" 2>/dev/null && WITH_DNS=1
fi
[ "$WITH_DNS" -eq 1 ] && AGENT_FLAGS="$AGENT_FLAGS --dns-listen :53"
# Other servers' agents reach this one here for database replication; it
# lets in only what the panel allows, with the panel's certificates.
AGENT_FLAGS="$AGENT_FLAGS --peer-listen :8444"
cat >"$UNITS/basalt-agent.service" <<EOF
[Unit]
Description=Basalt agent
After=network-online.target basalt-proxy.service
Wants=network-online.target basalt-proxy.service

[Service]
Environment=HOME=$LIB/agent
ExecStart=$BIN agent --data-dir $LIB/agent --proxy-dir $LIB/proxy --sites-root $SITES --proxy-user basalt-proxy $AGENT_FLAGS --log-format json
Restart=always
RestartSec=2
RuntimeDirectory=basalt
RuntimeDirectoryPreserve=yes
# Root, but only with the powers its work needs: managing site users and
# their files (chown, setgid folders, reading everything for backups),
# binding port 53, signalling PHP-FPM. No module loading, raw devices,
# tracing, clock or network administration.
CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER CAP_FSETID CAP_KILL CAP_SETUID CAP_SETGID CAP_NET_BIND_SERVICE CAP_AUDIT_WRITE
NoNewPrivileges=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectHome=read-only
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
LockPersonality=yes
RestrictRealtime=yes
RestrictNamespaces=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload

# ---------- start ----------

if [ "$MODE" = standby ] && [ ! -f "$LIB/server/standby.json" ]; then
	[ ! -f "$LIB/server/basalt.db" ] || die "this server holds a panel ($LIB/server/basalt.db): a standby needs a server without one"
	say "Registering this server as a standby panel"
	runuser -u basalt -- "$BIN" server standby-enroll --data-dir "$LIB/server" --panel "$PANEL" --token "$TOKEN" --ca-sha256 "$CA_SHA"
fi
if [ "$PANEL_HOST" -eq 1 ]; then
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

# Not enrolled yet, or removed from the panel and enrolling again.
if [ ! -f "$LIB/agent/agent.json" ] || [ -f "$LIB/agent/removed" ]; then
	if [ "$MODE" = panel ]; then
		say "Registering this server with the panel"
		ENROLL=$(runuser -u basalt -- "$BIN" server local-enroll --data-dir "$LIB/server" --agent-url https://127.0.0.1:8443 --name "$(hostname -s)")
	elif [ "$MODE" = standby ]; then
		say "Registering this server with the panel"
		ENROLL="--panel $PANEL --token $AGENT_TOKEN --ca-sha256 $CA_SHA"
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
	echo "  Agents:       other servers connect to $AGENT_HOST:8443. It must lead to this server"
	echo "                directly, not through a proxy such as Cloudflare's (use a DNS-only name"
	echo "                or the IP: re-run with --agent-host)."
	echo
	echo "  Open these ports: 80/tcp, 443/tcp+udp (sites), 8443/tcp (other servers' agents), 2222/tcp (SFTP), 8444/tcp between your servers (database replication)"
elif [ "$MODE" = standby ]; then
	say "This server is a standby of the panel at $PANEL"
	echo
	echo "  It keeps a copy of the panel, kept up to date (state: https://$ADDR/)."
	echo "  If the panel fails, make this server the panel:"
	echo "    systemctl stop basalt-server && basalt server promote && systemctl start basalt-server"
	echo "  Servers move here by themselves (they know its address $AGENT_HOST:8443); then point"
	echo "  the panel's domain here."
	echo
	echo "  Open these ports: 80/tcp, 443/tcp+udp (sites), 8443/tcp (agents, once it is the panel), 2222/tcp (SFTP), 8444/tcp between your servers (database replication)"
else
	say "This server is connected. Manage it in the panel."
	echo "  Open these ports: 80/tcp, 443/tcp+udp (sites), 2222/tcp (SFTP), 8444/tcp between your servers (database replication)"
fi
