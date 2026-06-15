#!/bin/sh
set -eu

repo="${CAPSULE_REPO:-z2z23n0/agent-capsule}"
version="${CAPSULE_VERSION:-latest}"

die() {
  printf 'agent-capsule install: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

if [ -z "${INSTALL_DIR:-}" ]; then
  [ -n "${HOME:-}" ] || die "HOME is not set; set INSTALL_DIR explicitly"
  install_dir="$HOME/.local/bin"
else
  install_dir="$INSTALL_DIR"
fi

need_cmd curl
need_cmd uname
need_cmd sed
need_cmd awk
need_cmd head
need_cmd mkdir
need_cmd cp

os_name="$(uname -s)"
machine="$(uname -m)"

case "$os_name" in
  Darwin) goos="darwin" ;;
  Linux) goos="linux" ;;
  CYGWIN*|MINGW*|MSYS*) goos="windows" ;;
  *) die "unsupported OS: $os_name" ;;
esac

case "$machine" in
  x86_64|amd64) goarch="amd64" ;;
  arm64|aarch64) goarch="arm64" ;;
  *) die "unsupported architecture: $machine" ;;
esac

if [ "$goos" = "windows" ] && [ "$goarch" != "amd64" ]; then
  die "windows arm64 release archives are not published yet"
fi

if [ "$version" = "latest" ]; then
  latest_json="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest")" || die "could not resolve latest release"
  version="$(printf '%s\n' "$latest_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  [ -n "$version" ] || die "could not read latest release tag"
fi

case "$goos" in
  windows) archive_ext="zip"; binary="capsule.exe" ;;
  *) archive_ext="tar.gz"; binary="capsule" ;;
esac

archive="agent-capsule_${version}_${goos}_${goarch}.${archive_ext}"
base_url="https://github.com/$repo/releases/download/$version"
tmp="${TMPDIR:-/tmp}/agent-capsule-install.$$"

mkdir -p "$tmp"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

curl -fsSL "$base_url/checksums.txt" -o "$tmp/checksums.txt" || die "could not download checksums for $version"
curl -fsSL "$base_url/$archive" -o "$tmp/$archive" || die "could not download $archive"

expected="$(awk -v file="$archive" '$2 == file { print $1; found = 1 } END { if (!found) exit 1 }' "$tmp/checksums.txt")" || die "checksum entry missing for $archive"

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$archive" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')"
else
  die "missing sha256sum or shasum for checksum verification"
fi

[ "$actual" = "$expected" ] || die "checksum mismatch for $archive"

case "$archive" in
  *.tar.gz)
    need_cmd tar
    LC_ALL=C tar -xzf "$tmp/$archive" -C "$tmp" || die "could not extract $archive"
    ;;
  *.zip)
    need_cmd unzip
    unzip -q "$tmp/$archive" -d "$tmp" || die "could not extract $archive"
    ;;
esac

target_dir="$tmp/agent-capsule_${version}_${goos}_${goarch}"
[ -f "$target_dir/$binary" ] || die "release archive did not contain $binary"

if [ -n "${CAPSULE_BINARY:-}" ]; then
  target_name="$CAPSULE_BINARY"
elif [ "$goos" = "windows" ]; then
  target_name="capsule.exe"
else
  target_name="capsule"
fi

mkdir -p "$install_dir"
cp "$target_dir/$binary" "$install_dir/$target_name"
chmod 755 "$install_dir/$target_name" 2>/dev/null || true

printf 'Installed agent-capsule %s to %s\n' "$version" "$install_dir/$target_name"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to PATH before running capsule.\n' "$install_dir" ;;
esac
