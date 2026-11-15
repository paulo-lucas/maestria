#!/usr/bin/env sh
# Maestria installer for macOS and Linux.
# Usage: curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/paulo-lucas/maestria/main/install.sh | sh
#
# What it checks before it installs anything:
#   - cosign installed: the release signature on SHA256SUMS must verify against the identity of
#     this repository's release workflow for exactly this tag. Anything missing or failing aborts.
#   - cosign not installed: a loud warning, then the checksum is the only check (it comes from
#     the same release as the archive, so it catches corruption, not a malicious release).
#     MAESTRIA_REQUIRE_SIGNATURE=1 turns that warning into an error.
#
# Nothing below runs until the very last line: only functions are defined before main is called,
# so a download cut short mid-pipe executes nothing.
set -eu

err() {
  printf 'maestria install: %s\n' "$*" >&2
  exit 1
}

warn() {
  printf 'maestria install: warning: %s\n' "$*" >&2
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

# fetch URL FILE. https only (file:// is kept for tests and offline mirrors), TLS 1.2 or newer,
# and a redirect may not leave https either.
fetch() {
  src="$1"
  dst="$2"
  case "$src" in
    file://*)
      cp "${src#file://}" "$dst" || err "failed to read $src"
      return 0
      ;;
    https://*) ;;
    *) err "refusing to download over a non-https URL: $src" ;;
  esac
  if command -v curl >/dev/null 2>&1; then
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL "$src" -o "$dst" || err "failed to download $src"
  elif command -v wget >/dev/null 2>&1; then
    wget --https-only --secure-protocol=TLSv1_2 -qO "$dst" "$src" || err "failed to download $src (install curl if your wget lacks --https-only)"
  else
    err "curl or wget is required"
  fi
}

resolve_latest_version() {
  latest_url="https://github.com/$MAESTRIA_REPO/releases/latest"
  if command -v curl >/dev/null 2>&1; then
    final_url=$(curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o /dev/null -w '%{url_effective}' "$latest_url") || err "failed to resolve latest release from $latest_url"
    tag="${final_url##*/}"
  elif command -v wget >/dev/null 2>&1; then
    tag=$(wget --https-only --secure-protocol=TLSv1_2 -qO- "https://api.github.com/repos/$MAESTRIA_REPO/releases/latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
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

# verify_signature SUMS_FILE BUNDLE_URL BUNDLE_FILE TAG
# The identity is exact: this repository's release workflow, run for this very tag, on a push,
# in this repository. Pinning the tag means another release's signed SHA256SUMS cannot be swapped
# in, and the repository and trigger claims mean a workflow that merely shares our file name in
# another repository does not count. The repository is MAESTRIA_REPO, the user's own choice;
# MAESTRIA_BASE_URL (a mirror) changes where files come from, never who must have signed them.
verify_signature() {
  sums="$1"
  bundle_url="$2"
  bundle="$3"
  sig_tag="$4"
  if ! command -v cosign >/dev/null 2>&1; then
    if [ "${MAESTRIA_REQUIRE_SIGNATURE:-}" = "1" ]; then
      err "cosign is required (MAESTRIA_REQUIRE_SIGNATURE=1) but was not found; install it from https://docs.sigstore.dev/cosign/system_config/installation/"
    fi
    warn "cosign was not found, so the release signature was NOT checked."
    warn "Only the checksum from the same release is verified, which does not prove who built it."
    warn "To check by hand, with cosign: cosign verify-blob --bundle SHA256SUMS.sigstore.json --certificate-identity https://github.com/$MAESTRIA_REPO/.github/workflows/release.yml@refs/tags/$sig_tag --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-github-workflow-repository $MAESTRIA_REPO --certificate-github-workflow-trigger push SHA256SUMS"
    warn "Or with the GitHub CLI, on the archive: gh attestation verify <archive> -R $MAESTRIA_REPO --signer-workflow $MAESTRIA_REPO/.github/workflows/release.yml"
    return 0
  fi
  printf 'Verifying the release signature with cosign...\n'
  fetch "$bundle_url" "$bundle"
  if ! cosign verify-blob \
    --bundle "$bundle" \
    --certificate-identity "https://github.com/$MAESTRIA_REPO/.github/workflows/release.yml@refs/tags/$sig_tag" \
    --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
    --certificate-github-workflow-repository "$MAESTRIA_REPO" \
    --certificate-github-workflow-trigger push \
    "$sums"; then
    cosign_version=$(cosign version 2>/dev/null | sed -n 's/^GitVersion:[[:space:]]*//p' | head -n 1)
    warn "installed cosign: ${cosign_version:-unknown version}"
    err "the signature on SHA256SUMS did not verify; nothing was installed. If the message above is about the bundle format, upgrade cosign (releases are signed with cosign v3) and run this again"
  fi
  printf 'Verified the release signature (built by %s release workflow for %s).\n' "$MAESTRIA_REPO" "$sig_tag"
}

main() {
  MAESTRIA_REPO="${MAESTRIA_REPO:-paulo-lucas/maestria}"
  MAESTRIA_VERSION="${MAESTRIA_VERSION:-latest}"
  MAESTRIA_INSTALL_DIR="${MAESTRIA_INSTALL_DIR:-$HOME/.local/bin}"
  MAESTRIA_BASE_URL="${MAESTRIA_BASE_URL:-}"

  # owner/name only, because it becomes part of a URL and of the signing identity.
  case "$MAESTRIA_REPO" in
    */*/* | /* | */ | "" | *[!A-Za-z0-9._/-]*) err "MAESTRIA_REPO must look like owner/name" ;;
    */*) ;;
    *) err "MAESTRIA_REPO must look like owner/name" ;;
  esac

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
  case "$tag" in
    *[!A-Za-z0-9._+-]*) err "unexpected characters in the version: $tag" ;;
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

  # The signature comes first: no line of SHA256SUMS is trusted until it has verified.
  verify_signature "$tmpdir/SHA256SUMS" "$base_url/SHA256SUMS.sigstore.json" "$tmpdir/SHA256SUMS.sigstore.json" "$tag"

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
}

main "$@"
