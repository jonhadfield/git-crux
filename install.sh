#!/bin/sh
# Install git-crux from the latest GitHub release into PREFIX/bin (default:
# /usr/local/bin). Linux amd64 and arm64 only — on macOS use the signed .pkg
# or Homebrew instead (see README).
#
#   curl -fsSL https://raw.githubusercontent.com/jonhadfield/git-crux/main/install.sh | sh
#
# Optional env:
#   PREFIX            install root (default /usr/local)
#   GIT_CRUX_VERSION  tag to install, with or without leading v (default: latest)
#   GIT_CRUX_REPO     owner/name (default jonhadfield/git-crux)

set -eu

REPO="${GIT_CRUX_REPO:-jonhadfield/git-crux}"
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="${PREFIX}/bin"
BASE="https://github.com/${REPO}/releases"

need() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "git-crux install: need '$1' on PATH" >&2
		exit 1
	}
}

need curl
need tar
need mktemp

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$os" in
linux) ;;
darwin)
	echo "git-crux install: on macOS use Homebrew or the signed installer:" >&2
	echo "  brew install jonhadfield/tap/git-crux" >&2
	echo "  curl -fLO ${BASE}/latest/download/git-crux_macos.pkg" >&2
	echo "  sudo installer -pkg git-crux_macos.pkg -target /" >&2
	exit 1
	;;
*)
	echo "git-crux install: unsupported OS '$os' (linux only)" >&2
	exit 1
	;;
esac

case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*)
	echo "git-crux install: unsupported architecture '$arch' (amd64 and arm64 only)" >&2
	exit 1
	;;
esac

if [ -n "${GIT_CRUX_VERSION:-}" ]; then
	tag="${GIT_CRUX_VERSION#v}"
	tag="v${tag}"
	asset_base="${BASE}/download/${tag}"
else
	tag=latest
	asset_base="${BASE}/latest/download"
fi

archive="git-crux_linux_${arch}.tar.gz"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

echo "git-crux install: downloading ${archive} (${tag})"
curl -fsSL "${asset_base}/${archive}" -o "${tmpdir}/${archive}"
curl -fsSL "${asset_base}/checksums.txt" -o "${tmpdir}/checksums.txt"

# Verify the archive against the published checksums file.
expected=$(awk -v f="$archive" '$2 == f { print $1; exit }' "${tmpdir}/checksums.txt")
if [ -z "$expected" ]; then
	echo "git-crux install: no checksum entry for ${archive}" >&2
	exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "${tmpdir}/${archive}" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "${tmpdir}/${archive}" | awk '{ print $1 }')
else
	echo "git-crux install: need sha256sum or shasum to verify the download" >&2
	exit 1
fi
if [ "$got" != "$expected" ]; then
	echo "git-crux install: checksum mismatch for ${archive}" >&2
	echo "  expected: ${expected}" >&2
	echo "  got:      ${got}" >&2
	exit 1
fi

tar -xzf "${tmpdir}/${archive}" -C "$tmpdir" git-crux
chmod 755 "${tmpdir}/git-crux"

dest="${BIN_DIR}/git-crux"
if mkdir -p "$BIN_DIR" 2>/dev/null && [ -w "$BIN_DIR" ]; then
	mv "${tmpdir}/git-crux" "$dest"
else
	need sudo
	sudo mkdir -p "$BIN_DIR"
	sudo mv "${tmpdir}/git-crux" "$dest"
fi

echo "git-crux install: installed to ${dest}"
# Best-effort only: do not fail the install if this host cannot exec the binary.
"$dest" version 2>/dev/null || true
if command -v git-crux >/dev/null 2>&1; then
	echo "git-crux install: ready — try: git crux -m \"...\""
else
	echo "git-crux install: add ${BIN_DIR} to PATH if git-crux is not found"
fi
