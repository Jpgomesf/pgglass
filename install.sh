#!/bin/sh
# Installs pgglass from a GitHub release on macOS or Linux, amd64 or arm64:
# downloads the archive for this platform, checks its SHA-256 against the
# release's checksums.txt and installs the binary only when they match.
#
#   curl -fsSL https://raw.githubusercontent.com/Jpgomesf/pgglass/main/install.sh | sh
#
# PGGLASS_VERSION       release to install, such as v1.0.0 (default: the latest)
# PGGLASS_INSTALL_DIR   directory to install into (default: ~/.local/bin)
# PGGLASS_RELEASES_URL  where the releases are (default: the GitHub releases)
set -eu

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

require() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

platform() {
	case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "unsupported system $(uname -s): pgglass runs on macOS and Linux" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "unsupported architecture $(uname -m): pgglass runs on amd64 and arm64" ;;
	esac
	echo "${os}_$arch"
}

latest_tag() {
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$releases/latest") ||
		fail "cannot find the latest release at $releases"
	case ${url##*/} in
	v[0-9]*) echo "${url##*/}" ;;
	*) fail "cannot find the latest release at $releases" ;;
	esac
}

download() {
	curl -fsSL -o "$tmp/$1" "$releases/download/$tag/$1" ||
		fail "cannot download $releases/download/$tag/$1"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum "$1")
	elif command -v shasum >/dev/null 2>&1; then
		sum=$(shasum -a 256 "$1")
	else
		fail "sha256sum or shasum is required to verify the download"
	fi
	echo "${sum%% *}"
}

verify() {
	want=$(awk -v name="$1" '$2 == name { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "checksums.txt of $tag lists no $1"
	got=$(sha256 "$tmp/$1")
	[ "$got" = "$want" ] || fail "checksum mismatch for $1: got $got, want $want"
}

main() {
	require curl
	require tar
	releases=${PGGLASS_RELEASES_URL:-https://github.com/Jpgomesf/pgglass/releases}
	dir=${PGGLASS_INSTALL_DIR:-$HOME/.local/bin}
	target=$(platform)
	tag=${PGGLASS_VERSION:-}
	[ -n "$tag" ] || tag=$(latest_tag)
	tag=v${tag#v}
	archive=pgglass_${tag#v}_$target.tar.gz

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	echo "Downloading pgglass $tag for $target"
	download "$archive"
	download checksums.txt
	verify "$archive"
	tar -xzf "$tmp/$archive" -C "$tmp" pgglass || fail "$archive holds no pgglass binary"
	mkdir -p "$dir" || fail "cannot create $dir"
	install -m 0755 "$tmp/pgglass" "$dir/pgglass" ||
		fail "cannot write $dir/pgglass: set PGGLASS_INSTALL_DIR to a directory you can write to"
	echo "Installed pgglass $tag in $dir/pgglass"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "Add $dir to your PATH to run pgglass by name." ;;
	esac
}

main
