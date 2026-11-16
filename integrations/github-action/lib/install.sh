#!/usr/bin/env bash
# Installs a pinned Abhed release archive, checked against a SHA-256 before
# anything in it is run, and puts abhed on PATH.
#
# In:  ABHED_VERSION (empty: the pinned version), ABHED_SHA256 (needed for any
#      other version), ABHED_PATH (an abhed already on the runner: skip the
#      download), ABHED_DOWNLOAD_BASE (default the project's releases),
#      ABHED_INSTALL_DIR, RUNNER_OS, RUNNER_ARCH.
# Out: the install dir on GITHUB_PATH; "path", "version" and "archive" on GITHUB_OUTPUT.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out() { [ -n "${GITHUB_OUTPUT:-}" ] && printf '%s=%s\n' "$1" "$2" >> "$GITHUB_OUTPUT"; return 0; }
fail() { echo "::error::abhed install: $*" >&2; exit 1; }

if [ -n "${ABHED_PATH:-}" ]; then
  [ -x "$ABHED_PATH" ] || fail "abhed-path $ABHED_PATH is not an executable file"
  dir="$(cd "$(dirname "$ABHED_PATH")" && pwd)"
  bin="$dir/$(basename "$ABHED_PATH")"
  [ -n "${GITHUB_PATH:-}" ] && echo "$dir" >> "$GITHUB_PATH"
  out path "$bin"
  out version "$("$bin" -version 2>/dev/null | head -1 || true)"
  exit 0
fi

case "${RUNNER_OS:-$(uname -s)}" in
  Linux) os=linux ;;
  macOS|Darwin) os=darwin ;;
  *) fail "Abhed releases are built for Linux and macOS runners, not ${RUNNER_OS:-$(uname -s)}" ;;
esac
case "${RUNNER_ARCH:-$(uname -m)}" in
  X64|x86_64|amd64) arch=amd64 ;;
  ARM64|arm64|aarch64) arch=arm64 ;;
  *) fail "no Abhed release for the ${RUNNER_ARCH:-$(uname -m)} architecture" ;;
esac

pinned="$(awk '$1 == "version" {print $2}' "$here/release.txt")"
version="${ABHED_VERSION:-$pinned}"
case "$version" in v*) ;; *) version="v$version" ;; esac
case "$version" in
  *[!A-Za-z0-9._+-]*) fail "abhed-version $version is not a version" ;;
esac
want="${ABHED_SHA256:-}"
if [ -z "$want" ]; then
  [ "$version" = "$pinned" ] || fail "abhed-version $version is not the one this action pins ($pinned); give its archive's SHA-256 as abhed-sha256"
  want="$(awk -v k="$os-$arch" '$1 == k {print $2}' "$here/release.txt")"
fi
want="$(printf '%s' "$want" | tr 'A-F' 'a-f')"
[[ "$want" =~ ^[0-9a-f]{64}$ ]] || fail "the expected SHA-256 for $os-$arch is not 64 hex digits"

name="abhed-$version-$os-$arch"
base="${ABHED_DOWNLOAD_BASE:-https://github.com/zybuu-ai/abhed/releases/download}"
dest="${ABHED_INSTALL_DIR:-${RUNNER_TEMP:-/tmp}/abhed-install}"
rm -rf "$dest" && mkdir -p "$dest"
archive="$dest/$name.tar.gz"
curl -fsSL --proto '=https,file' --retry 3 -o "$archive" "$base/$version/$name.tar.gz" || fail "could not download $name.tar.gz"

if command -v sha256sum >/dev/null; then got="$(sha256sum "$archive" | awk '{print $1}')"
else got="$(shasum -a 256 "$archive" | awk '{print $1}')"; fi
[ "$got" = "$want" ] || { rm -f "$archive"; fail "$name.tar.gz has SHA-256 $got, not the pinned $want; nothing was run"; }

# The release's own signature too, when asked: the checksum file must be
# signed by this project's release workflow on that tag, and list the archive.
if [ "${ABHED_VERIFY_SIGNATURE:-false}" = "true" ]; then
  command -v cosign >/dev/null || fail "verify-signature needs cosign on PATH"
  curl -fsSL --proto '=https,file' --retry 3 -o "$dest/SHA256SUMS" "$base/$version/SHA256SUMS" || fail "could not download SHA256SUMS"
  curl -fsSL --proto '=https,file' --retry 3 -o "$dest/SHA256SUMS.sigstore.json" "$base/$version/SHA256SUMS.sigstore.json" || fail "could not download SHA256SUMS.sigstore.json (releases before v1.2.8 are not signed)"
  cosign verify-blob "$dest/SHA256SUMS" --bundle "$dest/SHA256SUMS.sigstore.json" \
    --certificate-identity "https://github.com/${ABHED_RELEASE_REPO:-zybuu-ai/abhed}/.github/workflows/release.yml@refs/tags/$version" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null || fail "the signature over SHA256SUMS did not verify"
  grep -qxF "$got  $name.tar.gz" "$dest/SHA256SUMS" || fail "the signed SHA256SUMS does not list $name.tar.gz with SHA-256 $got"
  echo "signature over SHA256SUMS verified"
fi

tar -xzf "$archive" -C "$dest" --no-same-owner "$name/abhed" $(tar -tzf "$archive" | grep -E "^$name/abhed-helper-" || true)
bin="$dest/$name/abhed"
[ -x "$bin" ] || fail "the archive holds no $name/abhed"
[ -n "${GITHUB_PATH:-}" ] && echo "$dest/$name" >> "$GITHUB_PATH"
out path "$bin"
out version "$version"
out archive "$archive"
echo "abhed $version ($os-$arch), SHA-256 $got"
