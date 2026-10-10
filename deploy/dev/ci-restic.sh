#!/usr/bin/env bash
# Installs the restic release that deploy/install.sh pins (same version and
# checksum) into /usr/local/bin, for CI runners whose packages are too old.
set -euo pipefail
cd "$(dirname "$0")/../.."
version=$(sed -n 's/^RESTIC_VERSION=//p' deploy/install.sh)
sum=$(sed -n 's/^RESTIC_SHA256_amd64=//p' deploy/install.sh)
tmp=$(mktemp -d)
curl -fsSL --retry 3 -o "$tmp/restic.bz2" \
  "https://github.com/restic/restic/releases/download/v$version/restic_${version}_linux_amd64.bz2"
echo "$sum  $tmp/restic.bz2" | sha256sum -c --quiet -
bunzip2 -c "$tmp/restic.bz2" >"$tmp/restic"
sudo install -m 0755 "$tmp/restic" /usr/local/bin/restic
restic version
