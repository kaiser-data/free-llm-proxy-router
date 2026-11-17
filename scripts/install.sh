#!/bin/sh
# install.sh — download a free-llm-proxy-router release and verify its checksum.
#
#   curl -fsSL https://raw.githubusercontent.com/kaiser-data/free-llm-proxy-router/main/scripts/install.sh | sh
#
# Env:  VERSION=0.1.7   pin a release (default: latest)
#       DIR=./somewhere extract into this directory (default: current directory)
#
# POSIX sh, so it runs under busybox on small boards (Pi Zero, LicheeRV).
set -eu

REPO="kaiser-data/free-llm-proxy-router"
DIR="${DIR:-.}"

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
  final_url() { curl -fsSLI -o /dev/null -w '%{url_effective}' "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
  final_url() { wget -S --spider "$1" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1; }
else
  die "need curl or wget"
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS '$os' — on Windows use the .zip from https://github.com/$REPO/releases/latest" ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  armv7*)        arch=armv7 ;;
  armv6*)        arch=armv6 ;;
  riscv64)       arch=riscv64 ;;
  *) die "unsupported CPU '$(uname -m)'" ;;
esac
if [ "$os" = darwin ] && [ "$arch" != amd64 ] && [ "$arch" != arm64 ]; then
  die "no macOS build for $arch"
fi

version="${VERSION:-}"
if [ -z "$version" ]; then
  # /releases/latest redirects to /releases/tag/vX.Y.Z; no API token or jq needed.
  version=$(final_url "https://github.com/$REPO/releases/latest" | sed -n 's#.*/tag/v##p' | tr -d '\r')
  [ -n "$version" ] || die "could not resolve the latest release; set VERSION=x.y.z"
fi
version="${version#v}"

archive="free-llm-proxy-router_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/v$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $archive"
fetch "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: checksums.txt"

want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
else
  die "need sha256sum or shasum to verify the download"
fi
[ "$want" = "$got" ] || die "checksum mismatch for $archive (want $want, got $got)"
say "Checksum OK"

mkdir -p "$DIR"
tar xzf "$tmp/$archive" -C "$DIR"

say ""
say "Installed free-llm-proxy-router $version ($os/$arch) into $DIR"
say "  free-llm-proxy, free-llm-scan, free-llm-bench + configs/ (keep them together)"
say ""
say "Start it with one free Groq key (https://console.groq.com/keys):"
say "  cd $DIR && GROQ_API_KEY=gsk_... ./free-llm-proxy"
