#!/bin/sh
# Installs or updates trains from the GitHub release named in VERSION (or
# the version given), for this machine's architecture. Run as root from a
# clone of the trains branch, e.g. as the dashboard's deploy step:
#   sh deploy/install-release.sh [v0.1.0]
set -eu

here=$(dirname "$0")
version=${1:-$(tr -d '[:space:]' < "$here/../VERSION")}
case $(uname -m) in
	x86_64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
url=https://github.com/emdioh/things/releases/download/trains-$version

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "Downloading trains $version for linux/$arch"
wget -qO "$tmp/trains-linux-$arch" "$url/trains-linux-$arch"
wget -qO "$tmp/SHA256SUMS" "$url/SHA256SUMS"
(cd "$tmp" && grep " trains-linux-$arch\$" SHA256SUMS | sha256sum -c -)

sh "$here/install.sh" "$tmp/trains-linux-$arch"
