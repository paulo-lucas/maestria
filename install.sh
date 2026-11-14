#!/usr/bin/env sh
# Maestria installer for macOS and Linux.
# Usage: curl -fsSL https://raw.githubusercontent.com/maestria-dev/maestria/main/install.sh | sh
set -eu

MAESTRIA_REPO="${MAESTRIA_REPO:-maestria-dev/maestria}"
MAESTRIA_VERSION="${MAESTRIA_VERSION:-latest}"
MAESTRIA_INSTALL_DIR="${MAESTRIA_INSTALL_DIR:-$HOME/.local/bin}"
MAESTRIA_BASE_URL="${MAESTRIA_BASE_URL:-}"

err() {
  printf 'maestria install: %s\n' "$*" >&2
  exit 1
}

detect_os() {
  case "$(uname -s)" in
    Darwin) printf 'darwin' ;;
    Linux) printf 'linux' ;;
    *) err "unsupported OS: $(uname -s) (use install.ps1 on Windows)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) printf 'amd64' ;;
    arm64 | aarch64) printf 'arm64' ;;
    *) err "unsupported architecture: $(uname -m)" ;;
  esac
}

fetch() {
  src="$1"
  dst="$2"
  case "$src" in
    file://*)
      cp "${src#file://}" "$dst" || err "failed to read $src"
      return 0
      ;;
  esac
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$src" -o "$dst" || err "failed to download $src"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$dst" "$src" || err "failed to download $src"
  else
    err "curl or wget is required"
  fi
}

resolve_latest_version() {
  latest_url="https://github.com/$MAESTRIA_REPO/releases/latest"
  if command -v curl >/dev/null 2>&1; then
    final_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$latest_url") || err "failed to resolve latest release from $latest_url"
    tag="${final_url##*/}"
  elif command -v wget >/dev/null 2>&1; then
    tag=$(wget -qO- "https://api.github.com/repos/$MAESTRIA_REPO/releases/latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  else
    err "curl or wget is required"
  fi
  [ -n "$tag" ] && [ "$tag" != "latest" ] || err "could not determine latest release tag"
  printf '%s' "$tag"
}

sha256_file() {
  target="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$target" | awk '{print tolower($1)}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$target" | awk '{print tolower($1)}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$target" | awk '{print tolower($NF)}'
  else
    err "sha256sum, shasum, or openssl is required to verify checksums"
  fi
}

os=$(detect_os)
arch=$(detect_arch)

if [ "$MAESTRIA_VERSION" = "latest" ]; then
  if [ -n "$MAESTRIA_BASE_URL" ]; then
    err "MAESTRIA_VERSION must be set explicitly when MAESTRIA_BASE_URL is overridden"
  fi
  version=$(resolve_latest_version)
else
  version="$MAESTRIA_VERSION"
fi

case "$version" in
  v*) tag="$version"; ver_clean="${version#v}" ;;
  *) tag="v$version"; ver_clean="$version" ;;
esac

archive="maestria_${ver_clean}_${os}_${arch}.tar.gz"
if [ -n "$MAESTRIA_BASE_URL" ]; then
  base_url="${MAESTRIA_BASE_URL%/}"
else
  base_url="https://github.com/$MAESTRIA_REPO/releases/download/$tag"
fi

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t maestria-install)
trap 'rm -rf "$tmpdir"' EXIT INT TERM

printf 'Downloading %s...\n' "$archive"
fetch "$base_url/$archive" "$tmpdir/$archive"
fetch "$base_url/SHA256SUMS" "$tmpdir/SHA256SUMS"

expected=$(awk -v f="$archive" '$2 == f || $2 == ("*" f) { print tolower($1); exit }' "$tmpdir/SHA256SUMS")
[ -n "$expected" ] || err "SHA256SUMS does not contain an entry for $archive"

actual=$(sha256_file "$tmpdir/$archive")
if [ "$actual" != "$expected" ]; then
  err "checksum mismatch for $archive (expected $expected, got $actual)"
fi
printf 'Verified SHA256 checksum (%s).\n' "$actual"

mkdir -p "$tmpdir/extract" "$MAESTRIA_INSTALL_DIR"
tar -xzf "$tmpdir/$archive" -C "$tmpdir/extract"
[ -f "$tmpdir/extract/maestria" ] || err "archive $archive did not contain maestria binary"

cp "$tmpdir/extract/maestria" "$MAESTRIA_INSTALL_DIR/maestria"
chmod 0755 "$MAESTRIA_INSTALL_DIR/maestria"

printf 'Installed maestria to %s/maestria\n' "$MAESTRIA_INSTALL_DIR"
case ":${PATH:-}:" in
  *":$MAESTRIA_INSTALL_DIR:"*) ;;
  *)
    printf 'Note: %s is not in your PATH. Add it with:\n' "$MAESTRIA_INSTALL_DIR"
    printf '  export PATH="%s:$PATH"\n' "$MAESTRIA_INSTALL_DIR"
    ;;
esac
printf 'Run "maestria init" to set up Maestria.\n'
