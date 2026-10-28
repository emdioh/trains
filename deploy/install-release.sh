#!/bin/sh
# Installs or updates trains from a GitHub release, for this machine's
# architecture. Run as root:
#   sh deploy/install-release.sh [v0.1.0 | latest]
# With no version it installs the one named in VERSION, so run it from a
# clone of this repository, e.g. as the dashboard's deploy step. "latest"
# installs the newest release and works from a copy of this script alone.
# The release archive is checked against SHA256SUMS, unpacked and installed
# with its own deploy/install.sh; the downloads are removed afterwards.
set -eu

repo=emdioh/trains
here=$(dirname "$0")
version=${1:-}
if [ -z "$version" ]; then
	version=$(tr -d '[:space:]' < "$here/../VERSION")
elif [ "$version" = latest ]; then
	version=$(wget -qO- "https://api.github.com/repos/$repo/releases/latest" |
		sed -n 's/.*"tag_name": *"trains-\([^"]*\)".*/\1/p')
	if [ -z "$version" ]; then
		echo "could not find the latest release of $repo" >&2
		exit 1
	fi
fi
case $(uname -m) in
	x86_64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
url=https://github.com/$repo/releases/download/trains-$version
archive=trains-$version-linux-$arch.tar.gz

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "Downloading trains $version for linux/$arch"
wget -qO "$tmp/$archive" "$url/$archive"
wget -qO "$tmp/SHA256SUMS" "$url/SHA256SUMS"
(cd "$tmp" && grep " $archive\$" SHA256SUMS | sha256sum -c -)
tar -xzf "$tmp/$archive" -C "$tmp"

sh "$tmp/deploy/install.sh" "$tmp/trains-linux-$arch"
