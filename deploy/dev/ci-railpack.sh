#!/usr/bin/env bash
# Installs the Railpack release that deploy/install.sh pins (same version and
# checksum) into /usr/local/bin, for the Docker tests in CI.
set -euo pipefail
cd "$(dirname "$0")/../.."
version=$(sed -n 's/^RAILPACK_VERSION=//p' deploy/install.sh)
sum=$(sed -n 's/^RAILPACK_SHA256_amd64=//p' deploy/install.sh)
tmp=$(mktemp -d)
curl -fsSL --retry 3 -o "$tmp/railpack.tar.gz" \
  "https://github.com/railwayapp/railpack/releases/download/v$version/railpack-v$version-x86_64-unknown-linux-musl.tar.gz"
echo "$sum  $tmp/railpack.tar.gz" | sha256sum -c --quiet -
tar -xzf "$tmp/railpack.tar.gz" -C "$tmp" railpack
sudo install -m 0755 "$tmp/railpack" /usr/local/bin/railpack
railpack --version
