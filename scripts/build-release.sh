#!/usr/bin/env bash
# Build the Community Edition release artefacts: one archive per platform
# holding the binary, the licence, the README and the changelog, the
# microVM driver's two binaries (abhed-microvm and abhed-guest) for Linux,
# with -s an SPDX SBOM beside each bare binary (needs syft), plus a checksum
# file over every abhed-* file in OUTDIR. What the release workflow runs;
# runs the same on a laptop. Signing happens in the workflow, which has the
# OIDC identity.
#
# macOS binaries are built with cgo, so they can use the system's own
# facilities such as os_log; that needs a Mac with Xcode's command line
# tools, so on another host leave darwin out of -p and build those on a Mac.
# Every other platform is built without cgo and is static.
#
#   scripts/build-release.sh [-v VERSION] [-o OUTDIR] [-p PLATFORMS] [-m MICROVM_PLATFORMS] [-s]
set -euo pipefail
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
OUTDIR="dist"
PLATFORMS="linux/amd64 linux/arm64 linux/s390x darwin/arm64 darwin/amd64"
# The microVM driver runs on Linux only. The guest kernel and root
# filesystem are not shipped: build-rootfs.sh's output is not reproducible
# (mkfs.ext4 stamps a fresh UUID and times), so operators build their own.
MICROVM_PLATFORMS="linux/amd64 linux/arm64 linux/s390x"
SBOM=0
while getopts "v:o:p:m:sh" opt; do
  case $opt in
    v) VERSION="$OPTARG" ;;
    o) OUTDIR="$OPTARG" ;;
    p) PLATFORMS="$OPTARG" ;;
    m) MICROVM_PLATFORMS="$OPTARG" ;;
    s) SBOM=1 ;;
    h) sed -n '2,10p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [ "$SBOM" = 1 ] && ! command -v syft >/dev/null; then
  echo "-s needs syft on PATH" >&2; exit 2
fi
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd)"

echo "Abhed ${VERSION}"
echo "  embedding documentation"
python3 "$ROOT/scripts/docsite/build.py" --embed-only >/dev/null

for platform in $PLATFORMS; do
  os="${platform%/*}"; arch="${platform#*/}"
  name="abhed-${VERSION}-${os}-${arch}"
  dir="$STAGE/$name"
  mkdir -p "$dir"
  cgo=(CGO_ENABLED=0)
  if [ "$os" = darwin ]; then
    if [ "$(uname -s)" != Darwin ]; then
      echo "darwin/$arch is built with cgo and needs a Mac; leave it out of -p here" >&2; exit 2
    fi
    # Either architecture from either Mac: clang targets the one asked for.
    case $arch in amd64) carch=x86_64 ;; *) carch=$arch ;; esac
    cgo=(CGO_ENABLED=1 "CC=clang -arch $carch" "MACOSX_DEPLOYMENT_TARGET=${MACOSX_DEPLOYMENT_TARGET:-12.0}")
  fi
  env "${cgo[@]}" GOOS="$os" GOARCH="$arch" go build \
    -trimpath -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
    -o "$dir/abhed" "$ROOT/cmd/abhed"
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$ROOT/CHANGELOG.md" "$dir/"
  tar -czf "$OUTDIR/$name.tar.gz" -C "$STAGE" "$name"
  # The bare binary too, for a one-line install with curl.
  cp "$dir/abhed" "$OUTDIR/$name"
  echo "  $name ($(du -h "$OUTDIR/$name.tar.gz" | cut -f1))"
done

# Bare binaries, beside the archives: abhed-microvm runs as root on the host,
# abhed-guest goes into the root filesystem build-rootfs.sh -g makes.
for platform in $MICROVM_PLATFORMS; do
  os="${platform%/*}"; arch="${platform#*/}"
  for cmd in abhed-microvm abhed-guest; do
    name="${cmd}-${VERSION}-${os}-${arch}"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
      -trimpath -ldflags "-s -w" -o "$OUTDIR/$name" "$ROOT/cmd/$cmd"
    echo "  $name"
  done
done

# Each SBOM is read from the binary's own build info, so it lists the modules
# actually linked rather than everything go.mod names.
if [ "$SBOM" = 1 ]; then
  for f in "$OUTDIR"/abhed-*; do
    case "$f" in *.tar.gz|*.spdx.json) continue ;; esac
    syft scan "file:$f" -q -o "spdx-json=$f.spdx.json"
  done
  echo "  SBOMs written"
fi

(
  cd "$OUTDIR"
  if command -v sha256sum >/dev/null; then sha256sum abhed-*; else shasum -a 256 abhed-*; fi > SHA256SUMS
)
echo "  SHA256SUMS written"
echo "Verify a download with: sha256sum -c SHA256SUMS --ignore-missing (a release also checks SHA256SUMS.sigstore.json; see docs/guide/01-getting-started.md)"
