#!/bin/sh
# Install the latest msu-collect release from GitHub.
#
# Usage:
#   curl -sSLf https://github.com/andrei-zededa/monitor-system-usage/releases/latest/download/install.sh | sh
#
# Environment overrides:
#   PREFIX   install directory (default: /usr/local/bin)
#   VERSION  release tag to install (default: latest)

set -eu

REPO="andrei-zededa/monitor-system-usage"
PREFIX="${PREFIX:-/usr/local/bin}"
VERSION="${VERSION:-}"

err() { echo "install.sh: $*" >&2; exit 1; }

case "$(uname -s)" in
	Linux) os="linux" ;;
	*) err "msu-collect is only published for linux (got $(uname -s))" ;;
esac

case "$(uname -m)" in
	x86_64|amd64) arch="amd64" ;;
	*) err "msu-collect is only published for amd64 (got $(uname -m))" ;;
esac

command -v curl >/dev/null 2>&1 || err "curl is required"
command -v sha256sum >/dev/null 2>&1 || err "sha256sum is required"

if [ -z "$VERSION" ]; then
	# Resolve the latest tag by following the redirect of /releases/latest.
	# Avoids the GitHub API rate limits that can hit unauthenticated curl|sh runs.
	location=$(curl -sSLI -o /dev/null -w '%{url_effective}' \
		"https://github.com/${REPO}/releases/latest") \
		|| err "could not query latest release"
	VERSION="${location##*/}"
	[ -n "$VERSION" ] && [ "$VERSION" != "latest" ] || err "could not determine latest version"
fi

# Tag is like "v0.0.4"; asset filenames embed the bare version.
ver_no_v="${VERSION#v}"

bin_name="msu-collect_${ver_no_v}_${os}_${arch}"
sums_name="monitor-system-usage_${ver_no_v}_SHA256SUMS"
base_url="https://github.com/${REPO}/releases/download/${VERSION}"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

echo "Downloading msu-collect ${VERSION} (${os}/${arch})..."
curl -sSLf --fail-early -o "${tmpdir}/${bin_name}" "${base_url}/${bin_name}"
curl -sSLf --fail-early -o "${tmpdir}/${sums_name}" "${base_url}/${sums_name}"

echo "Verifying checksum..."
(cd "$tmpdir" && grep " ${bin_name}\$" "$sums_name" | sha256sum -c -) \
	|| err "checksum verification failed"

if [ -w "$PREFIX" ] || [ "$(id -u)" = "0" ]; then
	sudo_cmd=""
else
	command -v sudo >/dev/null 2>&1 || err "${PREFIX} is not writable and sudo is unavailable"
	sudo_cmd="sudo"
fi

dest="${PREFIX}/msu-collect"
echo "Installing to ${dest}..."
$sudo_cmd install -m 0755 -o root -g root "${tmpdir}/${bin_name}" "$dest" 2>/dev/null \
	|| $sudo_cmd install -m 0755 "${tmpdir}/${bin_name}" "$dest"

echo "Installed: $("$dest" --version 2>/dev/null || echo "$dest")"
